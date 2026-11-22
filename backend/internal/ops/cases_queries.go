package ops

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"superapp/backend/internal/money"
)

type CaseRow struct {
	ID         uuid.UUID
	Number     string
	SubjectID  uuid.UUID
	Subject    string // name, or masked phone
	Summary    string
	Score      int
	Amount     money.Money
	Status     string
	Assignee   string
	SLADue     time.Time
	Overdue    bool
	CreatedAt  time.Time
	Resolution string
}

const caseCols = `c.id, c.number, c.subject_id, COALESCE(NULLIF(u.display_name, ''), ''), COALESCE(u.phone_e164, ''), c.summary,
	c.risk_score, COALESCE(c.amount_minor, 0), COALESCE(c.currency::text, 'ETB'), c.status, COALESCE(st.full_name, ''),
	c.sla_due_at, c.created_at, COALESCE(c.resolution, '')`

func (s *Service) scanCase(r pgx.Row) (CaseRow, error) {
	var c CaseRow
	var name, phone, cur string
	err := r.Scan(&c.ID, &c.Number, &c.SubjectID, &name, &phone, &c.Summary, &c.Score, &c.Amount.AmountMinor, &cur, &c.Status,
		&c.Assignee, &c.SLADue, &c.CreatedAt, &c.Resolution)
	c.Amount.Currency = money.Currency(cur)
	c.Subject = MaskPhone(phone)
	if name != "" {
		c.Subject = name + " " + c.Subject
	}
	open := c.Status != "resolved" && c.Status != "escalated_aml"
	c.Overdue = open && s.now().After(c.SLADue)
	return c, err
}

// Cases lists open cases (most urgent first) or closed ones (newest first).
func (s *Service) Cases(ctx context.Context, open bool, limit int) ([]CaseRow, error) {
	order := `c.sla_due_at, c.risk_score DESC`
	if !open {
		order = `c.updated_at DESC`
	}
	rows, err := s.Pool.Query(ctx, `
		SELECT `+caseCols+` FROM risk.cases c
		LEFT JOIN identity.users u ON c.subject_type = 'user' AND u.id = c.subject_id
		LEFT JOIN admin.staff st ON st.id = c.assignee_id
		WHERE (c.status NOT IN ('resolved', 'escalated_aml')) = $1
		ORDER BY `+order+` LIMIT $2`, open, limit)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (CaseRow, error) { return s.scanCase(r) })
}

type CaseIntent struct {
	ID        uuid.UUID
	Kind      string
	Amount    money.Money
	Payee     string
	Status    string
	Failure   string
	Rules     []string
	CreatedAt time.Time
}

type CaseNote struct {
	Staff string
	Body  string
	At    time.Time
}

type CaseSubject struct {
	ID        uuid.UUID
	Name      string
	Phone     string // masked
	Status    string
	KYC       string
	CreatedAt time.Time
	Balance   money.Money
	Payments  int // succeeded payments in the last 30 days
}

type CaseDetail struct {
	CaseRow
	Subject CaseSubject
	Intents []CaseIntent
	Notes   []CaseNote
}

func (s *Service) Case(ctx context.Context, id uuid.UUID) (*CaseDetail, error) {
	c, err := s.scanCase(s.Pool.QueryRow(ctx, `
		SELECT `+caseCols+` FROM risk.cases c
		LEFT JOIN identity.users u ON c.subject_type = 'user' AND u.id = c.subject_id
		LEFT JOIN admin.staff st ON st.id = c.assignee_id
		WHERE c.id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	d := &CaseDetail{CaseRow: c}
	var phone string
	err = s.Pool.QueryRow(ctx, `
		SELECT u.id, COALESCE(u.display_name, ''), u.phone_e164, u.status::text, u.kyc_level::text, u.created_at,
		       COALESCE((SELECT b.balance_minor FROM ledger.accounts a JOIN ledger.balances b ON b.account_id = a.id
		                 WHERE a.owner_type = 'user' AND a.owner_id = u.id AND a.kind = 'user_wallet' AND a.currency = 'ETB'), 0),
		       (SELECT count(*) FROM payments.intents i WHERE i.payer_user_id = u.id AND i.status = 'succeeded'
		         AND i.completed_at > now() - interval '30 days')
		FROM identity.users u WHERE u.id = $1`, c.SubjectID).
		Scan(&d.Subject.ID, &d.Subject.Name, &phone, &d.Subject.Status, &d.Subject.KYC, &d.Subject.CreatedAt,
			&d.Subject.Balance.AmountMinor, &d.Subject.Payments)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return nil, err
	}
	d.Subject.Phone, d.Subject.Balance.Currency = MaskPhone(phone), "ETB"

	rows, err := s.Pool.Query(ctx, `
		SELECT i.id, i.kind::text, i.amount_minor, i.currency::text,
		       COALESCE(pu.display_name, pm.trade_name, i.rail::text, ''), i.status::text, COALESCE(i.failure_code, ''),
		       COALESCE(rd.rules_hit, '{}'), i.created_at
		FROM risk.case_intents ci JOIN payments.intents i ON i.id = ci.intent_id
		LEFT JOIN identity.users pu ON i.payee_type = 'user' AND pu.id = i.payee_id
		LEFT JOIN merchant.merchants pm ON i.payee_type = 'merchant' AND pm.id = i.payee_id
		LEFT JOIN risk.decisions rd ON rd.intent_id = i.id
		WHERE ci.case_id = $1 ORDER BY i.created_at`, id)
	if err != nil {
		return nil, err
	}
	d.Intents, err = pgx.CollectRows(rows, func(r pgx.CollectableRow) (CaseIntent, error) {
		var it CaseIntent
		var cur string
		err := r.Scan(&it.ID, &it.Kind, &it.Amount.AmountMinor, &cur, &it.Payee, &it.Status, &it.Failure, &it.Rules, &it.CreatedAt)
		it.Amount.Currency = money.Currency(cur)
		return it, err
	})
	if err != nil {
		return nil, err
	}
	rows, err = s.Pool.Query(ctx, `
		SELECT COALESCE(st.full_name, ''), n.body, n.created_at FROM risk.case_notes n
		LEFT JOIN admin.staff st ON st.id = n.staff_id WHERE n.case_id = $1 ORDER BY n.id`, id)
	if err != nil {
		return nil, err
	}
	d.Notes, err = pgx.CollectRows(rows, func(r pgx.CollectableRow) (CaseNote, error) {
		var n CaseNote
		return n, r.Scan(&n.Staff, &n.Body, &n.At)
	})
	return d, err
}

// ---------------------------------------------------------------------------
// Staff
// ---------------------------------------------------------------------------

type StaffRow struct {
	ID         uuid.UUID
	Name       string
	Email      string
	Status     string
	Roles      []string
	Invited    bool // hasn't signed in yet
	LastSignIn *time.Time
	CreatedAt  time.Time
}

func (s *Service) StaffList(ctx context.Context) ([]StaffRow, error) {
	rows, err := s.Pool.Query(ctx, `
		SELECT s.id, s.full_name, s.email::text, s.status,
		       COALESCE(array_agg(r.role_id ORDER BY r.role_id) FILTER (WHERE r.role_id IS NOT NULL), '{}'),
		       s.sso_subject LIKE 'invite:%', (SELECT max(created_at) FROM admin.staff_sessions ss WHERE ss.staff_id = s.id),
		       s.created_at
		FROM admin.staff s LEFT JOIN admin.staff_roles r ON r.staff_id = s.id
		WHERE s.id <> $1
		GROUP BY s.id ORDER BY s.status, s.full_name`, Bootstrap)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (StaffRow, error) {
		var st StaffRow
		return st, r.Scan(&st.ID, &st.Name, &st.Email, &st.Status, &st.Roles, &st.Invited, &st.LastSignIn, &st.CreatedAt)
	})
}
