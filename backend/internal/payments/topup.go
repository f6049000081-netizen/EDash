package payments

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"superapp/backend/internal/ledger"
	"superapp/backend/internal/money"
	"superapp/backend/internal/platform"
)

// ---------------------------------------------------------------------------
// Top-ups from partner rails
//
//  1. StartTopUp records a `processing` intent, then asks the rail for a checkout. Nothing is credited.
//  2. The customer pays inside the partner's own app or checkout page.
//  3. The partner notifies us. The HTTP layer checks the notification's signature and calls SettleTopUp,
//     which asks the partner for the order's state. Only that answer credits the wallet, so a forged or
//     replayed notification can at most trigger a status check.
//  4. A background sweep calls SettleTopUp for top-ups still processing, in case a notification was lost.
//     Orders that stay unpaid past TopUpLimits.Expiry are failed.
//
// The credit is one ledger entry: debit the rail's clearing account (money now held at the partner on
// our behalf), credit the user's wallet. Daily reconciliation matches these against the partner's
// statement.
// ---------------------------------------------------------------------------

// TopUpRail is a licensed partner that collects money into our merchant account.
type TopUpRail interface {
	CreateCheckout(ctx context.Context, orderID string, amt money.Money, title string) (RailCheckout, error)
	QueryOrder(ctx context.Context, orderID string) (RailOrder, error)
}

type RailCheckout struct {
	OrderRef    string // partner's id for the pending order (telebirr prepay_id)
	CheckoutURL string // where the customer pays
}

type RailState string

const (
	RailPending   RailState = "pending"
	RailSucceeded RailState = "succeeded"
	RailFailed    RailState = "failed"
)

type RailOrder struct {
	State      RailState
	Status     string      // partner's raw status, for the event log
	PartnerRef string      // partner's payment reference; set when succeeded
	Amount     money.Money // amount actually paid; set when succeeded
}

type TopUpLimits struct {
	MinMinor     int64
	MaxMinor     int64         // per top-up, until KYC limits are configured
	MaxPending   int           // unfinished top-ups per user
	Expiry       time.Duration // after this an unpaid order is failed
	CheckoutName string        // shown on the partner's checkout page
}

func DefaultTopUpLimits() TopUpLimits {
	return TopUpLimits{
		MinMinor:     100,        // ETB 1
		MaxMinor:     10_000_000, // ETB 100,000
		MaxPending:   5,
		Expiry:       2*time.Hour + 15*time.Minute, // telebirr orders time out after 120 minutes
		CheckoutName: "EDash wallet top-up",
	}
}

var (
	ErrRailError       = errors.New("the payment provider didn't respond")
	ErrTooManyPending  = errors.New("too many unfinished top-ups")
	errTopUpNotPending = errors.New("top-up already settled")
)

// OrderID is the merchant order id we give the partner: the intent id without dashes.
func OrderID(id uuid.UUID) string { return strings.ReplaceAll(id.String(), "-", "") }

// ParseOrderID reverses OrderID.
func ParseOrderID(s string) (uuid.UUID, error) {
	if len(s) != 32 {
		return uuid.Nil, ErrNotFound
	}
	id, err := uuid.Parse(s)
	if err != nil {
		return uuid.Nil, ErrNotFound
	}
	return id, nil
}

func (s *Service) StartTopUp(ctx context.Context, userID uuid.UUID, amt money.Money, rail string) (*Intent, RailCheckout, error) {
	r := s.Rails[rail]
	if r == nil {
		return nil, RailCheckout{}, ErrRailUnavailable
	}
	if amt.Currency != money.ETB {
		return nil, RailCheckout{}, &ValidationError{"Top-ups are in ETB."}
	}
	if amt.AmountMinor < s.TopUp.MinMinor || amt.AmountMinor > s.TopUp.MaxMinor {
		return nil, RailCheckout{}, &ValidationError{fmt.Sprintf("Top up between %s and %s.",
			money.New(s.TopUp.MinMinor, money.ETB), money.New(s.TopUp.MaxMinor, money.ETB))}
	}
	id, err := uuid.NewV7()
	if err != nil {
		return nil, RailCheckout{}, err
	}

	err = pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		// Lock the user row so parallel requests can't slip past the pending cap together.
		var status string
		err := tx.QueryRow(ctx, `SELECT status FROM identity.users WHERE id = $1 FOR UPDATE`, userID).Scan(&status)
		if errors.Is(err, pgx.ErrNoRows) || (err == nil && status != "active") {
			return ErrPayerInactive
		}
		if err != nil {
			return err
		}
		var pending int
		if err := tx.QueryRow(ctx, `
			SELECT count(*) FROM payments.intents
			WHERE payer_user_id = $1 AND kind = 'topup' AND status = 'processing'`, userID).Scan(&pending); err != nil {
			return err
		}
		if pending >= s.TopUp.MaxPending {
			return ErrTooManyPending
		}
		// Unfinished top-ups count toward the balance cap: they may all be paid.
		var pendingSum int64
		if err := tx.QueryRow(ctx, `
			SELECT COALESCE(sum(amount_minor), 0)::bigint FROM payments.intents
			WHERE payer_user_id = $1 AND kind = 'topup' AND status = 'processing' AND currency = $2`,
			userID, string(amt.Currency)).Scan(&pendingSum); err != nil {
			return err
		}
		if err := s.checkBalanceCap(ctx, tx, userID, amt.Currency, amt.AmountMinor+pendingSum, "balance"); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO payments.intents (id, kind, payer_user_id, payee_type, payee_id, amount_minor, currency, rail, status, confirmed_at)
			VALUES ($1, 'topup', $2, 'user', $2, $3, $4, $5, 'processing', now())`,
			id, userID, amt.AmountMinor, string(amt.Currency), rail); err != nil {
			return err
		}
		return addEvent(ctx, tx, id, "", StatusProcessing, "checkout started", "user:"+userID.String())
	})
	if err != nil {
		return nil, RailCheckout{}, err
	}

	// The intent is committed before calling the partner, so a notification can always find it.
	checkout, err := r.CreateCheckout(ctx, OrderID(id), amt, s.TopUp.CheckoutName)
	if err != nil {
		if cerr := s.closeTopUp(ctx, id, StatusFailed, "rail_error", "", "checkout failed", "system", "payment.failed", nil); cerr != nil && !errors.Is(cerr, errTopUpNotPending) {
			return nil, RailCheckout{}, cerr
		}
		return nil, RailCheckout{}, fmt.Errorf("%w: %v", ErrRailError, err)
	}
	in, err := s.Get(ctx, userID, id)
	return in, checkout, err
}

// SettleTopUp asks the rail about a processing top-up and applies the answer. It is safe to call
// any number of times, concurrently: only the first call that sees a final state changes anything.
func (s *Service) SettleTopUp(ctx context.Context, id uuid.UUID) (*Intent, error) {
	var payer uuid.UUID
	var rail, status, kind string
	var created time.Time
	err := s.Pool.QueryRow(ctx, `SELECT payer_user_id, rail, status, kind, created_at FROM payments.intents WHERE id = $1`, id).
		Scan(&payer, &rail, &status, &kind, &created)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && kind != string(KindTopUp)) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if Status(status) != StatusProcessing {
		return s.Get(ctx, payer, id)
	}
	r := s.Rails[rail]
	if r == nil {
		return nil, ErrRailUnavailable
	}
	ord, err := r.QueryOrder(ctx, OrderID(id))
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrRailError, err)
	}
	actor := "partner:" + rail

	switch ord.State {
	case RailPending:
		if time.Since(created) > s.TopUp.Expiry {
			err = s.closeTopUp(ctx, id, StatusFailed, "expired", "", "unpaid after "+s.TopUp.Expiry.String(), "system", "payment.failed", nil)
		}
	case RailFailed:
		err = s.closeTopUp(ctx, id, StatusFailed, "rail_declined", "", "partner status "+ord.Status, actor, "payment.failed", nil)
	case RailSucceeded:
		err = s.closeTopUp(ctx, id, StatusSucceeded, "", ord.PartnerRef, "partner status "+ord.Status, actor, "payment.succeeded", &ord.Amount)
	default:
		return nil, fmt.Errorf("unknown rail state %q", ord.State)
	}
	if err != nil && !errors.Is(err, errTopUpNotPending) {
		return nil, err
	}
	return s.Get(ctx, payer, id)
}

// closeTopUp moves a processing top-up to its final state in one transaction. For a success it
// checks the paid amount and the partner reference before crediting; a mismatch holds the top-up
// for review instead.
func (s *Service) closeTopUp(ctx context.Context, id uuid.UUID, to Status, failure, partnerRef, reason, actor, topic string, paid *money.Money) error {
	return pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		var payer uuid.UUID
		var status, rail, cur string
		var amount int64
		if err := tx.QueryRow(ctx, `
			SELECT payer_user_id, status, rail, amount_minor, currency FROM payments.intents WHERE id = $1 FOR UPDATE`, id).
			Scan(&payer, &status, &rail, &amount, &cur); err != nil {
			return err
		}
		if Status(status) != StatusProcessing {
			return errTopUpNotPending
		}
		want := money.New(amount, money.Currency(cur))

		if to == StatusSucceeded {
			// Serialise credits to one wallet so the balance-cap check below is exact.
			if _, err := tx.Exec(ctx, `SELECT 1 FROM identity.users WHERE id = $1 FOR UPDATE`, payer); err != nil {
				return err
			}
			capErr := s.checkBalanceCap(ctx, tx, payer, want.Currency, want.AmountMinor, "balance")
			var le *LimitError
			if capErr != nil && !errors.As(capErr, &le) && !errors.Is(capErr, ErrKYCRequired) {
				return capErr
			}
			var dup bool
			if err := tx.QueryRow(ctx, `
				SELECT EXISTS (SELECT 1 FROM payments.intents WHERE rail = $1 AND partner_ref = $2 AND id <> $3)`,
				rail, partnerRef, id).Scan(&dup); err != nil {
				return err
			}
			switch {
			case dup:
				// The same partner payment can't fund two top-ups.
				to, failure, partnerRef, topic = StatusHeldForReview, "duplicate_partner_ref", "", "payment.held"
				reason += " (payment reference already used)"
			case capErr != nil:
				// The customer has already paid the partner, so the money can't be refused here.
				// Hold it for operations to refund or release once the user's KYC level allows it.
				to, failure, topic = StatusHeldForReview, "limit_exceeded", "payment.held"
				reason += " (over the wallet's balance limit)"
			case paid == nil || *paid != want:
				got := "no amount"
				if paid != nil {
					got = paid.String()
				}
				to, failure, topic = StatusHeldForReview, "amount_mismatch", "payment.held"
				reason += fmt.Sprintf(" (paid %s, expected %s)", got, want)
			}
		}

		if _, err := tx.Exec(ctx, `
			UPDATE payments.intents
			SET status = $2::payments.intent_status, failure_code = NULLIF($3, ''), partner_ref = NULLIF($4, ''),
			    completed_at = CASE WHEN $2::payments.intent_status IN ('succeeded', 'failed') THEN now() END, updated_at = now()
			WHERE id = $1`, id, string(to), failure, partnerRef); err != nil {
			return err
		}
		if to == StatusSucceeded {
			clearing, err := s.Ledger.EnsureAccount(ctx, tx, ledger.PartnerClearing, ledger.OwnerPartner, partnerID(rail), want.Currency)
			if err != nil {
				return err
			}
			wallet, err := s.Ledger.EnsureAccount(ctx, tx, ledger.UserWallet, ledger.OwnerUser, payer, want.Currency)
			if err != nil {
				return err
			}
			if _, err := s.Ledger.Post(ctx, tx, ledger.Entry{Kind: "topup", SourceType: "payments.intent", SourceID: id, Postings: []ledger.Posting{
				{AccountID: clearing, Side: ledger.Debit, AmountMinor: want.AmountMinor, Currency: want.Currency},
				{AccountID: wallet, Side: ledger.Credit, AmountMinor: want.AmountMinor, Currency: want.Currency},
			}}); err != nil {
				return err
			}
		}
		if to == StatusHeldForReview {
			if err := openCaseTx(ctx, tx, payer, id, want, []string{"TOPUP_" + strings.ToUpper(failure)}, 0.5); err != nil {
				return err
			}
		}
		if err := addEvent(ctx, tx, id, StatusProcessing, to, reason, actor); err != nil {
			return err
		}
		return platform.WriteOutbox(ctx, tx, topic, id, map[string]any{
			"intent_id": id, "kind": KindTopUp, "payer_user_id": payer, "rail": rail,
			"amount": want, "status": to, "failure_code": failure, "partner_ref": partnerRef,
		})
	})
}

// SyncPendingTopUps settles top-ups that have been processing for longer than olderThan, oldest
// first. It returns how many reached a final state. Errors on single top-ups don't stop the sweep.
func (s *Service) SyncPendingTopUps(ctx context.Context, olderThan time.Duration, limit int) (int, error) {
	rows, err := s.Pool.Query(ctx, `
		SELECT id FROM payments.intents
		WHERE kind = 'topup' AND status = 'processing' AND created_at < now() - make_interval(secs => $1)
		ORDER BY created_at LIMIT $2`, olderThan.Seconds(), limit)
	if err != nil {
		return 0, err
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
	if err != nil {
		return 0, err
	}
	done := 0
	var firstErr error
	for _, id := range ids {
		in, err := s.SettleTopUp(ctx, id)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		if in.Status != StatusProcessing {
			done++
		}
	}
	return done, firstErr
}
