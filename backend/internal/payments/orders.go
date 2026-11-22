package payments

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"superapp/backend/internal/ledger"
	"superapp/backend/internal/money"
)

// Orders from shops in the app are paid like any other payment (PIN or biometric, risk checks, limits),
// but the money goes to order escrow, not to the shop: the shop is paid when the order is handed over,
// and the customer is refunded if it's cancelled.
//
//	pay:     Dr payer wallet     Cr order escrow
//	settle:  Dr order escrow     Cr merchant settlement (total − commission), Cr commission revenue
//	refund:  Dr order escrow     Cr payer wallet

// OrderHook keeps an order in step with its payment. It runs inside the payment's transaction.
type OrderHook interface {
	// PaymentFinished is called when the payment for order succeeds, fails, is held or is cancelled.
	PaymentFinished(ctx context.Context, tx pgx.Tx, order uuid.UUID, status Status, failure string) error
}

// CreateOrderIntentTx creates the payment for an order in the caller's transaction. The caller has
// checked the shop and worked out the amount; this checks the payer and their limits.
func (s *Service) CreateOrderIntentTx(ctx context.Context, tx pgx.Tx, payer, merchant uuid.UUID, order uuid.UUID,
	amount money.Money) (*Intent, error) {
	if err := s.requireActive(ctx, tx, payer); err != nil {
		return nil, err
	}
	if !amount.Currency.Valid() || amount.AmountMinor <= 0 || amount.AmountMinor > money.MaxAmountMinor {
		return nil, &ValidationError{"The order total isn't a valid amount."}
	}
	if err := s.checkSpend(ctx, tx, payer, amount, s.nowOr()); err != nil {
		return nil, err
	}
	in := &Intent{ID: uuid.New(), Kind: KindOrder, PayerID: payer, PayeeType: "merchant", PayeeID: merchant,
		PayeeVerified: true, Amount: amount, Rail: "wallet", Status: StatusRequiresConfirmation, OrderID: &order}
	if err := tx.QueryRow(ctx, `
		INSERT INTO payments.intents (id, kind, payer_user_id, payee_type, payee_id, amount_minor, currency, rail, status, order_id)
		VALUES ($1, 'order', $2, 'merchant', $3, $4, $5, 'wallet', 'requires_confirmation', $6)
		RETURNING created_at`, in.ID, payer, merchant, amount.AmountMinor, string(amount.Currency), order).Scan(&in.CreatedAt); err != nil {
		return nil, err
	}
	return in, addEvent(ctx, tx, in.ID, "", StatusRequiresConfirmation, "created", "user:"+payer.String())
}

// SettleOrderTx pays the shop for a handed-over order: escrow to the shop's settlement account, less
// the shop's commission. Payouts then pick it up like a QR sale.
func (s *Service) SettleOrderTx(ctx context.Context, tx pgx.Tx, order, merchant uuid.UUID, amount money.Money) error {
	cur, amt := amount.Currency, amount.AmountMinor
	escrow, err := s.Ledger.EnsureAccount(ctx, tx, ledger.OrderEscrow, ledger.OwnerPlatform, uuid.Nil, cur)
	if err != nil {
		return err
	}
	settlement, err := s.Ledger.EnsureAccount(ctx, tx, ledger.MerchantSettlement, ledger.OwnerMerchant, merchant, cur)
	if err != nil {
		return err
	}
	bps, err := s.merchantBps(ctx, tx, merchant)
	if err != nil {
		return err
	}
	commission := money.BasisPoints(amt, bps)
	e := ledger.Entry{Kind: "order_settlement", SourceType: "commerce.order", SourceID: order, Postings: []ledger.Posting{
		{AccountID: escrow, Side: ledger.Debit, AmountMinor: amt, Currency: cur},
		{AccountID: settlement, Side: ledger.Credit, AmountMinor: amt - commission, Currency: cur},
	}}
	if commission > 0 {
		rev, err := s.Ledger.EnsureAccount(ctx, tx, ledger.CommissionRevenue, ledger.OwnerPlatform, uuid.Nil, cur)
		if err != nil {
			return err
		}
		e.Postings = append(e.Postings, ledger.Posting{AccountID: rev, Side: ledger.Credit, AmountMinor: commission, Currency: cur})
	}
	_, err = s.Ledger.Post(ctx, tx, e)
	return err
}

// RefundOrderTx returns a cancelled order's payment from escrow to the payer, and marks the payment
// reversed so the customer's activity shows the refund.
func (s *Service) RefundOrderTx(ctx context.Context, tx pgx.Tx, order, intent, payer uuid.UUID, amount money.Money, actor string) error {
	cur := amount.Currency
	escrow, err := s.Ledger.EnsureAccount(ctx, tx, ledger.OrderEscrow, ledger.OwnerPlatform, uuid.Nil, cur)
	if err != nil {
		return err
	}
	wallet, err := s.Ledger.EnsureAccount(ctx, tx, ledger.UserWallet, ledger.OwnerUser, payer, cur)
	if err != nil {
		return err
	}
	if _, err := s.Ledger.Post(ctx, tx, ledger.Entry{Kind: "order_refund", SourceType: "commerce.order", SourceID: order,
		Postings: []ledger.Posting{
			{AccountID: escrow, Side: ledger.Debit, AmountMinor: amount.AmountMinor, Currency: cur},
			{AccountID: wallet, Side: ledger.Credit, AmountMinor: amount.AmountMinor, Currency: cur},
		}}); err != nil {
		return err
	}
	tag, err := tx.Exec(ctx, `UPDATE payments.intents SET status = 'reversed', updated_at = now()
		WHERE id = $1 AND status = 'succeeded'`, intent)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return errors.New("payments: refunding an order whose payment didn't succeed")
	}
	return addEvent(ctx, tx, intent, StatusSucceeded, StatusReversed, "order cancelled", actor)
}

// CancelOrderIntentTx ends an order's payment that was never confirmed (the customer left checkout).
func (s *Service) CancelOrderIntentTx(ctx context.Context, tx pgx.Tx, intent uuid.UUID, actor string) error {
	tag, err := tx.Exec(ctx, `UPDATE payments.intents SET status = 'cancelled', updated_at = now()
		WHERE id = $1 AND status = 'requires_confirmation'`, intent)
	if err != nil || tag.RowsAffected() == 0 {
		return err
	}
	return addEvent(ctx, tx, intent, StatusRequiresConfirmation, StatusCancelled, "order cancelled", actor)
}

func (s *Service) orderHook(ctx context.Context, tx pgx.Tx, in *Intent, to Status, failure string) error {
	if in.Kind != KindOrder || in.OrderID == nil || s.Orders == nil {
		return nil
	}
	return s.Orders.PaymentFinished(ctx, tx, *in.OrderID, to, failure)
}
