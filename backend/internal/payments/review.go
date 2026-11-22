package payments

import (
	"context"
	"errors"
	"math"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"superapp/backend/internal/ledger"
	"superapp/backend/internal/money"
	"superapp/backend/internal/platform"
)

// What staff see for each rule that held a payment.
var RuleText = map[string]string{
	"AMOUNT_ABOVE_REVIEW_THRESHOLD": "Payment above the review threshold",
	"P2P_VELOCITY":                  "Many payments in a short time",
	"RECENT_PIN_RESET":              "Payment within 24 hours of a PIN reset",
	"TOPUP_DUPLICATE_PARTNER_REF":   "Top-up reused a partner payment reference",
	"TOPUP_LIMIT_EXCEEDED":          "Top-up would go over the wallet's balance limit",
	"TOPUP_AMOUNT_MISMATCH":         "Top-up amount didn't match what was paid",
}

// openCaseTx records a held payment on the payer's open risk case, opening one if there's none.
// Urgent cases (high score or a large amount) are due in 4 hours, others in 24.
func openCaseTx(ctx context.Context, tx pgx.Tx, payer, intent uuid.UUID, amount money.Money, rules []string, score float64) error {
	texts := make([]string, 0, len(rules))
	for _, r := range rules {
		if t := RuleText[r]; t != "" {
			texts = append(texts, t)
		} else {
			texts = append(texts, r)
		}
	}
	summary := strings.Join(texts, "; ")
	if summary == "" {
		summary = "Payment held for review"
	}
	pts := int(math.Round(score * 100))
	sla := "24 hours"
	if pts >= 80 || amount.AmountMinor >= 5_000_000 {
		sla = "4 hours"
	}
	var caseID uuid.UUID
	err := tx.QueryRow(ctx, `
		INSERT INTO risk.cases (subject_type, subject_id, summary, risk_score, amount_minor, currency, status, sla_due_at)
		VALUES ('user', $1, $2, $3, $4, $5, 'unassigned', now() + $6::interval)
		ON CONFLICT (subject_type, subject_id) WHERE status NOT IN ('resolved', 'escalated_aml') DO UPDATE SET
			risk_score = GREATEST(risk.cases.risk_score, EXCLUDED.risk_score),
			amount_minor = CASE WHEN risk.cases.currency = EXCLUDED.currency
				THEN COALESCE(risk.cases.amount_minor, 0) + EXCLUDED.amount_minor ELSE risk.cases.amount_minor END,
			summary = CASE WHEN position(EXCLUDED.summary IN risk.cases.summary) > 0 THEN risk.cases.summary
				ELSE left(risk.cases.summary || '; ' || EXCLUDED.summary, 500) END,
			sla_due_at = LEAST(risk.cases.sla_due_at, EXCLUDED.sla_due_at),
			updated_at = now()
		RETURNING id`, payer, summary, pts, amount.AmountMinor, string(amount.Currency), sla).Scan(&caseID)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `INSERT INTO risk.case_intents (case_id, intent_id) VALUES ($1, $2) ON CONFLICT DO NOTHING`, caseID, intent)
	return err
}

// ReviewError explains why a held payment can't be released or declined now.
type ReviewError struct{ Msg string }

func (e *ReviewError) Error() string { return e.Msg }

// heldTx locks a held payment that staff can decide. Top-up holds can't be decided here: the customer has
// already paid the partner, so ending one needs the partner's refund flow, which isn't built yet.
func (s *Service) heldTx(ctx context.Context, tx pgx.Tx, id uuid.UUID) (*Intent, error) {
	var payer uuid.UUID
	var status, kind string
	err := tx.QueryRow(ctx, `SELECT payer_user_id, status, kind FROM payments.intents WHERE id = $1 FOR UPDATE`, id).
		Scan(&payer, &status, &kind)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if Status(status) != StatusHeldForReview {
		return nil, &ReviewError{"This payment isn't held any more (it is " + strings.ReplaceAll(status, "_", " ") + ")."}
	}
	if Kind(kind) == KindTopUp {
		return nil, &ReviewError{"Top-up holds need the partner's refund flow, which isn't built yet."}
	}
	return s.Get(ctx, payer, id)
}

// ReleaseHeldTx lets a held payment go through, as if it had passed review when it was confirmed. If the
// payer no longer has the money, it fails instead and nothing moves. It returns the payment's new status.
func (s *Service) ReleaseHeldTx(ctx context.Context, tx pgx.Tx, id, staff uuid.UUID) (Status, error) {
	in, err := s.heldTx(ctx, tx, id)
	if err != nil {
		return "", err
	}
	if err := s.requireActive(ctx, tx, in.PayerID); err != nil {
		return "", &ReviewError{"The payer's account isn't active (frozen or closed), so the payment can't go."}
	}
	if in.Kind == KindQRMerchant {
		var mstatus string
		if err := tx.QueryRow(ctx, `SELECT status FROM merchant.merchants WHERE id = $1`, in.PayeeID).Scan(&mstatus); err != nil {
			return "", err
		}
		if mstatus != "active" {
			return "", &ReviewError{"The shop isn't active any more, so the payment can't go to it."}
		}
	}
	if _, err := tx.Exec(ctx, `SELECT 1 FROM identity.users WHERE id = $1 FOR UPDATE`, in.PayerID); err != nil {
		return "", err
	}
	actor := "staff:" + staff.String()
	entry, err := s.buildEntry(ctx, tx, in)
	if err != nil {
		return "", err
	}
	sp, err := tx.Begin(ctx)
	if err != nil {
		return "", err
	}
	_, postErr := s.Ledger.Post(ctx, sp, entry)
	failure := ""
	var insufficient *ledger.InsufficientFundsError
	switch {
	case postErr == nil && in.Kind == KindP2P:
		if capErr := s.checkBalanceCap(ctx, sp, in.PayeeID, in.Amount.Currency, 0, "recipient"); capErr != nil {
			var le *LimitError
			if !errors.As(capErr, &le) && !errors.Is(capErr, ErrKYCRequired) {
				return "", capErr
			}
			failure = "limit_exceeded"
		}
	case errors.As(postErr, &insufficient):
		failure = "insufficient_funds"
	case postErr != nil:
		return "", postErr
	}
	if failure != "" {
		if err := sp.Rollback(ctx); err != nil {
			return "", err
		}
		return StatusFailed, s.settleTx(ctx, tx, in, StatusFailed, failure, actor, "payment.failed")
	}
	if err := sp.Commit(ctx); err != nil {
		return "", err
	}
	return StatusSucceeded, s.settleTx(ctx, tx, in, StatusSucceeded, "", actor, "payment.succeeded")
}

// DeclineHeldTx ends a held payment without moving money. The payer is told it didn't go through.
func (s *Service) DeclineHeldTx(ctx context.Context, tx pgx.Tx, id, staff uuid.UUID) error {
	in, err := s.heldTx(ctx, tx, id)
	if err != nil {
		return err
	}
	return s.settleTx(ctx, tx, in, StatusFailed, "declined_after_review", "staff:"+staff.String(), "payment.failed")
}

func (s *Service) settleTx(ctx context.Context, tx pgx.Tx, in *Intent, to Status, failure, actor, topic string) error {
	if _, err := tx.Exec(ctx, `
		UPDATE payments.intents SET status = $2::payments.intent_status, failure_code = NULLIF($3, ''),
		       completed_at = now(), updated_at = now()
		WHERE id = $1`, in.ID, string(to), failure); err != nil {
		return err
	}
	if err := addEvent(ctx, tx, in.ID, StatusHeldForReview, to, failure, actor); err != nil {
		return err
	}
	return platform.WriteOutbox(ctx, tx, topic, in.ID, map[string]any{
		"intent_id": in.ID, "kind": in.Kind, "payer_user_id": in.PayerID,
		"payee_type": in.PayeeType, "payee_id": in.PayeeID,
		"amount": in.Amount, "status": to, "failure_code": failure,
		"points_to_earn": in.PointsToEarn(),
	})
}
