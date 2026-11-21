package httpapi_test

// DPoP-bound sessions and biometric confirmation over HTTP, with real P-256 keys standing in for the
// phone's secure hardware. Needs TEST_DATABASE_URL.

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"superapp/backend/internal/httpapi"
	"superapp/backend/internal/identity"
	"superapp/backend/internal/ledger"
	"superapp/backend/internal/payments"
	"superapp/backend/internal/platform"
)

var b64u = base64.RawURLEncoding

// device holds the two keys the app keeps in secure hardware: one signs DPoP proofs on every
// request, the other (biometric-gated on a phone) signs payment confirmations.
type device struct {
	t         *testing.T
	base      string
	dpopKey   *ecdsa.PrivateKey
	bioKey    *ecdsa.PrivateKey
	token     string
	tokenType string
	refresh   string
	// lastProof is the most recent DPoP header sent, so a test can replay it.
	lastProof string
}

func newDevice(t *testing.T, base string) *device {
	k1, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	k2, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	return &device{t: t, base: base, dpopKey: k1, bioKey: k2}
}

func proof(key *ecdsa.PrivateKey, method, url, accessToken string, iat time.Time) string {
	pad := func(b *big.Int) string { return b64u.EncodeToString(b.FillBytes(make([]byte, 32))) }
	h, _ := json.Marshal(map[string]any{"typ": "dpop+jwt", "alg": "ES256",
		"jwk": map[string]string{"kty": "EC", "crv": "P-256", "x": pad(key.X), "y": pad(key.Y)}})
	claims := map[string]any{"jti": uuid.NewString(), "htm": method, "htu": url, "iat": iat.Unix()}
	if accessToken != "" {
		sum := sha256.Sum256([]byte(accessToken))
		claims["ath"] = b64u.EncodeToString(sum[:])
	}
	c, _ := json.Marshal(claims)
	signing := b64u.EncodeToString(h) + "." + b64u.EncodeToString(c)
	sum := sha256.Sum256([]byte(signing))
	r, s, _ := ecdsa.Sign(rand.Reader, key, sum[:])
	sig := append(r.FillBytes(make([]byte, 32)), s.FillBytes(make([]byte, 32))...)
	return signing + "." + b64u.EncodeToString(sig)
}

type reqOpt struct {
	noProof    bool
	proof      string // use this DPoP header as is
	bearer     bool   // send the token as a bearer token
	key        *ecdsa.PrivateKey
	iat        time.Time
	urlOverlay string
}

func (d *device) call(method, path string, body any, o reqOpt) (int, map[string]any) {
	d.t.Helper()
	var r io.Reader
	if body != nil {
		buf, _ := json.Marshal(body)
		r = bytes.NewReader(buf)
	}
	req, _ := http.NewRequest(method, d.base+path, r)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", uuid.NewString())
	key, iat := d.dpopKey, time.Now()
	if o.key != nil {
		key = o.key
	}
	if !o.iat.IsZero() {
		iat = o.iat
	}
	authed := d.token != "" && path != "/v1/auth/otp" && path != "/v1/auth/otp/verify" && path != "/v1/auth/token/refresh"
	if authed {
		scheme := "DPoP "
		if o.bearer {
			scheme = "Bearer "
		}
		req.Header.Set("Authorization", scheme+d.token)
	}
	switch {
	case o.proof != "":
		req.Header.Set("DPoP", o.proof)
	case !o.noProof:
		at := ""
		if authed {
			at = d.token
		}
		url := d.base + path
		if o.urlOverlay != "" {
			url = o.urlOverlay
		}
		d.lastProof = proof(key, method, url, at, iat)
		req.Header.Set("DPoP", d.lastProof)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		d.t.Fatal(err)
	}
	defer resp.Body.Close()
	out := map[string]any{}
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

func (d *device) signIn(phone, att string, box *inbox) {
	d.t.Helper()
	st, ch := d.call("POST", "/v1/auth/otp", map[string]any{"phone": phone, "purpose": "login"}, reqOpt{noProof: true})
	if st != http.StatusAccepted {
		d.t.Fatalf("otp: %d %v", st, ch)
	}
	box.mu.Lock()
	code := box.codes[phone]
	box.mu.Unlock()
	st, s := d.call("POST", "/v1/auth/otp/verify", map[string]any{
		"challenge_id": ch["challenge_id"], "code": code, "device": map[string]any{"platform": "android", "attestation": att},
	}, reqOpt{})
	if st != http.StatusOK {
		d.t.Fatalf("verify: %d %v", st, s)
	}
	d.token, d.tokenType, d.refresh = s["access_token"].(string), s["token_type"].(string), s["refresh_token"].(string)
}

func TestDPoPAndBiometricConfirmation(t *testing.T) {
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
	cfg.OTPResendAfter = 0
	ids, err := identity.NewService(pool, tokens, key, box, identity.DevAttestation{}, cfg, log)
	if err != nil {
		t.Fatal(err)
	}
	l := ledger.New()
	rules := payments.DefaultRules()
	rules.VelocityMax = 0
	pay := payments.NewService(pool, l, rules, &platform.PINVerifier{Pool: pool})
	pay.Biometric = &platform.BiometricVerifier{Pool: pool}
	srv := httptest.NewServer((&httpapi.Server{
		Pool: pool, Ledger: l, Payments: pay, Identity: ids, Auth: ids, Log: log, Sandbox: true,
	}).Routes())
	defer srv.Close()
	phone := func() string {
		n, _ := rand.Int(rand.Reader, big.NewInt(100_000_000))
		return fmt.Sprintf("+2519%08d", n.Int64())
	}

	// ---- DPoP ----
	payerPhone := phone()
	d := newDevice(t, srv.URL)
	d.signIn(payerPhone, "payer-phone", box)
	if d.tokenType != "DPoP" {
		t.Fatalf("token_type %q", d.tokenType)
	}
	if st, p := d.call("PUT", "/v1/auth/pin", map[string]any{"new_pin": "582047"}, reqOpt{}); st != http.StatusNoContent {
		t.Fatalf("set PIN: %d %v", st, p)
	}
	if st, _ := d.call("GET", "/v1/me", nil, reqOpt{}); st != http.StatusOK {
		t.Fatalf("with a proof: %d", st)
	}
	replay := d.lastProof
	for name, o := range map[string]reqOpt{
		"stolen token as bearer": {bearer: true, noProof: true},
		"no proof":               {noProof: true},
		"replayed proof":         {proof: replay},
		"another key":            {key: newDevice(t, "").dpopKey},
		"stale proof":            {iat: time.Now().Add(-10 * time.Minute)},
		"proof for another URL":  {urlOverlay: srv.URL + "/v1/wallet/balances"},
		"proof from the future":  {iat: time.Now().Add(5 * time.Minute)},
	} {
		if st, _ := d.call("GET", "/v1/me", nil, o); st != http.StatusUnauthorized {
			t.Errorf("%s: %d, want 401", name, st)
		}
	}
	// A bound session refreshes only with its own key; a thief with the refresh token alone gets nowhere
	// and doesn't sign the owner out either.
	thief := newDevice(t, srv.URL)
	if st, p := thief.call("POST", "/v1/auth/token/refresh", map[string]any{"refresh_token": d.refresh}, reqOpt{}); st != http.StatusUnauthorized || p["code"] != "invalid_dpop_proof" {
		t.Errorf("refresh with another key: %d %v", st, p)
	}
	if st, _ := d.call("POST", "/v1/auth/token/refresh", map[string]any{"refresh_token": d.refresh}, reqOpt{noProof: true}); st != http.StatusUnauthorized {
		t.Errorf("refresh without a proof: %d", st)
	}
	st, s := d.call("POST", "/v1/auth/token/refresh", map[string]any{"refresh_token": d.refresh}, reqOpt{})
	if st != http.StatusOK || s["token_type"] != "DPoP" {
		t.Fatalf("refresh: %d %v", st, s)
	}
	d.token, d.refresh = s["access_token"].(string), s["refresh_token"].(string)
	if st, _ := d.call("GET", "/v1/me", nil, reqOpt{}); st != http.StatusOK {
		t.Errorf("after refresh: %d", st)
	}

	// An app without DPoP still gets a bearer session (until DPOP_REQUIRED is switched on).
	old := newDevice(t, srv.URL)
	old.signInNoProof(phone(), "old-app", box)
	if old.tokenType != "Bearer" {
		t.Errorf("unbound sign-in: %q", old.tokenType)
	}
	if st, _ := old.call("GET", "/v1/me", nil, reqOpt{bearer: true, noProof: true}); st != http.StatusOK {
		t.Errorf("bearer session: %d", st)
	}
	if st, _ := old.call("GET", "/v1/me", nil, reqOpt{}); st != http.StatusUnauthorized {
		t.Errorf("a bearer token sent as DPoP: %d", st)
	}

	// ---- biometric confirmation ----
	payee := newDevice(t, srv.URL)
	payeePhone := phone()
	payee.signIn(payeePhone, "payee-phone", box)
	if st, p := payee.call("PUT", "/v1/auth/pin", map[string]any{"new_pin": "582047"}, reqOpt{}); st != http.StatusNoContent {
		t.Fatalf("payee PIN: %d %v", st, p)
	}
	if st, p := d.call("POST", "/v1/sandbox/topups", map[string]any{"amount": map[string]any{"amount_minor": 50_000, "currency": "ETB"}}, reqOpt{}); st != http.StatusCreated {
		t.Fatalf("top-up: %d %v", st, p)
	}
	intent := func(minor int) map[string]any {
		st, in := d.call("POST", "/v1/payment-intents", map[string]any{
			"kind": "p2p", "payee_phone": payeePhone, "amount": map[string]any{"amount_minor": minor, "currency": "ETB"},
		}, reqOpt{})
		if st != http.StatusCreated {
			t.Fatalf("intent: %d %v", st, in)
		}
		return in
	}
	sign := func(k *ecdsa.PrivateKey, challenge string) string {
		msg, _ := base64.StdEncoding.DecodeString(challenge)
		sum := sha256.Sum256(msg)
		sig, _ := ecdsa.SignASN1(rand.Reader, k, sum[:])
		return base64.StdEncoding.EncodeToString(sig)
	}
	spki := func(k *ecdsa.PrivateKey) string {
		der, _ := x509.MarshalPKIXPublicKey(&k.PublicKey)
		return base64.StdEncoding.EncodeToString(der)
	}

	in := intent(1_000)
	challenge, _ := in["confirm_challenge"].(string)
	msg, _ := base64.StdEncoding.DecodeString(challenge)
	if want := fmt.Sprintf("gebeya-one/confirm/v1\nintent:%s\namount:1000 ETB\nfee:0 ETB\npayee:user:", in["id"]); !bytes.HasPrefix(msg, []byte(want)) {
		t.Fatalf("challenge %q", msg)
	}
	confirm := func(id string, body map[string]any) (int, map[string]any) {
		return d.call("POST", "/v1/payment-intents/"+id+"/confirm", body, reqOpt{})
	}
	if st, p := confirm(in["id"].(string), map[string]any{"biometric_signature": sign(d.bioKey, challenge)}); st != http.StatusForbidden || p["code"] != "biometric_not_enrolled" {
		t.Fatalf("before enrolling: %d %v", st, p)
	}
	if st, p := d.call("PUT", "/v1/devices/current/biometric-key", map[string]any{"public_key": spki(d.bioKey), "pin": "000000"}, reqOpt{}); st != http.StatusForbidden || p["code"] != "invalid_pin" {
		t.Fatalf("enroll with a wrong PIN: %d %v", st, p)
	}
	if st, p := d.call("PUT", "/v1/devices/current/biometric-key", map[string]any{"public_key": "bm90IGEga2V5", "pin": "582047"}, reqOpt{}); st != http.StatusUnprocessableEntity {
		t.Fatalf("enroll with a bad key: %d %v", st, p)
	}
	if st, p := d.call("PUT", "/v1/devices/current/biometric-key", map[string]any{"public_key": spki(d.bioKey), "pin": "582047"}, reqOpt{}); st != http.StatusNoContent {
		t.Fatalf("enroll: %d %v", st, p)
	}
	// A signature over a different amount, or from another key, doesn't confirm.
	other := intent(2_000)
	if st, p := confirm(in["id"].(string), map[string]any{"biometric_signature": sign(d.bioKey, other["confirm_challenge"].(string))}); st != http.StatusForbidden || p["code"] != "biometric_failed" {
		t.Errorf("signature over another intent: %d %v", st, p)
	}
	if st, p := confirm(in["id"].(string), map[string]any{"biometric_signature": sign(newDevice(t, "").bioKey, challenge)}); st != http.StatusForbidden || p["code"] != "biometric_failed" {
		t.Errorf("signature from another key: %d %v", st, p)
	}
	if st, p := confirm(in["id"].(string), map[string]any{"biometric_signature": sign(d.bioKey, challenge), "pin": "582047"}); st != http.StatusUnprocessableEntity {
		t.Errorf("both: %d %v", st, p)
	}
	st, out := confirm(in["id"].(string), map[string]any{"biometric_signature": sign(d.bioKey, challenge)})
	if st != http.StatusOK || out["status"] != "succeeded" {
		t.Fatalf("biometric confirm: %d %v", st, out)
	}
	if _, ok := out["confirm_challenge"]; ok {
		t.Error("a finished intent still offers a challenge")
	}
	// The key belongs to this device only: the same user on another device can't use it.
	d2 := newDevice(t, srv.URL)
	d2.bioKey = d.bioKey
	d2.signIn(payerPhone, "payer-tablet", box)
	st, in2 := d2.call("POST", "/v1/payment-intents", map[string]any{
		"kind": "p2p", "payee_phone": payeePhone, "amount": map[string]any{"amount_minor": 500, "currency": "ETB"},
	}, reqOpt{})
	if st != http.StatusCreated {
		t.Fatalf("intent on the second device: %d %v", st, in2)
	}
	if st, p := d2.call("POST", "/v1/payment-intents/"+in2["id"].(string)+"/confirm",
		map[string]any{"biometric_signature": sign(d.bioKey, in2["confirm_challenge"].(string))}, reqOpt{}); st != http.StatusForbidden || p["code"] != "biometric_not_enrolled" {
		t.Errorf("another device: %d %v", st, p)
	}

	// A PIN reset (maybe by someone who took over the SIM) turns biometric confirmation off everywhere.
	r := newDevice(t, srv.URL)
	st, ch := r.call("POST", "/v1/auth/otp", map[string]any{"phone": payerPhone, "purpose": "pin_reset"}, reqOpt{noProof: true})
	if st != http.StatusAccepted {
		t.Fatalf("reset otp: %d", st)
	}
	box.mu.Lock()
	code := box.codes[payerPhone]
	box.mu.Unlock()
	st, rs := r.call("POST", "/v1/auth/otp/verify", map[string]any{"challenge_id": ch["challenge_id"], "code": code,
		"device": map[string]any{"platform": "android", "attestation": "payer-phone"}}, reqOpt{})
	if st != http.StatusOK {
		t.Fatalf("reset verify: %d %v", st, rs)
	}
	r.token = rs["access_token"].(string)
	if st, p := r.call("PUT", "/v1/auth/pin", map[string]any{"new_pin": "730918"}, reqOpt{}); st != http.StatusNoContent {
		t.Fatalf("reset PIN: %d %v", st, p)
	}
	in3 := intent3(t, r, payeePhone)
	if st, p := r.call("POST", "/v1/payment-intents/"+in3["id"].(string)+"/confirm",
		map[string]any{"biometric_signature": sign(d.bioKey, in3["confirm_challenge"].(string))}, reqOpt{}); st != http.StatusForbidden || p["code"] != "biometric_not_enrolled" {
		t.Errorf("after a PIN reset: %d %v", st, p)
	}
}

func intent3(t *testing.T, d *device, payeePhone string) map[string]any {
	t.Helper()
	st, in := d.call("POST", "/v1/payment-intents", map[string]any{
		"kind": "p2p", "payee_phone": payeePhone, "amount": map[string]any{"amount_minor": 300, "currency": "ETB"},
	}, reqOpt{})
	if st != http.StatusCreated {
		t.Fatalf("intent: %d %v", st, in)
	}
	return in
}

func (d *device) signInNoProof(phone, att string, box *inbox) {
	d.t.Helper()
	st, ch := d.call("POST", "/v1/auth/otp", map[string]any{"phone": phone, "purpose": "signup"}, reqOpt{noProof: true})
	if st != http.StatusAccepted {
		d.t.Fatalf("otp: %d %v", st, ch)
	}
	box.mu.Lock()
	code := box.codes[phone]
	box.mu.Unlock()
	st, s := d.call("POST", "/v1/auth/otp/verify", map[string]any{
		"challenge_id": ch["challenge_id"], "code": code, "device": map[string]any{"platform": "android", "attestation": att},
	}, reqOpt{noProof: true})
	if st != http.StatusOK {
		d.t.Fatalf("verify: %d %v", st, s)
	}
	d.token, d.tokenType, d.refresh = s["access_token"].(string), s["token_type"].(string), s["refresh_token"].(string)
}
