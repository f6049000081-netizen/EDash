// Package payouts pays merchants what their QR sales earned, through a licensed payout partner.
//
// Money movement, all in the double-entry ledger:
//
//	batch:   merchant_settlement  -> hold (merchant)      the net is reserved for this payout
//	settled: hold (merchant)      -> partner_clearing     the partner paid it out of our float
//
// A payout covers specific ledger entries (merchant.payout_items), so a sale is paid out exactly once and
// the statement can show gross, commission and net per sale. Until the money leaves, it stays reserved
// for the merchant: a failed transfer is retried or held for review, never silently returned or lost.
//
// No payout rail is real until a licensed partner's credentials are configured. Without one, batches are
// created and wait as "scheduled" with last_error "rail_unavailable"; nothing is simulated.
package payouts

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"superapp/backend/internal/ledger"
	"superapp/backend/internal/money"
	"superapp/backend/internal/platform"
)

// Ethiopia is UTC+3 all year. Daily payouts cover sales up to midnight in Addis Ababa.
var EAT = time.FixedZone("EAT", 3*60*60)

type Status string

const (
	Scheduled Status = "scheduled"
	Sent      Status = "sent"
	Settled   Status = "settled"
	OnHold    Status = "on_hold"
)

// Hold reasons, shown to the merchant and to operations.
const (
	HoldNoDestination     = "no_destination"
	HoldMerchantSuspended = "merchant_suspended"
	HoldRejected          = "rejected_by_partner"
	HoldTooManyFailures   = "too_many_failures"
)

type Destination struct {
	ID         uuid.UUID
	MerchantID uuid.UUID
	Rail       string
	BankCode   string
	AccountRef string
	Label      string
	CreatedAt  time.Time
}

type Payout struct {
	ID              uuid.UUID
	MerchantID      uuid.UUID
	PeriodStart     time.Time
	PeriodEnd       time.Time
	Gross           money.Money
	Commission      money.Money
	Fees            money.Money
	Refunds         money.Money
	Net             money.Money
	Status          Status
	HoldReason      string
	DestinationName string
	Rail            string
	PartnerRef      string
	Attempts        int
	LastError       string
	ScheduledFor    time.Time
	SentAt          *time.Time
	SettledAt       *time.Time
	CreatedAt       time.Time
}

// ---------------------------------------------------------------------------
// Rails
// ---------------------------------------------------------------------------

// Transfer is one request to a partner. RequestID is unique per attempt; the partner must treat a repeat
// of the same RequestID as the same transfer, so a timeout can be retried without paying twice.
type Transfer struct {
	RequestID   string
	PayoutID    uuid.UUID
	Amount      money.Money
	Destination Destination
	Narrative   string
}

type RailState string

const (
	RailPending RailState = "pending"
	RailPaid    RailState = "paid"
	RailFailed  RailState = "failed"
)

type RailResult struct {
	State     RailState
	Ref       string // partner's transfer id
	Permanent bool   // with RailFailed: retrying won't help (closed or wrong account)
	Reason    string // partner's code, for the event log
}

// Rail pays out through one licensed partner.
type Rail interface {
	Send(ctx context.Context, t Transfer) (RailResult, error)
	Query(ctx context.Context, requestID string) (RailResult, error)
}

// ---------------------------------------------------------------------------
// Service
// ---------------------------------------------------------------------------

type PINChecker interface {
	Verify(ctx context.Context, userID uuid.UUID, pin string) error
}

type Service struct {
	Pool   *pgxpool.Pool
	Ledger *ledger.Ledger
	PIN    PINChecker
	// Rails by payment rail name ("telebirr", "ethswitch"). A destination whose rail has no entry waits
	// with rail_unavailable.
	Rails map[string]Rail
	// CoolingOff delays payouts to a newly set destination, so the owner can stop a hijacked account
	// change after the notification reaches them.
	CoolingOff  time.Duration
	MinPayout   int64 // smaller balances carry over to the next run
	MaxAttempts int   // definite failures before a payout goes on hold
	Now         func() time.Time
	Log         *slog.Logger
}

func NewService(pool *pgxpool.Pool, l *ledger.Ledger, pin PINChecker) *Service {
	return &Service{Pool: pool, Ledger: l, PIN: pin, Rails: map[string]Rail{},
		CoolingOff: 48 * time.Hour, MinPayout: 10_00, MaxAttempts: 3}
}

var (
	ErrNotFound  = errors.New("payouts: not found")
	ErrForbidden = errors.New("payouts: not allowed for this role")
)

type ValidationError struct{ Msg string }

func (e *ValidationError) Error() string { return e.Msg }

func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

// PartnerID is the ledger owner id for a partner's clearing account (shared with payments).
func PartnerID(rail string) uuid.UUID {
	return uuid.NewSHA1(uuid.NameSpaceURL, []byte("https://gebeya.one/partners/"+rail))
}

// role returns the user's role at the merchant, or ErrNotFound if they aren't a member (so a stranger
// can't tell whether a merchant id exists).
func (s *Service) role(ctx context.Context, db platform.DB, user, merchant uuid.UUID) (string, error) {
	var role string
	err := db.QueryRow(ctx, `SELECT role FROM merchant.members WHERE merchant_id = $1 AND user_id = $2`, merchant, user).Scan(&role)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrNotFound
	}
	return role, err
}

func (s *Service) requireRole(ctx context.Context, db platform.DB, user, merchant uuid.UUID, allowed ...string) error {
	role, err := s.role(ctx, db, user, merchant)
	if err != nil {
		return err
	}
	for _, a := range allowed {
		if role == a {
			return nil
		}
	}
	return ErrForbidden
}

// ---------------------------------------------------------------------------
// Destination
// ---------------------------------------------------------------------------

var (
	mobileRE  = regexp.MustCompile(`^\+251[79][0-9]{8}$`)
	bankRE    = regexp.MustCompile(`^[A-Z0-9]{2,11}$`)
	accountRE = regexp.MustCompile(`^[0-9]{8,20}$`)
)

var railLabels = map[string]string{"telebirr": "telebirr", "cbe_birr": "CBE Birr", "mpesa_et": "M-PESA", "ethswitch": ""}

// SetDestination changes where the merchant's payouts go. Owners only, with their PIN. Every owner and
// accountant is told, and nothing is paid to the new destination until the cooling-off period has passed.
func (s *Service) SetDestination(ctx context.Context, user, merchant uuid.UUID, rail, bankCode, account, pin string) (*Destination, error) {
	if err := s.requireRole(ctx, s.Pool, user, merchant, "owner"); err != nil {
		return nil, err
	}
	account = strings.ReplaceAll(strings.TrimSpace(account), " ", "")
	bankCode = strings.ToUpper(strings.TrimSpace(bankCode))
	var label string
	switch rail {
	case "telebirr", "cbe_birr", "mpesa_et":
		if !mobileRE.MatchString(account) {
			return nil, &ValidationError{"Enter the wallet's phone number as +2519XXXXXXXX or +2517XXXXXXXX."}
		}
		bankCode = ""
		label = railLabels[rail] + " ••" + account[len(account)-4:]
	case "ethswitch":
		if !bankRE.MatchString(bankCode) || !accountRE.MatchString(account) {
			return nil, &ValidationError{"Enter the bank's code and an account number of 8 to 20 digits."}
		}
		label = bankCode + " ••" + account[len(account)-4:]
	default:
		return nil, &ValidationError{"Payouts can go to telebirr, CBE Birr, M-PESA or a bank account (ethswitch)."}
	}
	if err := s.PIN.Verify(ctx, user, pin); err != nil {
		return nil, err
	}
	now := s.now()
	d := &Destination{MerchantID: merchant, Rail: rail, BankCode: bankCode, AccountRef: account, Label: label, CreatedAt: now}
	err := pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT 1 FROM merchant.merchants WHERE id = $1 FOR UPDATE`, merchant); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `
			UPDATE merchant.payout_destinations SET replaced_at = $2 WHERE merchant_id = $1 AND replaced_at IS NULL`, merchant, now); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `
			INSERT INTO merchant.payout_destinations (merchant_id, rail, bank_code, account_ref, label, created_by, created_at)
			VALUES ($1, $2, NULLIF($3, ''), $4, $5, $6, $7) RETURNING id`,
			merchant, rail, bankCode, account, label, user, now).Scan(&d.ID); err != nil {
			return err
		}
		// Payouts waiting for a destination can go once the cooling-off period has passed.
		if _, err := tx.Exec(ctx, `
			UPDATE merchant.payouts SET status = 'scheduled', hold_reason = NULL, scheduled_for = $2, updated_at = $3
			WHERE merchant_id = $1 AND status = 'on_hold' AND hold_reason = 'no_destination'`,
			merchant, now.Add(s.CoolingOff), now); err != nil {
			return err
		}
		return platform.WriteOutbox(ctx, tx, "merchant.payout_destination_changed", merchant, map[string]any{
			"merchant_id": merchant, "label": label, "changed_by": user, "effective_at": now.Add(s.CoolingOff),
		})
	})
	if err != nil {
		return nil, err
	}
	return d, nil
}

// CurrentDestination returns where payouts go now, or nil. Owners, managers and accountants may look.
func (s *Service) CurrentDestination(ctx context.Context, user, merchant uuid.UUID) (*Destination, error) {
	if err := s.requireRole(ctx, s.Pool, user, merchant, "owner", "manager", "accountant"); err != nil {
		return nil, err
	}
	return s.destination(ctx, s.Pool, merchant)
}

func (s *Service) destination(ctx context.Context, db platform.DB, merchant uuid.UUID) (*Destination, error) {
	d := &Destination{MerchantID: merchant}
	var bank *string
	err := db.QueryRow(ctx, `
		SELECT id, rail::text, bank_code, account_ref, label, created_at FROM merchant.payout_destinations
		WHERE merchant_id = $1 AND replaced_at IS NULL`, merchant).
		Scan(&d.ID, &d.Rail, &bank, &d.AccountRef, &d.Label, &d.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if bank != nil {
		d.BankCode = *bank
	}
	return d, err
}

// ---------------------------------------------------------------------------
// Batching
// ---------------------------------------------------------------------------

// Cutoff is the end of the period a merchant's payout due at now covers.
func Cutoff(schedule string, now time.Time) time.Time {
	local := now.In(EAT)
	midnight := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, EAT)
	switch schedule {
	case "instant":
		return now
	case "weekly":
		back := (int(midnight.Weekday()) + 6) % 7 // days since Monday
		return midnight.AddDate(0, 0, -back)
	default: // daily
		return midnight
	}
}

// Batch creates a payout for the merchant's unpaid sales up to cutoff, reserving the net in the ledger.
// It returns nil when there's nothing to pay or the total is below MinPayout (it carries over).
func (s *Service) Batch(ctx context.Context, merchant uuid.UUID, cutoff time.Time) (*Payout, error) {
	cur := money.ETB
	var out *Payout
	err := pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		// One batch at a time per merchant, so two runs can't claim the same sales.
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('payout:' || $1::text, 0))`, merchant); err != nil {
			return err
		}
		settlement, ok, err := s.Ledger.FindAccount(ctx, tx, ledger.MerchantSettlement, ledger.OwnerMerchant, merchant, cur)
		if err != nil || !ok {
			return err
		}
		commissionAcct, _, err := s.Ledger.FindAccount(ctx, tx, ledger.CommissionRevenue, ledger.OwnerPlatform, uuid.Nil, cur)
		if err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `
			SELECT p.entry_id, sum(p.amount_minor), min(p.created_at),
			       COALESCE((SELECT sum(c.amount_minor) FROM ledger.postings c
			                 WHERE c.entry_id = p.entry_id AND c.account_id = $2 AND c.side = 'C'), 0)
			FROM ledger.postings p
			WHERE p.account_id = $1 AND p.side = 'C' AND p.created_at <= $3
			  AND NOT EXISTS (SELECT 1 FROM merchant.payout_items i WHERE i.entry_id = p.entry_id)
			GROUP BY p.entry_id`, settlement, commissionAcct, cutoff)
		if err != nil {
			return err
		}
		type item struct {
			entry           uuid.UUID
			net, commission int64
			at              time.Time
		}
		var items []item
		var net, commission int64
		var start time.Time
		for rows.Next() {
			var it item
			if err := rows.Scan(&it.entry, &it.net, &it.at, &it.commission); err != nil {
				rows.Close()
				return err
			}
			items = append(items, it)
			net += it.net
			commission += it.commission
			if start.IsZero() || it.at.Before(start) {
				start = it.at
			}
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		if len(items) == 0 || net < s.MinPayout {
			return nil
		}

		now := s.now()
		var merchantStatus string
		if err := tx.QueryRow(ctx, `SELECT status FROM merchant.merchants WHERE id = $1`, merchant).Scan(&merchantStatus); err != nil {
			return err
		}
		dest, err := s.destination(ctx, tx, merchant)
		if err != nil {
			return err
		}
		status, reason, when := Scheduled, "", now
		switch {
		case merchantStatus != "active":
			status, reason = OnHold, HoldMerchantSuspended
		case dest == nil:
			status, reason = OnHold, HoldNoDestination
		case dest.CreatedAt.Add(s.CoolingOff).After(now):
			when = dest.CreatedAt.Add(s.CoolingOff)
		}
		id, err := uuid.NewV7()
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO merchant.payouts (id, merchant_id, period_start, period_end, gross_minor, commission_minor,
				fees_minor, refunds_minor, net_minor, currency, status, hold_reason, scheduled_for, created_at, updated_at)
			VALUES ($1, $2, $3, $4, $5, $6, 0, 0, $7, $8, $9, NULLIF($10, ''), $11, $12, $12)`,
			id, merchant, start, cutoff, net+commission, commission, net, string(cur), string(status), reason, when, now); err != nil {
			return err
		}
		for _, it := range items {
			if _, err := tx.Exec(ctx, `
				INSERT INTO merchant.payout_items (entry_id, payout_id, gross_minor, commission_minor, net_minor)
				VALUES ($1, $2, $3, $4, $5)`, it.entry, id, it.net+it.commission, it.commission, it.net); err != nil {
				return err
			}
		}
		hold, err := s.Ledger.EnsureAccount(ctx, tx, ledger.Hold, ledger.OwnerMerchant, merchant, cur)
		if err != nil {
			return err
		}
		if _, err := s.Ledger.Post(ctx, tx, ledger.Entry{
			Kind: "payout_reserve", SourceType: "merchant.payout", SourceID: id,
			Postings: []ledger.Posting{
				{AccountID: settlement, Side: ledger.Debit, AmountMinor: net, Currency: cur},
				{AccountID: hold, Side: ledger.Credit, AmountMinor: net, Currency: cur},
			},
		}); err != nil {
			return err
		}
		if status == OnHold {
			if err := s.event(ctx, tx, "payout.on_hold", id, merchant, net, reason, ""); err != nil {
				return err
			}
		}
		out = &Payout{ID: id, MerchantID: merchant, PeriodStart: start, PeriodEnd: cutoff,
			Gross: money.New(net+commission, cur), Commission: money.New(commission, cur), Fees: money.New(0, cur),
			Refunds: money.New(0, cur), Net: money.New(net, cur), Status: status, HoldReason: reason, ScheduledFor: when, CreatedAt: now}
		return nil
	})
	return out, err
}

func (s *Service) event(ctx context.Context, tx pgx.Tx, topic string, payout, merchant uuid.UUID, net int64, reason, label string) error {
	return platform.WriteOutbox(ctx, tx, topic, merchant, map[string]any{
		"payout_id": payout, "merchant_id": merchant, "net": money.New(net, money.ETB), "reason": reason, "destination": label,
	})
}

// ---------------------------------------------------------------------------
// Sending and settling
// ---------------------------------------------------------------------------

// RunOnce batches every merchant whose payout is due, sends what can be sent and checks transfers in
// flight. The API server calls it every few minutes.
func (s *Service) RunOnce(ctx context.Context) error {
	now := s.now()
	rows, err := s.Pool.Query(ctx, `
		SELECT m.id, m.payout_schedule FROM merchant.merchants m
		JOIN ledger.accounts a ON a.owner_type = 'merchant' AND a.owner_id = m.id AND a.kind = 'merchant_settlement'
		JOIN ledger.balances b ON b.account_id = a.id
		WHERE b.balance_minor > 0`)
	if err != nil {
		return err
	}
	type due struct {
		id       uuid.UUID
		schedule string
	}
	var merchants []due
	for rows.Next() {
		var d due
		if err := rows.Scan(&d.id, &d.schedule); err != nil {
			rows.Close()
			return err
		}
		merchants = append(merchants, d)
	}
	rows.Close()
	var errs []error
	for _, m := range merchants {
		if _, err := s.Batch(ctx, m.id, Cutoff(m.schedule, now)); err != nil {
			errs = append(errs, fmt.Errorf("batch %s: %w", m.id, err))
		}
	}
	if err := s.SendDue(ctx); err != nil {
		errs = append(errs, err)
	}
	if err := s.CheckSent(ctx); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

// SendDue sends scheduled payouts whose time has come.
func (s *Service) SendDue(ctx context.Context) error {
	for {
		n, err := s.sendOne(ctx)
		if err != nil || n == 0 {
			return err
		}
	}
}

func (s *Service) sendOne(ctx context.Context) (int, error) {
	now := s.now()
	handled := 0
	err := pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		var id, merchant uuid.UUID
		var net int64
		var attempts int
		err := tx.QueryRow(ctx, `
			SELECT id, merchant_id, net_minor, attempts FROM merchant.payouts
			WHERE status = 'scheduled' AND scheduled_for <= $1
			ORDER BY scheduled_for LIMIT 1 FOR UPDATE SKIP LOCKED`, now).Scan(&id, &merchant, &net, &attempts)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		handled = 1
		hold := func(reason string) error {
			if _, err := tx.Exec(ctx, `UPDATE merchant.payouts SET status = 'on_hold', hold_reason = $2, updated_at = $3 WHERE id = $1`,
				id, reason, now); err != nil {
				return err
			}
			return s.event(ctx, tx, "payout.on_hold", id, merchant, net, reason, "")
		}
		later := func(at time.Time, lastErr string) error {
			_, err := tx.Exec(ctx, `UPDATE merchant.payouts SET scheduled_for = $2, last_error = NULLIF($3, ''), updated_at = $4 WHERE id = $1`,
				id, at, lastErr, now)
			return err
		}

		var merchantStatus string
		if err := tx.QueryRow(ctx, `SELECT status FROM merchant.merchants WHERE id = $1`, merchant).Scan(&merchantStatus); err != nil {
			return err
		}
		if merchantStatus != "active" {
			return hold(HoldMerchantSuspended)
		}
		dest, err := s.destination(ctx, tx, merchant)
		if err != nil {
			return err
		}
		if dest == nil {
			return hold(HoldNoDestination)
		}
		if ready := dest.CreatedAt.Add(s.CoolingOff); ready.After(now) {
			return later(ready, "") // the destination changed since this was scheduled
		}
		rail := s.Rails[dest.Rail]
		if rail == nil {
			return later(now.Add(time.Hour), "rail_unavailable")
		}
		reqID := fmt.Sprintf("%s-%d", id, attempts+1)
		res, err := rail.Send(ctx, Transfer{RequestID: reqID, PayoutID: id, Amount: money.New(net, money.ETB),
			Destination: *dest, Narrative: "EDash payout"})
		if err != nil {
			// Unknown outcome (timeout, 5xx). The same request id is sent again later, which the partner
			// treats as the same transfer.
			if s.Log != nil {
				s.Log.Warn("payout send failed", "payout", id, "err", err)
			}
			return later(now.Add(10*time.Minute), "send_error")
		}
		if _, err := tx.Exec(ctx, `
			UPDATE merchant.payouts SET destination_id = $2, rail = $3::payments.rail, attempts = $4, partner_ref = NULLIF($5, ''),
				status = 'sent', sent_at = $6, last_error = NULL, updated_at = $6
			WHERE id = $1`, id, dest.ID, dest.Rail, attempts+1, res.Ref, now); err != nil {
			return err
		}
		return s.apply(ctx, tx, id, merchant, net, attempts+1, dest.Rail, dest.Label, res, now)
	})
	return handled, err
}

// apply moves a sent payout on according to the partner's answer.
func (s *Service) apply(ctx context.Context, tx pgx.Tx, id, merchant uuid.UUID, net int64, attempts int, rail, label string, res RailResult, now time.Time) error {
	var base int
	if err := tx.QueryRow(ctx, `SELECT attempt_base FROM merchant.payouts WHERE id = $1`, id).Scan(&base); err != nil {
		return err
	}
	switch res.State {
	case RailPaid:
		cur := money.ETB
		hold, err := s.Ledger.EnsureAccount(ctx, tx, ledger.Hold, ledger.OwnerMerchant, merchant, cur)
		if err != nil {
			return err
		}
		clearing, err := s.Ledger.EnsureAccount(ctx, tx, ledger.PartnerClearing, ledger.OwnerPartner, PartnerID(rail), cur)
		if err != nil {
			return err
		}
		if _, err := s.Ledger.Post(ctx, tx, ledger.Entry{
			Kind: "payout", SourceType: "merchant.payout", SourceID: id,
			Postings: []ledger.Posting{
				{AccountID: hold, Side: ledger.Debit, AmountMinor: net, Currency: cur},
				{AccountID: clearing, Side: ledger.Credit, AmountMinor: net, Currency: cur},
			},
		}); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `
			UPDATE merchant.payouts SET status = 'settled', settled_at = $2, partner_ref = COALESCE(NULLIF($3, ''), partner_ref), updated_at = $2
			WHERE id = $1`, id, now, res.Ref); err != nil {
			return err
		}
		return s.event(ctx, tx, "payout.settled", id, merchant, net, "", label)
	case RailFailed:
		// The money is still reserved for the merchant in the hold account.
		reason := HoldRejected
		if !res.Permanent && attempts-base < s.MaxAttempts {
			_, err := tx.Exec(ctx, `
				UPDATE merchant.payouts SET status = 'scheduled', scheduled_for = $2, last_error = $3, updated_at = $4 WHERE id = $1`,
				id, now.Add(time.Duration(attempts)*30*time.Minute), "partner: "+res.Reason, now)
			return err
		}
		if !res.Permanent {
			reason = HoldTooManyFailures
		}
		if _, err := tx.Exec(ctx, `
			UPDATE merchant.payouts SET status = 'on_hold', hold_reason = $2, last_error = $3, updated_at = $4 WHERE id = $1`,
			id, reason, "partner: "+res.Reason, now); err != nil {
			return err
		}
		return s.event(ctx, tx, "payout.on_hold", id, merchant, net, reason, label)
	}
	return nil // pending: CheckSent asks again
}

// ReleaseTx sends a held payout again, after operations staff (two of them) have checked it. Failures are
// counted afresh; request ids keep counting up, so the partner never sees an old one reused. A payout
// held for a missing account, or for a shop that is still suspended, can't be released: it would only
// be held again.
func (s *Service) ReleaseTx(ctx context.Context, tx pgx.Tx, payout uuid.UUID) error {
	var status, reason, merchantStatus string
	var hasDest bool
	err := tx.QueryRow(ctx, `
		SELECT p.status, COALESCE(p.hold_reason, ''), m.status,
		       EXISTS (SELECT 1 FROM merchant.payout_destinations d WHERE d.merchant_id = p.merchant_id AND d.replaced_at IS NULL)
		FROM merchant.payouts p JOIN merchant.merchants m ON m.id = p.merchant_id
		WHERE p.id = $1 FOR UPDATE OF p`, payout).Scan(&status, &reason, &merchantStatus, &hasDest)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	switch {
	case status != string(OnHold):
		return &ValidationError{"Only payouts on hold can be released; this one is " + status + "."}
	case !hasDest:
		return &ValidationError{"The shop has no payout account. The payout goes by itself once the owner adds one."}
	case merchantStatus != "active":
		return &ValidationError{"The shop is " + merchantStatus + ". Reinstate it first."}
	}
	_, err = tx.Exec(ctx, `
		UPDATE merchant.payouts SET status = 'scheduled', hold_reason = NULL, attempt_base = attempts,
			scheduled_for = $2, updated_at = $2 WHERE id = $1`, payout, s.now())
	return err
}

// ResumeAfterReinstateTx schedules the payouts that were held only because the shop was suspended.
func (s *Service) ResumeAfterReinstateTx(ctx context.Context, tx pgx.Tx, merchant uuid.UUID) error {
	_, err := tx.Exec(ctx, `
		UPDATE merchant.payouts SET status = 'scheduled', hold_reason = NULL, scheduled_for = $2, updated_at = $2
		WHERE merchant_id = $1 AND status = 'on_hold' AND hold_reason = 'merchant_suspended'`, merchant, s.now())
	return err
}

// CheckSent asks the partner about transfers that haven't finished.
func (s *Service) CheckSent(ctx context.Context) error {
	now := s.now()
	rows, err := s.Pool.Query(ctx, `
		SELECT id FROM merchant.payouts WHERE status = 'sent' AND sent_at < $1 ORDER BY sent_at LIMIT 200`, now.Add(-time.Minute))
	if err != nil {
		return err
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
	if err != nil {
		return err
	}
	var errs []error
	for _, id := range ids {
		err := pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
			var merchant uuid.UUID
			var net int64
			var attempts int
			var rail, label, status string
			err := tx.QueryRow(ctx, `
				SELECT p.merchant_id, p.net_minor, p.attempts, p.rail::text, d.label, p.status FROM merchant.payouts p
				JOIN merchant.payout_destinations d ON d.id = p.destination_id
				WHERE p.id = $1 FOR UPDATE OF p`, id).Scan(&merchant, &net, &attempts, &rail, &label, &status)
			if err != nil || status != string(Sent) {
				return err
			}
			r := s.Rails[rail]
			if r == nil {
				return nil // configuration removed; keep it as sent until the partner can be asked
			}
			res, err := r.Query(ctx, fmt.Sprintf("%s-%d", id, attempts))
			if err != nil {
				return err
			}
			return s.apply(ctx, tx, id, merchant, net, attempts, rail, label, res, now)
		})
		if err != nil {
			errs = append(errs, fmt.Errorf("check %s: %w", id, err))
		}
	}
	return errors.Join(errs...)
}

// Run calls RunOnce every interval until ctx ends.
func (s *Service) Run(ctx context.Context, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		if err := s.RunOnce(ctx); err != nil && s.Log != nil && ctx.Err() == nil {
			s.Log.Error("payouts run", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// ---------------------------------------------------------------------------
// History
// ---------------------------------------------------------------------------

// List returns the merchant's payouts, newest first. Owners, managers and accountants may look.
func (s *Service) List(ctx context.Context, user, merchant uuid.UUID, before *time.Time, beforeID uuid.UUID, limit int) ([]Payout, error) {
	if err := s.requireRole(ctx, s.Pool, user, merchant, "owner", "manager", "accountant"); err != nil {
		return nil, err
	}
	if limit <= 0 || limit > 100 {
		limit = 30
	}
	t := time.Date(9999, 1, 1, 0, 0, 0, 0, time.UTC)
	if before != nil {
		t = *before
	} else {
		beforeID = uuid.Max
	}
	rows, err := s.Pool.Query(ctx, `
		SELECT p.id, p.period_start, p.period_end, p.gross_minor, p.commission_minor, p.fees_minor, p.refunds_minor,
		       p.net_minor, p.currency::text, p.status, COALESCE(p.hold_reason, ''),
		       COALESCE(d.label, cur.label, ''), COALESCE(p.rail::text, ''), COALESCE(p.partner_ref, ''), p.attempts,
		       COALESCE(p.last_error, ''), p.scheduled_for, p.sent_at, p.settled_at, p.created_at
		FROM merchant.payouts p
		LEFT JOIN merchant.payout_destinations d ON d.id = p.destination_id
		LEFT JOIN merchant.payout_destinations cur ON cur.merchant_id = p.merchant_id AND cur.replaced_at IS NULL
		WHERE p.merchant_id = $1 AND (p.created_at, p.id) < ($2, $3)
		ORDER BY p.created_at DESC, p.id DESC LIMIT $4`, merchant, t, beforeID, limit)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Payout, error) {
		var p Payout
		var g, c, f, rf, n int64
		var cur, status string
		err := r.Scan(&p.ID, &p.PeriodStart, &p.PeriodEnd, &g, &c, &f, &rf, &n, &cur, &status, &p.HoldReason,
			&p.DestinationName, &p.Rail, &p.PartnerRef, &p.Attempts, &p.LastError, &p.ScheduledFor, &p.SentAt, &p.SettledAt, &p.CreatedAt)
		cc := money.Currency(cur)
		p.MerchantID, p.Status = merchant, Status(status)
		p.Gross, p.Commission, p.Fees, p.Refunds, p.Net = money.New(g, cc), money.New(c, cc), money.New(f, cc), money.New(rf, cc), money.New(n, cc)
		return p, err
	})
}
