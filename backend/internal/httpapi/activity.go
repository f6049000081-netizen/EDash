package httpapi

import (
	"net/http"
	"strconv"
	"time"

	"superapp/backend/internal/money"
	"superapp/backend/internal/payments"
	"superapp/backend/internal/platform"
)

func (s *Server) listActivity(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	limit := 25
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 100 {
			platform.WriteProblem(w, platform.NewProblem(http.StatusUnprocessableEntity, "validation_failed", "Check the details", "limit must be 1 to 100"))
			return
		}
		limit = n
	}
	page, err := s.Payments.Activity(r.Context(), platform.UserFrom(r.Context()), q.Get("filter"), q.Get("cursor"), limit)
	if err != nil {
		s.fail(w, err)
		return
	}
	type item struct {
		ID          string      `json:"id"`
		Type        string      `json:"type"`
		Title       string      `json:"title"`
		Subtitle    string      `json:"subtitle"`
		Direction   string      `json:"direction"`
		Amount      money.Money `json:"amount"`
		Status      string      `json:"status"`
		Charged     *bool       `json:"charged,omitempty"`
		FailureCode string      `json:"failure_code,omitempty"`
		OccurredAt  time.Time   `json:"occurred_at"`
	}
	items := make([]item, 0, len(page.Items))
	for _, it := range page.Items {
		items = append(items, item{ID: it.ID.String(), Type: it.Type, Title: it.Title, Subtitle: it.Subtitle,
			Direction: it.Direction, Amount: it.Amount, Status: it.Status, Charged: it.Charged,
			FailureCode: it.FailureCode, OccurredAt: it.OccurredAt})
	}
	var next any
	if page.NextCursor != "" {
		next = page.NextCursor
	}
	platform.WriteJSON(w, http.StatusOK, map[string]any{"items": items, "next_cursor": next})
}

func limitMessage(e *payments.LimitError) string {
	switch e.Scope {
	case "daily":
		return "This is over your daily sending limit. You can send up to " + e.Remaining.String() + " more today."
	case "monthly":
		return "This is over your monthly sending limit. You can send up to " + e.Remaining.String() + " more this month."
	case "recipient":
		return "The recipient can't receive this amount right now. Nothing was taken from your account."
	default:
		return "This would take your balance over your limit. You can add up to " + e.Remaining.String() + "."
	}
}
