package httpapi_test

// telebirr top-up over HTTP: the app starts a top-up, the customer pays at (a test stand-in for)
// telebirr, and telebirr's signed notification arrives at the webhook. Needs TEST_DATABASE_URL.

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"superapp/backend/internal/httpapi"
	"superapp/backend/internal/identity"
	"superapp/backend/internal/ledger"
	"superapp/backend/internal/payments"
	"superapp/backend/internal/platform"
	"superapp/backend/internal/rails/telebirr"
	"superapp/backend/internal/rails/telebirr/telebirrtest"
)

func TestTelebirrTopUpOverHTTP(t *testing.T) {
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

	merchantKey, _ := rsa.GenerateKey(rand.Reader, 2048)
	tb := telebirrtest.New(t, merchantKey)
	tbClient, err := telebirr.New(tb.Config(merchantKey))
	if err != nil {
		t.Fatal(err)
	}

	seed, key := make([]byte, 32), make([]byte, 32)
	_, _ = rand.Read(seed)
	_, _ = rand.Read(key)
	tokens, _ := identity.NewTokenSigner(seed)
	box := &inbox{codes: map[string]string{}}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	ids, _ := identity.NewService(pool, tokens, key, box, identity.DevAttestation{}, identity.DefaultConfig(), log)
	l := ledger.New()
	pay := payments.NewService(pool, l, payments.DefaultRules(), &platform.PINVerifier{Pool: pool})
	pay.Rails["telebirr"] = tbClient
	srv := httptest.NewServer((&httpapi.Server{
		Pool: pool, Ledger: l, Payments: pay, Identity: ids, Auth: ids, Log: log,
		Webhooks: map[string]httpapi.NotificationVerifier{"telebirr": tbClient},
	}).Routes())
	defer srv.Close()

	// Sign up and set a PIN.
	n, _ := rand.Int(rand.Reader, big.NewInt(100_000_000))
	phone := fmt.Sprintf("+2519%08d", n.Int64())
	c := &client{t: t, base: srv.URL}
	_, ch := c.do("POST", "/v1/auth/otp", map[string]any{"phone": phone, "purpose": "signup"}, false)
	box.mu.Lock()
	code := box.codes[phone]
	box.mu.Unlock()
	_, s := c.do("POST", "/v1/auth/otp/verify", map[string]any{"challenge_id": ch["challenge_id"], "code": code,
		"device": map[string]any{"platform": "android", "attestation": "hanna-phone"}}, false)
	c.token = s["access_token"].(string)
	if st, _ := c.do("PUT", "/v1/auth/pin", map[string]any{"new_pin": "582047"}, true); st != http.StatusNoContent {
		t.Fatalf("set PIN: %d", st)
	}

	balance := func() float64 {
		_, b := c.do("GET", "/v1/wallet/balances", nil, false)
		return b["balances"].([]any)[0].(map[string]any)["available"].(map[string]any)["amount_minor"].(float64)
	}

	// Start the top-up: a checkout link comes back, nothing is credited.
	st, out := c.do("POST", "/v1/topups", map[string]any{"rail": "telebirr", "amount": map[string]any{"amount_minor": 75_000, "currency": "ETB"}}, true)
	if st != http.StatusCreated {
		t.Fatalf("start top-up: %d %v", st, out)
	}
	intent := out["intent"].(map[string]any)
	id := uuid.MustParse(intent["id"].(string))
	orderID := payments.OrderID(id)
	if intent["status"] != "processing" || !strings.Contains(out["checkout_url"].(string), "prepay_id=prepay-"+orderID) {
		t.Fatalf("top-up response: %v", out)
	}
	if tb.Order(orderID).Amount != "750.00" {
		t.Fatalf("telebirr order amount %q", tb.Order(orderID).Amount)
	}

	webhook := func(body []byte) int {
		resp, err := http.Post(srv.URL+"/v1/webhooks/rails/telebirr", "application/json", bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}

	// A forged "paid" notification is refused and credits nothing.
	tb.Pay(orderID, "PAY-"+orderID, "")
	forged := bytes.Replace(tb.Notification(orderID, false), []byte(`"750.00"`), []byte(`"7500.00"`), 1)
	if st := webhook(forged); st != http.StatusUnauthorized {
		t.Errorf("forged notification: %d", st)
	}
	if b := balance(); b != 0 {
		t.Fatalf("credited %v from a forged notification", b)
	}

	// The real notification credits once, however often telebirr retries it.
	genuine := tb.Notification(orderID, true) // body says transId, signature covers trans_id
	for i := 0; i < 3; i++ {
		if st := webhook(genuine); st != http.StatusOK {
			t.Fatalf("notification #%d: %d", i+1, st)
		}
	}
	if b := balance(); b != 75_000 {
		t.Errorf("balance %v, want 75000", b)
	}
	_, got := c.do("GET", "/v1/payment-intents/"+id.String(), nil, false)
	if got["status"] != "succeeded" {
		t.Errorf("intent: %v", got)
	}

	// While telebirr is unreachable, a notification for a pending order is answered 503 so it's retried.
	_, out = c.do("POST", "/v1/topups", map[string]any{"rail": "telebirr", "amount": map[string]any{"amount_minor": 10_000, "currency": "ETB"}}, true)
	second := payments.OrderID(uuid.MustParse(out["intent"].(map[string]any)["id"].(string)))
	tb.Pay(second, "PAY-"+second, "")
	tb.Down.Store(true)
	if st := webhook(tb.Notification(second, false)); st != http.StatusServiceUnavailable {
		t.Errorf("telebirr down: %d", st)
	}
	tb.Down.Store(false)
	if st := webhook(tb.Notification(second, false)); st != http.StatusOK || balance() != 85_000 {
		t.Errorf("after recovery: %d balance %v", st, balance())
	}

	if st := webhook([]byte(`{}`)); st != http.StatusUnauthorized {
		t.Errorf("unsigned empty notification: %d", st)
	}
	// A rail without credentials has no webhook.
	resp, _ := http.Post(srv.URL+"/v1/webhooks/rails/cbe_birr", "application/json", strings.NewReader("{}"))
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("unconfigured rail webhook: %d", resp.StatusCode)
	}
}
