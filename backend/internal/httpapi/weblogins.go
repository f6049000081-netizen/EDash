package httpapi

import (
	"errors"
	"net/http"

	"github.com/google/uuid"

	"superapp/backend/internal/bizweb"
	"superapp/backend/internal/platform"
)

// Signing in to the web business console from the app: the computer shows a code, the app looks it up
// (so the person sees which browser is asking) and approves it with the PIN.
func (s *Server) webLoginRoutes(mux *http.ServeMux, authed func(http.HandlerFunc) http.Handler) {
	mux.Handle("POST /v1/web-logins/lookup", authed(s.lookupWebLogin))
	mux.Handle("POST /v1/web-logins/approve", authed(s.approveWebLogin))
	mux.Handle("GET /v1/me/web-sessions", authed(s.listWebSessions))
	mux.Handle("DELETE /v1/me/web-sessions/{id}", authed(s.deleteWebSession))
}

func webLoginJSON(i *bizweb.LoginInfo) map[string]any {
	return map[string]any{"browser": i.Browser, "ip": i.IP, "created_at": i.CreatedAt, "expires_at": i.ExpiresAt}
}

func (s *Server) lookupWebLogin(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Code string `json:"code"`
	}
	if !decode(w, r, &body) {
		return
	}
	info, err := s.Web.LookupLogin(r.Context(), platform.UserFrom(r.Context()), body.Code)
	if err != nil {
		s.failWeb(w, err)
		return
	}
	platform.WriteJSON(w, http.StatusOK, webLoginJSON(info))
}

func (s *Server) approveWebLogin(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Code       string    `json:"code"`
		MerchantID uuid.UUID `json:"merchant_id"`
		PIN        string    `json:"pin"`
	}
	if !decode(w, r, &body) {
		return
	}
	info, err := s.Web.ApproveLogin(r.Context(), platform.UserFrom(r.Context()), body.MerchantID, body.Code, body.PIN)
	if err != nil {
		s.failWeb(w, err)
		return
	}
	platform.WriteJSON(w, http.StatusOK, webLoginJSON(info))
}

func (s *Server) listWebSessions(w http.ResponseWriter, r *http.Request) {
	list, err := s.Web.Sessions(r.Context(), platform.UserFrom(r.Context()))
	if err != nil {
		s.failWeb(w, err)
		return
	}
	items := make([]map[string]any, 0, len(list))
	for _, i := range list {
		items = append(items, map[string]any{"id": i.ID, "merchant_id": i.MerchantID, "shop_name": i.Shop,
			"browser": i.Browser, "ip": i.IP, "created_at": i.CreatedAt, "last_seen_at": i.LastSeenAt})
	}
	platform.WriteJSON(w, http.StatusOK, map[string]any{"items": items})
}

func (s *Server) deleteWebSession(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	if err := s.Web.Revoke(r.Context(), platform.UserFrom(r.Context()), id); err != nil {
		s.failWeb(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) failWeb(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, bizweb.ErrNotFound):
		platform.WriteProblem(w, platform.NewProblem(http.StatusNotFound, "code_not_found", "Code not found",
			"That code isn't right or has expired. Check the code on the computer, or get a new one there."))
	case errors.Is(err, bizweb.ErrForbidden):
		platform.WriteProblem(w, platform.NewProblem(http.StatusForbidden, "permission_denied", "Not allowed",
			"You can sign in on a computer only for an approved shop you belong to."))
	default:
		s.fail(w, err) // PIN errors and faults
	}
}
