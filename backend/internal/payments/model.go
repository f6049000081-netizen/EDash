// Package payments turns a user's intent to pay into ledger entries.
//
// Flow: CreateIntent (no money moves; returns the fee, total and payee for the confirm screen) →
// Confirm (proof of presence, risk decision, then one ledger entry) → succeeded | failed | held_for_review.
//
// Phase 1 executes the internal wallet rail only: wallet-to-wallet transfers and QR payments to
// merchants on the platform. External rails (Telebirr, EthSwitch, cards, remittance) plug in behind
// the same intent model through partner adapters and are refused with ErrRailUnavailable until then.
package payments

import (
	"errors"
	"time"

	"github.com/google/uuid"

	"superapp/backend/internal/money"
)

type Status string

const (
	StatusRequiresConfirmation Status = "requires_confirmation"
	StatusProcessing           Status = "processing"
	StatusHeldForReview        Status = "held_for_review"
	StatusSucceeded            Status = "succeeded"
	StatusFailed               Status = "failed"
	StatusCancelled            Status = "cancelled"
)

type Kind string

const (
	KindP2P        Kind = "p2p"
	KindQRMerchant Kind = "qr_merchant"
	KindTopUp      Kind = "topup"
)

type Intent struct {
	ID            uuid.UUID
	Kind          Kind
	PayerID       uuid.UUID
	PayeeType     string // user | merchant
	PayeeID       uuid.UUID
	PayeeName     string
	PayeeVerified bool
	Amount        money.Money
	FeeMinor      int64
	Rail          string
	Status        Status
	FailureCode   string
	RiskScore     *float64
	Note          string
	CreatedAt     time.Time
	CompletedAt   *time.Time
}

// Total is what leaves the payer's wallet.
func (i *Intent) Total() money.Money {
	return money.New(i.Amount.AmountMinor+i.FeeMinor, i.Amount.Currency)
}

// PointsToEarn: 1 point per ETB 10 on merchant payments. Transfers between people earn nothing.
func (i *Intent) PointsToEarn() int64 {
	if i.Kind != KindQRMerchant {
		return 0
	}
	return i.Amount.AmountMinor / 1_000
}

type CreateRequest struct {
	Kind            Kind
	Amount          *money.Money
	PayeeUserID     *uuid.UUID
	QRPayload       string
	FundingSourceID *uuid.UUID
	Note            string
}

// Errors the HTTP layer maps to problem codes in api/openapi.yaml.
var (
	ErrNotFound          = errors.New("payment intent not found")
	ErrInvalidState      = errors.New("payment intent can't be changed in its current state")
	ErrUnsupportedKind   = errors.New("this kind of payment isn't available yet")
	ErrPayeeNotFound     = errors.New("payee not found")
	ErrSelfPayment       = errors.New("you can't pay yourself")
	ErrRailUnavailable   = errors.New("this payment method isn't available yet")
	ErrInsufficientFunds = errors.New("insufficient funds")
	ErrBlocked           = errors.New("payment blocked by risk checks")
	ErrPayerInactive     = errors.New("your account can't make payments right now")
)

// ValidationError carries a message safe to show to the user.
type ValidationError struct{ Msg string }

func (e *ValidationError) Error() string { return e.Msg }
