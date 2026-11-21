package payments

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"superapp/backend/internal/ledger"
	"superapp/backend/internal/money"
	"superapp/backend/internal/platform"
)

// ---------------------------------------------------------------------------
// KYC limits
//
// identity.kyc_limits holds, per KYC level and currency, the most a wallet may hold and the most it
// may send per day and per calendar month (Ethiopian time). Compliance sets the values from the
// National Bank of Ethiopia directives; they are configuration, not code.
//
// Until values are configured a missing row means "no limit". Set Service.LimitsRequired in
// production so a missing row blocks the payment instead (fail closed).
// ---------------------------------------------------------------------------

// EAT is Ethiopia's time zone (UTC+3, no daylight saving). Limit days and months follow it.
var EAT = time.FixedZone("EAT", 3*60*60)

type Limits struct {
	MaxBalance int64
	DailyOut   int64
	MonthlyOut int64
}

// LimitError says which limit a payment hits and how much room is left.
type LimitError struct {
	Scope     string // daily, monthly, balance (the user's own wallet cap), recipient (payee's wallet cap)
	Remaining money.Money
}

func (e *LimitError) Error() string {
	return fmt.Sprintf("%s limit reached; %s remaining", e.Scope, e.Remaining)
}

var ErrKYCRequired = errors.New("identity verification required")

// limitsFor returns the limits for the user's KYC level. ok is false when none are configured.
func (s *Service) limitsFor(ctx context.Context, db platform.DB, user uuid.UUID, cur money.Currency) (Limits, bool, error) {
	var l Limits
	err := db.QueryRow(ctx, `
		SELECT l.max_balance_minor, l.daily_out_minor, l.monthly_out_minor
		FROM identity.users u JOIN identity.kyc_limits l ON l.level = u.kyc_level AND l.currency = $2::platform.currency
		WHERE u.id = $1`, user, string(cur)).Scan(&l.MaxBalance, &l.DailyOut, &l.MonthlyOut)
	if errors.Is(err, pgx.ErrNoRows) {
		if s.LimitsRequired {
			return l, false, ErrKYCRequired
		}
		return l, false, nil
	}
	return l, err == nil, err
}

func dayStart(t time.Time) time.Time {
	t = t.In(EAT)
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, EAT)
}

func monthStart(t time.Time) time.Time {
	t = t.In(EAT)
	return time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, EAT)
}

// spentSince sums outgoing payments confirmed since a moment. Held payments count: they may still go
// through, so their amount is already committed.
func spentSince(ctx context.Context, db platform.DB, user uuid.UUID, cur money.Currency, since time.Time) (int64, error) {
	var n int64
	err := db.QueryRow(ctx, `
		SELECT COALESCE(sum(amount_minor + fee_minor), 0)::bigint FROM payments.intents
		WHERE payer_user_id = $1 AND currency = $2 AND kind <> 'topup'
		  AND status IN ('succeeded', 'held_for_review', 'processing') AND confirmed_at >= $3`,
		user, string(cur), since).Scan(&n)
	return n, err
}

// checkSpend refuses a payment that would take the user over their daily or monthly limit. To be
// exact under concurrency the caller must hold the payer's identity.users row lock.
func (s *Service) checkSpend(ctx context.Context, db platform.DB, user uuid.UUID, amt money.Money, now time.Time) error {
	lim, ok, err := s.limitsFor(ctx, db, user, amt.Currency)
	if err != nil || !ok {
		return err
	}
	for _, w := range []struct {
		scope string
		since time.Time
		cap   int64
	}{{"daily", dayStart(now), lim.DailyOut}, {"monthly", monthStart(now), lim.MonthlyOut}} {
		spent, err := spentSince(ctx, db, user, amt.Currency, w.since)
		if err != nil {
			return err
		}
		if spent+amt.AmountMinor > w.cap {
			return &LimitError{Scope: w.scope, Remaining: money.New(max(0, w.cap-spent), amt.Currency)}
		}
	}
	return nil
}

func (s *Service) walletBalance(ctx context.Context, db platform.DB, user uuid.UUID, cur money.Currency) (int64, error) {
	acct, ok, err := s.Ledger.FindAccount(ctx, db, ledger.UserWallet, ledger.OwnerUser, user, cur)
	if err != nil || !ok {
		return 0, err
	}
	return s.Ledger.Balance(ctx, db, acct)
}

// checkBalanceCap refuses when the wallet balance plus extra would exceed the user's maximum balance.
func (s *Service) checkBalanceCap(ctx context.Context, db platform.DB, user uuid.UUID, cur money.Currency, extra int64, scope string) error {
	lim, ok, err := s.limitsFor(ctx, db, user, cur)
	if err != nil || !ok {
		return err
	}
	bal, err := s.walletBalance(ctx, db, user, cur)
	if err != nil {
		return err
	}
	if bal+extra > lim.MaxBalance {
		return &LimitError{Scope: scope, Remaining: money.New(max(0, lim.MaxBalance-bal), cur)}
	}
	return nil
}

// LimitStatus is what GET /v1/me shows: the balance cap and what's left to send today and this month.
type LimitStatus struct {
	MaxBalance       money.Money
	DailyRemaining   money.Money
	MonthlyRemaining money.Money
}

// LimitStatus returns nil when no limits are configured for the user's level and home currency.
func (s *Service) LimitStatus(ctx context.Context, user uuid.UUID) (*LimitStatus, error) {
	var cur string
	if err := s.Pool.QueryRow(ctx, `SELECT home_currency::text FROM identity.users WHERE id = $1`, user).Scan(&cur); err != nil {
		return nil, err
	}
	c := money.Currency(cur)
	lim, ok, err := s.limitsFor(ctx, s.Pool, user, c)
	if errors.Is(err, ErrKYCRequired) {
		return &LimitStatus{MaxBalance: money.New(0, c), DailyRemaining: money.New(0, c), MonthlyRemaining: money.New(0, c)}, nil
	}
	if err != nil || !ok {
		return nil, err
	}
	now := time.Now()
	day, err := spentSince(ctx, s.Pool, user, c, dayStart(now))
	if err != nil {
		return nil, err
	}
	month, err := spentSince(ctx, s.Pool, user, c, monthStart(now))
	if err != nil {
		return nil, err
	}
	monthLeft := max(0, lim.MonthlyOut-month)
	return &LimitStatus{
		MaxBalance:       money.New(lim.MaxBalance, c),
		DailyRemaining:   money.New(min(max(0, lim.DailyOut-day), monthLeft), c),
		MonthlyRemaining: money.New(monthLeft, c),
	}, nil
}
