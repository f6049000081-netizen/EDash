package identity

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/google/uuid"
)

// ---------------------------------------------------------------------------
// SMS
// ---------------------------------------------------------------------------

// SMSSender delivers a text message. Production uses an aggregator contracted with Ethio Telecom and
// Safaricom Ethiopia, plus an international route for diaspora numbers.
type SMSSender interface {
	Send(ctx context.Context, phoneE164, text string) error
}

// DevSMS writes codes to the server log instead of sending them. main allows it only in APP_ENV=dev.
type DevSMS struct{ Log *slog.Logger }

func (d DevSMS) Send(_ context.Context, phone, text string) error {
	d.Log.Warn("dev sms (not sent)", "to", maskPhone(phone), "text", text)
	return nil
}

func maskPhone(p string) string {
	if len(p) <= 4 {
		return "****"
	}
	return "****" + p[len(p)-4:]
}

// otpText is kept short: Ge'ez script sends as UCS-2, where one SMS segment holds only 70 characters.
// Afaan Oromo and Tigrinya fall back to English until the localisation team supplies reviewed copy.
func otpText(locale, code string) string {
	switch locale {
	case "am":
		return fmt.Sprintf("የEDash ኮድዎ %s ነው። ለማንም አያጋሩ።", code)
	default:
		return fmt.Sprintf("Your EDash code is %s. Never share it with anyone.", code)
	}
}

// ---------------------------------------------------------------------------
// Device attestation
// ---------------------------------------------------------------------------

// Attestation is what a verified device attestation tells us.
type Attestation struct {
	// Fingerprint is stable for one app install on one device; it keys identity.devices.
	Fingerprint string
}

// AttestationVerifier checks a Play Integrity (Android) or App Attest (iOS) token. The challenge id is
// the nonce, so a token captured from one sign-in cannot be replayed into another.
type AttestationVerifier interface {
	Verify(ctx context.Context, platform, token string, nonce uuid.UUID) (Attestation, error)
}

var ErrAttestationFailed = errors.New("device attestation failed")

// DevAttestation accepts any non-empty token and derives the fingerprint from it, so a sandbox client
// should send the same string on every sign-in from the same "device". main allows it only in dev.
type DevAttestation struct{}

func (DevAttestation) Verify(_ context.Context, platform, token string, _ uuid.UUID) (Attestation, error) {
	if token == "" || len(token) > 8192 {
		return Attestation{}, ErrAttestationFailed
	}
	sum := sha256.Sum256([]byte("dev:" + platform + ":" + token))
	return Attestation{Fingerprint: hex.EncodeToString(sum[:])}, nil
}

// ---------------------------------------------------------------------------
// Per-IP limiter for code requests
// ---------------------------------------------------------------------------

// ipLimiter is a fixed-window counter held in memory. It slows SMS pumping (attackers triggering
// codes to premium numbers) from one address. The per-number throttle and country allowlist are the
// main defence; this one must stay generous because mobile carriers put many users behind one
// CGNAT address. With several API instances it should move to Redis.
type ipLimiter struct {
	mu     sync.Mutex
	limit  int
	window time.Duration
	hits   map[string]*ipWindow
}

type ipWindow struct {
	start time.Time
	count int
}

func newIPLimiter(limit int, window time.Duration) *ipLimiter {
	return &ipLimiter{limit: limit, window: window, hits: map[string]*ipWindow{}}
}

// allow records a hit and reports whether it is within the limit, and if not, when to retry.
func (l *ipLimiter) allow(ip string, now time.Time) (bool, time.Duration) {
	if l == nil || l.limit <= 0 || ip == "" {
		return true, 0
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.hits) > 100_000 { // drop expired windows so the map cannot grow without bound
		for k, w := range l.hits {
			if now.Sub(w.start) >= l.window {
				delete(l.hits, k)
			}
		}
	}
	w, ok := l.hits[ip]
	if !ok || now.Sub(w.start) >= l.window {
		w = &ipWindow{start: now}
		l.hits[ip] = w
	}
	if w.count >= l.limit {
		return false, w.start.Add(l.window).Sub(now)
	}
	w.count++
	return true, 0
}
