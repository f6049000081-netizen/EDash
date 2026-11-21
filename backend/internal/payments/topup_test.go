package payments_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"superapp/backend/internal/money"
	"superapp/backend/internal/payments"
)

// fakeRail answers like a partner whose customer pays (or doesn't) when the test says so.
type fakeRail struct {
	mu           sync.Mutex
	orders       map[string]payments.RailOrder
	failCheckout bool
	queries      int
}

func newFakeRail() *fakeRail { return &fakeRail{orders: map[string]payments.RailOrder{}} }

func (f *fakeRail) CreateCheckout(_ context.Context, orderID string, _ money.Money, _ string) (payments.RailCheckout, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failCheckout {
		return payments.RailCheckout{}, errors.New("connection reset")
	}
	f.orders[orderID] = payments.RailOrder{State: payments.RailPending, Status: "WAIT_PAY"}
	return payments.RailCheckout{OrderRef: "prepay-" + orderID, CheckoutURL: "https://pay.example/" + orderID}, nil
}

func (f *fakeRail) QueryOrder(_ context.Context, orderID string) (payments.RailOrder, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.queries++
	o, ok := f.orders[orderID]
	if !ok {
		return payments.RailOrder{}, errors.New("order not found")
	}
	return o, nil
}

func (f *fakeRail) set(id uuid.UUID, o payments.RailOrder) {
	f.mu.Lock()
	f.orders[payments.OrderID(id)] = o
	f.mu.Unlock()
}

func (f *fakeRail) pay(id uuid.UUID, ref string, paid money.Money) {
	f.set(id, payments.RailOrder{State: payments.RailSucceeded, Status: "PAY_SUCCESS", PartnerRef: ref, Amount: paid})
}

func (e *env) withRail() *fakeRail {
	r := newFakeRail()
	e.svc.Rails["telebirr"] = r
	return r
}

func (e *env) startTopUp(user uuid.UUID, minor int64) *payments.Intent {
	e.t.Helper()
	in, co, err := e.svc.StartTopUp(e.ctx, user, money.New(minor, money.ETB), "telebirr")
	if err != nil {
		e.t.Fatalf("start top-up: %v", err)
	}
	if in.Status != payments.StatusProcessing || co.CheckoutURL == "" {
		e.t.Fatalf("started: %s %q", in.Status, co.CheckoutURL)
	}
	return in
}

func (e *env) settle(id uuid.UUID) *payments.Intent {
	e.t.Helper()
	in, err := e.svc.SettleTopUp(e.ctx, id)
	if err != nil {
		e.t.Fatalf("settle: %v", err)
	}
	return in
}

func TestTopUpCreditsOnlyAfterPartnerConfirms(t *testing.T) {
	e := setup(t)
	rail := e.withRail()
	u := e.user("Hanna")

	in := e.startTopUp(u, 50_000)
	if e.wallet(u) != 0 {
		t.Fatal("credited before payment")
	}
	if got := e.settle(in.ID); got.Status != payments.StatusProcessing || e.wallet(u) != 0 {
		t.Fatalf("unpaid order settled to %s", got.Status)
	}

	rail.pay(in.ID, "TB-"+uuid.NewString(), money.New(50_000, money.ETB))
	// Notifications arrive several times and in parallel. Only one credit may happen.
	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := e.svc.SettleTopUp(e.ctx, in.ID); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if got := e.settle(in.ID); got.Status != payments.StatusSucceeded {
		t.Fatalf("status %s", got.Status)
	}
	if b := e.wallet(u); b != 50_000 {
		t.Errorf("wallet %d after 11 settles, want 50000", b)
	}
	var entries int
	_ = e.pool.QueryRow(e.ctx, `SELECT count(*) FROM ledger.journal_entries WHERE source_id = $1`, in.ID).Scan(&entries)
	if entries != 1 {
		t.Errorf("%d journal entries, want 1", entries)
	}
}

func TestTopUpAmountMismatchIsHeld(t *testing.T) {
	e := setup(t)
	rail := e.withRail()
	u := e.user("Dawit")
	in := e.startTopUp(u, 100_000)
	rail.pay(in.ID, "TB-"+uuid.NewString(), money.New(10_000, money.ETB))
	got := e.settle(in.ID)
	if got.Status != payments.StatusHeldForReview || got.FailureCode != "amount_mismatch" || e.wallet(u) != 0 {
		t.Errorf("mismatch: %s/%s wallet %d", got.Status, got.FailureCode, e.wallet(u))
	}
}

func TestTopUpDeclinedAndExpired(t *testing.T) {
	e := setup(t)
	rail := e.withRail()
	u := e.user("Selam")

	declined := e.startTopUp(u, 10_000)
	rail.set(declined.ID, payments.RailOrder{State: payments.RailFailed, Status: "PAY_CANCEL"})
	if got := e.settle(declined.ID); got.Status != payments.StatusFailed || got.FailureCode != "rail_declined" {
		t.Errorf("declined: %s/%s", got.Status, got.FailureCode)
	}

	stale := e.startTopUp(u, 10_000)
	if _, err := e.pool.Exec(e.ctx, `UPDATE payments.intents SET created_at = now() - interval '3 hours' WHERE id = $1`, stale.ID); err != nil {
		t.Fatal(err)
	}
	if got := e.settle(stale.ID); got.Status != payments.StatusFailed || got.FailureCode != "expired" {
		t.Errorf("expired: %s/%s", got.Status, got.FailureCode)
	}
	// A late success can't revive a closed top-up; reconciliation catches the money instead.
	rail.pay(stale.ID, "TB-"+uuid.NewString(), money.New(10_000, money.ETB))
	if got := e.settle(stale.ID); got.Status != payments.StatusFailed || e.wallet(u) != 0 {
		t.Errorf("late success: %s wallet %d", got.Status, e.wallet(u))
	}
}

func TestPartnerReferenceCannotFundTwice(t *testing.T) {
	e := setup(t)
	rail := e.withRail()
	u := e.user("Yonas")
	ref := "TB-" + uuid.NewString()
	first, second := e.startTopUp(u, 20_000), e.startTopUp(u, 20_000)
	rail.pay(first.ID, ref, money.New(20_000, money.ETB))
	rail.pay(second.ID, ref, money.New(20_000, money.ETB))
	e.settle(first.ID)
	got := e.settle(second.ID)
	if got.Status != payments.StatusHeldForReview || got.FailureCode != "duplicate_partner_ref" || e.wallet(u) != 20_000 {
		t.Errorf("second use of a reference: %s/%s wallet %d", got.Status, got.FailureCode, e.wallet(u))
	}
}

func TestTopUpGuards(t *testing.T) {
	e := setup(t)
	u := e.user("Meron")
	if _, _, err := e.svc.StartTopUp(e.ctx, u, money.New(10_000, money.ETB), "telebirr"); !errors.Is(err, payments.ErrRailUnavailable) {
		t.Errorf("rail not configured: %v", err)
	}
	rail := e.withRail()
	var v *payments.ValidationError
	for _, amt := range []money.Money{money.New(10_000, money.USD), money.New(50, money.ETB), money.New(20_000_000, money.ETB)} {
		if _, _, err := e.svc.StartTopUp(e.ctx, u, amt, "telebirr"); !errors.As(err, &v) {
			t.Errorf("%s: %v", amt, err)
		}
	}

	rail.failCheckout = true
	if _, _, err := e.svc.StartTopUp(e.ctx, u, money.New(10_000, money.ETB), "telebirr"); !errors.Is(err, payments.ErrRailError) {
		t.Errorf("checkout failure: %v", err)
	}
	var status, code string
	_ = e.pool.QueryRow(e.ctx, `SELECT status, failure_code FROM payments.intents WHERE payer_user_id = $1 ORDER BY created_at DESC LIMIT 1`, u).Scan(&status, &code)
	if status != "failed" || code != "rail_error" {
		t.Errorf("intent after checkout failure: %s/%s", status, code)
	}
	rail.failCheckout = false

	for i := 0; i < 5; i++ {
		e.startTopUp(u, 10_000)
	}
	if _, _, err := e.svc.StartTopUp(e.ctx, u, money.New(10_000, money.ETB), "telebirr"); !errors.Is(err, payments.ErrTooManyPending) {
		t.Errorf("6th pending top-up: %v", err)
	}
	if _, err := e.svc.SettleTopUp(e.ctx, uuid.New()); !errors.Is(err, payments.ErrNotFound) {
		t.Errorf("unknown intent: %v", err)
	}
}

func TestSweepSettlesLostNotifications(t *testing.T) {
	e := setup(t)
	rail := e.withRail()
	u := e.user("Abel")
	in := e.startTopUp(u, 30_000)
	rail.pay(in.ID, "TB-"+uuid.NewString(), money.New(30_000, money.ETB))
	if _, err := e.pool.Exec(e.ctx, `UPDATE payments.intents SET created_at = now() - interval '5 minutes' WHERE id = $1`, in.ID); err != nil {
		t.Fatal(err)
	}
	// Other tests may leave processing top-ups this rail doesn't know; the sweep carries on past them.
	_, _ = e.svc.SyncPendingTopUps(e.ctx, 2*time.Minute, 1000)
	if got, _ := e.svc.Get(e.ctx, u, in.ID); got.Status != payments.StatusSucceeded || e.wallet(u) != 30_000 {
		t.Errorf("after sweep: %s wallet %d", got.Status, e.wallet(u))
	}
}
