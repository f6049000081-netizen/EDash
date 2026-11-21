package httpapi

import (
	"errors"
	"io"
	"net/http"

	"superapp/backend/internal/money"
	"superapp/backend/internal/payments"
	"superapp/backend/internal/platform"
)

// NotificationVerifier checks a partner's signed notification and returns our order id from it.
type NotificationVerifier interface {
	VerifyNotification(body []byte) (orderID string, err error)
}

func (s *Server) startTopUp(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Amount money.Money `json:"amount"`
		Rail   string      `json:"rail"`
	}
	if !decode(w, r, &body) {
		return
	}
	in, checkout, err := s.Payments.StartTopUp(r.Context(), platform.UserFrom(r.Context()), body.Amount, body.Rail)
	if err != nil {
		s.fail(w, err)
		return
	}
	platform.WriteJSON(w, http.StatusCreated, map[string]any{
		"intent":       present(in),
		"checkout_url": checkout.CheckoutURL,
	})
}

// railNotification receives a partner's payment notification. The signature is checked first; then
// the top-up is settled by asking the partner directly, so the body itself never credits anything.
// Answers: 200 when handled (or not ours to handle, so the partner stops retrying), 401 for a bad
// signature, 503 when the partner couldn't be reached and the notification should be retried.
func (s *Server) railNotification(w http.ResponseWriter, r *http.Request) {
	rail := r.PathValue("rail")
	v := s.Webhooks[rail]
	if v == nil {
		platform.WriteProblem(w, platform.NewProblem(http.StatusNotFound, "not_found", "Not found", ""))
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 64<<10))
	if err != nil {
		platform.WriteProblem(w, platform.NewProblem(http.StatusBadRequest, "validation_failed", "Unreadable body", ""))
		return
	}
	orderID, err := v.VerifyNotification(body)
	if err != nil {
		s.Log.Warn("rejected rail notification", "rail", rail, "err", err)
		platform.WriteProblem(w, platform.NewProblem(http.StatusUnauthorized, "unauthenticated", "Bad signature", ""))
		return
	}
	ack := map[string]any{"success": true}
	id, err := payments.ParseOrderID(orderID)
	if err != nil {
		s.Log.Warn("rail notification for an unknown order", "rail", rail, "order", orderID)
		platform.WriteJSON(w, http.StatusOK, ack)
		return
	}
	in, err := s.Payments.SettleTopUp(r.Context(), id)
	switch {
	case errors.Is(err, payments.ErrNotFound):
		s.Log.Warn("rail notification for an unknown order", "rail", rail, "order", orderID)
	case err != nil:
		s.Log.Error("settle top-up", "rail", rail, "intent", id, "err", err)
		platform.WriteProblem(w, platform.NewProblem(http.StatusServiceUnavailable, "rail_unavailable", "Try again", ""))
		return
	default:
		s.Log.Info("rail notification handled", "rail", rail, "intent", id, "status", in.Status)
	}
	platform.WriteJSON(w, http.StatusOK, ack)
}
