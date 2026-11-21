package payments_test

// Integration tests. They need a PostgreSQL database with db/schema.sql loaded:
//
//	TEST_DATABASE_URL=postgres://... go test ./...
//
// Without TEST_DATABASE_URL they are skipped. Each test creates its own users and merchants,
// so tests don't interfere and the database doesn't need resetting between runs.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"superapp/backend/internal/httpapi"
	"superapp/backend/internal/ledger"
	"superapp/backend/internal/money"
	"superapp/backend/internal/payments"
	"superapp/backend/internal/platform"
)

const pin = "482913"

type env struct {
	t    *testing.T
	ctx  context.Context
	pool *pgxpool.Pool
	l    *ledger.Ledger
	svc  *payments.Service
}

var (
	poolOnce sync.Once
	pool     *pgxpool.Pool
	poolErr  error
)

func setup(t *testing.T) *env {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}
	poolOnce.Do(func() { pool, poolErr = pgxpool.New(context.Background(), dsn) })
	if poolErr != nil {
		t.Fatal(poolErr)
	}
	l := ledger.New()
	rules := payments.DefaultRules()
	rules.VelocityMax = 0 // off unless a test turns it on
	return &env{t: t, ctx: context.Background(), pool: pool, l: l,
		svc: payments.NewService(pool, l, rules, &platform.PINVerifier{Pool: pool})}
}

var pinHashOnce sync.Once
var pinHash string

func (e *env) user(name string) uuid.UUID {
	e.t.Helper()
	pinHashOnce.Do(func() {
		var err error
		if pinHash, err = platform.HashPIN(pin); err != nil {
			e.t.Fatal(err)
		}
	})
	id := uuid.New()
	phone := "+2519" + digits(id) // unique enough for tests, valid E.164
	_, err := e.pool.Exec(e.ctx, `INSERT INTO identity.users (id, phone_e164, display_name, status, kyc_level) VALUES ($1, $2, $3, 'active', 'L2')`, id, phone, name)
	if err != nil {
		e.t.Fatal(err)
	}
	if _, err := e.pool.Exec(e.ctx, `INSERT INTO identity.credentials (user_id, pin_hash) VALUES ($1, $2)`, id, pinHash); err != nil {
		e.t.Fatal(err)
	}
	return id
}

func digits(id uuid.UUID) string {
	b := make([]byte, 8)
	for i := range b {
		b[i] = '0' + id[i]%10
	}
	return string(b)
}

func (e *env) fund(user uuid.UUID, minor int64) {
	e.t.Helper()
	if _, err := e.svc.SandboxTopUp(e.ctx, user, money.New(minor, money.ETB), "telebirr"); err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) merchant(name string, bps int64) (uuid.UUID, string) {
	e.t.Helper()
	id := uuid.New()
	payload := "000201010211-" + id.String()
	if _, err := e.pool.Exec(e.ctx, `INSERT INTO merchant.merchants (id, legal_name, trade_name, category_code, status) VALUES ($1, $2, $2, '5812', 'active')`, id, name); err != nil {
		e.t.Fatal(err)
	}
	if _, err := e.pool.Exec(e.ctx, `INSERT INTO payments.qr_codes (owner_type, owner_id, mode, payload) VALUES ('merchant', $1, 'static', $2)`, id, payload); err != nil {
		e.t.Fatal(err)
	}
	if bps >= 0 {
		if _, err := e.pool.Exec(e.ctx, `INSERT INTO merchant.commission_plans (merchant_id, channel, rate_bps, valid_from, approved_by) VALUES ($1, 'qr', $2, now() - interval '1 day', $3)`, id, bps, uuid.New()); err != nil {
			e.t.Fatal(err)
		}
	}
	return id, payload
}

func (e *env) wallet(user uuid.UUID) int64 {
	e.t.Helper()
	acct, ok, err := e.l.FindAccount(e.ctx, e.pool, ledger.UserWallet, ledger.OwnerUser, user, money.ETB)
	if err != nil {
		e.t.Fatal(err)
	}
	if !ok {
		return 0
	}
	cached, err := e.l.Balance(e.ctx, e.pool, acct)
	if err != nil {
		e.t.Fatal(err)
	}
	summed, err := e.l.RecomputedBalance(e.ctx, e.pool, acct)
	if err != nil {
		e.t.Fatal(err)
	}
	if cached != summed {
		e.t.Fatalf("balance cache %d disagrees with postings %d", cached, summed)
	}
	return cached
}

func (e *env) p2p(from, to uuid.UUID, minor int64) *payments.Intent {
	e.t.Helper()
	amt := money.New(minor, money.ETB)
	in, err := e.svc.CreateIntent(e.ctx, from, payments.CreateRequest{Kind: payments.KindP2P, Amount: &amt, PayeeUserID: &to})
	if err != nil {
		e.t.Fatal(err)
	}
	return in
}

// ---------------------------------------------------------------------------

func TestP2PMovesMoneyAndBalances(t *testing.T) {
	e := setup(t)
	abebe, sara := e.user("Abebe"), e.user("Sara")
	e.fund(abebe, 100_000)

	in := e.p2p(abebe, sara, 50_000)
	if in.Status != payments.StatusRequiresConfirmation {
		t.Fatalf("status %s", in.Status)
	}
	if e.wallet(abebe) != 100_000 {
		t.Fatal("creating an intent must not move money")
	}

	done, err := e.svc.Confirm(e.ctx, abebe, in.ID, pin)
	if err != nil {
		t.Fatal(err)
	}
	if done.Status != payments.StatusSucceeded {
		t.Fatalf("status %s", done.Status)
	}
	if got := e.wallet(abebe); got != 50_000 {
		t.Errorf("payer %d", got)
	}
	if got := e.wallet(sara); got != 50_000 {
		t.Errorf("payee %d", got)
	}

	// Confirming again is a no-op and doesn't post a second entry.
	again, err := e.svc.Confirm(e.ctx, abebe, in.ID, pin)
	if err != nil || again.Status != payments.StatusSucceeded {
		t.Fatalf("re-confirm: %v %v", again, err)
	}
	if e.wallet(abebe) != 50_000 {
		t.Fatal("re-confirm moved money")
	}

	var outbox int
	_ = e.pool.QueryRow(e.ctx, `SELECT count(*) FROM platform.outbox WHERE aggregate_id = $1 AND topic = 'payment.succeeded'`, in.ID).Scan(&outbox)
	if outbox != 1 {
		t.Errorf("want 1 payment.succeeded event, got %d", outbox)
	}
}

func TestInsufficientFundsFailsCleanly(t *testing.T) {
	e := setup(t)
	a, b := e.user("A"), e.user("B")
	e.fund(a, 10_000)

	in := e.p2p(a, b, 10_001)
	failed, err := e.svc.Confirm(e.ctx, a, in.ID, pin)
	if !errors.Is(err, payments.ErrInsufficientFunds) {
		t.Fatalf("want ErrInsufficientFunds, got %v", err)
	}
	if failed == nil || failed.Status != payments.StatusFailed || failed.FailureCode != "insufficient_funds" {
		t.Fatalf("intent not marked failed: %+v", failed)
	}
	if e.wallet(a) != 10_000 || e.wallet(b) != 0 {
		t.Fatal("money moved on a failed payment")
	}
}

func TestQRPaymentSplitsCommission(t *testing.T) {
	e := setup(t)
	payer := e.user("Hanna")
	e.fund(payer, 100_000)
	merchant, payload := e.merchant("Buna House", 150) // 1.5%

	amt := money.New(18_500, money.ETB)
	in, err := e.svc.CreateIntent(e.ctx, payer, payments.CreateRequest{Kind: payments.KindQRMerchant, Amount: &amt, QRPayload: payload})
	if err != nil {
		t.Fatal(err)
	}
	if in.PayeeName != "Buna House" || in.PointsToEarn() != 18 {
		t.Fatalf("payee %q points %d", in.PayeeName, in.PointsToEarn())
	}
	if _, err := e.svc.Confirm(e.ctx, payer, in.ID, pin); err != nil {
		t.Fatal(err)
	}

	settle, _, _ := e.l.FindAccount(e.ctx, e.pool, ledger.MerchantSettlement, ledger.OwnerMerchant, merchant, money.ETB)
	got, _ := e.l.Balance(e.ctx, e.pool, settle)
	// 18,500 × 1.5% = 277.5 → 278 commission; merchant gets 18,222
	if got != 18_222 {
		t.Errorf("merchant settlement %d, want 18222", got)
	}
	if e.wallet(payer) != 81_500 {
		t.Errorf("payer %d", e.wallet(payer))
	}
}

func TestConcurrentPaymentsNeverOverdraw(t *testing.T) {
	e := setup(t)
	payer := e.user("Payer")
	e.fund(payer, 100_000) // enough for exactly 10 payments of 10,000

	const n = 25
	intents := make([]*payments.Intent, n)
	for i := range intents {
		intents[i] = e.p2p(payer, e.user("Payee"), 10_000)
	}

	var wg sync.WaitGroup
	var mu sync.Mutex
	succeeded, short := 0, 0
	for _, in := range intents {
		wg.Add(1)
		go func(id uuid.UUID) {
			defer wg.Done()
			_, err := e.svc.Confirm(e.ctx, payer, id, pin)
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				succeeded++
			case errors.Is(err, payments.ErrInsufficientFunds):
				short++
			default:
				t.Errorf("unexpected error: %v", err)
			}
		}(in.ID)
	}
	wg.Wait()

	if succeeded != 10 || short != n-10 {
		t.Errorf("succeeded %d, insufficient %d; want 10 and %d", succeeded, short, n-10)
	}
	if got := e.wallet(payer); got != 0 {
		t.Errorf("payer balance %d, want 0", got)
	}
}

func TestOpposingTransfersDontDeadlock(t *testing.T) {
	e := setup(t)
	a, b := e.user("A"), e.user("B")
	e.fund(a, 1_000_000)
	e.fund(b, 1_000_000)

	var ins []*payments.Intent
	var payers []uuid.UUID
	for i := 0; i < 20; i++ {
		ins = append(ins, e.p2p(a, b, 1_000), e.p2p(b, a, 1_000))
		payers = append(payers, a, b)
	}
	var wg sync.WaitGroup
	for i, in := range ins {
		wg.Add(1)
		go func(payer, id uuid.UUID) {
			defer wg.Done()
			if _, err := e.svc.Confirm(e.ctx, payer, id, pin); err != nil {
				t.Errorf("confirm: %v", err)
			}
		}(payers[i], in.ID)
	}
	wg.Wait()
	if e.wallet(a) != 1_000_000 || e.wallet(b) != 1_000_000 {
		t.Errorf("balances %d / %d, want both unchanged", e.wallet(a), e.wallet(b))
	}
}

func TestWrongPINLocksAfterFiveAttempts(t *testing.T) {
	e := setup(t)
	a, b := e.user("A"), e.user("B")
	e.fund(a, 50_000)
	in := e.p2p(a, b, 1_000)

	for i := 1; i <= 4; i++ {
		if _, err := e.svc.Confirm(e.ctx, a, in.ID, "000000"); !errors.Is(err, platform.ErrInvalidPIN) {
			t.Fatalf("attempt %d: want ErrInvalidPIN, got %v", i, err)
		}
	}
	if _, err := e.svc.Confirm(e.ctx, a, in.ID, "000000"); !errors.Is(err, platform.ErrPINLocked) {
		t.Fatalf("5th attempt: want ErrPINLocked, got %v", err)
	}
	// Now even the right PIN is refused.
	if _, err := e.svc.Confirm(e.ctx, a, in.ID, pin); !errors.Is(err, platform.ErrPINLocked) {
		t.Fatalf("correct PIN while locked: want ErrPINLocked, got %v", err)
	}
	if e.wallet(a) != 50_000 {
		t.Fatal("money moved")
	}
}

func TestLargePaymentHeldForReview(t *testing.T) {
	e := setup(t)
	a, b := e.user("A"), e.user("B")
	e.fund(a, 10_000_000)
	in := e.p2p(a, b, 6_000_000) // ETB 60,000 > 50,000 threshold

	held, err := e.svc.Confirm(e.ctx, a, in.ID, pin)
	if err != nil {
		t.Fatal(err)
	}
	if held.Status != payments.StatusHeldForReview {
		t.Fatalf("status %s", held.Status)
	}
	if e.wallet(a) != 10_000_000 {
		t.Fatal("held payment moved money")
	}
	var action string
	_ = e.pool.QueryRow(e.ctx, `SELECT action FROM risk.decisions WHERE intent_id = $1`, in.ID).Scan(&action)
	if action != "hold" {
		t.Errorf("risk decision %q", action)
	}
}

func TestCannotSeeOrConfirmSomeoneElsesIntent(t *testing.T) {
	e := setup(t)
	a, b, mallory := e.user("A"), e.user("B"), e.user("Mallory")
	e.fund(a, 50_000)
	in := e.p2p(a, b, 1_000)

	if _, err := e.svc.Confirm(e.ctx, mallory, in.ID, pin); !errors.Is(err, payments.ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
	if _, err := e.svc.Cancel(e.ctx, mallory, in.ID); !errors.Is(err, payments.ErrNotFound) {
		t.Fatalf("cancel: want ErrNotFound, got %v", err)
	}
}

func TestExternalRailsRefused(t *testing.T) {
	e := setup(t)
	a, b := e.user("A"), e.user("B")
	amt := money.New(1_000, money.ETB)
	src := uuid.New()
	_, err := e.svc.CreateIntent(e.ctx, a, payments.CreateRequest{Kind: payments.KindP2P, Amount: &amt, PayeeUserID: &b, FundingSourceID: &src})
	if !errors.Is(err, payments.ErrRailUnavailable) {
		t.Fatalf("want ErrRailUnavailable, got %v", err)
	}
}

// ---------------------------------------------------------------------------
// HTTP: idempotency end to end
// ---------------------------------------------------------------------------

// testAuth trusts a header. It lives only in this test file; the server authenticates with tokens.
type testAuth struct{}

func (testAuth) Authenticate(r *http.Request) (platform.Principal, error) {
	id, err := uuid.Parse(r.Header.Get("X-Test-User"))
	if err != nil {
		return platform.Principal{}, platform.ErrUnauthenticated
	}
	return platform.Principal{UserID: id}, nil
}

func TestHTTPIdempotency(t *testing.T) {
	e := setup(t)
	a, b := e.user("A"), e.user("B")
	e.fund(a, 100_000)

	srv := httptest.NewServer((&httpapi.Server{
		Pool: e.pool, Ledger: e.l, Payments: e.svc, Auth: testAuth{},
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}).Routes())
	defer srv.Close()

	call := func(path, key string, body any) (int, map[string]any, http.Header) {
		t.Helper()
		buf, _ := json.Marshal(body)
		req, _ := http.NewRequest(http.MethodPost, srv.URL+path, bytes.NewReader(buf))
		req.Header.Set("X-Test-User", a.String())
		req.Header.Set("Content-Type", "application/json")
		if key != "" {
			req.Header.Set("Idempotency-Key", key)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var out map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&out)
		return resp.StatusCode, out, resp.Header
	}

	createBody := map[string]any{"kind": "p2p", "payee_user_id": b, "amount": map[string]any{"amount_minor": 25_000, "currency": "ETB"}}

	if st, _, _ := call("/v1/payment-intents", "", createBody); st != http.StatusBadRequest {
		t.Errorf("missing key: status %d", st)
	}

	key := uuid.NewString()
	st, first, _ := call("/v1/payment-intents", key, createBody)
	if st != http.StatusCreated {
		t.Fatalf("create: %d %v", st, first)
	}
	st, second, hdr := call("/v1/payment-intents", key, createBody)
	if st != http.StatusCreated || second["id"] != first["id"] || hdr.Get("Idempotent-Replayed") != "true" {
		t.Fatalf("replay: %d %v %v", st, second["id"], hdr)
	}
	createBody["amount"] = map[string]any{"amount_minor": 99_999, "currency": "ETB"}
	if st, p, _ := call("/v1/payment-intents", key, createBody); st != http.StatusConflict || p["code"] != "idempotency_conflict" {
		t.Fatalf("reuse with different body: %d %v", st, p)
	}

	confirmKey := uuid.NewString()
	path := "/v1/payment-intents/" + first["id"].(string) + "/confirm"
	for i := 0; i < 3; i++ {
		st, out, _ := call(path, confirmKey, map[string]any{"pin": pin})
		if st != http.StatusOK || out["status"] != "succeeded" {
			t.Fatalf("confirm #%d: %d %v", i+1, st, out)
		}
	}
	if e.wallet(a) != 75_000 || e.wallet(b) != 25_000 {
		t.Errorf("balances %d / %d after 3 confirms", e.wallet(a), e.wallet(b))
	}

	// Insufficient funds comes back as a problem that says nothing was charged.
	_, big, _ := call("/v1/payment-intents", uuid.NewString(), map[string]any{"kind": "p2p", "payee_user_id": b, "amount": map[string]any{"amount_minor": 1_000_000, "currency": "ETB"}})
	st, p, _ := call("/v1/payment-intents/"+big["id"].(string)+"/confirm", uuid.NewString(), map[string]any{"pin": pin})
	if st != http.StatusPaymentRequired || p["code"] != "insufficient_funds" || p["charged"] != false {
		t.Errorf("insufficient: %d %v", st, p)
	}
}
