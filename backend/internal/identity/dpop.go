package identity

import (
	"bytes"
	"context"
	"crypto/ecdh"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"math/big"
	"math/rand/v2"
	"net/http"
	"strings"
	"time"
)

// DPoP (RFC 9449) ties a session to a key pair the app keeps in the phone's secure hardware. Every
// request carries a short proof signed with that key over the method, URL and access token, so a token
// copied off the device (from a log, a proxy, a backup) is useless without the key.
//
// Only ES256 (ECDSA P-256) is accepted: it is what Android Keystore/StrongBox and the iOS Secure Enclave
// can hold. A session is bound when the sign-in or a refresh carries a proof; after that, every token of
// that session needs one. Config.DPoPRequired refuses sign-ins without a proof.

// ErrDPoP means the proof was missing, malformed, stale, replayed or for another key.
var ErrDPoP = errors.New("invalid DPoP proof")

const (
	dpopMaxAge    = 5 * time.Minute // phones' clocks drift; replays inside the window are caught by jti
	dpopMaxFuture = 1 * time.Minute
)

// DPoPProof is what a handler passes in from the request: the DPoP header and the request it came with.
type DPoPProof struct {
	Header string // the DPoP header value; empty means no proof was sent
	Method string
	URL    string // scheme://host/path as the client addressed it (see RequestURL)
}

// ProofFrom reads the DPoP header from a request.
func (s *Service) ProofFrom(r *http.Request) DPoPProof {
	return DPoPProof{Header: r.Header.Get("DPoP"), Method: r.Method, URL: s.RequestURL(r)}
}

// RequestURL is the URL the client used, which a proof's htu must match. Behind a proxy that terminates
// TLS or rewrites the host, set Config.PublicBaseURL (for example https://api.gebeya.one).
func (s *Service) RequestURL(r *http.Request) string {
	if s.Cfg.PublicBaseURL != "" {
		return strings.TrimRight(s.Cfg.PublicBaseURL, "/") + r.URL.Path
	}
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	return scheme + "://" + r.Host + r.URL.Path
}

type dpopJWK struct {
	Kty string `json:"kty"`
	Crv string `json:"crv"`
	X   string `json:"x"`
	Y   string `json:"y"`
	D   string `json:"d,omitempty"`
}

type dpopHeader struct {
	Typ string  `json:"typ"`
	Alg string  `json:"alg"`
	JWK dpopJWK `json:"jwk"`
}

type dpopClaims struct {
	JTI   string `json:"jti"`
	HTM   string `json:"htm"`
	HTU   string `json:"htu"`
	IAT   int64  `json:"iat"`
	ATH   string `json:"ath,omitempty"`
	Nonce string `json:"nonce,omitempty"`
}

// verifyDPoP checks a proof and returns the key's thumbprint (RFC 7638). accessToken is empty at sign-in
// and refresh; otherwise the proof must carry its hash. Each proof is accepted once.
func (s *Service) verifyDPoP(ctx context.Context, p DPoPProof, accessToken string, now time.Time) (string, error) {
	if p.Header == "" || len(p.Header) > 4096 || strings.Contains(p.Header, ",") {
		return "", ErrDPoP
	}
	parts := strings.Split(p.Header, ".")
	if len(parts) != 3 {
		return "", ErrDPoP
	}
	hb, err := b64.DecodeString(parts[0])
	if err != nil {
		return "", ErrDPoP
	}
	var h dpopHeader
	if err := json.Unmarshal(hb, &h); err != nil || h.Typ != "dpop+jwt" || h.Alg != "ES256" ||
		h.JWK.Kty != "EC" || h.JWK.Crv != "P-256" || h.JWK.D != "" {
		return "", ErrDPoP
	}
	pub, err := ecPublicKey(h.JWK.X, h.JWK.Y)
	if err != nil {
		return "", ErrDPoP
	}
	sig, err := b64.DecodeString(parts[2])
	if err != nil || len(sig) != 64 {
		return "", ErrDPoP
	}
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if !ecdsa.Verify(pub, digest[:], new(big.Int).SetBytes(sig[:32]), new(big.Int).SetBytes(sig[32:])) {
		return "", ErrDPoP
	}
	cb, err := b64.DecodeString(parts[1])
	if err != nil {
		return "", ErrDPoP
	}
	var c dpopClaims
	if err := json.Unmarshal(cb, &c); err != nil {
		return "", ErrDPoP
	}
	htu, _, _ := strings.Cut(c.HTU, "?")
	htu, _, _ = strings.Cut(htu, "#")
	iat := time.Unix(c.IAT, 0)
	if c.JTI == "" || len(c.JTI) > 128 || c.HTM != p.Method || !strings.EqualFold(htu, p.URL) ||
		iat.Before(now.Add(-dpopMaxAge)) || iat.After(now.Add(dpopMaxFuture)) {
		return "", ErrDPoP
	}
	if accessToken != "" {
		sum := sha256.Sum256([]byte(accessToken))
		if c.ATH != b64.EncodeToString(sum[:]) {
			return "", ErrDPoP
		}
	}
	jkt := thumbprint(h.JWK)
	// Replay check: the same proof (key + jti) is accepted once while it could still pass the age check.
	tag, err := s.Pool.Exec(ctx, `
		INSERT INTO identity.dpop_proofs (jkt, jti, expires_at) VALUES ($1, $2, $3)
		ON CONFLICT DO NOTHING`, jkt, c.JTI, iat.Add(dpopMaxAge+dpopMaxFuture))
	if err != nil {
		return "", err
	}
	if tag.RowsAffected() == 0 {
		return "", ErrDPoP
	}
	if rand.IntN(200) == 0 { // keep the table small without a separate job
		_, _ = s.Pool.Exec(ctx, `DELETE FROM identity.dpop_proofs WHERE expires_at < $1`, now)
	}
	return jkt, nil
}

func ecPublicKey(x, y string) (*ecdsa.PublicKey, error) {
	xb, err := b64.DecodeString(x)
	if err != nil || len(xb) != 32 {
		return nil, ErrDPoP
	}
	yb, err := b64.DecodeString(y)
	if err != nil || len(yb) != 32 {
		return nil, ErrDPoP
	}
	// crypto/ecdh rejects points that aren't on the curve, so a crafted key can't be used.
	if _, err := ecdh.P256().NewPublicKey(append(append([]byte{4}, xb...), yb...)); err != nil {
		return nil, ErrDPoP
	}
	return &ecdsa.PublicKey{Curve: elliptic.P256(), X: new(big.Int).SetBytes(xb), Y: new(big.Int).SetBytes(yb)}, nil
}

// thumbprint is the RFC 7638 SHA-256 JWK thumbprint: the required members in lexicographic order.
func thumbprint(k dpopJWK) string {
	var buf bytes.Buffer
	buf.WriteString(`{"crv":"P-256","kty":"EC","x":"`)
	buf.WriteString(k.X)
	buf.WriteString(`","y":"`)
	buf.WriteString(k.Y)
	buf.WriteString(`"}`)
	sum := sha256.Sum256(buf.Bytes())
	return b64.EncodeToString(sum[:])
}
