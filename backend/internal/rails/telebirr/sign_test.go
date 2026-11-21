package telebirr

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"testing"

	"superapp/backend/internal/payments"
)

func TestCanonicalFlattensSortsAndExcludes(t *testing.T) {
	top := map[string]string{
		"timestamp": "1700000000", "nonce_str": "ABC", "method": "payment.preorder", "version": "1.0",
		"sign": "x", "sign_type": "SHA256WithRSA", "biz_content": "ignored",
	}
	biz := map[string]string{"merch_order_id": "o1", "appid": "app", "total_amount": "10.00", "title": "Top up", "raw_request": "no"}
	want := "appid=app&merch_order_id=o1&method=payment.preorder&nonce_str=ABC&timestamp=1700000000&title=Top up&total_amount=10.00&version=1.0"
	if got := canonical(top, biz); got != want {
		t.Errorf("canonical:\n got %s\nwant %s", got, want)
	}
	// biz_content wins over a top-level field of the same name.
	if got := canonical(map[string]string{"appid": "top"}, map[string]string{"appid": "biz"}); got != "appid=biz" {
		t.Errorf("override: %s", got)
	}
}

func TestSignVerify(t *testing.T) {
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	other, _ := rsa.GenerateKey(rand.Reader, 2048)
	for _, pad := range []Padding{PaddingPSS, PaddingPKCS1} {
		sig, err := sign(key, pad, "a=1&b=2")
		if err != nil {
			t.Fatal(err)
		}
		if err := verify(&key.PublicKey, pad, "a=1&b=2", sig); err != nil {
			t.Errorf("%s: own signature rejected", pad)
		}
		if verify(&key.PublicKey, pad, "a=1&b=3", sig) == nil {
			t.Errorf("%s: altered text accepted", pad)
		}
		if verify(&other.PublicKey, pad, "a=1&b=2", sig) == nil {
			t.Errorf("%s: wrong key accepted", pad)
		}
	}
	pss, _ := sign(key, PaddingPSS, "x")
	if verify(&key.PublicKey, PaddingPKCS1, "x", pss) == nil {
		t.Error("PSS signature accepted as PKCS1")
	}
}

func TestKeyFormats(t *testing.T) {
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	pkcs8, _ := x509.MarshalPKCS8PrivateKey(key)
	pkcs1 := x509.MarshalPKCS1PrivateKey(key)
	pkix, _ := x509.MarshalPKIXPublicKey(&key.PublicKey)
	for name, s := range map[string]string{
		"pkcs8 pem":         string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: pkcs8})),
		"pkcs1 pem":         string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: pkcs1})),
		"pkcs8 b64":         base64.StdEncoding.EncodeToString(pkcs8),
		"pkcs1 b64 wrapped": base64.StdEncoding.EncodeToString(pkcs1)[:40] + "\n" + base64.StdEncoding.EncodeToString(pkcs1)[40:],
	} {
		k, err := ParsePrivateKey(s)
		if err != nil || !k.Equal(key) {
			t.Errorf("private %s: %v", name, err)
		}
	}
	for name, s := range map[string]string{
		"pkix pem":  string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: pkix})),
		"pkix b64":  base64.StdEncoding.EncodeToString(pkix),
		"pkcs1 pem": string(pem.EncodeToMemory(&pem.Block{Type: "RSA PUBLIC KEY", Bytes: x509.MarshalPKCS1PublicKey(&key.PublicKey)})),
	} {
		k, err := ParsePublicKey(s)
		if err != nil || !k.Equal(&key.PublicKey) {
			t.Errorf("public %s: %v", name, err)
		}
	}
	if _, err := ParsePrivateKey("not a key"); err == nil {
		t.Error("garbage accepted")
	}
}

func TestOrderState(t *testing.T) {
	cases := []struct {
		order, trade string
		want         payments.RailState
	}{
		{"PAY_SUCCESS", "", payments.RailSucceeded},
		{"", "Completed", payments.RailSucceeded},
		{"WAIT_PAY", "Completed", payments.RailPending}, // order_status wins when present
		{"PAY_FAILED", "", payments.RailFailed},
		{"PAY_CANCEL", "", payments.RailFailed},
		{"WAIT_PAY", "", payments.RailPending},
		{"SOMETHING_NEW", "", payments.RailPending},
		{"", "", payments.RailPending},
	}
	for _, c := range cases {
		if got := orderState(c.order, c.trade); got != c.want {
			t.Errorf("%q/%q: %s, want %s", c.order, c.trade, got, c.want)
		}
	}
}
