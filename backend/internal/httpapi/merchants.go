package httpapi

import (
	"encoding/base64"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"superapp/backend/internal/money"
	"superapp/backend/internal/payouts"
	"superapp/backend/internal/platform"
)

type payoutJSON struct {
	ID               uuid.UUID   `json:"id"`
	PeriodStart      time.Time   `json:"period_start"`
	PeriodEnd        time.Time   `json:"period_end"`
	Gross            money.Money `json:"gross"`
	Commission       money.Money `json:"commission"`
	Fees             money.Money `json:"fees"`
	Refunds          money.Money `json:"refunds"`
	Net              money.Money `json:"net"`
	Status           string      `json:"status"`
	HoldReason       *string     `json:"hold_reason"`
	DestinationLabel string      `json:"destination_label"`
	ScheduledFor     time.Time   `json:"scheduled_for"`
	SettledAt        *time.Time  `json:"settled_at"`
}

func (s *Server) listPayouts(w http.ResponseWriter, r *http.Request) {
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
	list, err := s.Payouts.List(r.Context(), platform.UserFrom(r.Context()), id, before, beforeID, limit+1)
	if err != nil {
		s.failPayouts(w, err)
		return
	}
	var next any
	if len(list) > limit {
		list = list[:limit]
		last := list[limit-1]
		next = encodeCursor(last.CreatedAt, last.ID)
	}
	items := make([]payoutJSON, 0, len(list))
	for _, p := range list {
		j := payoutJSON{ID: p.ID, PeriodStart: p.PeriodStart, PeriodEnd: p.PeriodEnd, Gross: p.Gross, Commission: p.Commission,
			Fees: p.Fees, Refunds: p.Refunds, Net: p.Net, Status: string(p.Status), DestinationLabel: p.DestinationName,
			ScheduledFor: p.ScheduledFor, SettledAt: p.SettledAt}
		if p.HoldReason != "" {
			reason := p.HoldReason
			j.HoldReason = &reason
		}
		items = append(items, j)
	}
	platform.WriteJSON(w, http.StatusOK, map[string]any{"items": items, "next_cursor": next})
}

func (s *Server) getPayoutDestination(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	d, err := s.Payouts.CurrentDestination(r.Context(), platform.UserFrom(r.Context()), id)
	if err != nil {
		s.failPayouts(w, err)
		return
	}
	if d == nil {
		platform.WriteProblem(w, platform.NewProblem(http.StatusNotFound, "not_found", "No payout account", "Add a payout account to receive payouts."))
		return
	}
	s.writeDestination(w, d)
}

func (s *Server) putPayoutDestination(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	var body struct {
		Rail     string `json:"rail"`
		BankCode string `json:"bank_code"`
		Account  string `json:"account"`
		PIN      string `json:"pin"`
	}
	if !decode(w, r, &body) {
		return
	}
	d, err := s.Payouts.SetDestination(r.Context(), platform.UserFrom(r.Context()), id, body.Rail, body.BankCode, body.Account, body.PIN)
	if err != nil {
		s.failPayouts(w, err)
		return
	}
	s.writeDestination(w, d)
}

func (s *Server) writeDestination(w http.ResponseWriter, d *payouts.Destination) {
	platform.WriteJSON(w, http.StatusOK, map[string]any{
		"id": d.ID, "rail": d.Rail, "label": d.Label, "created_at": d.CreatedAt,
		"payouts_from": d.CreatedAt.Add(s.Payouts.CoolingOff),
	})
}

func (s *Server) failPayouts(w http.ResponseWriter, err error) {
	var v *payouts.ValidationError
	switch {
	case errors.As(err, &v):
		platform.WriteProblem(w, platform.NewProblem(http.StatusUnprocessableEntity, "validation_failed", "Check the details", v.Msg))
	case errors.Is(err, payouts.ErrNotFound):
		platform.WriteProblem(w, platform.NewProblem(http.StatusNotFound, "not_found", "Not found", ""))
	case errors.Is(err, payouts.ErrForbidden):
		platform.WriteProblem(w, platform.NewProblem(http.StatusForbidden, "permission_denied", "Not allowed",
			"Your role at this shop doesn't allow this. Ask the shop's owner."))
	default:
		s.fail(w, err) // PIN errors and everything else
	}
}

func encodeCursor(t time.Time, id uuid.UUID) string {
	return base64.RawURLEncoding.EncodeToString([]byte(strconv.FormatInt(t.UnixNano(), 10) + "." + id.String()))
}

func decodeCursor(c string) (time.Time, uuid.UUID, error) {
	raw, err := base64.RawURLEncoding.DecodeString(c)
	if err != nil {
		return time.Time{}, uuid.Nil, err
	}
	ns, idStr, ok := strings.Cut(string(raw), ".")
	if !ok {
		return time.Time{}, uuid.Nil, errors.New("bad cursor")
	}
	n, err := strconv.ParseInt(ns, 10, 64)
	if err != nil {
		return time.Time{}, uuid.Nil, err
	}
	id, err := uuid.Parse(idStr)
	return time.Unix(0, n).UTC(), id, err
}
