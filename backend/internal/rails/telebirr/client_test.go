package telebirr_test

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"net/url"
	"strings"
	"testing"

	"superapp/backend/internal/money"
	"superapp/backend/internal/payments"
	"superapp/backend/internal/rails/telebirr"
	"superapp/backend/internal/rails/telebirr/telebirrtest"
)

func setup(t *testing.T) (*telebirr.Client, *telebirrtest.Server, *rsa.PrivateKey) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	srv := telebirrtest.New(t, key)
	c, err := telebirr.New(srv.Config(key))
	if err != nil {
		t.Fatal(err)
	}
	return c, srv, key
}

func TestCheckoutAndQuery(t *testing.T) {
	c, srv, key := setup(t)
	ctx := context.Background()

	co, err := c.CreateCheckout(ctx, "order1", money.New(123450, money.ETB), "Wallet top-up")
	if err != nil {
		t.Fatal(err)
	}
	if o := srv.Order("order1"); o == nil || o.Amount != "1234.50" {
		t.Fatalf("order at telebirr: %+v", o)
	}
	if co.OrderRef != "prepay-order1" {
		t.Errorf("prepay id %q", co.OrderRef)
	}

	// The checkout link is signed by us and survives URL decoding intact.
	u, err := url.Parse(co.CheckoutURL)
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	signed := map[string]string{}
	for _, k := range []string{"appid", "merch_code", "nonce_str", "prepay_id", "timestamp"} {
		signed[k] = q.Get(k)
	}
	if err := telebirr.VerifyFields(&key.PublicKey, telebirr.PaddingPSS, signed, nil, q.Get("sign")); err != nil {
		t.Errorf("checkout signature: %v (url %s)", err, co.CheckoutURL)
	}
	if q.Get("trade_type") != "Checkout" || q.Get("prepay_id") != "prepay-order1" {
		t.Errorf("checkout params: %v", q)
	}

	ord, err := c.QueryOrder(ctx, "order1")
	if err != nil || ord.State != payments.RailPending {
		t.Fatalf("unpaid: %+v %v", ord, err)
	}
	srv.Pay("order1", "PAY123", "")
	ord, err = c.QueryOrder(ctx, "order1")
	if err != nil || ord.State != payments.RailSucceeded || ord.PartnerRef != "PAY123" || ord.Amount != money.New(123450, money.ETB) {
		t.Fatalf("paid: %+v %v", ord, err)
	}
	if n := srv.TokenCalls.Load(); n != 1 {
		t.Errorf("fabric token fetched %d times, want 1 (cached)", n)
	}

	if _, err := c.CreateCheckout(ctx, "order2", money.New(100, money.USD), "x"); err == nil {
		t.Error("USD checkout accepted")
	}
	if _, err := c.QueryOrder(ctx, "no-such-order"); err == nil {
		t.Error("unknown order: no error")
	}
}

func TestRevokedTokenIsRefreshed(t *testing.T) {
	c, srv, _ := setup(t)
	ctx := context.Background()
	if _, err := c.CreateCheckout(ctx, "o", money.New(1000, money.ETB), "x"); err != nil {
		t.Fatal(err)
	}
	srv.RejectToken.Store(true)
	if _, err := c.QueryOrder(ctx, "o"); err != nil {
		t.Fatalf("after token revocation: %v", err)
	}
	if n := srv.TokenCalls.Load(); n != 2 {
		t.Errorf("token calls %d, want 2", n)
	}
}

func TestProviderDown(t *testing.T) {
	c, srv, _ := setup(t)
	srv.Down.Store(true)
	if _, err := c.CreateCheckout(context.Background(), "o", money.New(1000, money.ETB), "x"); err == nil {
		t.Error("no error while telebirr is down")
	}
}

func TestNotificationVerification(t *testing.T) {
	c, srv, key := setup(t)
	ctx := context.Background()
	if _, err := c.CreateCheckout(ctx, "order9", money.New(5000, money.ETB), "x"); err != nil {
		t.Fatal(err)
	}
	srv.Pay("order9", "PAY9", "")

	good := srv.Notification("order9", false)
	if id, err := c.VerifyNotification(good); err != nil || id != "order9" {
		t.Fatalf("valid notification: %q %v", id, err)
	}
	if id, err := c.VerifyNotification(srv.Notification("order9", true)); err != nil || id != "order9" {
		t.Errorf("transId quirk: %q %v", id, err)
	}
	wrapped, _ := json.Marshal(map[string]json.RawMessage{"data": good})
	if _, err := c.VerifyNotification(wrapped); err != nil {
		t.Errorf("data envelope: %v", err)
	}

	tampered := strings.Replace(string(good), `"total_amount":"50.00"`, `"total_amount":"5000.00"`, 1)
	if tampered == string(good) {
		t.Fatal("test setup: amount not found in notification")
	}
	var fields map[string]any
	_ = json.Unmarshal(good, &fields)
	delete(fields, "sign")
	unsigned, _ := json.Marshal(fields)

	// Signed with our own key instead of telebirr's: must not pass.
	str := map[string]string{}
	for k, v := range fields {
		if s, ok := v.(string); ok {
			str[k] = s
		}
	}
	forgedSig, _ := telebirr.SignFields(key, telebirr.PaddingPSS, str, nil)
	fields["sign"] = forgedSig
	forged, _ := json.Marshal(fields)

	for name, body := range map[string][]byte{
		"tampered amount": []byte(tampered), "unsigned": unsigned, "signed by merchant key": forged,
		"not json": []byte("<xml/>"), "empty": nil,
	} {
		if _, err := c.VerifyNotification(body); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestConfigValidation(t *testing.T) {
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	srv := telebirrtest.New(t, key)
	cfg := srv.Config(key)
	cfg.AppSecret = ""
	if _, err := telebirr.New(cfg); err == nil || !strings.Contains(err.Error(), "app secret") {
		t.Errorf("missing secret: %v", err)
	}
	cfg = srv.Config(key)
	cfg.BaseURL = "http://insecure.example"
	if _, err := telebirr.New(cfg); err == nil {
		t.Error("http base URL accepted")
	}
	cfg = srv.Config(key)
	cfg.Padding = "rsa-oaep"
	if _, err := telebirr.New(cfg); err == nil {
		t.Error("unknown padding accepted")
	}
}
