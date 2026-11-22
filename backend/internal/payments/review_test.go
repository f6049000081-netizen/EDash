package payments_test

import (
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"superapp/backend/internal/payments"
)

func TestHeldPaymentsOpenOneCaseAndStaffDecideThem(t *testing.T) {
	e := setup(t)
	a, b := e.user("A"), e.user("B")
	e.fund(a, 12_000_000)
	staff := uuid.New()

	hold := func(minor int64) *payments.Intent {
		in := e.p2p(a, b, minor)
		got, err := e.svc.Confirm(e.ctx, a, in.ID, pin)
		if err != nil || got.Status != payments.StatusHeldForReview {
			t.Fatalf("confirm: %v %+v", err, got)
		}
		return got
	}
	first, second, third := hold(6_000_000), hold(7_000_000), hold(5_500_000)

	var cases, intents int
	var number, summary string
	var amount int64
	_ = e.pool.QueryRow(e.ctx, `SELECT count(DISTINCT c.id), count(*), max(c.number), max(c.summary), max(c.amount_minor)
		FROM risk.cases c JOIN risk.case_intents ci ON ci.case_id = c.id WHERE c.subject_id = $1`, a).
		Scan(&cases, &intents, &number, &summary, &amount)
	if cases != 1 || intents != 3 || amount != 18_500_000 || summary != "Payment above the review threshold" || number[:3] != "FR-" {
		t.Fatalf("case: %d cases, %d payments, %s %q %d", cases, intents, number, summary, amount)
	}

	decide := func(f func(tx pgx.Tx) error) error {
		return pgx.BeginFunc(e.ctx, e.pool, f)
	}
	// Released: the money moves now.
	if err := decide(func(tx pgx.Tx) error {
		st, err := e.svc.ReleaseHeldTx(e.ctx, tx, first.ID, staff)
		if err == nil && st != payments.StatusSucceeded {
			t.Errorf("release: %s", st)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if e.wallet(a) != 6_000_000 || e.wallet(b) != 6_000_000 {
		t.Fatalf("after release: %d %d", e.wallet(a), e.wallet(b))
	}
	// Not enough left for the second: it fails and nothing moves.
	if err := decide(func(tx pgx.Tx) error {
		st, err := e.svc.ReleaseHeldTx(e.ctx, tx, second.ID, staff)
		if err == nil && st != payments.StatusFailed {
			t.Errorf("release without the money: %s", st)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if got, _ := e.svc.Get(e.ctx, a, second.ID); got.FailureCode != "insufficient_funds" || e.wallet(a) != 6_000_000 {
		t.Fatalf("second: %+v %d", got, e.wallet(a))
	}
	// Declined: it ends and the payer keeps the money.
	if err := decide(func(tx pgx.Tx) error { return e.svc.DeclineHeldTx(e.ctx, tx, third.ID, staff) }); err != nil {
		t.Fatal(err)
	}
	if got, _ := e.svc.Get(e.ctx, a, third.ID); got.Status != payments.StatusFailed || got.FailureCode != "declined_after_review" {
		t.Fatalf("third: %+v", got)
	}
	var r *payments.ReviewError
	if err := decide(func(tx pgx.Tx) error { return e.svc.DeclineHeldTx(e.ctx, tx, third.ID, staff) }); !errors.As(err, &r) {
		t.Errorf("deciding twice: %v", err)
	}
	var actor string
	_ = e.pool.QueryRow(e.ctx, `SELECT actor FROM payments.intent_events WHERE intent_id = $1 ORDER BY id DESC LIMIT 1`, third.ID).Scan(&actor)
	if actor != "staff:"+staff.String() {
		t.Errorf("event actor %q", actor)
	}
}

func TestAFrozenAccountCannotPay(t *testing.T) {
	e := setup(t)
	a, b := e.user("A"), e.user("B")
	e.fund(a, 10_000_000)
	held := e.p2p(a, b, 6_000_000)
	if got, err := e.svc.Confirm(e.ctx, a, held.ID, pin); err != nil || got.Status != payments.StatusHeldForReview {
		t.Fatalf("hold: %v", err)
	}
	small := e.p2p(a, b, 1_000)
	if _, err := e.pool.Exec(e.ctx, `UPDATE identity.users SET status = 'frozen' WHERE id = $1`, a); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.Confirm(e.ctx, a, small.ID, pin); !errors.Is(err, payments.ErrPayerInactive) {
		t.Errorf("frozen payer confirmed: %v", err)
	}
	var r *payments.ReviewError
	err := pgx.BeginFunc(e.ctx, e.pool, func(tx pgx.Tx) error {
		_, err := e.svc.ReleaseHeldTx(e.ctx, tx, held.ID, uuid.New())
		return err
	})
	if !errors.As(err, &r) || e.wallet(a) != 10_000_000 {
		t.Errorf("released a frozen payer's payment: %v, wallet %d", err, e.wallet(a))
	}
}
