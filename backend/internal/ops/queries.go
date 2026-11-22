package ops

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"superapp/backend/internal/money"
)

// MaskPhone shows the last four digits only ("••4471"), as the design asks for personal data.
func MaskPhone(p string) string {
	if len(p) < 4 {
		return "••"
	}
	return "••" + p[len(p)-4:]
}

type Counts struct {
	Applications, Approvals, HeldPayouts, Suspended int
	Cases, CasesOverdue                             int
	OrdersToSettle                                  int
}

func (s *Service) Counts(ctx context.Context, viewer uuid.UUID) (Counts, error) {
	var c Counts
	err := s.Pool.QueryRow(ctx, `
		SELECT (SELECT count(*) FROM merchant.merchants WHERE status IN ('applied', 'kyb_review')),
		       (SELECT count(*) FROM admin.approvals WHERE decision IS NULL AND expires_at > $1 AND requested_by <> $2),
		       (SELECT count(*) FROM merchant.payouts WHERE status = 'on_hold'),
		       (SELECT count(*) FROM merchant.merchants WHERE status = 'suspended'),
		       (SELECT count(*) FROM risk.cases WHERE status NOT IN ('resolved', 'escalated_aml')),
		       (SELECT count(*) FROM risk.cases WHERE status NOT IN ('resolved', 'escalated_aml') AND sla_due_at < $1),
		       (SELECT count(*) FROM commerce.orders WHERE review_at IS NOT NULL AND status IN ('accepted', 'ready', 'on_the_way'))`,
		s.now(), viewer).
		Scan(&c.Applications, &c.Approvals, &c.HeldPayouts, &c.Suspended, &c.Cases, &c.CasesOverdue, &c.OrdersToSettle)
	return c, err
}

type MerchantRow struct {
	ID         uuid.UUID
	TradeName  string
	LegalName  string
	TIN        string
	Licence    string
	Category   string
	City       string
	Status     string
	MerchantNo string
	Note       string
	OwnerName  string
	OwnerPhone string // masked
	OwnerKYC   string
	CreatedAt  time.Time
	ApprovedAt *time.Time
	Sales30d   money.Money
	Reserved   money.Money
}

const merchantSelect = `
	SELECT m.id, m.trade_name, m.legal_name, COALESCE(m.tin, ''), COALESCE(m.business_licence, ''), m.category_code,
	       COALESCE(m.city, ''), m.status, COALESCE(m.merchant_no, ''), COALESCE(m.decision_note, ''),
	       COALESCE(u.display_name, ''), COALESCE(u.phone_e164, ''), COALESCE(u.kyc_level::text, ''), m.created_at, m.approved_at
	FROM merchant.merchants m
	LEFT JOIN LATERAL (SELECT user_id FROM merchant.members WHERE merchant_id = m.id AND role = 'owner'
	                   ORDER BY created_at LIMIT 1) o ON true
	LEFT JOIN identity.users u ON u.id = COALESCE(m.applied_by, o.user_id)`

func scanMerchantRow(r pgx.Row) (MerchantRow, error) {
	var m MerchantRow
	err := r.Scan(&m.ID, &m.TradeName, &m.LegalName, &m.TIN, &m.Licence, &m.Category, &m.City, &m.Status, &m.MerchantNo,
		&m.Note, &m.OwnerName, &m.OwnerPhone, &m.OwnerKYC, &m.CreatedAt, &m.ApprovedAt)
	m.OwnerPhone = MaskPhone(m.OwnerPhone)
	return m, err
}

// MerchantList lists merchants with the given statuses, oldest application first (a queue).
func (s *Service) MerchantList(ctx context.Context, statuses []string, limit int) ([]MerchantRow, error) {
	rows, err := s.Pool.Query(ctx, merchantSelect+` WHERE m.status = ANY($1) ORDER BY m.created_at LIMIT $2`, statuses, limit)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (MerchantRow, error) { return scanMerchantRow(r) })
}

func (s *Service) Merchant(ctx context.Context, id uuid.UUID) (*MerchantRow, error) {
	m, err := scanMerchantRow(s.Pool.QueryRow(ctx, merchantSelect+` WHERE m.id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	var sales, reserved int64
	if err := s.Pool.QueryRow(ctx, `
		SELECT COALESCE((SELECT sum(amount_minor) FROM payments.intents WHERE payee_type = 'merchant' AND payee_id = $1
		                 AND status = 'succeeded' AND completed_at > $2), 0),
		       COALESCE((SELECT sum(b.balance_minor) FROM ledger.accounts a JOIN ledger.balances b ON b.account_id = a.id
		                 WHERE a.owner_type = 'merchant' AND a.owner_id = $1 AND a.kind IN ('merchant_settlement', 'hold')), 0)`,
		id, s.now().Add(-30*24*time.Hour)).Scan(&sales, &reserved); err != nil {
		return nil, err
	}
	m.Sales30d, m.Reserved = money.New(sales, money.ETB), money.New(reserved, money.ETB)
	return &m, nil
}

type PayoutRow struct {
	ID         uuid.UUID
	MerchantID uuid.UUID
	Merchant   string
	Net        money.Money
	Status     string
	HoldReason string
	LastError  string
	Attempts   int
	Account    string
	CreatedAt  time.Time
}

// PayoutList lists payouts with a status (all merchants when merchant is uuid.Nil), newest first.
func (s *Service) PayoutList(ctx context.Context, status string, merchant uuid.UUID, limit int) ([]PayoutRow, error) {
	rows, err := s.Pool.Query(ctx, `
		SELECT p.id, p.merchant_id, m.trade_name, p.net_minor, p.status, COALESCE(p.hold_reason, ''), COALESCE(p.last_error, ''),
		       p.attempts, COALESCE(d.label, ''), p.created_at
		FROM merchant.payouts p JOIN merchant.merchants m ON m.id = p.merchant_id
		LEFT JOIN merchant.payout_destinations d ON d.merchant_id = p.merchant_id AND d.replaced_at IS NULL
		WHERE ($1 = '' OR p.status = $1) AND ($2 = '00000000-0000-0000-0000-000000000000'::uuid OR p.merchant_id = $2)
		ORDER BY p.created_at DESC LIMIT $3`, status, merchant, limit)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (PayoutRow, error) {
		var p PayoutRow
		var net int64
		err := r.Scan(&p.ID, &p.MerchantID, &p.Merchant, &net, &p.Status, &p.HoldReason, &p.LastError, &p.Attempts, &p.Account, &p.CreatedAt)
		p.Net = money.New(net, money.ETB)
		return p, err
	})
}

type OrderRow struct {
	ID         uuid.UUID
	Number     string
	MerchantID uuid.UUID
	Shop       string
	ShopPhone  string // the owner's, masked
	Customer   string
	Phone      string // masked
	Channel    string
	Status     string
	Total      money.Money
	Items      string
	Tries      int // wrong handover codes
	AcceptedAt time.Time
	ReadyAt    *time.Time
	ReviewAt   time.Time
}

// OrdersToSettle lists accepted orders that were never handed over and went to operations, oldest first.
func (s *Service) OrdersToSettle(ctx context.Context, limit int) ([]OrderRow, error) {
	rows, err := s.Pool.Query(ctx, `
		SELECT o.id, o.number, o.merchant_id, m.trade_name, COALESCE(ou.phone_e164, ''), COALESCE(u.display_name, ''),
		       u.phone_e164, o.channel, o.status, o.total_minor, o.currency,
		       COALESCE((SELECT string_agg(i.quantity || ' × ' || i.title_snapshot, ', ' ORDER BY i.line_no)
		                 FROM commerce.order_items i WHERE i.order_id = o.id), ''),
		       o.handover_tries, o.accepted_at, o.ready_at, o.review_at
		FROM commerce.orders o JOIN merchant.merchants m ON m.id = o.merchant_id
		JOIN identity.users u ON u.id = o.user_id
		LEFT JOIN LATERAL (SELECT user_id FROM merchant.members WHERE merchant_id = m.id AND role = 'owner'
		                   ORDER BY created_at LIMIT 1) ow ON true
		LEFT JOIN identity.users ou ON ou.id = ow.user_id
		WHERE o.review_at IS NOT NULL AND o.status IN ('accepted', 'ready', 'on_the_way')
		ORDER BY o.review_at LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (OrderRow, error) {
		var o OrderRow
		var total int64
		var cur string
		err := r.Scan(&o.ID, &o.Number, &o.MerchantID, &o.Shop, &o.ShopPhone, &o.Customer, &o.Phone, &o.Channel, &o.Status,
			&total, &cur, &o.Items, &o.Tries, &o.AcceptedAt, &o.ReadyAt, &o.ReviewAt)
		o.Total = money.New(total, money.Currency(cur))
		o.Phone, o.ShopPhone = MaskPhone(o.Phone), MaskPhone(o.ShopPhone)
		return o, err
	})
}

type ApprovalRow struct {
	ID           uuid.UUID
	Kind         string
	Permission   string
	TargetType   string
	TargetID     uuid.UUID
	TargetName   string
	RequestedBy  uuid.UUID
	Requester    string
	RequestNote  string
	RequestedAt  time.Time
	ExpiresAt    time.Time
	Decision     string
	Decider      string
	DecisionNote string
	Params       map[string]string // the requested change's details (staff changes)
}

// Approvals lists requests: open ones (pending) or recently decided ones, newest first.
func (s *Service) Approvals(ctx context.Context, pending bool, target uuid.UUID, limit int) ([]ApprovalRow, error) {
	rows, err := s.Pool.Query(ctx, `
		SELECT a.id, a.payload->>'kind', a.permission, a.target_type, a.target_id,
		       COALESCE(m.trade_name, pm.trade_name, NULLIF(tu.display_name, ''), ts.full_name, rc.number, ic.number, ord.number,
		                a.payload->'params'->>'name', ''),
		       a.requested_by, rs.full_name, a.request_note, a.requested_at, a.expires_at,
		       COALESCE(a.decision, ''), COALESCE(ds.full_name, ''), COALESCE(a.decision_note, ''), a.payload->'params'
		FROM admin.approvals a
		JOIN admin.staff rs ON rs.id = a.requested_by
		LEFT JOIN admin.staff ds ON ds.id = a.decided_by
		LEFT JOIN merchant.merchants m ON a.target_type = 'merchant' AND m.id = a.target_id
		LEFT JOIN merchant.payouts p ON a.target_type = 'payout' AND p.id = a.target_id
		LEFT JOIN merchant.merchants pm ON pm.id = p.merchant_id
		LEFT JOIN identity.users tu ON a.target_type = 'user' AND tu.id = a.target_id
		LEFT JOIN admin.staff ts ON a.target_type = 'staff' AND ts.id = a.target_id
		LEFT JOIN risk.cases rc ON a.target_type = 'case' AND rc.id = a.target_id
		LEFT JOIN commerce.orders ord ON a.target_type = 'order' AND ord.id = a.target_id
		LEFT JOIN LATERAL (SELECT c.number FROM risk.case_intents ci JOIN risk.cases c ON c.id = ci.case_id
		                   WHERE a.target_type = 'payment' AND ci.intent_id = a.target_id LIMIT 1) ic ON true
		WHERE (($1 AND a.decision IS NULL AND a.expires_at > $2) OR (NOT $1 AND (a.decision IS NOT NULL OR a.expires_at <= $2)))
		  AND ($3 = '00000000-0000-0000-0000-000000000000'::uuid OR a.target_id = $3)
		ORDER BY a.requested_at DESC LIMIT $4`, pending, s.now(), target, limit)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (ApprovalRow, error) {
		var a ApprovalRow
		err := r.Scan(&a.ID, &a.Kind, &a.Permission, &a.TargetType, &a.TargetID, &a.TargetName, &a.RequestedBy, &a.Requester,
			&a.RequestNote, &a.RequestedAt, &a.ExpiresAt, &a.Decision, &a.Decider, &a.DecisionNote, &a.Params)
		if a.Decision == "" && !a.ExpiresAt.After(s.now()) {
			a.Decision = "lapsed"
		}
		return a, err
	})
}

type AuditRow struct {
	ID         int64
	At         time.Time
	Staff      string
	Role       string
	Action     string
	TargetType string
	TargetID   string
	Result     string
	Device     string
	IP         string
	Note       string
}

// Audit lists the newest audit entries, optionally for one target.
func (s *Service) Audit(ctx context.Context, target uuid.UUID, limit int) ([]AuditRow, error) {
	rows, err := s.Pool.Query(ctx, `
		SELECT l.id, l.occurred_at, COALESCE(st.full_name, l.actor_type), COALESCE(l.actor_role, ''), l.action,
		       COALESCE(l.target_type, ''), COALESCE(l.target_id::text, ''), l.result, COALESCE(l.device, ''),
		       COALESCE(host(l.ip), ''), COALESCE(l.detail->>'note', '')
		FROM admin.audit_log l LEFT JOIN admin.staff st ON st.id = l.actor_id
		WHERE ($1 = '00000000-0000-0000-0000-000000000000'::uuid OR l.target_id = $1 OR l.detail->>'merchant_id' = $1::text)
		ORDER BY l.id DESC LIMIT $2`, target, limit)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (AuditRow, error) {
		var a AuditRow
		err := r.Scan(&a.ID, &a.At, &a.Staff, &a.Role, &a.Action, &a.TargetType, &a.TargetID, &a.Result, &a.Device, &a.IP, &a.Note)
		return a, err
	})
}
