package httpapi

import (
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/google/uuid"

	"superapp/backend/internal/notify"
	"superapp/backend/internal/platform"
)

func (s *Server) listNotifications(w http.ResponseWriter, r *http.Request) {
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
	page, err := s.Notify.List(r.Context(), platform.UserFrom(r.Context()), q.Get("cursor"), limit)
	if err != nil {
		s.failNotify(w, err)
		return
	}
	type item struct {
		ID        uuid.UUID         `json:"id"`
		Kind      string            `json:"kind"`
		Title     string            `json:"title"`
		Body      string            `json:"body"`
		Data      map[string]string `json:"data"`
		CreatedAt time.Time         `json:"created_at"`
		Read      bool              `json:"read"`
	}
	items := make([]item, 0, len(page.Items))
	for _, n := range page.Items {
		items = append(items, item{n.ID, n.Kind, n.Title, n.Body, n.Data, n.CreatedAt, n.Read})
	}
	var next any
	if page.NextCursor != "" {
		next = page.NextCursor
	}
	platform.WriteJSON(w, http.StatusOK, map[string]any{"items": items, "next_cursor": next, "unread": page.Unread})
}

// markNotificationsRead is naturally idempotent (read stays read), so it needs no Idempotency-Key.
func (s *Server) markNotificationsRead(w http.ResponseWriter, r *http.Request) {
	var body struct {
		IDs []uuid.UUID `json:"ids"`
		All bool        `json:"all"`
	}
	if !decode(w, r, &body) {
		return
	}
	if len(body.IDs) == 0 && !body.All {
		platform.WriteProblem(w, platform.NewProblem(http.StatusUnprocessableEntity, "validation_failed", "Check the details",
			`Send "ids", or "all": true to mark everything read.`))
		return
	}
	if len(body.IDs) > 200 {
		platform.WriteProblem(w, platform.NewProblem(http.StatusUnprocessableEntity, "validation_failed", "Check the details", "At most 200 ids at a time."))
		return
	}
	n, err := s.Notify.MarkRead(r.Context(), platform.UserFrom(r.Context()), body.IDs)
	if err != nil {
		s.failNotify(w, err)
		return
	}
	platform.WriteJSON(w, http.StatusOK, map[string]any{"updated": n})
}

func (s *Server) putPushToken(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Token string `json:"token"`
	}
	if !decode(w, r, &body) {
		return
	}
	p := platform.PrincipalFrom(r.Context())
	if err := s.Notify.RegisterPushToken(r.Context(), p.UserID, p.DeviceID, body.Token); err != nil {
		s.failNotify(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) deletePushToken(w http.ResponseWriter, r *http.Request) {
	if err := s.Notify.RemovePushToken(r.Context(), platform.PrincipalFrom(r.Context()).DeviceID); err != nil {
		s.failNotify(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) failNotify(w http.ResponseWriter, err error) {
	var v *notify.ValidationError
	switch {
	case errors.As(err, &v):
		platform.WriteProblem(w, platform.NewProblem(http.StatusUnprocessableEntity, "validation_failed", "Check the details", v.Msg))
	case errors.Is(err, notify.ErrBadCursor):
		platform.WriteProblem(w, platform.NewProblem(http.StatusUnprocessableEntity, "validation_failed", "Check the details",
			"That page link is no longer valid. Reload the list."))
	default:
		s.fail(w, err)
	}
}
