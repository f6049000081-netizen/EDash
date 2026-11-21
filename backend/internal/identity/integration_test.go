package identity_test

// Integration tests. They need PostgreSQL with db/schema.sql loaded (TEST_DATABASE_URL), and are
// skipped without it. Every test uses fresh phone numbers, so runs don't interfere.

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"net/http/httptest"
	"os"
	"regexp"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"superapp/backend/internal/identity"
	"superapp/backend/internal/platform"
)

var (
	poolOnce sync.Once
	pool     *pgxpool.Pool
	poolErr  error
)

// fakeSMS keeps the last code sent to each number.
type fakeSMS struct {
	mu    sync.Mutex
	codes map[string]string
	sent  int
}

var codeRE = regexp.MustCompile(`[0-9]{6}`)

func (f *fakeSMS) Send(_ context.Context, phone, text string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.codes[phone] = codeRE.FindString(text)
	f.sent++
	return nil
}

func (f *fakeSMS) code(phone string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.codes[phone]
}

type env struct {
	t   *testing.T
	ctx context.Context
	svc *identity.Service
	sms *fakeSMS

	mu    sync.Mutex
	clock time.Time
}

func setup(t *testing.T) *env {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}
	poolOnce.Do(func() { pool, poolErr = pgxpool.New(context.Background(), dsn) })
	if poolErr != nil {
		t.Fatal(poolErr)
	}
	seed, key := make([]byte, 32), make([]byte, 32)
	_, _ = rand.Read(seed)
	_, _ = rand.Read(key)
	tokens, err := identity.NewTokenSigner(seed)
	if err != nil {
		t.Fatal(err)
	}
	sms := &fakeSMS{codes: map[string]string{}}
	cfg := identity.DefaultConfig()
	cfg.OTPPerIPHourly = 0
	svc, err := identity.NewService(pool, tokens, key, sms, identity.DevAttestation{}, cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	e := &env{t: t, ctx: context.Background(), svc: svc, sms: sms, clock: time.Now().UTC()}
	svc.Now = func() time.Time { e.mu.Lock(); defer e.mu.Unlock(); return e.clock }
	return e
}

func (e *env) advance(d time.Duration) { e.mu.Lock(); e.clock = e.clock.Add(d); e.mu.Unlock() }

func newPhone() string {
	n, _ := rand.Int(rand.Reader, big.NewInt(100_000_000))
	return fmt.Sprintf("+2519%08d", n.Int64())
}

func device(name string) identity.DeviceInfo {
	return identity.DeviceInfo{Platform: "android", Model: "Tecno Spark", Attestation: name}
}

// signIn runs request + verify and returns the session.
func (e *env) signIn(phone, purpose, dev string) *identity.Session {
	e.t.Helper()
	ch, err := e.svc.RequestOTP(e.ctx, identity.OTPRequest{Phone: phone, Purpose: purpose})
	if err != nil {
		e.t.Fatalf("request %s: %v", purpose, err)
	}
	s, err := e.svc.VerifyOTP(e.ctx, ch.ID, e.sms.code(phone), device(dev))
	if err != nil {
		e.t.Fatalf("verify %s: %v", purpose, err)
	}
	e.advance(31 * time.Second) // past the resend throttle for the next code
	return s
}

func (e *env) auth(access string) (platform.Principal, error) {
	r := httptest.NewRequest("GET", "/v1/me", nil)
	r.Header.Set("Authorization", "Bearer "+access)
	return e.svc.Authenticate(r)
}

func (e *env) principal(s *identity.Session) platform.Principal {
	e.t.Helper()
	p, err := e.auth(s.AccessToken)
	if err != nil {
		e.t.Fatalf("authenticate: %v", err)
	}
	return p
}

// ---------------------------------------------------------------------------

func TestSignUpSetPINAndAuthenticate(t *testing.T) {
	e := setup(t)
	phone := newPhone()

	ch, err := e.svc.RequestOTP(e.ctx, identity.OTPRequest{Phone: phone, Purpose: "signup", Locale: "am"})
	if err != nil {
		t.Fatal(err)
	}
	code := e.sms.code(phone)
	if code == "" {
		t.Fatal("no SMS sent")
	}
	s, err := e.svc.VerifyOTP(e.ctx, ch.ID, code, device("phone-1"))
	if err != nil {
		t.Fatal(err)
	}
	if !s.NeedsPIN || s.ExpiresIn != 900 {
		t.Errorf("session: needs_pin=%v expires_in=%d", s.NeedsPIN, s.ExpiresIn)
	}
	if _, err := e.svc.VerifyOTP(e.ctx, ch.ID, code, device("phone-1")); !errors.Is(err, identity.ErrOTPExpired) {
		t.Errorf("reusing a code: %v", err)
	}
	e.advance(31 * time.Second)

	p := e.principal(s)
	if p.UserID != s.UserID {
		t.Fatalf("principal %v, session user %v", p.UserID, s.UserID)
	}
	me, _ := e.svc.Me(e.ctx, p.UserID)
	if me.Status != "pending" || me.HasPIN || me.Phone != phone {
		t.Errorf("before PIN: %+v", me)
	}
	if err := e.svc.SetPIN(e.ctx, p, "", "123456"); err == nil {
		t.Error("sequential PIN accepted")
	}
	if err := e.svc.SetPIN(e.ctx, p, "", "582047"); err != nil {
		t.Fatal(err)
	}
	me, _ = e.svc.Me(e.ctx, p.UserID)
	if me.Status != "active" || !me.HasPIN {
		t.Errorf("after PIN: %+v", me)
	}

	// Signing in again later finds the same user and doesn't ask for a PIN.
	again := e.signIn(phone, "login", "phone-1")
	if again.UserID != s.UserID || again.NeedsPIN {
		t.Errorf("second sign-in: user %v needs_pin %v", again.UserID, again.NeedsPIN)
	}
}

func TestWrongCodesKillTheChallenge(t *testing.T) {
	e := setup(t)
	phone := newPhone()
	ch, _ := e.svc.RequestOTP(e.ctx, identity.OTPRequest{Phone: phone, Purpose: "login"})
	right := e.sms.code(phone)
	wrong := "000000"
	if right == wrong {
		wrong = "000001"
	}
	for want := 4; want >= 1; want-- {
		_, err := e.svc.VerifyOTP(e.ctx, ch.ID, wrong, device("d"))
		var bad *identity.OTPInvalidError
		if !errors.As(err, &bad) || bad.AttemptsLeft != want {
			t.Fatalf("wrong code: %v (want %d left)", err, want)
		}
	}
	if _, err := e.svc.VerifyOTP(e.ctx, ch.ID, wrong, device("d")); !errors.Is(err, identity.ErrOTPExpired) {
		t.Errorf("5th wrong code: %v", err)
	}
	if _, err := e.svc.VerifyOTP(e.ctx, ch.ID, right, device("d")); !errors.Is(err, identity.ErrOTPExpired) {
		t.Errorf("right code after lockout: %v", err)
	}
}

func TestCodeExpires(t *testing.T) {
	e := setup(t)
	phone := newPhone()
	ch, _ := e.svc.RequestOTP(e.ctx, identity.OTPRequest{Phone: phone, Purpose: "login"})
	e.advance(5*time.Minute + time.Second)
	if _, err := e.svc.VerifyOTP(e.ctx, ch.ID, e.sms.code(phone), device("d")); !errors.Is(err, identity.ErrOTPExpired) {
		t.Errorf("expired code: %v", err)
	}
}

func TestResendThrottle(t *testing.T) {
	e := setup(t)
	phone := newPhone()
	req := identity.OTPRequest{Phone: phone, Purpose: "login"}
	if _, err := e.svc.RequestOTP(e.ctx, req); err != nil {
		t.Fatal(err)
	}
	var rl *identity.RateLimitedError
	if _, err := e.svc.RequestOTP(e.ctx, req); !errors.As(err, &rl) || rl.RetryAfter <= 0 || rl.RetryAfter > 30*time.Second {
		t.Fatalf("immediate resend: %v", err)
	}
	for i := 2; i <= 5; i++ {
		e.advance(31 * time.Second)
		if _, err := e.svc.RequestOTP(e.ctx, req); err != nil {
			t.Fatalf("code %d: %v", i, err)
		}
	}
	e.advance(31 * time.Second)
	if _, err := e.svc.RequestOTP(e.ctx, req); !errors.As(err, &rl) || rl.RetryAfter < 50*time.Minute {
		t.Fatalf("6th code in an hour: %v", err)
	}
	e.advance(time.Hour)
	if _, err := e.svc.RequestOTP(e.ctx, req); err != nil {
		t.Fatalf("after an hour: %v", err)
	}
}

func TestConcurrentRequestsCannotBeatThrottle(t *testing.T) {
	e := setup(t)
	phone := newPhone()
	var wg sync.WaitGroup
	var mu sync.Mutex
	ok := 0
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := e.svc.RequestOTP(e.ctx, identity.OTPRequest{Phone: phone, Purpose: "login"}); err == nil {
				mu.Lock()
				ok++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if ok != 1 {
		t.Errorf("%d of 10 concurrent requests got a code, want 1", ok)
	}
}

func TestUnknownNumberResetSendsNothing(t *testing.T) {
	e := setup(t)
	phone := newPhone()
	ch, err := e.svc.RequestOTP(e.ctx, identity.OTPRequest{Phone: phone, Purpose: "pin_reset"})
	if err != nil || ch.ID == uuid.Nil {
		t.Fatalf("response should look normal: %v", err)
	}
	if e.sms.code(phone) != "" {
		t.Error("sent a reset code to an unregistered number")
	}
}

func TestPhoneValidation(t *testing.T) {
	e := setup(t)
	var v *identity.ValidationError
	for _, p := range []string{"0911234567", "+25191234", "+251811234567", "+8613800138000"} {
		if _, err := e.svc.RequestOTP(e.ctx, identity.OTPRequest{Phone: p, Purpose: "login"}); !errors.As(err, &v) {
			t.Errorf("%s: %v", p, err)
		}
	}
	uk := "+447700" + newPhone()[6:] // fresh UK number each run, so the resend throttle never applies
	if _, err := e.svc.RequestOTP(e.ctx, identity.OTPRequest{Phone: uk, Purpose: "login"}); err != nil {
		t.Errorf("UK diaspora number refused: %v", err)
	}
}

func TestConcurrentVerifyIssuesOneSession(t *testing.T) {
	e := setup(t)
	phone := newPhone()
	ch, _ := e.svc.RequestOTP(e.ctx, identity.OTPRequest{Phone: phone, Purpose: "signup"})
	code := e.sms.code(phone)
	var wg sync.WaitGroup
	var mu sync.Mutex
	wins := 0
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := e.svc.VerifyOTP(e.ctx, ch.ID, code, device("d")); err == nil {
				mu.Lock()
				wins++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if wins != 1 {
		t.Errorf("%d sessions from one code, want 1", wins)
	}
}

func TestRefreshRotationAndReuseDetection(t *testing.T) {
	e := setup(t)
	s1 := e.signIn(newPhone(), "signup", "d")

	s2, err := e.svc.Refresh(e.ctx, s1.RefreshToken)
	if err != nil {
		t.Fatal(err)
	}
	if s2.RefreshToken == s1.RefreshToken || s2.UserID != s1.UserID {
		t.Fatal("refresh did not rotate")
	}
	e.principal(s2)

	// The old refresh token turns up again: someone copied it. Everything in the family dies.
	if _, err := e.svc.Refresh(e.ctx, s1.RefreshToken); !errors.Is(err, identity.ErrSessionInvalid) {
		t.Fatalf("reused token: %v", err)
	}
	if _, err := e.auth(s2.AccessToken); err == nil {
		t.Error("access token still valid after reuse detection")
	}
	if _, err := e.svc.Refresh(e.ctx, s2.RefreshToken); !errors.Is(err, identity.ErrSessionInvalid) {
		t.Error("newest refresh token still valid after reuse detection")
	}
	for _, bad := range []string{"", "rt_nope", "garbage"} {
		if _, err := e.svc.Refresh(e.ctx, bad); !errors.Is(err, identity.ErrSessionInvalid) {
			t.Errorf("refresh %q: %v", bad, err)
		}
	}
}

func TestLogoutEndsOnlyThatSession(t *testing.T) {
	e := setup(t)
	phone := newPhone()
	a := e.signIn(phone, "signup", "phone")
	b := e.signIn(phone, "login", "tablet")
	if err := e.svc.Logout(e.ctx, e.principal(a)); err != nil {
		t.Fatal(err)
	}
	if _, err := e.auth(a.AccessToken); err == nil {
		t.Error("access token works after logout")
	}
	if _, err := e.svc.Refresh(e.ctx, a.RefreshToken); err == nil {
		t.Error("refresh works after logout")
	}
	e.principal(b) // the other device is unaffected
}

func TestPINChangeAndReset(t *testing.T) {
	e := setup(t)
	phone := newPhone()
	first := e.signIn(phone, "signup", "phone")
	p := e.principal(first)
	if err := e.svc.SetPIN(e.ctx, p, "", "582047"); err != nil {
		t.Fatal(err)
	}
	tablet := e.signIn(phone, "login", "tablet")

	if err := e.svc.SetPIN(e.ctx, p, "", "730918"); !errors.Is(err, identity.ErrCurrentPINRequired) {
		t.Errorf("change without current PIN: %v", err)
	}
	if err := e.svc.SetPIN(e.ctx, p, "999999", "730918"); !errors.Is(err, platform.ErrInvalidPIN) {
		t.Errorf("change with wrong current PIN: %v", err)
	}
	if err := e.svc.SetPIN(e.ctx, p, "582047", "730918"); err != nil {
		t.Fatalf("change: %v", err)
	}

	// Forgot the PIN and locked it out.
	for i := 0; i < platform.MaxPINFailures; i++ {
		_ = e.svc.PIN.Verify(e.ctx, p.UserID, "111111")
	}
	if err := e.svc.PIN.Verify(e.ctx, p.UserID, "730918"); !errors.Is(err, platform.ErrPINLocked) {
		t.Fatalf("expected lockout: %v", err)
	}

	reset := e.signIn(phone, "pin_reset", "phone")
	rp := e.principal(reset)
	if err := e.svc.SetPIN(e.ctx, rp, "", "204961"); err != nil {
		t.Fatalf("reset: %v", err)
	}
	if err := e.svc.PIN.Verify(e.ctx, rp.UserID, "204961"); err != nil {
		t.Errorf("new PIN after reset: %v", err)
	}
	var resetAt *time.Time
	_ = pool.QueryRow(e.ctx, `SELECT reset_at FROM identity.credentials WHERE user_id = $1`, rp.UserID).Scan(&resetAt)
	if resetAt == nil {
		t.Error("reset_at not stamped")
	}
	// The reset signs out every other session, including the tablet.
	if _, err := e.auth(tablet.AccessToken); err == nil {
		t.Error("other device still signed in after reset")
	}
	if _, err := e.auth(first.AccessToken); err == nil {
		t.Error("old session on the same phone still signed in after reset")
	}
	// The reset allowance is single use.
	if err := e.svc.SetPIN(e.ctx, rp, "", "917305"); !errors.Is(err, identity.ErrCurrentPINRequired) {
		t.Errorf("second reset without PIN: %v", err)
	}
}

func TestResetAllowanceExpires(t *testing.T) {
	e := setup(t)
	phone := newPhone()
	s := e.signIn(phone, "signup", "phone")
	if err := e.svc.SetPIN(e.ctx, e.principal(s), "", "582047"); err != nil {
		t.Fatal(err)
	}
	reset := e.signIn(phone, "pin_reset", "phone")
	rp := e.principal(reset)
	e.advance(16 * time.Minute)
	if err := e.svc.SetPIN(e.ctx, rp, "", "204961"); !errors.Is(err, identity.ErrCurrentPINRequired) {
		t.Errorf("reset after the window: %v", err)
	}
}
