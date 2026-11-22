package httpapi

import (
	"errors"
	"net/http"

	"github.com/google/uuid"

	"superapp/backend/internal/merchants"
	"superapp/backend/internal/platform"
)

func (s *Server) teamRoutes(mux *http.ServeMux, authed func(http.HandlerFunc) http.Handler) {
	mux.Handle("GET /v1/merchants/{id}/team", authed(s.getTeam))
	mux.Handle("POST /v1/merchants/{id}/invitations", authed(s.inviteMember))
	mux.Handle("DELETE /v1/merchants/{id}/invitations/{invitation}", authed(s.cancelInvitation))
	mux.Handle("PUT /v1/merchants/{id}/members/{user}", authed(s.setMemberRole))
	mux.Handle("DELETE /v1/merchants/{id}/members/{user}", authed(s.removeMember))
	mux.Handle("GET /v1/me/invitations", authed(s.myInvitations))
	mux.Handle("POST /v1/me/invitations/{invitation}/accept", authed(s.answerInvitation(true)))
	mux.Handle("POST /v1/me/invitations/{invitation}/decline", authed(s.answerInvitation(false)))
}

func invitationJSON(i merchants.Invitation, withPhone bool) map[string]any {
	out := map[string]any{"id": i.ID, "merchant_id": i.MerchantID, "shop": i.Shop, "role": i.Role,
		"invited_by": i.InvitedBy, "created_at": i.CreatedAt, "expires_at": i.ExpiresAt}
	if withPhone {
		out["phone"] = i.Phone
	}
	return out
}

func pathUUID(w http.ResponseWriter, r *http.Request, name string) (uuid.UUID, bool) {
	id, err := uuid.Parse(r.PathValue(name))
	if err != nil {
		platform.WriteProblem(w, platform.NewProblem(http.StatusNotFound, "not_found", "Not found", ""))
		return uuid.Nil, false
	}
	return id, true
}

func (s *Server) failTeam(w http.ResponseWriter, err error) {
	if errors.Is(err, merchants.ErrInvitationGone) {
		platform.WriteProblem(w, platform.NewProblem(http.StatusGone, "invitation_gone", "Invitation closed",
			"This invitation has expired, was cancelled or was already answered."))
		return
	}
	s.failMerchants(w, err)
}

func (s *Server) getTeam(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	me := platform.UserFrom(r.Context())
	t, err := s.Merchants.Team(r.Context(), me, id)
	if err != nil {
		s.failTeam(w, err)
		return
	}
	members := make([]map[string]any, len(t.Members))
	for i, m := range t.Members {
		members[i] = map[string]any{"user_id": m.UserID, "name": m.Name, "phone": m.Phone, "role": m.Role, "since": m.Since,
			"you": m.UserID == me}
	}
	invites := make([]map[string]any, len(t.Invitations))
	for i, inv := range t.Invitations {
		invites[i] = invitationJSON(inv, true)
	}
	platform.WriteJSON(w, http.StatusOK, map[string]any{"members": members, "invitations": invites})
}

func (s *Server) inviteMember(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	var body struct {
		Phone string `json:"phone"`
		Role  string `json:"role"`
	}
	if !decode(w, r, &body) {
		return
	}
	inv, err := s.Merchants.Invite(r.Context(), platform.UserFrom(r.Context()), id, body.Phone, body.Role)
	if err != nil {
		s.failTeam(w, err)
		return
	}
	platform.WriteJSON(w, http.StatusCreated, invitationJSON(*inv, true))
}

func (s *Server) cancelInvitation(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	inv, ok := pathUUID(w, r, "invitation")
	if !ok {
		return
	}
	if err := s.Merchants.CancelInvitation(r.Context(), platform.UserFrom(r.Context()), id, inv); err != nil {
		s.failTeam(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) setMemberRole(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	member, ok := pathUUID(w, r, "user")
	if !ok {
		return
	}
	var body struct {
		Role string `json:"role"`
	}
	if !decode(w, r, &body) {
		return
	}
	if err := s.Merchants.SetRole(r.Context(), platform.UserFrom(r.Context()), id, member, body.Role); err != nil {
		s.failTeam(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// removeMember removes someone, or, with the caller's own user ID, leaves the shop.
func (s *Server) removeMember(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	member, ok := pathUUID(w, r, "user")
	if !ok {
		return
	}
	if err := s.Merchants.RemoveMember(r.Context(), platform.UserFrom(r.Context()), id, member); err != nil {
		s.failTeam(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) myInvitations(w http.ResponseWriter, r *http.Request) {
	list, err := s.Merchants.MyInvitations(r.Context(), platform.UserFrom(r.Context()))
	if err != nil {
		s.failTeam(w, err)
		return
	}
	items := make([]map[string]any, len(list))
	for i, inv := range list {
		items[i] = invitationJSON(inv, false)
	}
	platform.WriteJSON(w, http.StatusOK, map[string]any{"items": items})
}

func (s *Server) answerInvitation(accept bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		inv, ok := pathUUID(w, r, "invitation")
		if !ok {
			return
		}
		m, err := s.Merchants.AnswerInvitation(r.Context(), platform.UserFrom(r.Context()), inv, accept)
		if err != nil {
			s.failTeam(w, err)
			return
		}
		if m == nil {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		platform.WriteJSON(w, http.StatusOK, toMerchantJSON(m))
	}
}
