package ops

import (
	"context"
	"errors"
	"fmt"
	"net/mail"
	"slices"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"superapp/backend/internal/merchants"
	"superapp/backend/internal/payments"
	"superapp/backend/internal/platform"
)

// ErrSelf: nobody changes their own access, as requester or as approver.
var ErrSelf = errors.New("ops: you can't change your own access")

// ---------------------------------------------------------------------------
// Risk cases: taking a case and writing notes (no second person needed; recorded)
// ---------------------------------------------------------------------------

// TakeCase assigns an open case to the staff member and starts its review.
func (s *Service) TakeCase(ctx context.Context, a Actor, id uuid.UUID) error {
	if !a.Staff.Can("risk.cases", Approve) {
		s.denied(ctx, a, "case.take", "case", id, map[string]any{})
		return ErrForbidden
	}
	return pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			UPDATE risk.cases SET assignee_id = $2, status = CASE WHEN status = 'unassigned' THEN 'in_review' ELSE status END,
			       updated_at = now()
			WHERE id = $1 AND status IN ('unassigned', 'in_review', 'awaiting_approval')`, id, a.Staff.ID)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return ErrDecided
		}
		return s.audit(ctx, tx, a, "case.take", "case", id, "allowed", map[string]any{})
	})
}

// NoteCase adds a working note to a case. Notes are internal: customers never see them.
func (s *Service) NoteCase(ctx context.Context, a Actor, id uuid.UUID, body string) error {
	body = strings.TrimSpace(body)
	if !a.Staff.Can("risk.cases", Approve) {
		s.denied(ctx, a, "case.note", "case", id, map[string]any{})
		return ErrForbidden
	}
	if body == "" || len([]rune(body)) > 4000 {
		return ErrNoteMissing
	}
	return pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `INSERT INTO risk.case_notes (case_id, staff_id, body) SELECT id, $2, $3 FROM risk.cases WHERE id = $1`,
			id, a.Staff.ID, body)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return ErrNotFound
		}
		return s.audit(ctx, tx, a, "case.note", "case", id, "allowed", map[string]any{"note": truncate(body, 500)})
	})
}

// ---------------------------------------------------------------------------
// Applying risk and staff actions (called from apply)
// ---------------------------------------------------------------------------

func (s *Service) applyRisk(ctx context.Context, tx pgx.Tx, staff uuid.UUID, act Action) error {
	switch act.Kind {
	case "payment.release":
		_, err := s.Payments.ReleaseHeldTx(ctx, tx, act.Target, staff)
		return reviewErr(err)
	case "payment.decline":
		return reviewErr(s.Payments.DeclineHeldTx(ctx, tx, act.Target, staff))
	case "user.freeze", "user.unfreeze":
		from, to, topic := "active", "frozen", "identity.account_frozen"
		if act.Kind == "user.unfreeze" {
			from, to, topic = "frozen", "active", "identity.account_unfrozen"
		}
		tag, err := tx.Exec(ctx, `UPDATE identity.users SET status = $3::identity.user_status, updated_at = now()
			WHERE id = $1 AND status = $2::identity.user_status`, act.Target, from, to)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return &merchants.ConflictError{Msg: "The account isn't " + from + "."}
		}
		return platform.WriteOutbox(ctx, tx, topic, act.Target, map[string]any{"user_id": act.Target})
	case "case.escalate", "case.resolve":
		var held int
		if err := tx.QueryRow(ctx, `
			SELECT count(*) FILTER (WHERE i.status = 'held_for_review') FROM risk.cases c
			LEFT JOIN risk.case_intents ci ON ci.case_id = c.id LEFT JOIN payments.intents i ON i.id = ci.intent_id
			WHERE c.id = $1`, act.Target).Scan(&held); err != nil {
			return err
		}
		to := "escalated_aml"
		if act.Kind == "case.resolve" {
			to = "resolved"
			if held > 0 {
				return &merchants.ConflictError{Msg: "Release or decline the held payments before resolving the case."}
			}
		}
		tag, err := tx.Exec(ctx, `UPDATE risk.cases SET status = $2, resolution = $3, updated_at = now()
			WHERE id = $1 AND status NOT IN ('resolved', 'escalated_aml')`, act.Target, to, act.Note)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return &merchants.ConflictError{Msg: "The case is already closed."}
		}
		return nil
	}
	return fmt.Errorf("ops: unknown action %q", act.Kind)
}

func reviewErr(err error) error {
	var r *payments.ReviewError
	if errors.As(err, &r) {
		return &merchants.ConflictError{Msg: r.Msg}
	}
	if errors.Is(err, payments.ErrNotFound) {
		return ErrNotFound
	}
	return err
}

// Roles staff can be given (admin.roles).
var StaffRoles = []string{"super_admin", "finance_officer", "risk_analyst", "support_agent", "merchant_ops", "content_editor"}

func (s *Service) applyStaff(ctx context.Context, tx pgx.Tx, staff uuid.UUID, act Action) error {
	if act.Target == staff {
		return ErrSelf
	}
	roles := []string{}
	for _, r := range strings.Split(act.Params["roles"], ",") {
		if r = strings.TrimSpace(r); r != "" && !slices.Contains(roles, r) {
			roles = append(roles, r)
		}
	}
	for _, r := range roles {
		if !slices.Contains(StaffRoles, r) {
			return &merchants.ConflictError{Msg: "Unknown role " + r + "."}
		}
	}
	switch act.Kind {
	case "staff.invite":
		email := strings.ToLower(strings.TrimSpace(act.Params["email"]))
		name := strings.Join(strings.Fields(act.Params["name"]), " ")
		if addr, err := mail.ParseAddress(email); err != nil || addr.Address != email {
			return &merchants.ConflictError{Msg: "That email address doesn't look right."}
		}
		if name == "" || len(roles) == 0 {
			return &merchants.ConflictError{Msg: "Give the person's name and at least one role."}
		}
		_, err := tx.Exec(ctx, `INSERT INTO admin.staff (id, email, full_name, sso_subject) VALUES ($1, $2::text, $3, 'invite:' || $2::text)`,
			act.Target, email, name)
		if isUnique(err) {
			return &merchants.ConflictError{Msg: "Someone with that email is already staff."}
		}
		if err != nil {
			return err
		}
		return setRoles(ctx, tx, act.Target, roles, staff)
	case "staff.roles":
		if len(roles) == 0 {
			return &merchants.ConflictError{Msg: "Give at least one role, or suspend the person instead."}
		}
		if err := staffExists(ctx, tx, act.Target); err != nil {
			return err
		}
		if err := setRoles(ctx, tx, act.Target, roles, staff); err != nil {
			return err
		}
	case "staff.suspend", "staff.reinstate":
		from, to := "active", "suspended"
		if act.Kind == "staff.reinstate" {
			from, to = "suspended", "active"
		}
		tag, err := tx.Exec(ctx, `UPDATE admin.staff SET status = $3 WHERE id = $1 AND status = $2`, act.Target, from, to)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return &merchants.ConflictError{Msg: "That staff member isn't " + from + "."}
		}
		if to == "suspended" {
			if _, err := tx.Exec(ctx, `UPDATE admin.staff_sessions SET revoked_at = now() WHERE staff_id = $1 AND revoked_at IS NULL`,
				act.Target); err != nil {
				return err
			}
		}
	default:
		return fmt.Errorf("ops: unknown action %q", act.Kind)
	}
	// Someone must always be able to manage staff.
	var admins int
	if err := tx.QueryRow(ctx, `SELECT count(DISTINCT s.id) FROM admin.staff s JOIN admin.staff_roles r ON r.staff_id = s.id
		WHERE r.role_id = 'super_admin' AND s.status = 'active'`).Scan(&admins); err != nil {
		return err
	}
	if admins == 0 {
		return &merchants.ConflictError{Msg: "That would leave no active super admin."}
	}
	return nil
}

func staffExists(ctx context.Context, tx pgx.Tx, id uuid.UUID) error {
	var ok bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM admin.staff WHERE id = $1 AND id <> $2)`, id, Bootstrap).Scan(&ok); err != nil {
		return err
	}
	if !ok {
		return ErrNotFound
	}
	return nil
}

// setRoles makes the staff member's roles exactly these.
func setRoles(ctx context.Context, tx pgx.Tx, id uuid.UUID, roles []string, grantedBy uuid.UUID) error {
	if _, err := tx.Exec(ctx, `DELETE FROM admin.staff_roles WHERE staff_id = $1 AND NOT (role_id = ANY($2))`, id, roles); err != nil {
		return err
	}
	for _, r := range roles {
		if _, err := tx.Exec(ctx, `INSERT INTO admin.staff_roles (staff_id, role_id, granted_by) VALUES ($1, $2, $3)
			ON CONFLICT DO NOTHING`, id, r, grantedBy); err != nil {
			return err
		}
	}
	return nil // sessions reload roles on every request, so the change applies at once
}

func isUnique(err error) bool {
	var pg *pgconn.PgError
	return errors.As(err, &pg) && pg.Code == "23505"
}
