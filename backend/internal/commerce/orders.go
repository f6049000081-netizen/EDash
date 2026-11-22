package commerce

import (
	"context"
	"errors"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"superapp/backend/internal/money"
	"superapp/backend/internal/payments"
	"superapp/backend/internal/platform"
)

type Order struct {
	ID              uuid.UUID
	Number          string
	Status          string
	Channel         string // pickup, delivery
	MerchantID      uuid.UUID
	ShopName        string
	Items           []Item
	Subtotal        money.Money
	DeliveryFee     money.Money
	Total           money.Money
	DeliveryAddress string
	Note            string
	PaymentIntentID *uuid.UUID
	PaymentStatus   string
	HandoverCode    string // the customer's view only
	CancelReason    string
	CancelNote      string
	CustomerName    string // the shop's view only
	CustomerPhone   string // the shop's view: full for delivery orders in progress, masked otherwise
	CreatedAt       time.Time
	PlacedAt        *time.Time
	AcceptedAt      *time.Time
	ReadyAt         *time.Time
	CompletedAt     *time.Time
	CancelledAt     *time.Time
	AcceptBy        *time.Time // placed orders: when it's cancelled if the shop hasn't accepted
}

type Item struct {
	ProductID uuid.UUID
	Title     string
	UnitPrice money.Money
	Quantity  int
}

func (i Item) LineTotal() money.Money {
	return money.New(i.UnitPrice.AmountMinor*int64(i.Quantity), i.UnitPrice.Currency)
}

type PlaceRequest struct {
	MerchantID      uuid.UUID
	Channel         string
	Items           []LineRequest
	DeliveryAddress string
	Note            string
}

type LineRequest struct {
	ProductID uuid.UUID
	Quantity  int
}

// Place creates an order and its payment, which the customer then confirms with their PIN like any
// payment. Stock is set aside now and given back if the order is cancelled or never paid.
func (s *Service) Place(ctx context.Context, user uuid.UUID, req PlaceRequest) (*Order, error) {
	req.DeliveryAddress, req.Note = strings.TrimSpace(req.DeliveryAddress), strings.TrimSpace(req.Note)
	switch {
	case len(req.Items) == 0:
		return nil, &ValidationError{"Add something to the basket first."}
	case len(req.Items) > 50:
		return nil, &ValidationError{"An order can have up to 50 different products."}
	case req.Channel != "pickup" && req.Channel != "delivery":
		return nil, &ValidationError{"channel must be pickup or delivery"}
	case req.Channel == "delivery" && req.DeliveryAddress == "":
		return nil, &ValidationError{"Write the delivery address, with a landmark the shop can find."}
	case len([]rune(req.DeliveryAddress)) > 300:
		return nil, &ValidationError{"The address can be up to 300 characters."}
	case len([]rune(req.Note)) > 280:
		return nil, &ValidationError{"The note can be up to 280 characters."}
	}
	if req.Channel == "pickup" {
		req.DeliveryAddress = ""
	}
	qty := map[uuid.UUID]int{}
	for _, l := range req.Items {
		if l.Quantity < 1 || l.Quantity > 99 {
			return nil, &ValidationError{"Quantities must be from 1 to 99."}
		}
		qty[l.ProductID] += l.Quantity
		if qty[l.ProductID] > 99 {
			return nil, &ValidationError{"Quantities must be from 1 to 99."}
		}
	}

	var id uuid.UUID
	err := pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		var status string
		var open, pickup, delivery bool
		var fee int64
		err := tx.QueryRow(ctx, `
			SELECT m.status, s.open, s.pickup, s.delivery, s.delivery_fee_minor
			FROM commerce.shops s JOIN merchant.merchants m ON m.id = s.merchant_id
			WHERE s.merchant_id = $1 FOR SHARE OF s`, req.MerchantID).Scan(&status, &open, &pickup, &delivery, &fee)
		if errors.Is(err, pgx.ErrNoRows) || (err == nil && status != "active") {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		switch {
		case !open:
			return &ConflictError{"This shop isn't taking orders right now."}
		case req.Channel == "pickup" && !pickup:
			return &ConflictError{"This shop only delivers."}
		case req.Channel == "delivery" && !delivery:
			return &ConflictError{"This shop doesn't deliver. Choose pickup."}
		}
		var member bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM merchant.members WHERE merchant_id = $1 AND user_id = $2)`,
			req.MerchantID, user).Scan(&member); err != nil {
			return err
		}
		if member {
			return &ConflictError{"You can't order from your own shop."}
		}
		if req.Channel == "pickup" {
			fee = 0
		}

		// Lock the products in a fixed order so two orders for the same products can't deadlock.
		ids := make([]uuid.UUID, 0, len(qty))
		for pid := range qty {
			ids = append(ids, pid)
		}
		sort.Slice(ids, func(a, b int) bool { return ids[a].String() < ids[b].String() })
		var items []Item
		var subtotal int64
		for _, pid := range ids {
			var it Item
			var stock *int
			var pstatus string
			err := tx.QueryRow(ctx, `
				SELECT title, price_minor, stock, status FROM commerce.products WHERE id = $1 AND merchant_id = $2 FOR UPDATE`,
				pid, req.MerchantID).Scan(&it.Title, &it.UnitPrice.AmountMinor, &stock, &pstatus)
			if errors.Is(err, pgx.ErrNoRows) || (err == nil && pstatus != "active") {
				return &ConflictError{"Something in your basket is no longer sold here. Refresh the shop and try again."}
			}
			if err != nil {
				return err
			}
			it.ProductID, it.UnitPrice.Currency, it.Quantity = pid, money.ETB, qty[pid]
			if stock != nil && *stock < it.Quantity {
				if *stock == 0 {
					return &ConflictError{it.Title + " is sold out."}
				}
				return &ConflictError{"Only " + itoa(*stock) + " of " + it.Title + " left. Lower the quantity."}
			}
			if stock != nil {
				if _, err := tx.Exec(ctx, `UPDATE commerce.products SET stock = stock - $2 WHERE id = $1`, pid, it.Quantity); err != nil {
					return err
				}
			}
			subtotal += it.LineTotal().AmountMinor
			items = append(items, it)
		}
		code, err := handoverCode()
		if err != nil {
			return err
		}
		if id, err = uuid.NewV7(); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO commerce.orders (id, user_id, merchant_id, channel, status, subtotal_minor, delivery_fee_minor,
				total_minor, currency, delivery_address, note, handover_code)
			VALUES ($1, $2, $3, $4, 'pending_payment', $5, $6, $7, 'ETB', NULLIF($8, ''), NULLIF($9, ''), $10)`,
			id, user, req.MerchantID, req.Channel, subtotal, fee, subtotal+fee, req.DeliveryAddress, req.Note, code); err != nil {
			return err
		}
		for i, it := range items {
			if _, err := tx.Exec(ctx, `
				INSERT INTO commerce.order_items (order_id, line_no, product_id, title_snapshot, unit_price_minor, quantity)
				VALUES ($1, $2, $3, $4, $5, $6)`, id, i+1, it.ProductID, it.Title, it.UnitPrice.AmountMinor, it.Quantity); err != nil {
				return err
			}
		}
		in, err := s.Payments.CreateOrderIntentTx(ctx, tx, user, req.MerchantID, id, money.New(subtotal+fee, money.ETB))
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `UPDATE commerce.orders SET intent_id = $2 WHERE id = $1`, id, in.ID)
		return err
	})
	if err != nil {
		return nil, err
	}
	return s.Order(ctx, user, id)
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for ; n > 0; n /= 10 {
		b = append([]byte{byte('0' + n%10)}, b...)
	}
	return string(b)
}

// ---------------------------------------------------------------------------
// Reading orders
// ---------------------------------------------------------------------------

const orderCols = `o.id, o.number, o.status, o.channel, o.merchant_id, m.trade_name, o.subtotal_minor, o.delivery_fee_minor,
	o.total_minor, o.currency, COALESCE(o.delivery_address, ''), COALESCE(o.note, ''), o.intent_id,
	COALESCE(i.status::text, ''), COALESCE(o.handover_code, ''), COALESCE(o.cancel_reason, ''), COALESCE(o.cancel_note, ''),
	COALESCE(u.display_name, ''), u.phone_e164, o.created_at, o.placed_at, o.accepted_at, o.ready_at, o.completed_at,
	o.cancelled_at`

const orderFrom = `FROM commerce.orders o JOIN merchant.merchants m ON m.id = o.merchant_id
	JOIN identity.users u ON u.id = o.user_id LEFT JOIN payments.intents i ON i.id = o.intent_id`

func (s *Service) scanOrders(ctx context.Context, rows pgx.Rows, forShop bool) ([]Order, error) {
	list, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (Order, error) {
		var o Order
		var sub, fee, total int64
		var cur, phone string
		err := r.Scan(&o.ID, &o.Number, &o.Status, &o.Channel, &o.MerchantID, &o.ShopName, &sub, &fee, &total, &cur,
			&o.DeliveryAddress, &o.Note, &o.PaymentIntentID, &o.PaymentStatus, &o.HandoverCode, &o.CancelReason,
			&o.CancelNote, &o.CustomerName, &phone, &o.CreatedAt, &o.PlacedAt, &o.AcceptedAt, &o.ReadyAt,
			&o.CompletedAt, &o.CancelledAt)
		c := money.Currency(cur)
		o.Subtotal, o.DeliveryFee, o.Total = money.New(sub, c), money.New(fee, c), money.New(total, c)
		if forShop {
			o.HandoverCode = ""
			active := o.Status == "placed" || o.Status == "accepted" || o.Status == "ready" || o.Status == "on_the_way"
			if o.Channel == "delivery" && active {
				o.CustomerPhone = phone
			} else {
				o.CustomerPhone = "••" + phone[max(0, len(phone)-4):]
			}
		} else {
			o.CustomerName = ""
			if o.Status != "accepted" && o.Status != "ready" && o.Status != "on_the_way" && o.Status != "placed" {
				o.HandoverCode = ""
			}
		}
		if o.Status == "placed" && o.PlacedAt != nil {
			by := o.PlacedAt.Add(s.AcceptWithin)
			o.AcceptBy = &by
		}
		return o, err
	})
	if err != nil {
		return nil, err
	}
	if len(list) == 0 {
		return list, nil
	}
	ids := make([]uuid.UUID, len(list))
	at := map[uuid.UUID]int{}
	for i, o := range list {
		ids[i], at[o.ID] = o.ID, i
	}
	irows, err := s.Pool.Query(ctx, `
		SELECT order_id, product_id, title_snapshot, unit_price_minor, quantity FROM commerce.order_items
		WHERE order_id = ANY($1) ORDER BY order_id, line_no`, ids)
	if err != nil {
		return nil, err
	}
	defer irows.Close()
	for irows.Next() {
		var oid uuid.UUID
		var it Item
		if err := irows.Scan(&oid, &it.ProductID, &it.Title, &it.UnitPrice.AmountMinor, &it.Quantity); err != nil {
			return nil, err
		}
		o := &list[at[oid]]
		it.UnitPrice.Currency = o.Total.Currency
		o.Items = append(o.Items, it)
	}
	return list, irows.Err()
}

// Order is one of the customer's own orders.
func (s *Service) Order(ctx context.Context, user, id uuid.UUID) (*Order, error) {
	rows, err := s.Pool.Query(ctx, `SELECT `+orderCols+` `+orderFrom+` WHERE o.id = $1 AND o.user_id = $2`, id, user)
	if err != nil {
		return nil, err
	}
	list, err := s.scanOrders(ctx, rows, false)
	if err != nil {
		return nil, err
	}
	if len(list) == 0 {
		return nil, ErrNotFound
	}
	return &list[0], nil
}

// Orders lists the customer's orders, newest first. Orders never paid for are left out.
func (s *Service) Orders(ctx context.Context, user uuid.UUID, limit int) ([]Order, error) {
	if limit <= 0 || limit > 100 {
		limit = 30
	}
	rows, err := s.Pool.Query(ctx, `SELECT `+orderCols+` `+orderFrom+`
		WHERE o.user_id = $1 AND NOT (o.status = 'cancelled' AND o.placed_at IS NULL)
		ORDER BY o.created_at DESC LIMIT $2`, user, limit)
	if err != nil {
		return nil, err
	}
	return s.scanOrders(ctx, rows, false)
}

// ShopOrders lists a shop's paid orders: in progress (oldest first, so the next to handle is on top)
// or finished (newest first).
func (s *Service) ShopOrders(ctx context.Context, user, merchant uuid.UUID, active bool, limit int) ([]Order, error) {
	if err := s.requireRole(ctx, s.Pool, user, merchant, viewRoles); err != nil {
		return nil, err
	}
	if limit <= 0 || limit > 100 {
		limit = 50
	}
	q := `SELECT ` + orderCols + ` ` + orderFrom + ` WHERE o.merchant_id = $1 AND o.status IN ('placed', 'accepted', 'ready', 'on_the_way')
		ORDER BY o.placed_at LIMIT $2`
	if !active {
		q = `SELECT ` + orderCols + ` ` + orderFrom + ` WHERE o.merchant_id = $1 AND o.placed_at IS NOT NULL
			AND o.status IN ('delivered', 'cancelled') ORDER BY o.updated_at DESC LIMIT $2`
	}
	rows, err := s.Pool.Query(ctx, q, merchant, limit)
	if err != nil {
		return nil, err
	}
	return s.scanOrders(ctx, rows, true)
}

// ShopOrder is one of the shop's orders as its staff see it.
func (s *Service) ShopOrder(ctx context.Context, user, merchant, id uuid.UUID) (*Order, error) {
	if err := s.requireRole(ctx, s.Pool, user, merchant, viewRoles); err != nil {
		return nil, err
	}
	return s.shopOrder(ctx, merchant, id)
}

func (s *Service) shopOrder(ctx context.Context, merchant, id uuid.UUID) (*Order, error) {
	rows, err := s.Pool.Query(ctx, `SELECT `+orderCols+` `+orderFrom+` WHERE o.id = $1 AND o.merchant_id = $2 AND o.placed_at IS NOT NULL`,
		id, merchant)
	if err != nil {
		return nil, err
	}
	list, err := s.scanOrders(ctx, rows, true)
	if err != nil {
		return nil, err
	}
	if len(list) == 0 {
		return nil, ErrNotFound
	}
	return &list[0], nil
}

// ---------------------------------------------------------------------------
// The order's payment
// ---------------------------------------------------------------------------

// PaymentFinished implements payments.OrderHook, inside the payment's transaction.
func (s *Service) PaymentFinished(ctx context.Context, tx pgx.Tx, order uuid.UUID, st payments.Status, failure string) error {
	o, err := lockOrder(ctx, tx, order)
	if err != nil {
		return err
	}
	switch st {
	case payments.StatusSucceeded:
		if o.status != "pending_payment" {
			return errors.New("commerce: payment succeeded for an order that isn't waiting for it")
		}
		if _, err := tx.Exec(ctx, `UPDATE commerce.orders SET status = 'placed', placed_at = $2, updated_at = $2 WHERE id = $1`,
			order, s.clock()); err != nil {
			return err
		}
		return s.event(ctx, tx, "order.placed", o, "", "")
	case payments.StatusFailed, payments.StatusCancelled:
		if o.status != "pending_payment" {
			return nil
		}
		reason := "payment_failed"
		if st == payments.StatusCancelled {
			reason = "payment_not_finished"
		}
		return s.cancelTx(ctx, tx, o, reason, "", "")
	}
	return nil // held for review: the order waits
}

// lockWithIntent locks an order's payment before the order itself, the same order as confirming a
// payment does (payments locks the intent, then the hook locks the order), so the two can't deadlock.
// It returns the payment's status.
func lockWithIntent(ctx context.Context, tx pgx.Tx, id uuid.UUID) (*lockedOrder, string, error) {
	var intent *uuid.UUID
	err := tx.QueryRow(ctx, `SELECT intent_id FROM commerce.orders WHERE id = $1`, id).Scan(&intent)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, "", ErrNotFound
	}
	if err != nil {
		return nil, "", err
	}
	var st string
	if intent != nil {
		if err := tx.QueryRow(ctx, `SELECT status::text FROM payments.intents WHERE id = $1 FOR UPDATE`, intent).Scan(&st); err != nil {
			return nil, "", err
		}
	}
	o, err := lockOrder(ctx, tx, id)
	return o, st, err
}

type lockedOrder struct {
	id, user, merchant uuid.UUID
	intent             *uuid.UUID
	status, channel    string
	number, code       string
	tries              int
	total              money.Money
	placedAt           *time.Time
}

func lockOrder(ctx context.Context, tx pgx.Tx, id uuid.UUID) (*lockedOrder, error) {
	o := &lockedOrder{id: id}
	var cur string
	err := tx.QueryRow(ctx, `
		SELECT user_id, merchant_id, intent_id, status, channel, number, COALESCE(handover_code, ''), handover_tries,
		       total_minor, currency, placed_at
		FROM commerce.orders WHERE id = $1 FOR UPDATE`, id).
		Scan(&o.user, &o.merchant, &o.intent, &o.status, &o.channel, &o.number, &o.code, &o.tries,
			&o.total.AmountMinor, &cur, &o.placedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	o.total.Currency = money.Currency(cur)
	return o, err
}

// cancelTx cancels an order not yet handed over: stock goes back, and a paid order is refunded.
func (s *Service) cancelTx(ctx context.Context, tx pgx.Tx, o *lockedOrder, reason, note, actor string) error {
	if _, err := tx.Exec(ctx, `
		UPDATE commerce.products p SET stock = p.stock + i.quantity, updated_at = now()
		FROM commerce.order_items i WHERE i.order_id = $1 AND p.id = i.product_id AND p.stock IS NOT NULL`, o.id); err != nil {
		return err
	}
	now := s.clock()
	if _, err := tx.Exec(ctx, `
		UPDATE commerce.orders SET status = 'cancelled', cancel_reason = $2, cancel_note = NULLIF($3, ''), cancelled_at = $4,
			handover_code = NULL, updated_at = $4
		WHERE id = $1`, o.id, reason, note, now); err != nil {
		return err
	}
	if o.placedAt == nil { // never paid
		if o.intent != nil && actor != "" {
			return s.Payments.CancelOrderIntentTx(ctx, tx, *o.intent, actor)
		}
		return nil
	}
	if err := s.Payments.RefundOrderTx(ctx, tx, o.id, *o.intent, o.user, o.total, or(actor, "system")); err != nil {
		return err
	}
	return s.event(ctx, tx, "order.cancelled", o, reason, note)
}

func or(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

func (s *Service) event(ctx context.Context, tx pgx.Tx, topic string, o *lockedOrder, reason, note string) error {
	return platform.WriteOutbox(ctx, tx, topic, o.id, map[string]any{
		"order_id": o.id, "number": o.number, "user_id": o.user, "merchant_id": o.merchant, "channel": o.channel,
		"total": o.total, "reason": reason, "note": note,
	})
}

// ---------------------------------------------------------------------------
// The customer's actions
// ---------------------------------------------------------------------------

// Cancel is the customer cancelling: freely until the shop accepts the order, refunded if paid.
func (s *Service) Cancel(ctx context.Context, user, id uuid.UUID) (*Order, error) {
	err := pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		o, intentStatus, err := lockWithIntent(ctx, tx, id)
		if err != nil {
			return err
		}
		if o.user != user {
			return ErrNotFound
		}
		switch o.status {
		case "pending_payment":
			if intentStatus != string(payments.StatusRequiresConfirmation) {
				return &ConflictError{"Your payment for this order is being checked. You can cancel once that's done."}
			}
			return s.cancelTx(ctx, tx, o, "customer_cancelled", "", "user:"+user.String())
		case "placed":
			return s.cancelTx(ctx, tx, o, "customer_cancelled", "", "user:"+user.String())
		case "cancelled":
			return nil
		case "delivered":
			return &ConflictError{"This order was already handed over."}
		default:
			return &ConflictError{"The shop has already started on your order. Ask them to cancel it."}
		}
	})
	if err != nil {
		return nil, err
	}
	return s.Order(ctx, user, id)
}

// Received is the customer confirming they have the order, for when the shop couldn't take the code.
func (s *Service) Received(ctx context.Context, user, id uuid.UUID) (*Order, error) {
	err := pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		o, err := lockOrder(ctx, tx, id)
		if err != nil {
			return err
		}
		if o.user != user {
			return ErrNotFound
		}
		switch o.status {
		case "delivered":
			return nil
		case "accepted", "ready", "on_the_way":
			return s.completeTx(ctx, tx, o, "user:"+user.String())
		default:
			return &ConflictError{"You can confirm once the shop has accepted the order."}
		}
	})
	if err != nil {
		return nil, err
	}
	return s.Order(ctx, user, id)
}

// completeTx hands the order over: the shop is paid from escrow.
func (s *Service) completeTx(ctx context.Context, tx pgx.Tx, o *lockedOrder, _ string) error {
	if err := s.Payments.SettleOrderTx(ctx, tx, o.id, o.merchant, o.total); err != nil {
		return err
	}
	now := s.clock()
	if _, err := tx.Exec(ctx, `UPDATE commerce.orders SET status = 'delivered', completed_at = $2, handover_code = NULL,
		updated_at = $2 WHERE id = $1`, o.id, now); err != nil {
		return err
	}
	return s.event(ctx, tx, "order.delivered", o, "", "")
}

// ---------------------------------------------------------------------------
// The shop's actions
// ---------------------------------------------------------------------------

// Steps a shop moves an order through. Delivery orders go ready (packed) then on_the_way; pickup
// orders stop at ready until the customer collects.
var steps = map[string]struct{ from, to, stamp string }{
	"accept":     {"placed", "accepted", "accepted_at"},
	"ready":      {"accepted", "ready", "ready_at"},
	"on_the_way": {"ready", "on_the_way", ""},
}

// Advance moves an order to its next step.
func (s *Service) Advance(ctx context.Context, user, merchant, id uuid.UUID, step string) (*Order, error) {
	st, ok := steps[step]
	if !ok {
		return nil, &ValidationError{"step must be accept, ready or on_the_way"}
	}
	err := pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		if err := s.requireRole(ctx, tx, user, merchant, handleRoles); err != nil {
			return err
		}
		o, err := lockOrder(ctx, tx, id)
		if err != nil || o.merchant != merchant || o.placedAt == nil {
			return ErrNotFound
		}
		if o.status == st.to {
			return nil // a repeated tap
		}
		if o.status != st.from {
			return &ConflictError{"This order is " + strings.ReplaceAll(o.status, "_", " ") + " now. Reload it."}
		}
		if step == "on_the_way" && o.channel != "delivery" {
			return &ConflictError{"Pickup orders wait at the shop until the customer collects them."}
		}
		now := s.clock()
		q := `UPDATE commerce.orders SET status = $2, updated_at = $3 WHERE id = $1`
		if st.stamp != "" {
			q = `UPDATE commerce.orders SET status = $2, ` + st.stamp + ` = $3, updated_at = $3 WHERE id = $1`
		}
		if _, err := tx.Exec(ctx, q, id, st.to, now); err != nil {
			return err
		}
		return s.event(ctx, tx, "order."+st.to, o, "", "")
	})
	if err != nil {
		return nil, err
	}
	return s.shopOrder(ctx, merchant, id)
}

// MaxHandoverTries: after this many wrong codes the shop can't complete the order with a code; the
// customer confirms in their app instead.
const MaxHandoverTries = 5

var ErrWrongCode = errors.New("commerce: wrong handover code")

// ErrHandoverLocked: too many wrong codes; the customer confirms in their app instead.
var ErrHandoverLocked = &ConflictError{"Too many wrong codes. Ask the customer to tap \"I have my order\" in their app."}

// HandOver completes an order with the code the customer shows. The shop is paid at once.
func (s *Service) HandOver(ctx context.Context, user, merchant, id uuid.UUID, code string) (*Order, error) {
	code = strings.TrimSpace(code)
	var wrong bool
	err := pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		if err := s.requireRole(ctx, tx, user, merchant, handleRoles); err != nil {
			return err
		}
		o, err := lockOrder(ctx, tx, id)
		if err != nil || o.merchant != merchant || o.placedAt == nil {
			return ErrNotFound
		}
		switch o.status {
		case "delivered":
			return nil
		case "accepted", "ready", "on_the_way":
		default:
			return &ConflictError{"This order is " + strings.ReplaceAll(o.status, "_", " ") + " and can't be handed over."}
		}
		if o.tries >= MaxHandoverTries {
			return ErrHandoverLocked
		}
		if !codesEqual(code, o.code) {
			wrong = true
			_, err := tx.Exec(ctx, `UPDATE commerce.orders SET handover_tries = handover_tries + 1 WHERE id = $1`, id)
			return err
		}
		return s.completeTx(ctx, tx, o, "user:"+user.String())
	})
	if err != nil {
		return nil, err
	}
	if wrong {
		return nil, ErrWrongCode
	}
	return s.shopOrder(ctx, merchant, id)
}

// CancelByShop cancels a paid order the shop can't fill. The customer is refunded and sees the note.
func (s *Service) CancelByShop(ctx context.Context, user, merchant, id uuid.UUID, note string) (*Order, error) {
	note = strings.TrimSpace(note)
	if note == "" || len([]rune(note)) > 280 {
		return nil, &ValidationError{"Tell the customer why, in up to 280 characters."}
	}
	err := pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		if err := s.requireRole(ctx, tx, user, merchant, handleRoles); err != nil {
			return err
		}
		o, err := lockOrder(ctx, tx, id)
		if err != nil || o.merchant != merchant || o.placedAt == nil {
			return ErrNotFound
		}
		reason := "shop_cancelled"
		switch o.status {
		case "cancelled":
			return nil
		case "delivered":
			return &ConflictError{"This order was already handed over and paid. Refunds after handover aren't available yet."}
		case "placed":
			reason = "shop_declined"
		}
		return s.cancelTx(ctx, tx, o, reason, note, "user:"+user.String())
	})
	if err != nil {
		return nil, err
	}
	return s.shopOrder(ctx, merchant, id)
}

// ---------------------------------------------------------------------------
// Timeouts
// ---------------------------------------------------------------------------

// RunOnce cancels orders left unpaid past PayWithin, and refunds paid orders the shop didn't accept
// within AcceptWithin. The API server calls it every minute. It returns how many it cancelled.
func (s *Service) RunOnce(ctx context.Context) (int, error) {
	now := s.clock()
	rows, err := s.Pool.Query(ctx, `
		SELECT o.id, 'payment_not_finished' FROM commerce.orders o JOIN payments.intents i ON i.id = o.intent_id
		WHERE o.status = 'pending_payment' AND i.status = 'requires_confirmation' AND o.created_at < $1
		UNION ALL
		SELECT o.id, 'no_response' FROM commerce.orders o WHERE o.status = 'placed' AND o.placed_at < $2
		LIMIT 200`, now.Add(-s.PayWithin), now.Add(-s.AcceptWithin))
	if err != nil {
		return 0, err
	}
	type due struct {
		id     uuid.UUID
		reason string
	}
	list, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (due, error) {
		var d due
		return d, r.Scan(&d.id, &d.reason)
	})
	if err != nil {
		return 0, err
	}
	n := 0
	for _, d := range list {
		err := pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
			o, st, err := lockWithIntent(ctx, tx, d.id)
			if err != nil {
				return err
			}
			want := map[string]string{"payment_not_finished": "pending_payment", "no_response": "placed"}[d.reason]
			if o.status != want { // moved on since the query
				return nil
			}
			if d.reason == "payment_not_finished" && st != string(payments.StatusRequiresConfirmation) {
				return nil
			}
			n++
			return s.cancelTx(ctx, tx, o, d.reason, "", "system")
		})
		if err != nil {
			return n, err
		}
	}
	return n, nil
}

// Run calls RunOnce every interval until ctx ends.
func (s *Service) Run(ctx context.Context, every time.Duration, log interface{ Error(string, ...any) }) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if _, err := s.RunOnce(ctx); err != nil && ctx.Err() == nil {
				log.Error("order timeouts", "err", err)
			}
		}
	}
}
