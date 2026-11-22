package opsweb

import (
	"errors"
	"net/http"
	"slices"
	"strings"

	"github.com/google/uuid"

	"superapp/backend/internal/ops"
)

func (s *Server) caseList(w http.ResponseWriter, r *http.Request, sess *ops.Session) {
	if !sess.Staff.Can("risk.cases", ops.View) {
		s.render(w, r, http.StatusForbidden, "forbidden", sess, nil)
		return
	}
	open := r.URL.Query().Get("closed") == ""
	list, err := s.Ops.Cases(r.Context(), open, 200)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	s.render(w, r, http.StatusOK, "cases", sess, map[string]any{"Open": open, "List": list})
}

func (s *Server) casePage(w http.ResponseWriter, r *http.Request, sess *ops.Session) {
	if !sess.Staff.Can("risk.cases", ops.View) {
		s.render(w, r, http.StatusForbidden, "forbidden", sess, nil)
		return
	}
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	c, err := s.Ops.Case(r.Context(), id)
	if errors.Is(err, ops.ErrNotFound) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	// Requests waiting on, and the history of, this case, its payments and its customer.
	var pending []ops.ApprovalRow
	var audit []ops.AuditRow
	for _, t := range append([]uuid.UUID{c.ID, c.SubjectID}, intentIDs(c)...) {
		p, err := s.Ops.Approvals(r.Context(), true, t, 20)
		if err != nil {
			s.serverError(w, r, err)
			return
		}
		pending = append(pending, p...)
		a, err := s.Ops.Audit(r.Context(), t, 50)
		if err != nil {
			s.serverError(w, r, err)
			return
		}
		audit = append(audit, a...)
	}
	slices.SortFunc(audit, func(a, b ops.AuditRow) int { return int(b.ID - a.ID) })
	if len(audit) > 100 {
		audit = audit[:100]
	}
	s.render(w, r, http.StatusOK, "case", sess, map[string]any{"C": c, "Pending": pending, "Audit": audit})
}

func intentIDs(c *ops.CaseDetail) []uuid.UUID {
	ids := make([]uuid.UUID, len(c.Intents))
	for i, it := range c.Intents {
		ids[i] = it.ID
	}
	return ids
}

func (s *Server) caseTake(w http.ResponseWriter, r *http.Request, sess *ops.Session) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	err = s.Ops.TakeCase(r.Context(), actor(r, sess.Staff), id)
	code := "taken"
	if err != nil {
		code = outcomeCode(ops.Outcome{}, err)
	}
	s.logUnexpected(r, err)
	s.back(w, r, "/ops/cases/"+id.String(), code)
}

func (s *Server) caseNote(w http.ResponseWriter, r *http.Request, sess *ops.Session) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	err = s.Ops.NoteCase(r.Context(), actor(r, sess.Staff), id, r.PostFormValue("body"))
	code := "noted"
	switch {
	case errors.Is(err, ops.ErrNoteMissing):
		code = "note_empty"
	case err != nil:
		code = outcomeCode(ops.Outcome{}, err)
	}
	s.logUnexpected(r, err)
	s.back(w, r, "/ops/cases/"+id.String(), code)
}

// caseAction runs an action on the case, one of its payments, or its customer. The target must belong
// to the case, so a form can't be pointed at something else.
func (s *Server) caseAction(w http.ResponseWriter, r *http.Request, sess *ops.Session) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	c, err := s.Ops.Case(r.Context(), id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	kind := r.PostFormValue("kind")
	target := c.ID
	switch {
	case kind == "case.escalate" || kind == "case.resolve":
	case kind == "user.freeze" || kind == "user.unfreeze":
		target = c.SubjectID
	case kind == "payment.release" || kind == "payment.decline":
		t, err := uuid.Parse(r.PostFormValue("target"))
		if err != nil || !slices.Contains(intentIDs(c), t) {
			http.NotFound(w, r)
			return
		}
		target = t
	default:
		http.NotFound(w, r)
		return
	}
	out, err := s.Ops.Do(r.Context(), actor(r, sess.Staff), ops.Action{Kind: kind, Target: target, Note: r.PostFormValue("note")})
	s.logUnexpected(r, err)
	s.back(w, r, "/ops/cases/"+id.String(), outcomeCode(out, err))
}

// goToCase sends a link to a payment or a customer (from the approvals page) to their newest case.
func (s *Server) goToCase(w http.ResponseWriter, r *http.Request, _ *ops.Session) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	var caseID uuid.UUID
	err = s.Ops.Pool.QueryRow(r.Context(), `
		SELECT c.id FROM risk.cases c LEFT JOIN risk.case_intents ci ON ci.case_id = c.id
		WHERE ci.intent_id = $1 OR (c.subject_type = 'user' AND c.subject_id = $1)
		ORDER BY c.created_at DESC LIMIT 1`, id).Scan(&caseID)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	http.Redirect(w, r, "/ops/cases/"+caseID.String(), http.StatusFound)
}

// ---------------------------------------------------------------------------
// Staff
// ---------------------------------------------------------------------------

func (s *Server) staffPage(w http.ResponseWriter, r *http.Request, sess *ops.Session) {
	if !sess.Staff.Can("staff.manage", ops.View) {
		s.render(w, r, http.StatusForbidden, "forbidden", sess, nil)
		return
	}
	list, err := s.Ops.StaffList(r.Context())
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	pending, err := s.Ops.Approvals(r.Context(), true, uuid.Nil, 100)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	var staffPending []ops.ApprovalRow
	for _, p := range pending {
		if p.TargetType == "staff" {
			staffPending = append(staffPending, p)
		}
	}
	s.render(w, r, http.StatusOK, "staff", sess, map[string]any{"List": list, "Roles": ops.StaffRoles, "Pending": staffPending})
}

func (s *Server) staffInvite(w http.ResponseWriter, r *http.Request, sess *ops.Session) {
	_ = r.ParseForm()
	out, err := s.Ops.Do(r.Context(), actor(r, sess.Staff), ops.Action{Kind: "staff.invite", Target: uuid.New(),
		Note: r.PostFormValue("note"), Params: map[string]string{
			"email": strings.TrimSpace(r.PostFormValue("email")), "name": strings.TrimSpace(r.PostFormValue("name")),
			"roles": strings.Join(r.PostForm["roles"], ","),
		}})
	s.logUnexpected(r, err)
	s.back(w, r, "/ops/staff", outcomeCode(out, err))
}

func (s *Server) staffAction(w http.ResponseWriter, r *http.Request, sess *ops.Session) {
	id, err := uuid.Parse(r.PathValue("id"))
	kind := r.PostFormValue("kind")
	if err != nil || (kind != "staff.roles" && kind != "staff.suspend" && kind != "staff.reinstate") {
		http.NotFound(w, r)
		return
	}
	act := ops.Action{Kind: kind, Target: id, Note: r.PostFormValue("note")}
	if kind == "staff.roles" {
		act.Params = map[string]string{"roles": strings.Join(r.PostForm["roles"], ",")}
	}
	out, err := s.Ops.Do(r.Context(), actor(r, sess.Staff), act)
	s.logUnexpected(r, err)
	s.back(w, r, "/ops/staff", outcomeCode(out, err))
}
