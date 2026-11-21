package identity

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Access tokens are compact JWS (RFC 7515) signed with Ed25519 ("EdDSA", RFC 8037).
// Ed25519 lets other services verify tokens with the public key alone. The verifier accepts exactly
// one algorithm and one token type, so "alg: none" and algorithm-confusion tricks are rejected.

const (
	tokenIssuer   = "gebeya-one"
	tokenAudience = "gebeya-one-api"
	tokenType     = "at+jwt" // RFC 9068 access token
	clockLeeway   = 30 * time.Second
)

var errBadToken = errors.New("invalid access token")

type Claims struct {
	Issuer    string    `json:"iss"`
	Audience  string    `json:"aud"`
	Subject   uuid.UUID `json:"sub"`
	SessionID uuid.UUID `json:"sid"` // session family
	DeviceID  uuid.UUID `json:"did"`
	IssuedAt  int64     `json:"iat"`
	ExpiresAt int64     `json:"exp"`
	ID        uuid.UUID `json:"jti"`
}

type tokenHeader struct {
	Alg string `json:"alg"`
	Typ string `json:"typ"`
	Kid string `json:"kid"`
}

// TokenSigner signs with the current key and verifies against the current key plus any retired
// keys still inside the access-token lifetime, so keys can rotate without signing everyone out.
type TokenSigner struct {
	kid    string
	key    ed25519.PrivateKey
	public map[string]ed25519.PublicKey
}

// NewTokenSigner takes a 32-byte Ed25519 seed for the signing key and optional retired public keys.
func NewTokenSigner(seed []byte, retired ...ed25519.PublicKey) (*TokenSigner, error) {
	if len(seed) != ed25519.SeedSize {
		return nil, errors.New("signing key seed must be 32 bytes")
	}
	priv := ed25519.NewKeyFromSeed(seed)
	pub := priv.Public().(ed25519.PublicKey)
	s := &TokenSigner{kid: KeyID(pub), key: priv, public: map[string]ed25519.PublicKey{KeyID(pub): pub}}
	for _, k := range retired {
		s.public[KeyID(k)] = k
	}
	return s, nil
}

// KeyID is the first 16 hex characters of the SHA-256 of the public key.
func KeyID(pub ed25519.PublicKey) string {
	sum := sha256.Sum256(pub)
	return hex.EncodeToString(sum[:8])
}

// PublicKey returns the current verification key, for publishing to other services.
func (s *TokenSigner) PublicKey() ed25519.PublicKey { return s.key.Public().(ed25519.PublicKey) }

var b64 = base64.RawURLEncoding

func (s *TokenSigner) Sign(c Claims) (string, error) {
	h, err := json.Marshal(tokenHeader{Alg: "EdDSA", Typ: tokenType, Kid: s.kid})
	if err != nil {
		return "", err
	}
	p, err := json.Marshal(c)
	if err != nil {
		return "", err
	}
	signing := b64.EncodeToString(h) + "." + b64.EncodeToString(p)
	sig := ed25519.Sign(s.key, []byte(signing))
	return signing + "." + b64.EncodeToString(sig), nil
}

// Verify checks the signature, header, issuer, audience and lifetime. It does not check revocation;
// the authenticator does that against the session table.
func (s *TokenSigner) Verify(token string, now time.Time) (Claims, error) {
	var c Claims
	parts := strings.Split(token, ".")
	if len(parts) != 3 || len(token) > 4096 {
		return c, errBadToken
	}
	hb, err := b64.DecodeString(parts[0])
	if err != nil {
		return c, errBadToken
	}
	var h tokenHeader
	if err := strictUnmarshal(hb, &h); err != nil || h.Alg != "EdDSA" || h.Typ != tokenType {
		return c, errBadToken
	}
	pub, ok := s.public[h.Kid]
	if !ok {
		return c, errBadToken
	}
	sig, err := b64.DecodeString(parts[2])
	if err != nil || !ed25519.Verify(pub, []byte(parts[0]+"."+parts[1]), sig) {
		return c, errBadToken
	}
	pb, err := b64.DecodeString(parts[1])
	if err != nil {
		return c, errBadToken
	}
	if err := strictUnmarshal(pb, &c); err != nil {
		return c, errBadToken
	}
	if c.Issuer != tokenIssuer || c.Audience != tokenAudience || c.Subject == uuid.Nil || c.SessionID == uuid.Nil {
		return c, errBadToken
	}
	t := now.Unix()
	if t >= c.ExpiresAt+int64(clockLeeway.Seconds()) || c.IssuedAt > t+int64(clockLeeway.Seconds()) {
		return c, errBadToken
	}
	return c, nil
}

func strictUnmarshal(b []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	return dec.Decode(v)
}
