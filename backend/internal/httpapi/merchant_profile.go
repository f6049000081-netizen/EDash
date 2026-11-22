package httpapi

import (
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/google/uuid"

	"superapp/backend/internal/merchants"
	"superapp/backend/internal/money"
	"superapp/backend/internal/platform"
)

type merchantJSON struct {
	ID             uuid.UUID  `json:"id"`
	TradeName      string     `json:"trade_name"`
	LegalName      string     `json:"legal_name,omitempty"`
	TIN            string     `json:"tin,omitempty"`
	Licence        string     `json:"business_licence,omitempty"`
	CategoryCode   string     `json:"category_code"`
	City           string     `json:"city"`
	Status         string     `json:"status"`
	MerchantNo     *string    `json:"merchant_no"`
	Role           string     `json:"role"`
	PayoutSchedule string     `json:"payout_schedule"`
	CreatedAt      time.Time  `json:"created_at"`
	ApprovedAt     *time.Time `json:"approved_at"`
}

func toMerchantJSON(m *merchants.Merchant) merchantJSON {
	j := merchantJSON{ID: m.ID, TradeName: m.TradeName, LegalName: m.LegalName, TIN: m.TIN, Licence: m.Licence,
		CategoryCode: m.Category, City: m.City, Status: m.Status, Role: m.Role, PayoutSchedule: m.PayoutPlan,
		CreatedAt: m.CreatedAt, ApprovedAt: m.ApprovedAt}
	if m.MerchantNo != "" {
		no := m.MerchantNo
		j.MerchantNo = &no
	}
	return j
}

func (s *Server) applyMerchant(w http.ResponseWriter, r *http.Request) {
	var body struct {
		LegalName string `json:"legal_name"`
		TradeName string `json:"trade_name"`
		TIN       string `json:"tin"`
		Licence   string `json:"business_licence"`
		Category  string `json:"category_code"`
		City      string `json:"city"`
	}
	if !decode(w, r, &body) {
		return
	}
	m, err := s.Merchants.Apply(r.Context(), platform.UserFrom(r.Context()), merchants.Application{
		LegalName: body.LegalName, TradeName: body.TradeName, TIN: body.TIN, Licence: body.Licence, Category: body.Category, City: body.City})
	if err != nil {
		s.failMerchants(w, err)
		return
	}
	platform.WriteJSON(w, http.StatusCreated, toMerchantJSON(m))
}

func (s *Server) myMerchants(w http.ResponseWriter, r *http.Request) {
	list, err := s.Merchants.Mine(r.Context(), platform.UserFrom(r.Context()))
	if err != nil {
		s.failMerchants(w, err)
		return
	}
	items := make([]merchantJSON, 0, len(list))
	for i := range list {
		items = append(items, toMerchantJSON(&list[i]))
	}
	platform.WriteJSON(w, http.StatusOK, map[string]any{"items": items})
}

func (s *Server) getMerchant(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	m, err := s.Merchants.Get(r.Context(), platform.UserFrom(r.Context()), id)
	if err != nil {
		s.failMerchants(w, err)
		return
	}
	platform.WriteJSON(w, http.StatusOK, toMerchantJSON(m))
}

func (s *Server) merchantSummary(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	sum, err := s.Merchants.Summary(r.Context(), platform.UserFrom(r.Context()), id)
	if err != nil {
		s.failMerchants(w, err)
		return
	}
	out := map[string]any{"sales_today": sum.SalesToday, "payments_today": sum.PaymentsToday,
		"average_basket": sum.AverageBasket, "pending_payout": nil, "next_payout": nil}
	if sum.Pending.Currency != "" {
		out["pending_payout"] = sum.Pending
	}
	if np := sum.NextPayout; np != nil {
		var reason *string
		if np.HoldReason != "" {
			reason = &np.HoldReason
		}
		out["next_payout"] = map[string]any{"id": np.ID, "net": np.Net, "status": np.Status, "hold_reason": reason,
			"scheduled_for": np.ScheduledFor}
	}
	platform.WriteJSON(w, http.StatusOK, out)
}

func (s *Server) merchantPayments(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	limit := 30
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 100 {
			platform.WriteProblem(w, platform.NewProblem(http.StatusUnprocessableEntity, "validation_failed", "Check the details", "limit must be 1 to 100"))
			return
		}
		limit = n
	}
	var before *time.Time
	beforeID := uuid.Nil
	if c := q.Get("cursor"); c != "" {
		t, cid, err := decodeCursor(c)
		if err != nil {
			platform.WriteProblem(w, platform.NewProblem(http.StatusUnprocessableEntity, "validation_failed", "Check the details",
				"That page link is no longer valid. Reload the list."))
			return
		}
		before, beforeID = &t, cid
	}
	list, err := s.Merchants.Payments(r.Context(), platform.UserFrom(r.Context()), id, before, beforeID, limit+1)
	if err != nil {
		s.failMerchants(w, err)
		return
	}
	var next any
	if len(list) > limit {
		list = list[:limit]
		last := list[limit-1]
		next = encodeCursor(last.CompletedAt, last.ID)
	}
	type item struct {
		ID          uuid.UUID   `json:"id"`
		PayerName   string      `json:"payer_name"`
		Amount      money.Money `json:"amount"`
		CompletedAt time.Time   `json:"completed_at"`
	}
	items := make([]item, 0, len(list))
	for _, p := range list {
		items = append(items, item{p.ID, p.PayerName, p.Amount, p.CompletedAt})
	}
	platform.WriteJSON(w, http.StatusOK, map[string]any{"items": items, "next_cursor": next})
}

func (s *Server) merchantQR(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	payload, m, err := s.Merchants.QR(r.Context(), platform.UserFrom(r.Context()), id)
	if err != nil {
		s.failMerchants(w, err)
		return
	}
	platform.WriteJSON(w, http.StatusOK, map[string]any{"payload": payload, "merchant_no": m.MerchantNo, "trade_name": m.TradeName})
}

func (s *Server) failMerchants(w http.ResponseWriter, err error) {
	var v *merchants.ValidationError
	var c *merchants.ConflictError
	switch {
	case errors.As(err, &v):
		platform.WriteProblem(w, platform.NewProblem(http.StatusUnprocessableEntity, "validation_failed", "Check the details", v.Msg))
	case errors.As(err, &c):
		platform.WriteProblem(w, platform.NewProblem(http.StatusConflict, "conflict", "Can't apply now", c.Msg))
	case errors.Is(err, merchants.ErrNotFound):
		platform.WriteProblem(w, platform.NewProblem(http.StatusNotFound, "not_found", "Not found", ""))
	case errors.Is(err, merchants.ErrNotActive):
		platform.WriteProblem(w, platform.NewProblem(http.StatusConflict, "merchant_not_active", "Not approved yet",
			"Your shop can take payments once its application is approved."))
	case errors.Is(err, merchants.ErrForbidden):
		platform.WriteProblem(w, platform.NewProblem(http.StatusForbidden, "permission_denied", "Not allowed",
			"Your role at this shop doesn't allow this. Ask the shop's owner."))
	default:
		s.fail(w, err)
	}
}
