// Package identity signs users in with a one-time SMS code, binds the device, issues tokens and
// manages the payment PIN.
//
// Sign-in flow:
//  1. RequestOTP: send a 6-digit code. The answer is the same whether or not the number is registered.
//  2. VerifyOTP: check the code, create the user on first sign-in, register the device and start a
//     session. A session is a family of refresh tokens; each refresh rotates the token, and presenting
//     an already-rotated token revokes the whole family (it means the token was copied).
//  3. Access tokens live 15 minutes. Every request checks the session family is still live, so logout,
//     PIN reset and device removal take effect immediately rather than when the token expires.
package identity

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"net/http"
	"regexp"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"superapp/backend/internal/platform"
)

type Config struct {
	OTPTTL          time.Duration // how long a code is valid
	OTPMaxAttempts  int           // wrong guesses before the code is dead
	OTPResendAfter  time.Duration // minimum gap between codes to one number
	OTPHourlyMax    int           // codes per number per hour
	OTPPerIPHourly  int           // code requests per client IP per hour; 0 disables
	AllowedPrefixes []string      // E.164 prefixes we send codes to
	AccessTTL       time.Duration
	RefreshTTL      time.Duration // idle lifetime; each refresh extends it
	PINResetWindow  time.Duration // how long a pin_reset session may set a new PIN without the old one
}

// DefaultPrefixes covers Ethiopia and the largest diaspora countries. It is a starting point for the
// fraud team to review: +1 also reaches Caribbean numbers that are common SMS-pumping targets.
var DefaultPrefixes = []string{
	"+251",                                                                     // Ethiopia
	"+1", "+44", "+49", "+46", "+47", "+31", "+39", "+33", "+32", "+41", "+45", // North America, Europe
	"+61", "+971", "+966", "+972", "+974", "+965", "+254", "+27", // Australia, Gulf, Israel, Kenya, South Africa
}

func DefaultConfig() Config {
	return Config{
		OTPTTL:          5 * time.Minute,
		OTPMaxAttempts:  5,
		OTPResendAfter:  30 * time.Second,
		OTPHourlyMax:    5,
		OTPPerIPHourly:  60,
		AllowedPrefixes: DefaultPrefixes,
		AccessTTL:       15 * time.Minute,
		RefreshTTL:      30 * 24 * time.Hour,
		PINResetWindow:  15 * time.Minute,
	}
}

type Service struct {
	Pool   *pgxpool.Pool
	Tokens *TokenSigner
	SMS    SMSSender
	Attest AttestationVerifier
	PIN    *platform.PINVerifier
	Cfg    Config
	Log    *slog.Logger
	Now    func() time.Time

	otpKey []byte
	ips    *ipLimiter
}

// NewService needs a secret of at least 32 bytes for hashing codes. Codes have only a million
// values, so a plain hash would fall to brute force if the table leaked; an HMAC under a key kept
// outside the database does not.
func NewService(pool *pgxpool.Pool, tokens *TokenSigner, otpKey []byte, sms SMSSender, attest AttestationVerifier, cfg Config, log *slog.Logger) (*Service, error) {
	if len(otpKey) < 32 {
		return nil, errors.New("OTP HMAC key must be at least 32 bytes")
	}
	return &Service{
		Pool: pool, Tokens: tokens, SMS: sms, Attest: attest, Cfg: cfg, Log: log,
		PIN:    &platform.PINVerifier{Pool: pool},
		otpKey: otpKey,
		ips:    newIPLimiter(cfg.OTPPerIPHourly, time.Hour),
	}, nil
}

// ---------------------------------------------------------------------------
// errors
// ---------------------------------------------------------------------------

type ValidationError struct{ Msg string }

func (e *ValidationError) Error() string { return e.Msg }

type RateLimitedError struct{ RetryAfter time.Duration }

func (e *RateLimitedError) Error() string {
	return fmt.Sprintf("rate limited, retry in %s", e.RetryAfter)
}

type OTPInvalidError struct{ AttemptsLeft int }

func (e *OTPInvalidError) Error() string {
	return fmt.Sprintf("wrong code, %d attempts left", e.AttemptsLeft)
}

var (
	ErrOTPExpired         = errors.New("code expired or used; request a new one")
	ErrSessionInvalid     = errors.New("session expired or revoked; sign in again")
	ErrAccountClosed      = errors.New("account closed")
	ErrCurrentPINRequired = errors.New("current PIN required")
	ErrSMSUnavailable     = errors.New("couldn't send the code")
)

// ---------------------------------------------------------------------------
// one-time codes
// ---------------------------------------------------------------------------

var (
	phoneRE   = regexp.MustCompile(`^\+[1-9][0-9]{7,14}$`)
	etPhoneRE = regexp.MustCompile(`^\+251[79][0-9]{8}$`) // Ethio Telecom 9x, Safaricom Ethiopia 7x
	codeRE    = regexp.MustCompile(`^[0-9]{6}$`)
	pinRE     = codeRE
)

type OTPRequest struct {
	Phone    string
	Purpose  string // signup, login, pin_reset, device_bind
	Locale   string
	ClientIP string
}

type OTPChallenge struct {
	ID          uuid.UUID
	ExpiresAt   time.Time
	ResendAfter time.Duration
}

func (s *Service) RequestOTP(ctx context.Context, req OTPRequest) (*OTPChallenge, error) {
	switch req.Purpose {
	case "signup", "login", "pin_reset", "device_bind":
	default:
		return nil, &ValidationError{"purpose must be signup, login, pin_reset or device_bind"}
	}
	if !phoneRE.MatchString(req.Phone) || (strings.HasPrefix(req.Phone, "+251") && !etPhoneRE.MatchString(req.Phone)) {
		return nil, &ValidationError{"Enter the number in international format, like +251911234567."}
	}
	if !s.prefixAllowed(req.Phone) {
		return nil, &ValidationError{"We can't send codes to numbers in this country yet."}
	}
	switch req.Locale {
	case "":
		req.Locale = "am"
	case "am", "om", "ti", "en":
	default:
		return nil, &ValidationError{"locale must be am, om, ti or en"}
	}
	now := s.now()
	if ok, wait := s.ips.allow(req.ClientIP, now); !ok {
		return nil, &RateLimitedError{wait}
	}

	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after Commit

	// Serialise requests for one number so concurrent calls can't slip past the throttle together.
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, req.Phone); err != nil {
		return nil, err
	}
	var latest, oldest *time.Time
	var recent int
	if err := tx.QueryRow(ctx, `
		SELECT max(created_at), min(created_at), count(*) FROM identity.otp_challenges
		WHERE phone_e164 = $1 AND created_at > $2`, req.Phone, now.Add(-time.Hour)).Scan(&latest, &oldest, &recent); err != nil {
		return nil, err
	}
	if latest != nil && now.Sub(*latest) < s.Cfg.OTPResendAfter {
		return nil, &RateLimitedError{s.Cfg.OTPResendAfter - now.Sub(*latest)}
	}
	if recent >= s.Cfg.OTPHourlyMax {
		return nil, &RateLimitedError{oldest.Add(time.Hour).Sub(now)}
	}

	// signup and login are one flow: an unknown number becomes a new account on verify. pin_reset and
	// device_bind need an existing account; for unknown numbers we store a challenge (so the response
	// looks identical) but send nothing, so the code can never be entered.
	var exists bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM identity.users WHERE phone_e164 = $1)`, req.Phone).Scan(&exists); err != nil {
		return nil, err
	}
	send := req.Purpose == "signup" || req.Purpose == "login" || exists

	code, err := newCode()
	if err != nil {
		return nil, err
	}
	id, err := uuid.NewV7()
	if err != nil {
		return nil, err
	}
	ch := &OTPChallenge{ID: id, ExpiresAt: now.Add(s.Cfg.OTPTTL), ResendAfter: s.Cfg.OTPResendAfter}
	if _, err := tx.Exec(ctx, `
		INSERT INTO identity.otp_challenges (id, phone_e164, purpose, code_hash, expires_at, created_at)
		VALUES ($1, $2, $3, $4, $5, $6)`, id, req.Phone, req.Purpose, s.codeHash(id, code), ch.ExpiresAt, now); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}

	if send {
		if err := s.SMS.Send(ctx, req.Phone, otpText(req.Locale, code)); err != nil {
			s.Log.Error("sms send failed", "err", err)
			return nil, ErrSMSUnavailable
		}
	}
	return ch, nil
}

type DeviceInfo struct {
	Platform           string
	Model              string
	Attestation        string
	BiometricPublicKey string
}

type Session struct {
	AccessToken  string
	RefreshToken string
	ExpiresIn    int
	UserID       uuid.UUID
	NeedsPIN     bool
}

func (s *Service) VerifyOTP(ctx context.Context, challengeID uuid.UUID, code string, dev DeviceInfo) (*Session, error) {
	if !codeRE.MatchString(code) {
		return nil, &ValidationError{"The code is 6 digits."}
	}
	switch dev.Platform {
	case "android", "ios", "web":
	default:
		return nil, &ValidationError{"device.platform must be android, ios or web"}
	}
	if len(dev.Model) > 100 || len(dev.BiometricPublicKey) > 1024 {
		return nil, &ValidationError{"device details are too long"}
	}
	att, err := s.Attest.Verify(ctx, dev.Platform, dev.Attestation, challengeID)
	if err != nil {
		return nil, ErrAttestationFailed
	}

	now := s.now()
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after Commit

	var phone, purpose, hash string
	var attempts int
	var expires time.Time
	var consumed *time.Time
	err = tx.QueryRow(ctx, `
		SELECT phone_e164, purpose, code_hash, attempts, expires_at, consumed_at
		FROM identity.otp_challenges WHERE id = $1 FOR UPDATE`, challengeID).
		Scan(&phone, &purpose, &hash, &attempts, &expires, &consumed)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrOTPExpired
	}
	if err != nil {
		return nil, err
	}
	if consumed != nil || !now.Before(expires) || attempts >= s.Cfg.OTPMaxAttempts {
		return nil, ErrOTPExpired
	}
	if !hmac.Equal([]byte(s.codeHash(challengeID, code)), []byte(hash)) {
		attempts++
		if _, err := tx.Exec(ctx, `UPDATE identity.otp_challenges SET attempts = $2 WHERE id = $1`, challengeID, attempts); err != nil {
			return nil, err
		}
		if err := tx.Commit(ctx); err != nil {
			return nil, err
		}
		if left := s.Cfg.OTPMaxAttempts - attempts; left > 0 {
			return nil, &OTPInvalidError{AttemptsLeft: left}
		}
		return nil, ErrOTPExpired
	}
	if _, err := tx.Exec(ctx, `UPDATE identity.otp_challenges SET consumed_at = $2 WHERE id = $1`, challengeID, now); err != nil {
		return nil, err
	}

	if purpose == "signup" || purpose == "login" {
		newID, err := uuid.NewV7()
		if err != nil {
			return nil, err
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO identity.users (id, phone_e164, created_at, updated_at) VALUES ($1, $2, $3, $3)
			ON CONFLICT (phone_e164) DO NOTHING`, newID, phone, now); err != nil {
			return nil, err
		}
	}
	var userID uuid.UUID
	var status string
	err = tx.QueryRow(ctx, `SELECT id, status FROM identity.users WHERE phone_e164 = $1`, phone).Scan(&userID, &status)
	if errors.Is(err, pgx.ErrNoRows) {
		// pin_reset or device_bind for an unknown number: no code was ever sent.
		return nil, ErrOTPExpired
	}
	if err != nil {
		return nil, err
	}
	if status == "closed" {
		if err := tx.Commit(ctx); err != nil { // keep the code consumed
			return nil, err
		}
		return nil, ErrAccountClosed
	}

	deviceRowID, err := uuid.NewV7()
	if err != nil {
		return nil, err
	}
	var deviceID uuid.UUID
	var newDevice bool
	if err := tx.QueryRow(ctx, `
		INSERT INTO identity.devices (id, user_id, fingerprint_hash, platform, model, biometric_key_pub, first_seen_at, last_seen_at)
		VALUES ($1, $2, $3, $4, NULLIF($5, ''), NULLIF($6, ''), $7, $7)
		ON CONFLICT (user_id, fingerprint_hash) DO UPDATE SET
			platform = EXCLUDED.platform,
			model = COALESCE(EXCLUDED.model, identity.devices.model),
			biometric_key_pub = COALESCE(EXCLUDED.biometric_key_pub, identity.devices.biometric_key_pub),
			last_seen_at = EXCLUDED.last_seen_at,
			revoked_at = NULL
		RETURNING id, (xmax = 0)`,
		deviceRowID, userID, att.Fingerprint, dev.Platform, dev.Model, dev.BiometricPublicKey, now).Scan(&deviceID, &newDevice); err != nil {
		return nil, err
	}

	var resetUntil *time.Time
	if purpose == "pin_reset" {
		t := now.Add(s.Cfg.PINResetWindow)
		resetUntil = &t
	}
	family, err := uuid.NewV7()
	if err != nil {
		return nil, err
	}
	refresh, err := s.insertSession(ctx, tx, userID, deviceID, family, resetUntil, now)
	if err != nil {
		return nil, err
	}
	var hasPIN bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM identity.credentials WHERE user_id = $1)`, userID).Scan(&hasPIN); err != nil {
		return nil, err
	}
	// Notifications pick this up to warn the user about sign-ins from new devices.
	if err := platform.WriteOutbox(ctx, tx, "identity.signed_in", userID, map[string]any{
		"user_id": userID, "device_id": deviceID, "new_device": newDevice, "platform": dev.Platform, "purpose": purpose,
	}); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return s.session(userID, family, deviceID, refresh, !hasPIN, now)
}

func (s *Service) prefixAllowed(phone string) bool {
	for _, p := range s.Cfg.AllowedPrefixes {
		if strings.HasPrefix(phone, p) {
			return true
		}
	}
	return false
}

func newCode() (string, error) {
	n, err := rand.Int(rand.Reader, big.NewInt(1_000_000))
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%06d", n.Int64()), nil
}

func (s *Service) codeHash(id uuid.UUID, code string) string {
	m := hmac.New(sha256.New, s.otpKey)
	m.Write(id[:])
	m.Write([]byte(code))
	return hex.EncodeToString(m.Sum(nil))
}

// ---------------------------------------------------------------------------
// sessions
// ---------------------------------------------------------------------------

func (s *Service) insertSession(ctx context.Context, tx pgx.Tx, user, device, family uuid.UUID, resetUntil *time.Time, now time.Time) (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	token := "rt_" + b64.EncodeToString(raw)
	id, err := uuid.NewV7()
	if err != nil {
		return "", err
	}
	_, err = tx.Exec(ctx, `
		INSERT INTO identity.sessions (id, user_id, device_id, refresh_token_hash, family_id, created_at, expires_at, pin_reset_until)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
		id, user, device, hashRefresh(token), family, now, now.Add(s.Cfg.RefreshTTL), resetUntil)
	return token, err
}

// Refresh tokens carry 256 random bits, so a plain SHA-256 is enough to make a leaked table useless.
func hashRefresh(t string) string {
	sum := sha256.Sum256([]byte(t))
	return hex.EncodeToString(sum[:])
}

func (s *Service) session(user, family, device uuid.UUID, refresh string, needsPIN bool, now time.Time) (*Session, error) {
	jti, err := uuid.NewV7()
	if err != nil {
		return nil, err
	}
	access, err := s.Tokens.Sign(Claims{
		Issuer: tokenIssuer, Audience: tokenAudience, Subject: user, SessionID: family, DeviceID: device,
		IssuedAt: now.Unix(), ExpiresAt: now.Add(s.Cfg.AccessTTL).Unix(), ID: jti,
	})
	if err != nil {
		return nil, err
	}
	return &Session{AccessToken: access, RefreshToken: refresh, ExpiresIn: int(s.Cfg.AccessTTL.Seconds()), UserID: user, NeedsPIN: needsPIN}, nil
}

// Refresh rotates a refresh token. Presenting a token that was already rotated means two parties hold
// it, so the whole family is revoked and the user must sign in again with a code.
func (s *Service) Refresh(ctx context.Context, token string) (*Session, error) {
	if !strings.HasPrefix(token, "rt_") || len(token) > 128 {
		return nil, ErrSessionInvalid
	}
	now := s.now()
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after Commit

	var id, user, device, family uuid.UUID
	var expires time.Time
	var revoked, resetUntil *time.Time
	err = tx.QueryRow(ctx, `
		SELECT id, user_id, device_id, family_id, expires_at, revoked_at, pin_reset_until
		FROM identity.sessions WHERE refresh_token_hash = $1 FOR UPDATE`, hashRefresh(token)).
		Scan(&id, &user, &device, &family, &expires, &revoked, &resetUntil)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrSessionInvalid
	}
	if err != nil {
		return nil, err
	}
	if revoked != nil {
		n, err := s.revokeFamily(ctx, tx, family, now)
		if err != nil {
			return nil, err
		}
		if n > 0 {
			if err := platform.WriteOutbox(ctx, tx, "identity.refresh_token_reused", user, map[string]any{
				"user_id": user, "device_id": device, "session_id": family,
			}); err != nil {
				return nil, err
			}
		}
		if err := tx.Commit(ctx); err != nil {
			return nil, err
		}
		return nil, ErrSessionInvalid
	}
	if !now.Before(expires) {
		return nil, ErrSessionInvalid
	}
	var status string
	var deviceRevoked *time.Time
	if err := tx.QueryRow(ctx, `
		SELECT u.status, d.revoked_at FROM identity.users u JOIN identity.devices d ON d.user_id = u.id
		WHERE u.id = $1 AND d.id = $2`, user, device).Scan(&status, &deviceRevoked); err != nil {
		return nil, err
	}
	if status == "closed" || deviceRevoked != nil {
		if _, err := s.revokeFamily(ctx, tx, family, now); err != nil {
			return nil, err
		}
		if err := tx.Commit(ctx); err != nil {
			return nil, err
		}
		return nil, ErrSessionInvalid
	}

	if _, err := tx.Exec(ctx, `UPDATE identity.sessions SET revoked_at = $2 WHERE id = $1`, id, now); err != nil {
		return nil, err
	}
	if _, err := tx.Exec(ctx, `UPDATE identity.devices SET last_seen_at = $2 WHERE id = $1`, device, now); err != nil {
		return nil, err
	}
	refresh, err := s.insertSession(ctx, tx, user, device, family, resetUntil, now)
	if err != nil {
		return nil, err
	}
	var hasPIN bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM identity.credentials WHERE user_id = $1)`, user).Scan(&hasPIN); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return s.session(user, family, device, refresh, !hasPIN, now)
}

func (s *Service) revokeFamily(ctx context.Context, db platform.DB, family uuid.UUID, now time.Time) (int64, error) {
	tag, err := db.Exec(ctx, `UPDATE identity.sessions SET revoked_at = $2 WHERE family_id = $1 AND revoked_at IS NULL`, family, now)
	return tag.RowsAffected(), err
}

// Logout ends the caller's session family on this device.
func (s *Service) Logout(ctx context.Context, p platform.Principal) error {
	_, err := s.Pool.Exec(ctx, `
		UPDATE identity.sessions SET revoked_at = $3
		WHERE family_id = $1 AND user_id = $2 AND revoked_at IS NULL`, p.SessionID, p.UserID, s.now())
	return err
}

// Authenticate implements platform.Authenticator for "Authorization: Bearer <access token>".
func (s *Service) Authenticate(r *http.Request) (platform.Principal, error) {
	tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok {
		return platform.Principal{}, platform.ErrUnauthenticated
	}
	now := s.now()
	c, err := s.Tokens.Verify(strings.TrimSpace(tok), now)
	if err != nil {
		return platform.Principal{}, platform.ErrUnauthenticated
	}
	var live bool
	if err := s.Pool.QueryRow(r.Context(), `
		SELECT EXISTS (
			SELECT 1 FROM identity.sessions s
			JOIN identity.devices d ON d.id = s.device_id
			JOIN identity.users u ON u.id = s.user_id
			WHERE s.family_id = $1 AND s.user_id = $2 AND s.device_id = $3
			  AND s.revoked_at IS NULL AND s.expires_at > $4
			  AND d.revoked_at IS NULL AND u.status <> 'closed')`,
		c.SessionID, c.Subject, c.DeviceID, now).Scan(&live); err != nil {
		return platform.Principal{}, err
	}
	if !live {
		return platform.Principal{}, platform.ErrUnauthenticated
	}
	return platform.Principal{UserID: c.Subject, SessionID: c.SessionID, DeviceID: c.DeviceID}, nil
}

// ---------------------------------------------------------------------------
// PIN
// ---------------------------------------------------------------------------

// SetPIN sets the first PIN, changes it (with the current PIN), or resets it from a session that
// came from a pin_reset code. A reset clears the lockout, signs out every other session, and
// stamps reset_at so payments are held for a cooling-off period (protection against SIM swap).
func (s *Service) SetPIN(ctx context.Context, p platform.Principal, current, newPIN string) error {
	if !pinRE.MatchString(newPIN) || (current != "" && !pinRE.MatchString(current)) {
		return &ValidationError{"Your PIN is 6 digits."}
	}
	var phone string
	if err := s.Pool.QueryRow(ctx, `SELECT phone_e164 FROM identity.users WHERE id = $1`, p.UserID).Scan(&phone); err != nil {
		return err
	}
	if weakPIN(newPIN, phone) {
		return &ValidationError{"Pick a PIN that's harder to guess. Avoid repeated or sequential digits and the end of your phone number."}
	}
	hash, err := platform.HashPIN(newPIN)
	if err != nil {
		return err
	}
	now := s.now()

	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after Commit

	var one int
	err = tx.QueryRow(ctx, `SELECT 1 FROM identity.credentials WHERE user_id = $1 FOR UPDATE`, p.UserID).Scan(&one)
	switch {
	case errors.Is(err, pgx.ErrNoRows): // first PIN
		tag, err := tx.Exec(ctx, `
			INSERT INTO identity.credentials (user_id, pin_hash, updated_at) VALUES ($1, $2, $3)
			ON CONFLICT (user_id) DO NOTHING`, p.UserID, hash, now)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 { // set by a concurrent request
			return ErrCurrentPINRequired
		}
		if _, err := tx.Exec(ctx, `UPDATE identity.users SET status = 'active', updated_at = $2 WHERE id = $1 AND status = 'pending'`, p.UserID, now); err != nil {
			return err
		}
		if err := platform.WriteOutbox(ctx, tx, "identity.pin_set", p.UserID, map[string]any{"user_id": p.UserID, "device_id": p.DeviceID}); err != nil {
			return err
		}
		return tx.Commit(ctx)
	case err != nil:
		return err
	}

	// A pin_reset session may replace the PIN once without the old one.
	tag, err := tx.Exec(ctx, `
		UPDATE identity.sessions SET pin_reset_until = NULL
		WHERE family_id = $1 AND user_id = $2 AND revoked_at IS NULL AND pin_reset_until > $3`, p.SessionID, p.UserID, now)
	if err != nil {
		return err
	}
	if tag.RowsAffected() > 0 {
		if _, err := tx.Exec(ctx, `
			UPDATE identity.credentials SET pin_hash = $2, pin_failures = 0, locked_until = NULL, reset_at = $3, updated_at = $3
			WHERE user_id = $1`, p.UserID, hash, now); err != nil {
			return err
		}
		others, err := tx.Exec(ctx, `
			UPDATE identity.sessions SET revoked_at = $3
			WHERE user_id = $1 AND family_id <> $2 AND revoked_at IS NULL`, p.UserID, p.SessionID, now)
		if err != nil {
			return err
		}
		if err := platform.WriteOutbox(ctx, tx, "identity.pin_reset", p.UserID, map[string]any{
			"user_id": p.UserID, "device_id": p.DeviceID, "sessions_revoked": others.RowsAffected(),
		}); err != nil {
			return err
		}
		return tx.Commit(ctx)
	}

	// Ordinary change: release the row lock, then check the current PIN through the verifier, which
	// commits failed attempts in its own transaction so they count toward the lockout.
	if err := tx.Rollback(ctx); err != nil {
		return err
	}
	if current == "" {
		return ErrCurrentPINRequired
	}
	if err := s.PIN.Verify(ctx, p.UserID, current); err != nil {
		return err
	}
	tx, err = s.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after Commit
	if _, err := tx.Exec(ctx, `UPDATE identity.credentials SET pin_hash = $2, updated_at = $3 WHERE user_id = $1`, p.UserID, hash, now); err != nil {
		return err
	}
	if err := platform.WriteOutbox(ctx, tx, "identity.pin_changed", p.UserID, map[string]any{"user_id": p.UserID, "device_id": p.DeviceID}); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

var commonPINs = map[string]bool{
	"112233": true, "121212": true, "123321": true, "111222": true, "696969": true,
	"101010": true, "131313": true, "159753": true, "007007": true, "102030": true,
}

// weakPIN rejects PINs an attacker would try first: one digit repeated, straight runs, repeated
// halves or pairs, common patterns, and the last six digits of the user's own phone number.
func weakPIN(pin, phone string) bool {
	same, up, down := true, true, true
	for i := 1; i < len(pin); i++ {
		d := int(pin[i]) - int(pin[i-1])
		same = same && d == 0
		up = up && d == 1
		down = down && d == -1
	}
	return same || up || down ||
		pin[:3] == pin[3:] ||
		(pin[0:2] == pin[2:4] && pin[2:4] == pin[4:6]) ||
		commonPINs[pin] ||
		strings.HasSuffix(phone, pin)
}

// ---------------------------------------------------------------------------
// profile
// ---------------------------------------------------------------------------

type Profile struct {
	ID          uuid.UUID
	Phone       string
	Email       string
	DisplayName string
	Locale      string
	KYCLevel    string
	Status      string
	HasPIN      bool
}

func (s *Service) Me(ctx context.Context, userID uuid.UUID) (*Profile, error) {
	p := &Profile{ID: userID}
	err := s.Pool.QueryRow(ctx, `
		SELECT u.phone_e164, COALESCE(u.email::text, ''), COALESCE(u.display_name, ''), u.locale::text, u.kyc_level::text, u.status::text,
		       EXISTS (SELECT 1 FROM identity.credentials c WHERE c.user_id = u.id)
		FROM identity.users u WHERE u.id = $1`, userID).
		Scan(&p.Phone, &p.Email, &p.DisplayName, &p.Locale, &p.KYCLevel, &p.Status, &p.HasPIN)
	return p, err
}

// ProfileUpdate holds the fields PATCH /v1/me may change. Nil means "leave as is".
type ProfileUpdate struct {
	DisplayName *string
	Email       *string // "" removes the email
	Locale      *string
}

var (
	emailRE       = regexp.MustCompile(`^[^@\s]{1,64}@[^@\s]+\.[^@\s]{2,}$`)
	ErrEmailInUse = errors.New("email already used by another account")
)

func (s *Service) UpdateProfile(ctx context.Context, userID uuid.UUID, u ProfileUpdate) (*Profile, error) {
	if u.DisplayName != nil {
		name := strings.Join(strings.Fields(*u.DisplayName), " ")
		if name == "" || utf8.RuneCountInString(name) > 60 || strings.ContainsFunc(name, unicode.IsControl) {
			return nil, &ValidationError{"Enter a name of up to 60 characters."}
		}
		u.DisplayName = &name
	}
	if u.Email != nil {
		e := strings.TrimSpace(*u.Email)
		if e != "" && (len(e) > 254 || !emailRE.MatchString(e)) {
			return nil, &ValidationError{"Enter a valid email address."}
		}
		u.Email = &e
	}
	if u.Locale != nil {
		switch *u.Locale {
		case "am", "om", "ti", "en":
		default:
			return nil, &ValidationError{"locale must be am, om, ti or en"}
		}
	}
	_, err := s.Pool.Exec(ctx, `
		UPDATE identity.users SET
			display_name = COALESCE($2, display_name),
			email = CASE WHEN $3::text IS NULL THEN email ELSE NULLIF($3, '')::citext END,
			locale = COALESCE($4::platform.locale, locale),
			updated_at = $5
		WHERE id = $1`, userID, u.DisplayName, u.Email, u.Locale, s.now())
	if platform.IsUniqueViolation(err) {
		return nil, ErrEmailInUse
	}
	if err != nil {
		return nil, err
	}
	return s.Me(ctx, userID)
}

func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now().UTC()
	}
	return time.Now().UTC()
}
