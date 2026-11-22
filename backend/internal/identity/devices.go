package identity

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"superapp/backend/internal/platform"
)

var (
	ErrDeviceNotFound = errors.New("no such signed-in device")
	ErrThisDevice     = errors.New("use sign out to sign this device out")
)

// Device is a phone signed in to the account: one with at least one live session.
type Device struct {
	ID         uuid.UUID
	Platform   string
	Model      string
	FirstSeen  time.Time // first signed in on this device
	LastActive time.Time // last sign-in or token refresh, so within a token's lifetime of the last use
	Current    bool
	Biometric  bool // payments can be confirmed with the fingerprint or face on it
	// KeyAttestation is the last sign-in's DPoP key check: strongbox, tee, none, failed, or "" (not checked).
	KeyAttestation string
}

// Devices lists the phones signed in to the account, this one first, then the most recently used.
func (s *Service) Devices(ctx context.Context, p platform.Principal) ([]Device, error) {
	rows, err := s.Pool.Query(ctx, `
		SELECT d.id, d.platform, coalesce(d.model, ''), d.first_seen_at,
		       (SELECT max(x.created_at) FROM identity.sessions x WHERE x.device_id = d.id),
		       d.id = $2, d.biometric_key_pub IS NOT NULL, coalesce(d.key_attestation, '')
		FROM identity.devices d
		WHERE d.user_id = $1 AND d.revoked_at IS NULL
		  AND EXISTS (SELECT 1 FROM identity.sessions x
		              WHERE x.device_id = d.id AND x.revoked_at IS NULL AND x.expires_at > $3)
		ORDER BY d.id = $2 DESC, 5 DESC`, p.UserID, p.DeviceID, s.now())
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Device, error) {
		var d Device
		err := r.Scan(&d.ID, &d.Platform, &d.Model, &d.FirstSeen, &d.LastActive, &d.Current, &d.Biometric, &d.KeyAttestation)
		return d, err
	})
}

// SignOutDevice signs one of the account's other devices out, with the PIN (wrong PINs count toward the
// lockout). Its sessions end at once, and its biometric key is removed, so signing in on it again
// needs an SMS code, and paying with the fingerprint needs the PIN to set it up again. This device
// signs out with Logout instead.
func (s *Service) SignOutDevice(ctx context.Context, p platform.Principal, device uuid.UUID, pin string) error {
	if device == p.DeviceID {
		return ErrThisDevice
	}
	if err := s.PIN.Verify(ctx, p.UserID, pin); err != nil {
		return err
	}
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after Commit
	n, err := s.signOut(ctx, tx, p.UserID, []uuid.UUID{device})
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrDeviceNotFound
	}
	return tx.Commit(ctx)
}

// SignOutOtherDevices signs out every device but this one, with the PIN, and returns the ones signed
// out. Older sessions left on this device (from an earlier sign-in) end too.
func (s *Service) SignOutOtherDevices(ctx context.Context, p platform.Principal, pin string) ([]uuid.UUID, error) {
	if err := s.PIN.Verify(ctx, p.UserID, pin); err != nil {
		return nil, err
	}
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after Commit
	rows, err := tx.Query(ctx, `
		SELECT id FROM identity.devices WHERE user_id = $1 AND id <> $2 AND revoked_at IS NULL
		ORDER BY id FOR UPDATE`, p.UserID, p.DeviceID)
	if err != nil {
		return nil, err
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
	if err != nil {
		return nil, err
	}
	if _, err := s.signOut(ctx, tx, p.UserID, ids); err != nil {
		return nil, err
	}
	if _, err := tx.Exec(ctx, `
		UPDATE identity.sessions SET revoked_at = $4
		WHERE user_id = $1 AND device_id = $2 AND family_id <> $3 AND revoked_at IS NULL`,
		p.UserID, p.DeviceID, p.SessionID, s.now()); err != nil {
		return nil, err
	}
	return ids, tx.Commit(ctx)
}

// signOut revokes the given devices of the user and their sessions, and tells the user about each one
// that was still signed in. It returns how many of the devices belonged to the user and were not
// already signed out.
func (s *Service) signOut(ctx context.Context, tx pgx.Tx, user uuid.UUID, devices []uuid.UUID) (int, error) {
	now := s.now()
	rows, err := tx.Query(ctx, `
		UPDATE identity.devices SET revoked_at = $3, biometric_key_pub = NULL, biometric_enrolled_at = NULL,
		                             biometric_key_attestation = NULL
		WHERE user_id = $1 AND id = ANY($2) AND revoked_at IS NULL
		RETURNING id, platform, coalesce(model, '')`, user, devices, now)
	if err != nil {
		return 0, err
	}
	type gone struct {
		id              uuid.UUID
		platform, model string
	}
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (gone, error) {
		var g gone
		err := r.Scan(&g.id, &g.platform, &g.model)
		return g, err
	})
	if err != nil {
		return 0, err
	}
	for _, g := range out {
		var live int
		if err := tx.QueryRow(ctx, `
			WITH ended AS (
				UPDATE identity.sessions SET revoked_at = $3 WHERE user_id = $1 AND device_id = $2 AND revoked_at IS NULL
				RETURNING expires_at)
			SELECT count(*) FILTER (WHERE expires_at > $3) FROM ended`, user, g.id, now).Scan(&live); err != nil {
			return 0, err
		}
		if live == 0 {
			continue // its sessions had already expired; nothing to tell
		}
		if err := platform.WriteOutbox(ctx, tx, "identity.device_signed_out", user, map[string]any{
			"user_id": user, "device_id": g.id, "platform": g.platform, "model": g.model,
		}); err != nil {
			return 0, err
		}
	}
	return len(out), nil
}
