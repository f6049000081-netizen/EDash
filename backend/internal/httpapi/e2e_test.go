package httpapi_test

// End to end over HTTP with real tokens: sign up two users, set PINs, fund one, pay the other,
// then reset a PIN by SMS code and check that the next payment is held. Needs TEST_DATABASE_URL.

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"superapp/backend/internal/httpapi"
	"superapp/backend/internal/identity"
	"superapp/backend/internal/ledger"
	"superapp/backend/internal/notify"
	"superapp/backend/internal/outbox"
	"superapp/backend/internal/payments"
	"superapp/backend/internal/payouts"
	"superapp/backend/internal/platform"
)

type inbox struct {
	mu    sync.Mutex
	codes map[string]string
}

func (b *inbox) Send(_ context.Context, phone, text string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.codes[phone] = regexp.MustCompile(`[0-9]{6}`).FindString(text)
	return nil
}

type client struct {
	t     *testing.T
	base  string
	token string
}

func (c *client) do(method, path string, body any, idem bool) (int, map[string]any) {
	c.t.Helper()
	var r io.Reader
	if body != nil {
		buf, _ := json.Marshal(body)
		r = bytes.NewReader(buf)
	}
	req, _ := http.NewRequest(method, c.base+path, r)
	req.Header.Set("Content-Type", "application/json")
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	if idem {
		req.Header.Set("Idempotency-Key", uuid.NewString())
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		c.t.Fatal(err)
	}
	defer resp.Body.Close()
	out := map[string]any{}
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

func TestEndToEndSignInAndPay(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()

	seed, key := make([]byte, 32), make([]byte, 32)
	_, _ = rand.Read(seed)
	_, _ = rand.Read(key)
	tokens, _ := identity.NewTokenSigner(seed)
	box := &inbox{codes: map[string]string{}}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	cfg := identity.DefaultConfig()
	cfg.OTPResendAfter = 0 // this test asks for a second code to the same number straight away
	ids, err := identity.NewService(pool, tokens, key, box, identity.DevAttestation{}, cfg, log)
	if err != nil {
		t.Fatal(err)
	}
	l := ledger.New()
	rules := payments.DefaultRules()
	rules.VelocityMax = 0
	pay := payments.NewService(pool, l, rules, &platform.PINVerifier{Pool: pool})
	notifier := &notify.Service{Pool: pool, Log: log}
	srv := httptest.NewServer((&httpapi.Server{
		Pool: pool, Ledger: l, Payments: pay, Identity: ids, Auth: ids, Log: log, Sandbox: true, Notify: notifier,
		Payouts: payouts.NewService(pool, l, &platform.PINVerifier{Pool: pool}),
	}).Routes())
	defer srv.Close()

	signIn := func(phone, purpose, dev string) *client {
		c := &client{t: t, base: srv.URL}
		st, ch := c.do("POST", "/v1/auth/otp", map[string]any{"phone": phone, "purpose": purpose, "locale": "en"}, false)
		if st != http.StatusAccepted {
			t.Fatalf("otp %s: %d %v", purpose, st, ch)
		}
		box.mu.Lock()
		code := box.codes[phone]
		box.mu.Unlock()
		st, s := c.do("POST", "/v1/auth/otp/verify", map[string]any{
			"challenge_id": ch["challenge_id"], "code": code,
			"device": map[string]any{"platform": "android", "attestation": dev},
		}, false)
		if st != http.StatusOK {
			t.Fatalf("verify %s: %d %v", purpose, st, s)
		}
		c.token = s["access_token"].(string)
		return c
	}
	phone := func() string {
		n, _ := rand.Int(rand.Reader, big.NewInt(100_000_000))
		return fmt.Sprintf("+2519%08d", n.Int64())
	}

	// No token, no access.
	anon := &client{t: t, base: srv.URL}
	if st, p := anon.do("GET", "/v1/wallet/balances", nil, false); st != http.StatusUnauthorized || p["code"] != "unauthenticated" {
		t.Fatalf("anonymous: %d %v", st, p)
	}
	anon.token = "eyJhbGciOiJub25lIn0.e30."
	if st, _ := anon.do("GET", "/v1/me", nil, false); st != http.StatusUnauthorized {
		t.Fatalf("alg none token: %d", st)
	}

	alicePhone := phone()
	alice := signIn(alicePhone, "signup", "alice-phone")
	bob := signIn(phone(), "signup", "bob-phone")

	// A pending user (no PIN yet) can't pay.
	if st, _ := alice.do("PUT", "/v1/auth/pin", map[string]any{"new_pin": "111111"}, true); st != http.StatusUnprocessableEntity {
		t.Errorf("weak PIN: %d", st)
	}
	for _, c := range []*client{alice, bob} {
		if st, p := c.do("PUT", "/v1/auth/pin", map[string]any{"new_pin": "582047"}, true); st != http.StatusNoContent {
			t.Fatalf("set PIN: %d %v", st, p)
		}
	}
	_, me := alice.do("GET", "/v1/me", nil, false)
	_, bobMe := bob.do("GET", "/v1/me", nil, false)
	if me["status"] != "active" || me["has_pin"] != true || me["phone"] != alicePhone {
		t.Fatalf("me: %v", me)
	}

	if st, p := alice.do("POST", "/v1/sandbox/topups", map[string]any{"amount": map[string]any{"amount_minor": 100_000, "currency": "ETB"}}, true); st != http.StatusCreated {
		t.Fatalf("top-up: %d %v", st, p)
	}
	payBob := func(minor int, pin string) map[string]any {
		st, in := alice.do("POST", "/v1/payment-intents", map[string]any{
			"kind": "p2p", "payee_user_id": bobMe["id"], "amount": map[string]any{"amount_minor": minor, "currency": "ETB"},
		}, true)
		if st != http.StatusCreated {
			t.Fatalf("create intent: %d %v", st, in)
		}
		_, out := alice.do("POST", "/v1/payment-intents/"+in["id"].(string)+"/confirm", map[string]any{"pin": pin}, true)
		return out
	}
	if out := payBob(25_000, "582047"); out["status"] != "succeeded" {
		t.Fatalf("payment: %v", out)
	}

	// Alice "forgets" her PIN (or a SIM swapper has her number) and resets it by SMS code.
	reset := signIn(alicePhone, "pin_reset", "new-phone")
	if st, p := reset.do("PUT", "/v1/auth/pin", map[string]any{"new_pin": "730918"}, true); st != http.StatusNoContent {
		t.Fatalf("reset PIN: %d %v", st, p)
	}
	if st, _ := alice.do("GET", "/v1/me", nil, false); st != http.StatusUnauthorized {
		t.Errorf("old session survived the reset: %d", st)
	}
	alice = reset
	if out := payBob(10_000, "730918"); out["status"] != "held_for_review" {
		t.Errorf("payment right after a PIN reset: %v", out)
	}

	// Activity: newest first, the held payment pending, the sent transfer, the top-up.
	st, act := alice.do("GET", "/v1/activity?limit=10", nil, false)
	items, _ := act["items"].([]any)
	if st != http.StatusOK || len(items) != 3 {
		t.Fatalf("activity: %d %v", st, act)
	}
	first := items[0].(map[string]any)
	if first["status"] != "pending" || first["type"] != "transfer_out" || items[2].(map[string]any)["type"] != "topup" {
		t.Errorf("activity items: %v", items)
	}
	if st, p := alice.do("GET", "/v1/activity?cursor=garbage", nil, false); st != http.StatusUnprocessableEntity {
		t.Errorf("bad cursor: %d %v", st, p)
	}

	// Profile edits; limits are null while compliance hasn't configured L0.
	st, prof := alice.do("PATCH", "/v1/me", map[string]any{"display_name": "Alice Tadesse", "locale": "en"}, false)
	if st != http.StatusOK || prof["display_name"] != "Alice Tadesse" || prof["locale"] != "en" {
		t.Errorf("patch me: %d %v", st, prof)
	}
	if v, present := prof["limits"]; !present || v != nil {
		t.Errorf("limits: %v", prof["limits"])
	}
	_, bobFeed := bob.do("GET", "/v1/activity", nil, false)
	if bi, _ := bobFeed["items"].([]any); len(bi) != 1 || bi[0].(map[string]any)["title"] != "Alice Tadesse" {
		t.Errorf("bob's feed should name the sender: %v", bobFeed)
	}

	// Notifications: the outbox relay hands events to the notifications service (in-process here, as on
	// the dev server). Other packages' tests add outbox rows meanwhile, so run it until Bob's shows up.
	relay := &outbox.Relay{Pool: pool, Publisher: outbox.Direct{Handlers: []outbox.Handler{notifier}}}
	var bobNotes map[string]any
	for i := 0; i < 200; i++ {
		if _, err := relay.RunOnce(context.Background()); err != nil {
			t.Fatal(err)
		}
		_, bobNotes = bob.do("GET", "/v1/notifications", nil, false)
		if n, _ := bobNotes["items"].([]any); len(n) > 0 {
			break
		}
	}
	bn, _ := bobNotes["items"].([]any)
	if len(bn) != 1 || bn[0].(map[string]any)["kind"] != "money_received" || bobNotes["unread"] != float64(1) {
		t.Fatalf("bob's notifications: %v", bobNotes)
	}
	for i := 0; i < 50; i++ { // drain the rest, so Alice's alerts are in
		if n, err := relay.RunOnce(context.Background()); err != nil || n == 0 {
			break
		}
	}
	_, aliceNotes := alice.do("GET", "/v1/notifications", nil, false)
	var kinds []string
	for _, n := range aliceNotes["items"].([]any) {
		kinds = append(kinds, n.(map[string]any)["kind"].(string))
	}
	if strings.Join(kinds, ",") != "pin_reset,new_device" {
		t.Errorf("alice's alerts: %v", kinds)
	}
	if st, p := bob.do("POST", "/v1/notifications/read", map[string]any{"all": true}, false); st != http.StatusOK || p["updated"] != float64(1) {
		t.Errorf("mark read: %d %v", st, p)
	}
	if st, p := bob.do("POST", "/v1/notifications/read", map[string]any{}, false); st != http.StatusUnprocessableEntity {
		t.Errorf("mark read with nothing: %d %v", st, p)
	}
	if st, p := alice.do("PUT", "/v1/devices/current/push-token", map[string]any{"token": "fcm-" + phone()}, false); st != http.StatusNoContent {
		t.Errorf("push token: %d %v", st, p)
	}

	// Merchant payout account: owner only, with the PIN; other roles and strangers can't.
	shop := uuid.New()
	if _, err := pool.Exec(ctx, `INSERT INTO merchant.merchants (id, legal_name, trade_name, category_code, status) VALUES ($1, 'Alem PLC', 'Alem Shop', '5812', 'active')`, shop); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO merchant.members (merchant_id, user_id, role) VALUES ($1, $2, 'owner'), ($1, $3, 'cashier')`,
		shop, me["id"], bobMe["id"]); err != nil {
		t.Fatal(err)
	}
	dest := map[string]any{"rail": "telebirr", "account": "+251911223344", "pin": "730918"}
	if st, p := bob.do("PUT", "/v1/merchants/"+shop.String()+"/payout-destination", dest, false); st != http.StatusForbidden {
		t.Errorf("cashier sets the payout account: %d %v", st, p)
	}
	if st, p := alice.do("GET", "/v1/merchants/"+uuid.NewString()+"/payouts", nil, false); st != http.StatusNotFound {
		t.Errorf("someone else's shop: %d %v", st, p)
	}
	if st, p := alice.do("GET", "/v1/merchants/"+shop.String()+"/payout-destination", nil, false); st != http.StatusNotFound {
		t.Errorf("no account yet: %d %v", st, p)
	}
	st, d := alice.do("PUT", "/v1/merchants/"+shop.String()+"/payout-destination", dest, false)
	if st != http.StatusOK || d["label"] != "telebirr ••3344" || d["payouts_from"] == nil {
		t.Fatalf("set payout account: %d %v", st, d)
	}
	if st, p := alice.do("GET", "/v1/merchants/"+shop.String()+"/payouts", nil, false); st != http.StatusOK || len(p["items"].([]any)) != 0 {
		t.Errorf("payouts: %d %v", st, p)
	}

	// Refresh and logout over HTTP.
	_, bal := alice.do("GET", "/v1/wallet/balances", nil, false)
	if bal["balances"] == nil {
		t.Errorf("balances: %v", bal)
	}
	if st, _ := alice.do("POST", "/v1/auth/logout", nil, false); st != http.StatusNoContent {
		t.Errorf("logout: %d", st)
	}
	if st, _ := alice.do("GET", "/v1/me", nil, false); st != http.StatusUnauthorized {
		t.Errorf("after logout: %d", st)
	}
}
