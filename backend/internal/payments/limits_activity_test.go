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

// ---------------------------------------------------------------------------
// activity
// ---------------------------------------------------------------------------

func (e *env) pay(from, to uuid.UUID, minor int64) (*payments.Intent, error) {
	e.t.Helper()
	amt := money.New(minor, money.ETB)
	in, err := e.svc.CreateIntent(e.ctx, from, payments.CreateRequest{Kind: payments.KindP2P, PayeeUserID: &to, Amount: &amt})
	if err != nil {
		return nil, err
	}
	return e.svc.Confirm(e.ctx, from, in.ID, pin)
}

func TestActivityFeed(t *testing.T) {
	e := setup(t)
	a, b := e.user("Almaz"), e.user("Bekele")
	_, qr := e.merchant("Buna House", 50)
	e.fund(a, 100_000)

	if _, err := e.pay(a, b, 25_000); err != nil {
		t.Fatal(err)
	}
	amt := money.New(10_000, money.ETB)
	in, err := e.svc.CreateIntent(e.ctx, a, payments.CreateRequest{Kind: payments.KindQRMerchant, QRPayload: qr, Amount: &amt})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.Confirm(e.ctx, a, in.ID, pin); err != nil {
		t.Fatal(err)
	}
	// Created but never confirmed: not in the feed.
	if _, err := e.svc.CreateIntent(e.ctx, a, payments.CreateRequest{Kind: payments.KindP2P, PayeeUserID: &b, Amount: &amt}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.pay(a, b, 1_000_000); !errors.Is(err, payments.ErrInsufficientFunds) {
		t.Fatalf("overdraft: %v", err)
	}

	page, err := e.svc.Activity(e.ctx, a, "all", "", 25)
	if err != nil {
		t.Fatal(err)
	}
	want := []struct {
		typ, title, status, dir string
		amount                  int64
		charged                 *bool
	}{
		{"transfer_out", "Bekele", "failed", "out", 1_000_000, ptr(false)},
		{"payment", "Buna House", "completed", "out", 10_000, ptr(true)},
		{"transfer_out", "Bekele", "completed", "out", 25_000, ptr(true)},
		{"topup", "telebirr", "completed", "in", 100_000, nil},
	}
	if len(page.Items) != len(want) || page.NextCursor != "" {
		t.Fatalf("%d items (cursor %q): %+v", len(page.Items), page.NextCursor, page.Items)
	}
	for i, w := range want {
		g := page.Items[i]
		if g.Type != w.typ || g.Title != w.title || g.Status != w.status || g.Direction != w.dir || g.Amount.AmountMinor != w.amount ||
			(w.charged == nil) != (g.Charged == nil) || (w.charged != nil && *w.charged != *g.Charged) {
			t.Errorf("item %d: %+v, want %+v", i, g, w)
		}
	}

	bPage, _ := e.svc.Activity(e.ctx, b, "", "", 25)
	if len(bPage.Items) != 1 || bPage.Items[0].Type != "transfer_in" || bPage.Items[0].Title != "Almaz" || bPage.Items[0].Amount.AmountMinor != 25_000 {
		t.Errorf("payee feed: %+v", bPage.Items)
	}

	// Paging one at a time visits the same items in the same order.
	var cursor string
	for i := 0; ; i++ {
		p, err := e.svc.Activity(e.ctx, a, "money", cursor, 1)
		if err != nil {
			t.Fatal(err)
		}
		if len(p.Items) != 1 || p.Items[0].ID != page.Items[i].ID {
			t.Fatalf("page %d: %+v", i, p.Items)
		}
		if p.NextCursor == "" {
			if i != len(want)-1 {
				t.Fatalf("paging stopped after %d items", i+1)
			}
			break
		}
		cursor = p.NextCursor
	}

	var v *payments.ValidationError
	if _, err := e.svc.Activity(e.ctx, a, "", "not-a-cursor", 25); !errors.As(err, &v) {
		t.Errorf("bad cursor: %v", err)
	}
	if _, err := e.svc.Activity(e.ctx, a, "everything", "", 25); !errors.As(err, &v) {
		t.Errorf("bad filter: %v", err)
	}
	if p, _ := e.svc.Activity(e.ctx, a, "orders", "", 25); len(p.Items) != 0 {
		t.Errorf("orders: %+v", p.Items)
	}
}

func ptr[T any](v T) *T { return &v }

type instantPIN struct{}

func (instantPIN) Verify(context.Context, uuid.UUID, string) error { return nil }

// ---------------------------------------------------------------------------
// KYC limits. Level L1 is reserved for these tests: no other test creates L1 users.
// ---------------------------------------------------------------------------

const (
	l1MaxBalance = 200_000 // ETB 2,000
	l1Daily      = 100_000 // ETB 1,000
	l1Monthly    = 300_000 // ETB 3,000
)

func (e *env) limitedUser(name string) uuid.UUID {
	e.t.Helper()
	if _, err := e.pool.Exec(e.ctx, `
		INSERT INTO identity.kyc_limits (level, currency, max_balance_minor, daily_out_minor, monthly_out_minor)
		VALUES ('L1', 'ETB', $1, $2, $3)
		ON CONFLICT (level, currency) DO UPDATE SET max_balance_minor = $1, daily_out_minor = $2, monthly_out_minor = $3`,
		l1MaxBalance, l1Daily, l1Monthly); err != nil {
		e.t.Fatal(err)
	}
	id := e.user(name)
	if _, err := e.pool.Exec(e.ctx, `UPDATE identity.users SET kyc_level = 'L1' WHERE id = $1`, id); err != nil {
		e.t.Fatal(err)
	}
	return id
}

func limitScope(err error) string {
	var le *payments.LimitError
	if errors.As(err, &le) {
		return le.Scope
	}
	return ""
}

func TestDailyLimit(t *testing.T) {
	e := setup(t)
	a, b := e.limitedUser("Abebe"), e.user("Tsion")
	e.fund(a, 150_000)

	amt := money.New(120_000, money.ETB)
	_, err := e.svc.CreateIntent(e.ctx, a, payments.CreateRequest{Kind: payments.KindP2P, PayeeUserID: &b, Amount: &amt})
	var le *payments.LimitError
	if !errors.As(err, &le) || le.Scope != "daily" || le.Remaining.AmountMinor != l1Daily {
		t.Fatalf("over the daily limit at create: %v", err)
	}
	if _, err := e.pay(a, b, 60_000); err != nil {
		t.Fatal(err)
	}

	// Eight payments of ETB 100 are created while ETB 400 is left, then confirmed together.
	// Exactly four fit. The PIN check is made instant here: the real argon2 check is slow enough to
	// stagger the confirms and hide a missing lock.
	e.svc.Presence = instantPIN{}
	var ids []uuid.UUID
	for i := 0; i < 8; i++ {
		amt := money.New(10_000, money.ETB)
		in, err := e.svc.CreateIntent(e.ctx, a, payments.CreateRequest{Kind: payments.KindP2P, PayeeUserID: &b, Amount: &amt})
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, in.ID)
	}
	var wg sync.WaitGroup
	var mu sync.Mutex
	ok, limited := 0, 0
	for _, id := range ids {
		wg.Add(1)
		go func(id uuid.UUID) {
			defer wg.Done()
			_, err := e.svc.Confirm(e.ctx, a, id, pin)
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				ok++
			case limitScope(err) == "daily":
				limited++
			default:
				t.Error(err)
			}
		}(id)
	}
	wg.Wait()
	if ok != 4 || limited != 4 {
		t.Errorf("%d succeeded, %d refused; want 4 and 4", ok, limited)
	}
	if w := e.wallet(a); w != 150_000-60_000-40_000 {
		t.Errorf("wallet %d", w)
	}
	var failedCount int
	_ = e.pool.QueryRow(e.ctx, `SELECT count(*) FROM payments.intents WHERE payer_user_id = $1 AND failure_code = 'limit_exceeded'`, a).Scan(&failedCount)
	if failedCount != 4 {
		t.Errorf("%d intents recorded as limit_exceeded, want 4", failedCount)
	}

	st, err := e.svc.LimitStatus(e.ctx, a)
	if err != nil || st == nil {
		t.Fatal(err)
	}
	if st.DailyRemaining.AmountMinor != 0 || st.MonthlyRemaining.AmountMinor != l1Monthly-100_000 || st.MaxBalance.AmountMinor != l1MaxBalance {
		t.Errorf("limit status: %+v", st)
	}
	if st, _ := e.svc.LimitStatus(e.ctx, b); st != nil {
		t.Errorf("L2 has no configured limits, got %+v", st)
	}
}

func TestMonthlyLimit(t *testing.T) {
	if time.Now().In(payments.EAT).Day() == 1 {
		t.Skip("no earlier day this month to backdate payments to")
	}
	e := setup(t)
	a, b := e.limitedUser("Kidist"), e.user("Haile")
	e.fund(a, 400_000)
	earlier := time.Date(time.Now().In(payments.EAT).Year(), time.Now().In(payments.EAT).Month(), 1, 10, 0, 0, 0, payments.EAT)
	for i := 0; i < 3; i++ {
		in, err := e.pay(a, b, 90_000)
		if err != nil {
			t.Fatal(err)
		}
		// Move it to the 1st of the month so it counts for the month but not for today.
		if _, err := e.pool.Exec(e.ctx, `UPDATE payments.intents SET confirmed_at = $2 WHERE id = $1`, in.ID, earlier); err != nil {
			t.Fatal(err)
		}
	}
	_, err := e.pay(a, b, 40_000)
	var le *payments.LimitError
	if !errors.As(err, &le) || le.Scope != "monthly" || le.Remaining.AmountMinor != l1Monthly-270_000 {
		t.Errorf("over the monthly limit: %v", err)
	}
}

func TestRecipientBalanceCap(t *testing.T) {
	e := setup(t)
	a, b := e.user("Yared"), e.limitedUser("Mahlet")
	e.fund(a, 100_000)
	e.fund(b, 190_000)
	in, err := e.pay(a, b, 20_000)
	if limitScope(err) != "recipient" {
		t.Fatalf("payee over cap: %v", err)
	}
	if in == nil || in.Status != payments.StatusFailed || in.FailureCode != "limit_exceeded" {
		t.Errorf("intent: %+v", in)
	}
	if e.wallet(a) != 100_000 || e.wallet(b) != 190_000 {
		t.Errorf("balances moved: %d / %d", e.wallet(a), e.wallet(b))
	}
	if _, err := e.pay(a, b, 10_000); err != nil {
		t.Errorf("payment that fits under the cap: %v", err)
	}
}

func TestTopUpBalanceCap(t *testing.T) {
	e := setup(t)
	rail := e.withRail()
	u := e.limitedUser("Feven")
	e.fund(u, 150_000)

	if _, _, err := e.svc.StartTopUp(e.ctx, u, money.New(60_000, money.ETB), "telebirr"); limitScope(err) != "balance" {
		t.Fatalf("top-up over cap: %v", err)
	}
	first := e.startTopUp(u, 30_000)
	// Unfinished top-ups count: 150 + 30 pending + 30 > 200.
	if _, _, err := e.svc.StartTopUp(e.ctx, u, money.New(30_000, money.ETB), "telebirr"); limitScope(err) != "balance" {
		t.Fatalf("second pending top-up over cap: %v", err)
	}

	// The balance grows by another route before the customer pays: the paid top-up no longer
	// fits, so it is held rather than credited.
	e.fund(u, 30_000)
	rail.pay(first.ID, "TB-"+uuid.NewString(), money.New(30_000, money.ETB))
	got := e.settle(first.ID)
	if got.Status != payments.StatusHeldForReview || got.FailureCode != "limit_exceeded" || e.wallet(u) != 180_000 {
		t.Errorf("settle over cap: %s/%s wallet %d", got.Status, got.FailureCode, e.wallet(u))
	}
}

func TestKYCRequiredWhenLimitsAreMandatory(t *testing.T) {
	e := setup(t)
	e.svc.LimitsRequired = true
	a, b := e.user("Lensa"), e.user("Natnael")
	if _, err := e.pool.Exec(e.ctx, `UPDATE identity.users SET kyc_level = 'L3' WHERE id = $1`, a); err != nil {
		t.Fatal(err)
	}
	e.fund(a, 10_000)
	amt := money.New(1_000, money.ETB)
	if _, err := e.svc.CreateIntent(e.ctx, a, payments.CreateRequest{Kind: payments.KindP2P, PayeeUserID: &b, Amount: &amt}); !errors.Is(err, payments.ErrKYCRequired) {
		t.Errorf("no limits configured for L3: %v", err)
	}
}

func TestPayByPhone(t *testing.T) {
	e := setup(t)
	a, b := e.user("Ruth"), e.user("Samuel")
	e.fund(a, 50_000)
	var phone string
	if err := e.pool.QueryRow(e.ctx, `SELECT phone_e164 FROM identity.users WHERE id = $1`, b).Scan(&phone); err != nil {
		t.Fatal(err)
	}
	amt := money.New(5_000, money.ETB)
	in, err := e.svc.CreateIntent(e.ctx, a, payments.CreateRequest{Kind: payments.KindP2P, PayeePhone: phone, Amount: &amt})
	if err != nil || in.PayeeID != b || in.PayeeName != "Samuel" {
		t.Fatalf("by phone: %+v %v", in, err)
	}
	if _, err := e.svc.Confirm(e.ctx, a, in.ID, pin); err != nil || e.wallet(b) != 5_000 {
		t.Fatalf("confirm: %v, payee wallet %d", err, e.wallet(b))
	}
	for _, p := range []string{"+251900000000", "not-a-phone"} {
		if _, err := e.svc.CreateIntent(e.ctx, a, payments.CreateRequest{Kind: payments.KindP2P, PayeePhone: p, Amount: &amt}); !errors.Is(err, payments.ErrPayeeNotFound) {
			t.Errorf("%s: %v", p, err)
		}
	}
	var own string
	_ = e.pool.QueryRow(e.ctx, `SELECT phone_e164 FROM identity.users WHERE id = $1`, a).Scan(&own)
	if _, err := e.svc.CreateIntent(e.ctx, a, payments.CreateRequest{Kind: payments.KindP2P, PayeePhone: own, Amount: &amt}); !errors.Is(err, payments.ErrSelfPayment) {
		t.Errorf("own number: %v", err)
	}
}
