package bizweb

import (
	"errors"
	"net/http"
	"strings"

	"github.com/google/uuid"

	"superapp/backend/internal/merchants"
)

// The Team page: owners and managers see who works at the shop; owners invite people by phone number,
// change roles, remove people and withdraw invitations. Joining is always confirmed by the invited
// person in their app.

type teamData struct {
	Team  *merchants.Team
	Owner bool
	Roles []string
	Phone string // the invite form, kept after a refusal
	Role  string
}

func (s *Server) teamRoutes(mux *http.ServeMux) {
	mux.Handle("GET /business/team", s.authed(s.team))
	mux.Handle("POST /business/team/invite", s.authed(s.teamInvite))
	mux.Handle("POST /business/team/invitations/{id}/cancel", s.authed(s.teamCancelInvite))
	mux.Handle("POST /business/team/members/{id}/role", s.authed(s.teamRole))
	mux.Handle("POST /business/team/members/{id}/remove", s.authed(s.teamRemove))
}

func (s *Server) renderTeam(w http.ResponseWriter, r *http.Request, sess *Session, status int, d teamData, msg string) {
	t, err := s.Merchants.Team(r.Context(), sess.UserID, sess.MerchantID)
	if errors.Is(err, merchants.ErrForbidden) {
		s.renderPage(w, r, http.StatusForbidden, "team", sess, page{Error: translate(lang(r, sess), "team_forbidden")})
		return
	}
	if err != nil {
		s.serverError(w, r, sess, err)
		return
	}
	d.Team, d.Owner, d.Roles = t, sess.Role == "owner", merchants.TeamRoles
	if d.Role == "" {
		d.Role = "cashier"
	}
	s.renderPage(w, r, status, "team", sess, page{Data: d, Error: msg})
}

func (s *Server) team(w http.ResponseWriter, r *http.Request, sess *Session) {
	s.renderTeam(w, r, sess, http.StatusOK, teamData{}, "")
}

// teamDone redirects with a message, or shows a refusal on the page.
func (s *Server) teamDone(w http.ResponseWriter, r *http.Request, sess *Session, err error, msg string, d teamData) {
	var v *merchants.ValidationError
	var c *merchants.ConflictError
	l := lang(r, sess)
	switch {
	case err == nil:
		http.Redirect(w, r, "/business/team?msg="+msg, http.StatusSeeOther)
	case errors.As(err, &v):
		s.renderTeam(w, r, sess, http.StatusUnprocessableEntity, d, v.Msg)
	case errors.As(err, &c):
		s.renderTeam(w, r, sess, http.StatusConflict, d, c.Msg)
	case errors.Is(err, merchants.ErrForbidden):
		s.renderTeam(w, r, sess, http.StatusForbidden, d, translate(l, "team_owner_only"))
	case errors.Is(err, merchants.ErrNotFound), errors.Is(err, merchants.ErrInvitationGone):
		http.Redirect(w, r, "/business/team?msg=conflict", http.StatusSeeOther)
	default:
		s.serverError(w, r, sess, err)
	}
}

func (s *Server) teamInvite(w http.ResponseWriter, r *http.Request, sess *Session) {
	d := teamData{Phone: strings.TrimSpace(r.PostFormValue("phone")), Role: r.PostFormValue("role")}
	_, err := s.Merchants.Invite(r.Context(), sess.UserID, sess.MerchantID, d.Phone, d.Role)
	s.teamDone(w, r, sess, err, "invited", d)
}

func formID(w http.ResponseWriter, r *http.Request) (uuid.UUID, bool) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		http.Redirect(w, r, "/business/team?msg=notfound", http.StatusSeeOther)
		return uuid.Nil, false
	}
	return id, true
}

func (s *Server) teamCancelInvite(w http.ResponseWriter, r *http.Request, sess *Session) {
	id, ok := formID(w, r)
	if !ok {
		return
	}
	err := s.Merchants.CancelInvitation(r.Context(), sess.UserID, sess.MerchantID, id)
	s.teamDone(w, r, sess, err, "invite_cancelled", teamData{})
}

func (s *Server) teamRole(w http.ResponseWriter, r *http.Request, sess *Session) {
	id, ok := formID(w, r)
	if !ok {
		return
	}
	err := s.Merchants.SetRole(r.Context(), sess.UserID, sess.MerchantID, id, r.PostFormValue("role"))
	s.teamDone(w, r, sess, err, "role_changed", teamData{})
}

func (s *Server) teamRemove(w http.ResponseWriter, r *http.Request, sess *Session) {
	id, ok := formID(w, r)
	if !ok {
		return
	}
	if id == sess.UserID { // leaving is done in the app, where the person confirms it
		s.teamDone(w, r, sess, merchants.ErrForbidden, "", teamData{})
		return
	}
	err := s.Merchants.RemoveMember(r.Context(), sess.UserID, sess.MerchantID, id)
	s.teamDone(w, r, sess, err, "member_removed", teamData{})
}
