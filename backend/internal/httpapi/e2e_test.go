package httpapi_test

// End to end over HTTP with real tokens: sign up two users, set PINs, fund one, pay the other,
// then reset a PIN by SMS code and check that the next payment is held. Needs TEST_DATABASE_URL.

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
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
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"superapp/backend/internal/bizweb"
	"superapp/backend/internal/commerce"
	"superapp/backend/internal/httpapi"
	"superapp/backend/internal/identity"
	"superapp/backend/internal/kyb"
	"superapp/backend/internal/kyb/kybtest"
	"superapp/backend/internal/ledger"
	"superapp/backend/internal/merchants"
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
	sealer, _ := kyb.ParseKeys("k1:" + base64.StdEncoding.EncodeToString(key))
	docs := &kyb.Service{Pool: pool, Store: kybtest.NewMemStore(), Sealer: sealer}
	web := &bizweb.Store{Pool: pool, PIN: &platform.PINVerifier{Pool: pool}}
	srv := httptest.NewServer((&httpapi.Server{
		Pool: pool, Ledger: l, Payments: pay, Identity: ids, Auth: ids, Log: log, Sandbox: true, Notify: notifier,
		Payouts:   payouts.NewService(pool, l, &platform.PINVerifier{Pool: pool}),
		Merchants: &merchants.Service{Pool: pool}, KYB: docs, Commerce: commerce.New(pool, pay), Web: web,
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

	// Applying as a merchant: under review, no QR until operations approve it; then the QR pays the shop.
	taxID := fmt.Sprintf("%010d", time.Now().UnixNano()%10_000_000_000)
	application := map[string]any{"legal_name": "Bob Trading", "trade_name": "Bob's Kiosk", "tin": taxID,
		"category_code": "5411", "city": "Adama"}
	st, applied := bob.do("POST", "/v1/merchants", application, false)
	if st != http.StatusCreated || applied["status"] != "kyb_review" || applied["role"] != "owner" || applied["merchant_no"] != nil {
		t.Fatalf("apply: %d %v", st, applied)
	}
	kiosk := applied["id"].(string)
	if st, p := bob.do("GET", "/v1/merchants/"+kiosk+"/qr", nil, false); st != http.StatusConflict || p["code"] != "merchant_not_active" {
		t.Errorf("QR before approval: %d %v", st, p)
	}
	if st, p := alice.do("GET", "/v1/merchants/"+kiosk, nil, false); st != http.StatusNotFound {
		t.Errorf("someone else's application: %d %v", st, p)
	}

	// KYB documents: the owner sends a photo as the raw body; the server checks what the file really is.
	upload := func(c *client, kind string, body []byte) (int, map[string]any) {
		req, _ := http.NewRequest("POST", srv.URL+"/v1/merchants/"+kiosk+"/documents?kind="+kind, bytes.NewReader(body))
		req.Header.Set("Content-Type", "image/jpeg")
		req.Header.Set("Authorization", "Bearer "+c.token)
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		out := map[string]any{}
		_ = json.NewDecoder(res.Body).Decode(&out)
		return res.StatusCode, out
	}
	if st, p := bob.do("GET", "/v1/merchants/"+kiosk+"/documents", nil, false); st != http.StatusOK ||
		fmt.Sprint(p["required"]) != "[tin_certificate owner_id]" || len(p["items"].([]any)) != 0 {
		t.Errorf("documents before upload: %d %v", st, p)
	}
	if st, p := upload(bob, "owner_id", append([]byte("<svg onload=alert(1)>"), make([]byte, 2048)...)); st != http.StatusUnprocessableEntity {
		t.Errorf("not a photo: %d %v", st, p)
	}
	if st, p := upload(alice, "owner_id", kybtest.Photo("alice")); st != http.StatusNotFound {
		t.Errorf("someone else's shop: %d %v", st, p)
	}
	if st, p := upload(bob, "owner_id", append([]byte{0xFF, 0xD8, 0xFF}, make([]byte, kyb.MaxSize+1)...)); st != http.StatusRequestEntityTooLarge {
		t.Errorf("too big: %d %v", st, p)
	}
	photo := kybtest.Photo(uuid.NewString())
	st, doc := upload(bob, "owner_id", photo)
	if st != http.StatusCreated || doc["status"] != "submitted" || doc["content_type"] != "image/jpeg" || doc["size_bytes"] != float64(len(photo)) {
		t.Fatalf("upload: %d %v", st, doc)
	}
	if st, again := upload(bob, "owner_id", photo); st != http.StatusCreated || again["id"] != doc["id"] {
		t.Errorf("same photo again: %d %v", st, again)
	}
	if st, p := bob.do("GET", "/v1/merchants/"+kiosk+"/documents", nil, false); st != http.StatusOK || len(p["items"].([]any)) != 1 {
		t.Errorf("documents after upload: %d %v", st, p)
	}
	if _, err := (&merchants.Service{Pool: pool}).Approve(ctx, uuid.MustParse(kiosk), uuid.New()); err != nil {
		t.Fatal(err)
	}
	st, qr := bob.do("GET", "/v1/merchants/"+kiosk+"/qr", nil, false)
	if st != http.StatusOK || !strings.HasPrefix(qr["payload"].(string), "000201") || qr["merchant_no"] == "" {
		t.Fatalf("QR: %d %v", st, qr)
	}
	carol := signIn(phone(), "signup", "carol-phone")
	if st, p := carol.do("PUT", "/v1/auth/pin", map[string]any{"new_pin": "582047"}, true); st != http.StatusNoContent {
		t.Fatalf("carol's PIN: %d %v", st, p)
	}
	if st, p := carol.do("POST", "/v1/sandbox/topups", map[string]any{"amount": map[string]any{"amount_minor": 500_00, "currency": "ETB"}}, true); st != http.StatusCreated {
		t.Fatalf("carol's top-up: %d %v", st, p)
	}
	st, qrIntent := carol.do("POST", "/v1/payment-intents", map[string]any{
		"kind": "qr_merchant", "qr_payload": qr["payload"], "amount": map[string]any{"amount_minor": 120_00, "currency": "ETB"}}, true)
	if st != http.StatusCreated || qrIntent["payee"].(map[string]any)["display_name"] != "Bob's Kiosk" {
		t.Fatalf("paying the new QR: %d %v", st, qrIntent)
	}
	if st, p := carol.do("POST", "/v1/payment-intents/"+qrIntent["id"].(string)+"/confirm", map[string]any{"pin": "582047"}, true); st != http.StatusOK || p["status"] != "succeeded" {
		t.Fatalf("confirm: %d %v", st, p)
	}
	st, sum := bob.do("GET", "/v1/merchants/"+kiosk+"/summary", nil, false)
	if st != http.StatusOK || sum["payments_today"] != float64(1) || sum["sales_today"].(map[string]any)["amount_minor"] != float64(120_00) {
		t.Errorf("summary: %d %v", st, sum)
	}
	st, feed := bob.do("GET", "/v1/merchants/"+kiosk+"/payments", nil, false)
	if items, _ := feed["items"].([]any); st != http.StatusOK || len(items) != 1 {
		t.Errorf("payments: %d %v", st, feed)
	}
	if st, p := bob.do("GET", "/v1/me/merchants", nil, false); st != http.StatusOK || len(p["items"].([]any)) != 2 {
		t.Errorf("bob's businesses (cashier at Alem Shop, owner of the kiosk): %d %v", st, p)
	}

	// Commerce: Bob puts the kiosk's catalog up and opens for orders; Carol orders, pays with her PIN,
	// and collects with her handover code.
	base := "/v1/merchants/" + kiosk
	if st, p := bob.do("PUT", base+"/shop", map[string]any{"open": true, "pickup": true}, true); st != http.StatusConflict {
		t.Errorf("opening with no products: %d %v", st, p)
	}
	st, buna := bob.do("POST", base+"/products", map[string]any{"title": "Buna, 250 g", "category": "Coffee",
		"price": map[string]any{"amount_minor": 150_00, "currency": "ETB"}, "stock": 5}, true)
	if st != http.StatusCreated || buna["sold_out"] != false {
		t.Fatalf("product: %d %v", st, buna)
	}
	if st, p := bob.do("PUT", base+"/shop", map[string]any{"open": true, "pickup": true, "about": "Fresh coffee"}, true); st != http.StatusOK || p["open"] != true {
		t.Fatalf("open shop: %d %v", st, p)
	}
	if st, p := carol.do("GET", "/v1/shops/"+kiosk, nil, false); st != http.StatusOK || len(p["products"].([]any)) != 1 {
		t.Fatalf("shop page: %d %v", st, p)
	}
	st, ord := carol.do("POST", "/v1/orders", map[string]any{"merchant_id": kiosk, "channel": "pickup",
		"items": []any{map[string]any{"product_id": buna["id"], "quantity": 2}}}, true)
	if st != http.StatusCreated || ord["status"] != "pending_payment" || ord["total"].(map[string]any)["amount_minor"] != float64(300_00) {
		t.Fatalf("place order: %d %v", st, ord)
	}
	orderID := ord["id"].(string)
	if st, p := carol.do("POST", "/v1/payment-intents/"+ord["payment_intent_id"].(string)+"/confirm", map[string]any{"pin": "582047"}, true); st != http.StatusOK || p["status"] != "succeeded" {
		t.Fatalf("pay order: %d %v", st, p)
	}
	for _, step := range []string{"accept", "ready"} {
		if st, p := bob.do("POST", base+"/orders/"+orderID+"/"+step, map[string]any{}, true); st != http.StatusOK || p["handover_code"] != nil {
			t.Fatalf("%s: %d %v", step, st, p)
		}
	}
	_, mine := carol.do("GET", "/v1/orders/"+orderID, nil, false)
	code, _ := mine["handover_code"].(string)
	if mine["status"] != "ready" || len(code) != 4 {
		t.Fatalf("carol's order: %v", mine)
	}
	if st, p := alice.do("GET", "/v1/orders/"+orderID, nil, false); st != http.StatusNotFound {
		t.Errorf("someone else's order: %d %v", st, p)
	}
	if st, p := bob.do("POST", base+"/orders/"+orderID+"/hand_over", map[string]any{"code": code}, true); st != http.StatusOK || p["status"] != "delivered" {
		t.Fatalf("hand over: %d %v", st, p)
	}
	if st, p := carol.do("GET", "/v1/activity?filter=orders", nil, false); st != http.StatusOK || len(p["items"].([]any)) != 1 {
		t.Errorf("order in activity: %d %v", st, p)
	}

	// The web business console: a computer shows a code; Bob checks which browser it is and approves it
	// with his PIN, for the kiosk. Carol isn't a member, so she can't.
	login, err := web.StartLogin(context.Background(), "192.0.2.7", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) Chrome/131.0")
	if err != nil {
		t.Fatal(err)
	}
	if st, p := bob.do("POST", "/v1/web-logins/lookup", map[string]any{"code": "ZZZZ-ZZZZ"}, false); st != http.StatusNotFound || p["code"] != "code_not_found" {
		t.Errorf("unknown code: %d %v", st, p)
	}
	if st, p := bob.do("POST", "/v1/web-logins/lookup", map[string]any{"code": bizweb.FormatCode(login.Code)}, false); st != http.StatusOK || p["browser"] != "Chrome on Windows" || p["ip"] != "192.0.2.7" {
		t.Errorf("lookup: %d %v", st, p)
	}
	approve := map[string]any{"code": login.Code, "merchant_id": kiosk, "pin": "582047"}
	if st, p := carol.do("POST", "/v1/web-logins/approve", map[string]any{"code": login.Code, "merchant_id": kiosk, "pin": "582047"}, false); st != http.StatusForbidden {
		t.Errorf("a customer approved a shop sign-in: %d %v", st, p)
	}
	if st, p := bob.do("POST", "/v1/web-logins/approve", map[string]any{"code": login.Code, "merchant_id": kiosk, "pin": "000000"}, false); st != http.StatusForbidden || p["code"] != "invalid_pin" {
		t.Errorf("wrong PIN: %d %v", st, p)
	}
	if st, p := bob.do("POST", "/v1/web-logins/approve", approve, false); st != http.StatusOK || p["browser"] != "Chrome on Windows" {
		t.Fatalf("approve: %d %v", st, p)
	}
	if state, _, err := web.PollLogin(context.Background(), login.Secret, login.Code, "192.0.2.7", "test"); err != nil || state != bizweb.LoginApproved {
		t.Fatalf("browser picks up its session: %v %v", state, err)
	}
	st, sessions := bob.do("GET", "/v1/me/web-sessions", nil, false)
	browsers, _ := sessions["items"].([]any)
	if st != http.StatusOK || len(browsers) != 1 {
		t.Fatalf("web sessions: %d %v", st, sessions)
	}
	sid := browsers[0].(map[string]any)["id"].(string)
	if st, _ := carol.do("DELETE", "/v1/me/web-sessions/"+sid, nil, false); st != http.StatusNotFound {
		t.Errorf("someone else signed out bob's browser: %d", st)
	}
	if st, _ := bob.do("DELETE", "/v1/me/web-sessions/"+sid, nil, false); st != http.StatusNoContent {
		t.Errorf("sign out browser: %d", st)
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
