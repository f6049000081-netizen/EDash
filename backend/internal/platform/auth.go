package platform

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/crypto/argon2"
)

// ---------------------------------------------------------------------------
// Request authentication
// ---------------------------------------------------------------------------

// Principal is the authenticated caller: the user, the session family their token belongs to,
// and the bound device.
type Principal struct {
	UserID    uuid.UUID
	SessionID uuid.UUID // session family id; survives refresh-token rotation
	DeviceID  uuid.UUID
}

// Authenticator turns a request into the calling principal.
type Authenticator interface {
	Authenticate(r *http.Request) (Principal, error)
}

var ErrUnauthenticated = errors.New("unauthenticated")

type ctxKey struct{}

// WithPrincipal stores the authenticated principal on the request context.
func WithPrincipal(ctx context.Context, p Principal) context.Context {
	return context.WithValue(ctx, ctxKey{}, p)
}

// PrincipalFrom returns the principal set by the auth middleware.
func PrincipalFrom(ctx context.Context) Principal {
	p, _ := ctx.Value(ctxKey{}).(Principal)
	return p
}

// UserFrom returns the authenticated user id set by the auth middleware.
func UserFrom(ctx context.Context) uuid.UUID { return PrincipalFrom(ctx).UserID }

// ---------------------------------------------------------------------------
// PIN hashing (argon2id) and verification with lockout
// ---------------------------------------------------------------------------

const (
	argonTime    = 3
	argonMemory  = 64 * 1024 // KiB
	argonThreads = 2
	argonKeyLen  = 32

	MaxPINFailures = 5
	PINLockout     = 30 * time.Minute
)

var (
	ErrInvalidPIN = errors.New("invalid PIN")
	ErrPINLocked  = errors.New("PIN locked")
	ErrNoPIN      = errors.New("no PIN set")
)

// HashPIN returns a PHC-format argon2id hash.
func HashPIN(pin string) (string, error) {
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	key := argon2.IDKey([]byte(pin), salt, argonTime, argonMemory, argonThreads, argonKeyLen)
	b64 := base64.RawStdEncoding
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, argonMemory, argonTime, argonThreads, b64.EncodeToString(salt), b64.EncodeToString(key)), nil
}

func checkPINHash(pin, encoded string) (bool, error) {
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[1] != "argon2id" {
		return false, errors.New("unsupported PIN hash format")
	}
	var mem, t uint32
	var p uint8
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &mem, &t, &p); err != nil {
		return false, err
	}
	b64 := base64.RawStdEncoding
	salt, err := b64.DecodeString(parts[4])
	if err != nil {
		return false, err
	}
	want, err := b64.DecodeString(parts[5])
	if err != nil {
		return false, err
	}
	got := argon2.IDKey([]byte(pin), salt, t, mem, p, uint32(len(want)))
	return subtle.ConstantTimeCompare(got, want) == 1, nil
}

// PINVerifier checks a user's PIN, counting failures and locking the PIN after MaxPINFailures.
// It commits in its own transaction so a failed attempt is recorded even though the payment is refused.
type PINVerifier struct {
	Pool *pgxpool.Pool
	Now  func() time.Time
}

func (v *PINVerifier) Verify(ctx context.Context, userID uuid.UUID, pin string) error {
	now := time.Now
	if v.Now != nil {
		now = v.Now
	}
	tx, err := v.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after Commit

	var hash string
	var failures int
	var lockedUntil *time.Time
	err = tx.QueryRow(ctx, `
		SELECT pin_hash, pin_failures, locked_until FROM identity.credentials
		WHERE user_id = $1 FOR UPDATE`, userID).Scan(&hash, &failures, &lockedUntil)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNoPIN
	}
	if err != nil {
		return err
	}
	if lockedUntil != nil && now().Before(*lockedUntil) {
		return ErrPINLocked
	}
	ok, err := checkPINHash(pin, hash)
	if err != nil {
		return err
	}

	// Record the outcome and commit before reporting it, so failed attempts count even though
	// the caller will refuse the payment.
	var result error
	if ok {
		_, err = tx.Exec(ctx, `UPDATE identity.credentials SET pin_failures = 0, locked_until = NULL, updated_at = now() WHERE user_id = $1`, userID)
	} else {
		failures++
		var lock *time.Time
		result = ErrInvalidPIN
		if failures >= MaxPINFailures {
			l := now().Add(PINLockout)
			lock, failures, result = &l, 0, ErrPINLocked
		}
		_, err = tx.Exec(ctx, `UPDATE identity.credentials SET pin_failures = $2, locked_until = $3, updated_at = now() WHERE user_id = $1`,
			userID, failures, lock)
	}
	if err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	return result
}
