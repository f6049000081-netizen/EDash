package identity

import (
	"crypto/rand"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func testSigner(t *testing.T) *TokenSigner {
	t.Helper()
	seed := make([]byte, 32)
	_, _ = rand.Read(seed)
	s, err := NewTokenSigner(seed)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func claimsAt(now time.Time) Claims {
	return Claims{Issuer: tokenIssuer, Audience: tokenAudience, Subject: uuid.New(), SessionID: uuid.New(),
		DeviceID: uuid.New(), IssuedAt: now.Unix(), ExpiresAt: now.Add(15 * time.Minute).Unix(), ID: uuid.New()}
}

func TestTokenRoundTrip(t *testing.T) {
	s := testSigner(t)
	now := time.Now()
	c := claimsAt(now)
	tok, err := s.Sign(c)
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.Verify(tok, now)
	if err != nil || got != c {
		t.Fatalf("verify: %v %+v", err, got)
	}
}

func TestTokenRejections(t *testing.T) {
	s := testSigner(t)
	now := time.Now()
	c := claimsAt(now)
	tok, _ := s.Sign(c)
	parts := strings.Split(tok, ".")

	forge := func(header, claims any) string {
		h, _ := json.Marshal(header)
		p, _ := json.Marshal(claims)
		return b64.EncodeToString(h) + "." + b64.EncodeToString(p) + "." + parts[2]
	}
	other := c
	other.Subject = uuid.New()
	otherSigner := testSigner(t)
	foreign, _ := otherSigner.Sign(c)
	wrongAud := c
	wrongAud.Audience = "someone-else"
	wrongAudTok, _ := s.Sign(wrongAud)

	cases := map[string]struct {
		tok string
		at  time.Time
	}{
		"payload swapped":  {forge(tokenHeader{"EdDSA", tokenType, s.kid}, other), now},
		"alg none":         {forge(tokenHeader{"none", tokenType, s.kid}, c), now},
		"alg HS256":        {forge(tokenHeader{"HS256", tokenType, s.kid}, c), now},
		"wrong typ":        {forge(tokenHeader{"EdDSA", "JWT", s.kid}, c), now},
		"unknown kid":      {forge(tokenHeader{"EdDSA", tokenType, "deadbeefdeadbeef"}, c), now},
		"signed by other":  {foreign, now},
		"wrong audience":   {wrongAudTok, now},
		"expired":          {tok, now.Add(16 * time.Minute)},
		"issued in future": {tok, now.Add(-5 * time.Minute)},
		"no signature":     {parts[0] + "." + parts[1] + ".", now},
		"two parts":        {parts[0] + "." + parts[1], now},
		"garbage":          {"not-a-token", now},
	}
	for name, tc := range cases {
		if _, err := s.Verify(tc.tok, tc.at); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestTokenKeyRotation(t *testing.T) {
	old := testSigner(t)
	now := time.Now()
	tok, _ := old.Sign(claimsAt(now))

	seed := make([]byte, 32)
	_, _ = rand.Read(seed)
	rotated, err := NewTokenSigner(seed, old.PublicKey())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := rotated.Verify(tok, now); err != nil {
		t.Errorf("token from retired key rejected: %v", err)
	}
	fresh, _ := NewTokenSigner(seed)
	if _, err := fresh.Verify(tok, now); err == nil {
		t.Error("token from a key that is no longer listed was accepted")
	}
}

func TestWeakPIN(t *testing.T) {
	phone := "+251911482913"
	for _, p := range []string{"000000", "111111", "123456", "234567", "987654", "654321", "121212", "123123", "112233", "482913"} {
		if !weakPIN(p, phone) {
			t.Errorf("%s accepted", p)
		}
	}
	for _, p := range []string{"582047", "730918", "204961", "911482"} {
		if weakPIN(p, phone) {
			t.Errorf("%s rejected", p)
		}
	}
}

func TestIPLimiter(t *testing.T) {
	l := newIPLimiter(3, time.Hour)
	now := time.Now()
	for i := 0; i < 3; i++ {
		if ok, _ := l.allow("1.2.3.4", now); !ok {
			t.Fatalf("hit %d refused", i+1)
		}
	}
	if ok, wait := l.allow("1.2.3.4", now.Add(time.Minute)); ok || wait != 59*time.Minute {
		t.Errorf("4th hit: ok=%v wait=%v", ok, wait)
	}
	if ok, _ := l.allow("5.6.7.8", now); !ok {
		t.Error("other IP refused")
	}
	if ok, _ := l.allow("1.2.3.4", now.Add(time.Hour)); !ok {
		t.Error("new window refused")
	}
}
