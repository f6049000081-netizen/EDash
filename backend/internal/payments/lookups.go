package payments

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// LookupLimits bound how many phone numbers one person, or one phone, can look up by starting a
// transfer. Starting a transfer shows the recipient's verified name before the PIN (the design asks
// for it, so people see who they're paying), which would otherwise let anyone check which numbers
// have accounts and whose they are.
//
// Only distinct numbers count, so typing the same number again is free, and people the user has
// already exchanged money with never count at all. A phone shared by several accounts is limited too.
type LookupLimits struct {
	PerHour        int // distinct new numbers per hour
	PerDay         int // distinct new numbers per day
	NotFoundPerDay int // distinct numbers without an account per day
}

// DefaultLookupLimits leave plenty for real use (a shop paying suppliers, someone sending to a list of
// relatives) and stop sweeps of number ranges.
func DefaultLookupLimits() LookupLimits {
	return LookupLimits{PerHour: 20, PerDay: 50, NotFoundPerDay: 15}
}

// LookupLimitError is returned when a lookup would go over a limit. RetryAfter is when the oldest
// counted lookup leaves the window.
type LookupLimitError struct {
	RetryAfter time.Duration
	Daily      bool
}

func (e *LookupLimitError) Error() string {
	return fmt.Sprintf("too many phone number lookups; try again in %s", e.RetryAfter.Round(time.Minute))
}

// lookupRuleText is the risk case summary for an account that reached a daily limit.
const lookupRuleText = "Looked up many phone numbers (possible number harvesting)"

// lookupKeep is how long lookups are kept: long enough for the day window.
const lookupKeep = 48 * time.Hour

func numberHash(phone string) string {
	sum := sha256.Sum256([]byte("payee-lookup:" + phone))
	return hex.EncodeToString(sum[:])
}

// lookupPayee resolves [phone] to an active account for [payer], counting the lookup against the
// limits. Unknown, inactive and malformed numbers all return ErrPayeeNotFound and count the same.
func (s *Service) lookupPayee(ctx context.Context, payer uuid.UUID, device *uuid.UUID, phone string) (uuid.UUID, error) {
	var payee uuid.UUID
	err := s.Pool.QueryRow(ctx, `SELECT id FROM identity.users WHERE phone_e164 = $1 AND status = 'active'`, phone).Scan(&payee)
	found := err == nil
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, err
	}
	lim := s.Lookups
	if lim == (LookupLimits{}) {
		if !found {
			return uuid.Nil, ErrPayeeNotFound
		}
		return payee, nil
	}
	if found {
		// Someone the payer has sent money to or received it from: they know each other already.
		var known bool
		if err := s.Pool.QueryRow(ctx, `
			SELECT EXISTS (SELECT 1 FROM payments.intents
			               WHERE kind = 'p2p' AND status = 'succeeded' AND payee_type = 'user'
			                 AND ((payer_user_id = $1 AND payee_id = $2) OR (payer_user_id = $2 AND payee_id = $1)))`,
			payer, payee).Scan(&known); err != nil {
			return uuid.Nil, err
		}
		if known {
			return payee, nil
		}
	}

	now := s.nowOr()
	hash := numberHash(phone)
	err = pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		// One lookup at a time per account and per phone, so parallel requests can't slip past a limit.
		// Always account first, then phone, so two accounts on one phone can't deadlock.
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('payee_lookup:user:' || $1::text, 0))`, payer); err != nil {
			return err
		}
		if device != nil {
			if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('payee_lookup:device:' || $1::text, 0))`, *device); err != nil {
				return err
			}
		}
		if _, err := tx.Exec(ctx, `DELETE FROM payments.payee_lookups WHERE user_id = $1 AND created_at < $2`, payer, now.Add(-lookupKeep)); err != nil {
			return err
		}
		if err := s.checkLookups(ctx, tx, "user_id", payer, hash, found, now, lim); err != nil {
			return err
		}
		if device != nil {
			if err := s.checkLookups(ctx, tx, "device_id", *device, hash, found, now, lim); err != nil {
				return err
			}
		}
		_, err := tx.Exec(ctx, `
			INSERT INTO payments.payee_lookups (user_id, device_id, number_hash, found, created_at) VALUES ($1, $2, $3, $4, $5)`,
			payer, device, hash, found, now)
		return err
	})
	var le *LookupLimitError
	if errors.As(err, &le) && le.Daily {
		// Reaching a daily limit is worth a look by the risk team. One open case per customer: this
		// adds to it if there is one.
		if cerr := pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error { return openLookupCaseTx(ctx, tx, payer) }); cerr != nil {
			return uuid.Nil, cerr
		}
	}
	if err != nil {
		return uuid.Nil, err
	}
	if !found {
		return uuid.Nil, ErrPayeeNotFound
	}
	return payee, nil
}

// checkLookups refuses a lookup of a number not yet looked up in the day if [col] (user_id or device_id)
// already reached a limit.
func (s *Service) checkLookups(ctx context.Context, tx pgx.Tx, col string, id uuid.UUID, hash string, found bool,
	now time.Time, lim LookupLimits) error {
	var seen bool
	var hour, day, missing int
	var hourOldest, dayOldest, missingOldest *time.Time
	// col is one of two constants from this file, never input.
	err := tx.QueryRow(ctx, `
		WITH firsts AS (
			SELECT number_hash, min(created_at) AS at, bool_or(found) AS found
			FROM payments.payee_lookups WHERE `+col+` = $1 AND created_at > $2::timestamptz - interval '24 hours'
			GROUP BY number_hash)
		SELECT coalesce(bool_or(number_hash = $3), false),
		       count(*) FILTER (WHERE at > $2::timestamptz - interval '1 hour'),
		       count(*),
		       count(*) FILTER (WHERE NOT found),
		       min(at) FILTER (WHERE at > $2::timestamptz - interval '1 hour'),
		       min(at),
		       min(at) FILTER (WHERE NOT found)
		FROM firsts`, id, now, hash).Scan(&seen, &hour, &day, &missing, &hourOldest, &dayOldest, &missingOldest)
	if err != nil {
		return err
	}
	if seen {
		return nil // already looked up today: free
	}
	after := func(oldest *time.Time, window time.Duration) time.Duration {
		if oldest == nil {
			return window
		}
		if d := oldest.Add(window).Sub(now); d > time.Minute {
			return d
		}
		return time.Minute
	}
	switch {
	case lim.PerDay > 0 && day >= lim.PerDay:
		return &LookupLimitError{RetryAfter: after(dayOldest, 24*time.Hour), Daily: true}
	case !found && lim.NotFoundPerDay > 0 && missing >= lim.NotFoundPerDay:
		return &LookupLimitError{RetryAfter: after(missingOldest, 24*time.Hour), Daily: true}
	case lim.PerHour > 0 && hour >= lim.PerHour:
		return &LookupLimitError{RetryAfter: after(hourOldest, time.Hour)}
	}
	return nil
}

func openLookupCaseTx(ctx context.Context, tx pgx.Tx, user uuid.UUID) error {
	_, err := tx.Exec(ctx, `
		INSERT INTO risk.cases (subject_type, subject_id, summary, risk_score, status, sla_due_at)
		VALUES ('user', $1, $2, 50, 'unassigned', now() + interval '24 hours')
		ON CONFLICT (subject_type, subject_id) WHERE status NOT IN ('resolved', 'escalated_aml') DO UPDATE SET
			risk_score = GREATEST(risk.cases.risk_score, EXCLUDED.risk_score),
			summary = CASE WHEN position(EXCLUDED.summary IN risk.cases.summary) > 0 THEN risk.cases.summary
				ELSE left(risk.cases.summary || '; ' || EXCLUDED.summary, 500) END,
			updated_at = CASE WHEN position(EXCLUDED.summary IN risk.cases.summary) > 0 THEN risk.cases.updated_at ELSE now() END`,
		user, lookupRuleText)
	return err
}
