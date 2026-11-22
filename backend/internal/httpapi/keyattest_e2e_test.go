package httpapi_test

// Android key attestation over HTTP: sign-in and fingerprint set-up with chains from a test authority
// (keyattesttest) standing in for Google's attestation roots. Needs TEST_DATABASE_URL.

import (
	"context"
	"crypto/ecdsa"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"superapp/backend/internal/httpapi"
	"superapp/backend/internal/identity"
	kt "superapp/backend/internal/keyattest/keyattesttest"
	"superapp/backend/internal/ledger"
	"superapp/backend/internal/payments"
	"superapp/backend/internal/platform"
)

func chainJSON(chain [][]byte) []string {
	out := make([]string, len(chain))
	for i, c := range chain {
		out[i] = base64.StdEncoding.EncodeToString(c)
	}
	return out
}

func spkiB64(k *ecdsa.PrivateKey) string {
	der, _ := x509.MarshalPKIXPublicKey(&k.PublicKey)
	return base64.StdEncoding.EncodeToString(der)
}

func jkt(k *ecdsa.PublicKey) string {
	pad := func(b *big.Int) string { return b64u.EncodeToString(b.FillBytes(make([]byte, 32))) }
	sum := sha256.Sum256([]byte(`{"crv":"P-256","kty":"EC","x":"` + pad(k.X) + `","y":"` + pad(k.Y) + `"}`))
	return b64u.EncodeToString(sum[:])
}

// signInWith asks for a code and verifies it, letting [chain] build the key attestation from the
// challenge ID. It returns the verify status and body.
func (d *device) signInWith(phone, platformName, att string, box *inbox, o reqOpt, chain func(challengeID string) [][]byte) (int, map[string]any) {
	d.t.Helper()
	st, ch := d.call("POST", "/v1/auth/otp", map[string]any{"phone": phone, "purpose": "login"}, reqOpt{noProof: true})
	if st != http.StatusAccepted {
		d.t.Fatalf("otp: %d %v", st, ch)
	}
	box.mu.Lock()
	code := box.codes[phone]
	box.mu.Unlock()
	dev := map[string]any{"platform": platformName, "attestation": att}
	if chain != nil {
		dev["key_attestation"] = chainJSON(chain(ch["challenge_id"].(string)))
	}
	st, s := d.call("POST", "/v1/auth/otp/verify", map[string]any{"challenge_id": ch["challenge_id"], "code": code, "device": dev}, o)
	if st == http.StatusOK {
		d.token, d.tokenType, d.refresh = s["access_token"].(string), s["token_type"].(string), s["refresh_token"].(string)
	}
	return st, s
}

func attestServer(t *testing.T, pool *pgxpool.Pool, box *inbox, a *kt.Authority, mode string) *httptest.Server {
	seed, key := make([]byte, 32), make([]byte, 32)
	_, _ = rand.Read(seed)
	_, _ = rand.Read(key)
	tokens, _ := identity.NewTokenSigner(seed)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	cfg := identity.DefaultConfig()
	cfg.OTPResendAfter = 0
	cfg.KeyAttestation = mode
	cfg.OTPHourlyMax = 100 // this test signs one number in many times
	ids, err := identity.NewService(pool, tokens, key, box, identity.DevAttestation{}, cfg, log)
	if err != nil {
		t.Fatal(err)
	}
	ids.KeyAttester = a.Verifier()
	l := ledger.New()
	pay := payments.NewService(pool, l, payments.DefaultRules(), &platform.PINVerifier{Pool: pool})
	return httptest.NewServer((&httpapi.Server{Pool: pool, Ledger: l, Payments: pay, Identity: ids, Auth: ids, Log: log}).Routes())
}

func TestKeyAttestation(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	box := &inbox{codes: map[string]string{}}
	a := kt.New(t)
	phone := func() string {
		n, _ := rand.Int(rand.Reader, big.NewInt(100_000_000))
		return fmt.Sprintf("+2519%08d", n.Int64())
	}
	keyLevel := func(d *device) any {
		_, list := d.call("GET", "/v1/me/devices", nil, reqOpt{})
		items, _ := list["items"].([]any)
		if len(items) == 0 {
			t.Fatalf("devices: %v", list)
		}
		return items[0].(map[string]any)["key_attestation"]
	}

	// ---- require ----
	srv := attestServer(t, pool, box, a, identity.AttestRequire)
	defer srv.Close()
	p := phone()
	d := newDevice(t, srv.URL)
	good := func(id string) [][]byte {
		return a.Chain(&d.dpopKey.PublicKey, kt.Good(identity.DPoPAttestChallenge(id)))
	}
	refusals := map[string]struct {
		o     reqOpt
		chain func(string) [][]byte
		want  int
		code  string
	}{
		"no chain":      {reqOpt{}, nil, http.StatusForbidden, "device_not_trusted"},
		"no DPoP proof": {reqOpt{noProof: true}, good, http.StatusUnauthorized, "invalid_dpop_proof"},
		"another key": {reqOpt{}, func(id string) [][]byte {
			return a.Chain(&newDevice(t, "").dpopKey.PublicKey, kt.Good(identity.DPoPAttestChallenge(id)))
		}, http.StatusForbidden, "device_not_trusted"},
		"another sign-in": {reqOpt{}, func(string) [][]byte { return good("00000000-0000-0000-0000-000000000000") }, http.StatusForbidden, "device_not_trusted"},
		"software key": {reqOpt{}, func(id string) [][]byte {
			g := kt.Good(identity.DPoPAttestChallenge(id))
			g.Level = 0
			return a.Chain(&d.dpopKey.PublicKey, g)
		}, http.StatusForbidden, "device_not_trusted"},
		"untrusted root": {reqOpt{}, func(id string) [][]byte {
			return kt.New(t).Chain(&d.dpopKey.PublicKey, kt.Good(identity.DPoPAttestChallenge(id)))
		}, http.StatusForbidden, "device_not_trusted"},
		"bad certificates": {reqOpt{}, func(string) [][]byte { return [][]byte{[]byte("not a certificate")} }, http.StatusForbidden, "device_not_trusted"},
	}
	for name, c := range refusals {
		if st, body := d.signInWith(p, "android", "attest-phone", box, c.o, c.chain); st != c.want || body["code"] != c.code {
			t.Errorf("%s: %d %v", name, st, body)
		}
	}
	if st, body := d.signInWith(p, "android", "attest-phone", box, reqOpt{}, good); st != http.StatusOK || d.tokenType != "DPoP" {
		t.Fatalf("good chain: %d %v", st, body)
	}
	if got := keyLevel(d); got != "tee" {
		t.Errorf("device key: %v", got)
	}
	if st, p := d.call("PUT", "/v1/auth/pin", map[string]any{"new_pin": "582047"}, reqOpt{}); st != http.StatusNoContent {
		t.Fatalf("set PIN: %d %v", st, p)
	}

	// Fingerprint set-up needs the biometric key attested, biometric-gated, and tied to this session's key.
	bioChallenge := identity.BiometricAttestChallenge(jkt(&d.dpopKey.PublicKey))
	bio := func(desc kt.Desc) map[string]any {
		return map[string]any{"public_key": spkiB64(d.bioKey), "pin": "582047",
			"key_attestation": chainJSON(a.Chain(&d.bioKey.PublicKey, desc))}
	}
	notGated := kt.Good(bioChallenge)
	gated := kt.Good(bioChallenge)
	gated.Biometric = true
	otherSession := kt.Good(identity.BiometricAttestChallenge("another-session"))
	otherSession.Biometric = true
	for name, body := range map[string]map[string]any{
		"no chain":        {"public_key": spkiB64(d.bioKey), "pin": "582047"},
		"not biometric":   bio(notGated),
		"another session": bio(otherSession),
		"chain for the DPoP key": {"public_key": spkiB64(d.bioKey), "pin": "582047",
			"key_attestation": chainJSON(a.Chain(&d.dpopKey.PublicKey, gated))},
	} {
		if st, p := d.call("PUT", "/v1/devices/current/biometric-key", body, reqOpt{}); st != http.StatusForbidden || p["code"] != "device_not_trusted" {
			t.Errorf("biometric %s: %d %v", name, st, p)
		}
	}
	if st, p := d.call("PUT", "/v1/devices/current/biometric-key", bio(gated), reqOpt{}); st != http.StatusNoContent {
		t.Fatalf("biometric: %d %v", st, p)
	}
	var bioLevel string
	if err := pool.QueryRow(context.Background(), `
		SELECT biometric_key_attestation FROM identity.devices d JOIN identity.users u ON u.id = d.user_id
		WHERE u.phone_e164 = $1`, p).Scan(&bioLevel); err != nil || bioLevel != "tee" {
		t.Errorf("biometric attestation stored: %q %v", bioLevel, err)
	}

	// iPhones have no device keys yet: require mode doesn't apply to them.
	ios := newDevice(t, srv.URL)
	if st, body := ios.signInWith(phone(), "ios", "attest-iphone", box, reqOpt{noProof: true}, nil); st != http.StatusOK || ios.tokenType != "Bearer" {
		t.Errorf("iPhone: %d %v", st, body)
	}

	// ---- record ----
	rec := attestServer(t, pool, box, a, identity.AttestRecord)
	defer rec.Close()
	r := newDevice(t, rec.URL)
	rp := phone()
	if st, body := r.signInWith(rp, "android", "record-phone", box, reqOpt{}, nil); st != http.StatusOK {
		t.Fatalf("record, no chain: %d %v", st, body)
	}
	if got := keyLevel(r); got != "none" {
		t.Errorf("record, no chain: %v", got)
	}
	if st, body := r.signInWith(rp, "android", "record-phone", box, reqOpt{}, func(string) [][]byte { return good("x") }); st != http.StatusOK {
		t.Fatalf("record, bad chain: %d %v", st, body)
	}
	if got := keyLevel(r); got != "failed" {
		t.Errorf("record, bad chain: %v", got)
	}
	strong := func(id string) [][]byte {
		g := kt.Good(identity.DPoPAttestChallenge(id))
		g.Level = 2
		return a.Chain(&r.dpopKey.PublicKey, g)
	}
	if st, body := r.signInWith(rp, "android", "record-phone", box, reqOpt{}, strong); st != http.StatusOK {
		t.Fatalf("record, StrongBox: %d %v", st, body)
	}
	if got := keyLevel(r); got != "strongbox" {
		t.Errorf("record, StrongBox: %v", got)
	}
}
