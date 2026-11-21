package payments

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

type Action string

const (
	ActionAllow Action = "allow"
	ActionHold  Action = "hold"
	ActionBlock Action = "block"
)

type Decision struct {
	Action Action
	Score  float64 // 0 to 1
	Rules  []string
	Model  string
}

// RiskEngine decides whether a confirmed payment may execute. It runs inside the confirm transaction.
type RiskEngine interface {
	Assess(ctx context.Context, tx pgx.Tx, in *Intent) (Decision, error)
}

// RuleEngine is the Phase 1 rule set. An ML score replaces Score later; the rules stay as guard rails.
type RuleEngine struct {
	HoldAboveMinor int64         // single payments above this go to review
	VelocityWindow time.Duration // look-back window for the velocity rule
	VelocityMax    int           // succeeded payments in the window before new ones are held
	// PostResetHold holds every outgoing payment for this long after the PIN was reset by SMS code.
	// A SIM swap gives an attacker the code; the hold gives the real owner time to notice and call us.
	PostResetHold time.Duration
}

func DefaultRules() RuleEngine {
	return RuleEngine{
		HoldAboveMinor: 5_000_000, // ETB 50,000
		VelocityWindow: 10 * time.Minute,
		VelocityMax:    5,
		PostResetHold:  24 * time.Hour,
	}
}

func (r RuleEngine) Assess(ctx context.Context, tx pgx.Tx, in *Intent) (Decision, error) {
	d := Decision{Action: ActionAllow, Model: "rules-v1"}
	if r.HoldAboveMinor > 0 && in.Amount.AmountMinor > r.HoldAboveMinor {
		d.Rules = append(d.Rules, "AMOUNT_ABOVE_REVIEW_THRESHOLD")
		d.Score += 0.5
	}
	if r.VelocityMax > 0 {
		var recent int
		err := tx.QueryRow(ctx, `
			SELECT count(*) FROM payments.intents
			WHERE payer_user_id = $1 AND status = 'succeeded' AND kind <> 'topup'
			  AND completed_at > now() - make_interval(secs => $2)`,
			in.PayerID, r.VelocityWindow.Seconds()).Scan(&recent)
		if err != nil {
			return d, err
		}
		if recent >= r.VelocityMax {
			d.Rules = append(d.Rules, "P2P_VELOCITY")
			d.Score += 0.45
		}
	}
	if r.PostResetHold > 0 {
		var resetAt *time.Time
		err := tx.QueryRow(ctx, `SELECT reset_at FROM identity.credentials WHERE user_id = $1`, in.PayerID).Scan(&resetAt)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return d, err
		}
		if resetAt != nil && time.Since(*resetAt) < r.PostResetHold {
			d.Rules = append(d.Rules, "RECENT_PIN_RESET")
			d.Score += 0.4
		}
	}
	if len(d.Rules) > 0 {
		d.Action = ActionHold
	}
	if d.Score > 1 {
		d.Score = 1
	}
	return d, nil
}
