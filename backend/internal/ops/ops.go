// Package ops is what operations staff can do, and the controls around it:
//
//   - Grants come from roles (admin.role_permissions): "full" acts alone, "approve" needs a second
//     person, "view" only reads.
//   - Maker-checker: an action under an "approve" grant becomes a request (admin.approvals). A different
//     staff member with "approve" or "full" on the same permission decides it; the database also refuses
//     a decision by the requester. The change is applied in the same transaction as the approval.
//   - Every action, request, decision, refusal and sign-in goes to the audit log, which is append-only
//     (a trigger refuses updates and deletes) and hash-chained, so editing a row breaks every later hash.
package ops

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"superapp/backend/internal/kyb"
	"superapp/backend/internal/merchants"
	"superapp/backend/internal/payouts"
)

type Grant int

const (
	None Grant = iota
	View
	Approve
	Full
)

func (g Grant) String() string { return [...]string{"none", "view", "approve", "full"}[g] }

func parseGrant(s string) Grant {
	switch s {
	case "full":
		return Full
	case "approve":
		return Approve
	case "view":
		return View
	}
	return None
}

var (
	ErrForbidden   = errors.New("ops: not allowed")
	ErrOwnRequest  = errors.New("ops: nobody approves their own request")
	ErrNotFound    = errors.New("ops: not found")
	ErrDecided     = errors.New("ops: already decided or expired")
	ErrDuplicate   = errors.New("ops: the same request is already waiting")
	ErrNoteMissing = errors.New("ops: a note of at least 10 characters is required")
)

// Staff is a signed-in staff member and what their roles allow.
type Staff struct {
	ID     uuid.UUID
	Email  string
	Name   string
	Roles  []string
	grants map[string]Grant
}

func (s *Staff) Grant(permission string) Grant { return s.grants[permission] }

// Can reports whether the staff member can see (View) or act on (Approve/Full) a permission.
func (s *Staff) Can(permission string, at Grant) bool { return s.grants[permission] >= at }

func (s *Staff) RoleList() string { return strings.Join(s.Roles, ",") }

// Actor is who is acting and from where, for the audit log.
type Actor struct {
	Staff  *Staff
	IP     string
	Device string
}

type Service struct {
	Pool      *pgxpool.Pool
	Merchants *merchants.Service
	Payouts   *payouts.Service
	KYB       *kyb.Service // nil: no documents (approval then doesn't check them)
	Now       func() time.Time
	// ApprovalTTL is how long a request waits for a second person before it lapses.
	ApprovalTTL time.Duration
}

func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func (s *Service) ttl() time.Duration {
	if s.ApprovalTTL > 0 {
		return s.ApprovalTTL
	}
	return 72 * time.Hour
}

// LoadStaff returns an active staff member with their roles and grants, or ErrForbidden.
func (s *Service) LoadStaff(ctx context.Context, id uuid.UUID) (*Staff, error) {
	st := &Staff{ID: id, grants: map[string]Grant{}}
	var status string
	err := s.Pool.QueryRow(ctx, `SELECT email, full_name, status FROM admin.staff WHERE id = $1`, id).Scan(&st.Email, &st.Name, &status)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && status != "active") {
		return nil, ErrForbidden
	}
	if err != nil {
		return nil, err
	}
	rows, err := s.Pool.Query(ctx, `
		SELECT sr.role_id, rp.permission, rp.grant_level FROM admin.staff_roles sr
		LEFT JOIN admin.role_permissions rp ON rp.role_id = sr.role_id
		WHERE sr.staff_id = $1`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	roles := map[string]bool{}
	for rows.Next() {
		var role string
		var perm, level *string
		if err := rows.Scan(&role, &perm, &level); err != nil {
			return nil, err
		}
		roles[role] = true
		if perm != nil {
			if g := parseGrant(*level); g > st.grants[*perm] {
				st.grants[*perm] = g
			}
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(roles) == 0 {
		return nil, ErrForbidden // staff without a role can't do anything, so they can't sign in
	}
	for r := range roles {
		st.Roles = append(st.Roles, r)
	}
	sort.Strings(st.Roles)
	return st, nil
}

// ---------------------------------------------------------------------------
// Actions and maker-checker
// ---------------------------------------------------------------------------

// Action kinds, each guarded by one permission.
var actionPermission = map[string]string{
	"merchant.approve":   "merchants.approve",
	"merchant.reject":    "merchants.approve",
	"merchant.suspend":   "accounts.freeze",
	"merchant.reinstate": "accounts.freeze",
	"payout.release":     "payouts.release",
}

var actionTarget = map[string]string{
	"merchant.approve": "merchant", "merchant.reject": "merchant", "merchant.suspend": "merchant",
	"merchant.reinstate": "merchant", "payout.release": "payout",
}

// Permission is the permission an action kind needs.
func Permission(kind string) string { return actionPermission[kind] }

type Action struct {
	Kind   string
	Target uuid.UUID
	Note   string // why: shown to the checker, kept in the audit log; for rejections and suspensions, also told to the merchant
}

type Outcome struct {
	Applied    bool
	ApprovalID uuid.UUID // set when the action now waits for a second person
}

// Do applies an action directly under a "full" grant, or files it for a second person's approval under an
// "approve" grant. Anything less is refused and logged.
func (s *Service) Do(ctx context.Context, a Actor, act Action) (Outcome, error) {
	perm, ok := actionPermission[act.Kind]
	if !ok {
		return Outcome{}, fmt.Errorf("ops: unknown action %q", act.Kind)
	}
	act.Note = strings.TrimSpace(act.Note)
	if len([]rune(act.Note)) < 10 {
		return Outcome{}, ErrNoteMissing
	}
	detail := map[string]any{"note": act.Note, "permission": perm}
	switch a.Staff.Grant(perm) {
	case Full:
		err := pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
			if err := s.apply(ctx, tx, a.Staff.ID, act); err != nil {
				return err
			}
			return s.audit(ctx, tx, a, act.Kind, actionTarget[act.Kind], act.Target, "allowed", detail)
		})
		s.failed(ctx, a, act.Kind, act.Target, detail, err)
		return Outcome{Applied: err == nil}, err
	case Approve:
		var id uuid.UUID
		err := pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
			// One open request per action and target, so the queue isn't flooded and nobody approves a stale copy.
			var dup bool
			if err := tx.QueryRow(ctx, `
				SELECT EXISTS (SELECT 1 FROM admin.approvals WHERE target_id = $1 AND payload->>'kind' = $2
				               AND decision IS NULL AND expires_at > $3)`, act.Target, act.Kind, s.now()).Scan(&dup); err != nil {
				return err
			}
			if dup {
				return ErrDuplicate
			}
			// Check it could be applied now, so nobody is asked to approve something impossible.
			sp, err := tx.Begin(ctx)
			if err != nil {
				return err
			}
			trial := s.apply(ctx, sp, a.Staff.ID, act)
			if err := sp.Rollback(ctx); err != nil {
				return err
			}
			if trial != nil {
				return trial
			}
			payload, _ := json.Marshal(map[string]any{"kind": act.Kind})
			now := s.now()
			if err := tx.QueryRow(ctx, `
				INSERT INTO admin.approvals (permission, target_type, target_id, payload, requested_by, requested_at, expires_at, request_note)
				VALUES ($1, $2, $3, $4, $5, $6, $7, $8) RETURNING id`,
				perm, actionTarget[act.Kind], act.Target, payload, a.Staff.ID, now, now.Add(s.ttl()), act.Note).Scan(&id); err != nil {
				return err
			}
			detail["approval_id"] = id
			return s.audit(ctx, tx, a, act.Kind, actionTarget[act.Kind], act.Target, "pending", detail)
		})
		s.failed(ctx, a, act.Kind, act.Target, detail, err)
		return Outcome{ApprovalID: id}, err
	default:
		s.denied(ctx, a, act.Kind, actionTarget[act.Kind], act.Target, detail)
		return Outcome{}, ErrForbidden
	}
}

// Decide approves or rejects another staff member's request. Approving applies the change.
func (s *Service) Decide(ctx context.Context, a Actor, approval uuid.UUID, approve bool, note string) error {
	note = strings.TrimSpace(note)
	err := pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		var perm, targetType string
		var target, requester uuid.UUID
		var payload []byte
		var decided bool
		var expires time.Time
		err := tx.QueryRow(ctx, `
			SELECT permission, target_type, target_id, payload, requested_by, decision IS NOT NULL, expires_at
			FROM admin.approvals WHERE id = $1 FOR UPDATE`, approval).
			Scan(&perm, &targetType, &target, &payload, &requester, &decided, &expires)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		detail := map[string]any{"approval_id": approval, "permission": perm, "note": note}
		action := "approval.reject"
		if approve {
			action = "approval.approve"
		}
		if requester == a.Staff.ID {
			return &deniedError{ErrOwnRequest, action, targetType, target, detail}
		}
		if a.Staff.Grant(perm) < Approve {
			return &deniedError{ErrForbidden, action, targetType, target, detail}
		}
		if decided || !s.now().Before(expires) {
			return ErrDecided
		}
		decision := "rejected"
		if approve {
			decision = "approved"
			if len([]rune(note)) < 10 {
				return ErrNoteMissing
			}
			var p struct {
				Kind string `json:"kind"`
			}
			if err := json.Unmarshal(payload, &p); err != nil {
				return err
			}
			var requestNote string
			if err := tx.QueryRow(ctx, `SELECT request_note FROM admin.approvals WHERE id = $1`, approval).Scan(&requestNote); err != nil {
				return err
			}
			// The merchant is told the maker's reason, which the checker has just read and agreed with.
			if err := s.apply(ctx, tx, a.Staff.ID, Action{Kind: p.Kind, Target: target, Note: requestNote}); err != nil {
				return err
			}
			detail["kind"] = p.Kind
		}
		if _, err := tx.Exec(ctx, `
			UPDATE admin.approvals SET decided_by = $2, decision = $3, decided_at = $4, decision_note = NULLIF($5, '') WHERE id = $1`,
			approval, a.Staff.ID, decision, s.now(), note); err != nil {
			return err
		}
		return s.audit(ctx, tx, a, action, targetType, target, "allowed", detail)
	})
	var d *deniedError
	if errors.As(err, &d) {
		s.denied(ctx, a, d.action, d.targetType, d.target, d.detail)
		return d.err
	}
	return err
}

type deniedError struct {
	err        error
	action     string
	targetType string
	target     uuid.UUID
	detail     map[string]any
}

func (d *deniedError) Error() string { return d.err.Error() }

// denied records a refused attempt in its own transaction, so it is kept even though the action isn't.
func (s *Service) denied(ctx context.Context, a Actor, action, targetType string, target uuid.UUID, detail map[string]any) {
	_ = pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		return s.audit(ctx, tx, a, action, targetType, target, "denied", detail)
	})
}

// failed records an allowed action that the rules refused (wrong state, missing documents, a duplicate
// request), so the log shows every attempt and not only the ones that went through.
func (s *Service) failed(ctx context.Context, a Actor, kind string, target uuid.UUID, detail map[string]any, err error) {
	if err == nil || errors.Is(err, context.Canceled) {
		return
	}
	d := map[string]any{"error": err.Error()}
	for k, v := range detail {
		d[k] = v
	}
	_ = pgx.BeginFunc(context.WithoutCancel(ctx), s.Pool, func(tx pgx.Tx) error {
		return s.audit(ctx, tx, a, kind, actionTarget[kind], target, "failed", d)
	})
}

func (s *Service) apply(ctx context.Context, tx pgx.Tx, staff uuid.UUID, act Action) error {
	switch act.Kind {
	case "merchant.approve":
		var status string
		if err := tx.QueryRow(ctx, `SELECT status FROM merchant.merchants WHERE id = $1`, act.Target).Scan(&status); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrNotFound
			}
			return err
		}
		if status != "applied" && status != "kyb_review" {
			return &merchants.ConflictError{Msg: "Only applications under review can be approved; this merchant is " + status + "."}
		}
		if s.KYB != nil {
			missing, err := kyb.MissingTx(ctx, tx, act.Target)
			if err != nil {
				return err
			}
			if len(missing) > 0 {
				names := make([]string, len(missing))
				for i, k := range missing {
					names[i] = kyb.Kinds[k]
				}
				return &merchants.ConflictError{Msg: "Accept these documents first: " + strings.Join(names, ", ") + "."}
			}
		}
		return s.Merchants.ApproveTx(ctx, tx, act.Target, staff)
	case "merchant.reject":
		return s.Merchants.RejectTx(ctx, tx, act.Target, act.Note)
	case "merchant.suspend":
		return s.Merchants.SuspendTx(ctx, tx, act.Target, act.Note)
	case "merchant.reinstate":
		if err := s.Merchants.ReinstateTx(ctx, tx, act.Target, act.Note); err != nil {
			return err
		}
		return s.Payouts.ResumeAfterReinstateTx(ctx, tx, act.Target)
	case "payout.release":
		return s.Payouts.ReleaseTx(ctx, tx, act.Target)
	}
	return fmt.Errorf("ops: unknown action %q", act.Kind)
}

// ---------------------------------------------------------------------------
// KYB documents
// ---------------------------------------------------------------------------

// OpenDocument lets staff who can see applications read a document. Every opening is recorded.
func (s *Service) OpenDocument(ctx context.Context, a Actor, id uuid.UUID) (*kyb.Document, []byte, error) {
	if !a.Staff.Can("merchants.approve", View) {
		s.denied(ctx, a, "document.view", "merchant_document", id, map[string]any{})
		return nil, nil, ErrForbidden
	}
	d, data, err := s.KYB.Open(ctx, id)
	if err != nil {
		return nil, nil, err
	}
	err = pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		return s.audit(ctx, tx, a, "document.view", "merchant_document", id, "allowed",
			map[string]any{"merchant_id": d.MerchantID, "kind": d.Kind})
	})
	if err != nil {
		return nil, nil, err
	}
	return d, data, nil
}

// ReviewDocument accepts or rejects a document. Staff who can approve applications (alone or with a
// second person) check documents directly: the approval itself is what needs the second person.
func (s *Service) ReviewDocument(ctx context.Context, a Actor, id uuid.UUID, accept bool, note string) (*kyb.Document, error) {
	note = strings.TrimSpace(note)
	action := map[bool]string{true: "document.accept", false: "document.reject"}[accept]
	if !a.Staff.Can("merchants.approve", Approve) {
		s.denied(ctx, a, action, "merchant_document", id, map[string]any{"note": note})
		return nil, ErrForbidden
	}
	var doc *kyb.Document
	err := pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		d, err := s.KYB.ReviewTx(ctx, tx, id, a.Staff.ID, accept, note)
		if err != nil {
			return err
		}
		doc = d
		return s.audit(ctx, tx, a, action, "merchant_document", id, "allowed",
			map[string]any{"merchant_id": d.MerchantID, "kind": d.Kind, "note": note})
	})
	return doc, err
}

// ---------------------------------------------------------------------------
// Audit log
// ---------------------------------------------------------------------------

// AuditSystem records something staff did that isn't an Action, such as signing in or out.
func (s *Service) AuditSystem(ctx context.Context, a Actor, action, result string, detail map[string]any) error {
	return pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		return s.audit(ctx, tx, a, action, "", uuid.Nil, result, detail)
	})
}

func (s *Service) audit(ctx context.Context, tx pgx.Tx, a Actor, action, targetType string, target uuid.UUID, result string, detail map[string]any) error {
	// One writer at a time, so each row's prev_hash is the row before it.
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('admin.audit_log', 0))`); err != nil {
		return err
	}
	var prev string
	err := tx.QueryRow(ctx, `SELECT row_hash FROM admin.audit_log ORDER BY id DESC LIMIT 1`).Scan(&prev)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	raw, err := json.Marshal(detail)
	if err != nil {
		return err
	}
	var canon string // JSONB's own text form, which is what a verifier reads back
	if err := tx.QueryRow(ctx, `SELECT $1::jsonb::text`, string(raw)).Scan(&canon); err != nil {
		return err
	}
	r := auditRow{
		OccurredAt: s.now().UTC().Truncate(time.Microsecond),
		ActorType:  "staff",
		Action:     action,
		TargetType: targetType,
		Result:     result,
		Device:     truncate(a.Device, 200),
		Detail:     canon,
	}
	if a.Staff != nil {
		r.ActorID, r.ActorRole = a.Staff.ID.String(), a.Staff.RoleList()
	}
	if target != uuid.Nil {
		r.TargetID = target.String()
	}
	if ip := net.ParseIP(a.IP); ip != nil {
		r.IP = ip.String()
	}
	hash := r.hash(prev)
	_, err = tx.Exec(ctx, `
		INSERT INTO admin.audit_log (occurred_at, actor_type, actor_id, actor_role, action, target_type, target_id, result,
			device, ip, detail, prev_hash, row_hash)
		VALUES ($1, $2, NULLIF($3, '')::uuid, NULLIF($4, ''), $5, NULLIF($6, ''), NULLIF($7, '')::uuid, $8,
			NULLIF($9, ''), NULLIF($10, '')::inet, $11::jsonb, NULLIF($12, ''), $13)`,
		r.OccurredAt, r.ActorType, r.ActorID, r.ActorRole, r.Action, r.TargetType, r.TargetID, r.Result,
		r.Device, r.IP, r.Detail, prev, hash)
	return err
}

type auditRow struct {
	OccurredAt                                        time.Time
	ActorType, ActorID, ActorRole, Action, TargetType string
	TargetID, Result, Device, IP, Detail              string
}

func (r auditRow) hash(prev string) string {
	b, _ := json.Marshal([]string{prev, r.OccurredAt.UTC().Format(time.RFC3339Nano), r.ActorType, r.ActorID, r.ActorRole,
		r.Action, r.TargetType, r.TargetID, r.Result, r.Device, r.IP, r.Detail})
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func truncate(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n])
	}
	return s
}

// VerifyAudit recomputes the hash chain. It returns the number of rows checked and the id of the first
// row that doesn't match (0 when the chain is intact).
func (s *Service) VerifyAudit(ctx context.Context) (int, int64, error) {
	rows, err := s.Pool.Query(ctx, `
		SELECT id, occurred_at, actor_type, COALESCE(actor_id::text, ''), COALESCE(actor_role, ''), action,
		       COALESCE(target_type, ''), COALESCE(target_id::text, ''), result, COALESCE(device, ''),
		       COALESCE(host(ip), ''), COALESCE(detail::text, ''), COALESCE(prev_hash, ''), row_hash
		FROM admin.audit_log ORDER BY id`)
	if err != nil {
		return 0, 0, err
	}
	defer rows.Close()
	prev, n := "", 0
	for rows.Next() {
		var id int64
		var r auditRow
		var prevHash, rowHash string
		if err := rows.Scan(&id, &r.OccurredAt, &r.ActorType, &r.ActorID, &r.ActorRole, &r.Action, &r.TargetType,
			&r.TargetID, &r.Result, &r.Device, &r.IP, &r.Detail, &prevHash, &rowHash); err != nil {
			return n, 0, err
		}
		n++
		if prevHash != prev || r.hash(prev) != rowHash {
			return n, id, nil
		}
		prev = rowHash
	}
	return n, 0, rows.Err()
}

// ---------------------------------------------------------------------------
// Sessions
// ---------------------------------------------------------------------------

const (
	SessionIdle     = 30 * time.Minute
	SessionAbsolute = 8 * time.Hour
)

type Session struct {
	Staff *Staff
	CSRF  string
	hash  string
}

func randomToken() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

func tokenHash(t string) string {
	sum := sha256.Sum256([]byte(t))
	return hex.EncodeToString(sum[:])
}

// StartSession signs a staff member in and returns the cookie token. Only its hash is stored.
func (s *Service) StartSession(ctx context.Context, staff uuid.UUID, ip, userAgent string) (string, error) {
	token := randomToken()
	now := s.now()
	var addr *string
	if p := net.ParseIP(ip); p != nil {
		v := p.String()
		addr = &v
	}
	_, err := s.Pool.Exec(ctx, `
		INSERT INTO admin.staff_sessions (token_hash, staff_id, csrf_token, ip, user_agent, created_at, last_seen_at, expires_at)
		VALUES ($1, $2, $3, $4::inet, $5, $6, $6, $7)`,
		tokenHash(token), staff, randomToken(), addr, truncate(userAgent, 200), now, now.Add(SessionAbsolute))
	return token, err
}

// SessionFrom returns the live session for a cookie token and marks it as used, or ErrForbidden.
func (s *Service) SessionFrom(ctx context.Context, token string) (*Session, error) {
	if token == "" {
		return nil, ErrForbidden
	}
	h := tokenHash(token)
	now := s.now()
	var staff uuid.UUID
	var csrf string
	err := s.Pool.QueryRow(ctx, `
		UPDATE admin.staff_sessions SET last_seen_at = $2
		WHERE token_hash = $1 AND revoked_at IS NULL AND expires_at > $2 AND last_seen_at > $3
		RETURNING staff_id, csrf_token`, h, now, now.Add(-SessionIdle)).Scan(&staff, &csrf)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrForbidden
	}
	if err != nil {
		return nil, err
	}
	st, err := s.LoadStaff(ctx, staff) // a suspended staff member, or one whose roles were removed, is out at once
	if err != nil {
		return nil, err
	}
	return &Session{Staff: st, CSRF: csrf, hash: h}, nil
}

func (s *Service) EndSession(ctx context.Context, token string) error {
	_, err := s.Pool.Exec(ctx, `UPDATE admin.staff_sessions SET revoked_at = $2 WHERE token_hash = $1 AND revoked_at IS NULL`,
		tokenHash(token), s.now())
	return err
}

// StaffForLogin finds the staff member for a verified identity from the company's identity provider.
// The first sign-in binds an invited email ("invite:<email>") to the provider's subject; after that only
// the subject counts, so changing an email at the provider can't take over another staff account.
func (s *Service) StaffForLogin(ctx context.Context, subject, email string) (uuid.UUID, error) {
	var id uuid.UUID
	err := s.Pool.QueryRow(ctx, `SELECT id FROM admin.staff WHERE sso_subject = $1`, subject).Scan(&id)
	if err == nil {
		return id, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, err
	}
	err = s.Pool.QueryRow(ctx, `
		UPDATE admin.staff SET sso_subject = $1 WHERE lower(email) = lower($2::text) AND sso_subject = 'invite:' || lower($2::text)
		RETURNING id`, subject, email).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, ErrForbidden
	}
	return id, err
}

// Bootstrap is the system account that grants the first roles.
var Bootstrap = uuid.MustParse("00000000-0000-0000-0000-000000000001")

// Invite adds a staff member who can sign in once with the given email at the identity provider.
// Used by devtool and, later, by staff management (which needs a second person).
func (s *Service) Invite(ctx context.Context, email, name string, roles []string, grantedBy uuid.UUID) (uuid.UUID, error) {
	email = strings.ToLower(strings.TrimSpace(email))
	var id uuid.UUID
	err := pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `
			INSERT INTO admin.staff (email, full_name, sso_subject) VALUES ($1::text, $2, 'invite:' || $1::text)
			ON CONFLICT (email) DO UPDATE SET full_name = EXCLUDED.full_name RETURNING id`, email, name).Scan(&id); err != nil {
			return err
		}
		for _, r := range roles {
			if _, err := tx.Exec(ctx, `
				INSERT INTO admin.staff_roles (staff_id, role_id, granted_by) VALUES ($1, $2, $3) ON CONFLICT DO NOTHING`,
				id, r, grantedBy); err != nil {
				return err
			}
		}
		return s.audit(ctx, tx, Actor{Staff: &Staff{ID: grantedBy}}, "staff.invite", "staff", id, "allowed",
			map[string]any{"email": email, "roles": roles})
	})
	return id, err
}
