package notify

import (
	"context"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"superapp/backend/internal/money"
	"superapp/backend/internal/outbox"
)

type orderEvent struct {
	OrderID    uuid.UUID   `json:"order_id"`
	Number     string      `json:"number"`
	UserID     uuid.UUID   `json:"user_id"`
	MerchantID uuid.UUID   `json:"merchant_id"`
	Channel    string      `json:"channel"`
	Total      money.Money `json:"total"`
	Reason     string      `json:"reason"`
	Note       string      `json:"note"`
}

// order tells the shop about new and cancelled orders, and the customer about each step.
// The handover code is never put in a notification: it shows on lock screens.
func (s *Service) order(ctx context.Context, m outbox.Message, ev orderEvent) error {
	var shop string
	if err := s.Pool.QueryRow(ctx, `SELECT trade_name FROM merchant.merchants WHERE id = $1`, ev.MerchantID).Scan(&shop); err != nil {
		if err == pgx.ErrNoRows {
			return nil
		}
		return err
	}
	args := map[string]string{"shop": shop, "number": ev.Number, "amount": ev.Total.Display(), "note": ev.Note}
	data := map[string]string{"kind": "order", "order_id": ev.OrderID.String()}

	toShop := func(kind string) error {
		data["merchant_id"] = ev.MerchantID.String()
		rows, err := s.Pool.Query(ctx, `
			SELECT user_id FROM merchant.members WHERE merchant_id = $1 AND role IN ('owner', 'manager', 'cashier')`, ev.MerchantID)
		if err != nil {
			return err
		}
		users, err := pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
		if err != nil {
			return err
		}
		for _, u := range users {
			if err := s.add(ctx, m, u, kind, args, data); err != nil {
				return err
			}
		}
		return nil
	}

	switch m.Topic {
	case "order.placed":
		return toShop("order_new_" + ev.Channel)
	case "order.accepted":
		return s.add(ctx, m, ev.UserID, "order_accepted", args, data)
	case "order.ready":
		if ev.Channel != "pickup" {
			return nil // delivery orders: the customer hears when it's on the way
		}
		return s.add(ctx, m, ev.UserID, "order_ready_pickup", args, data)
	case "order.on_the_way":
		return s.add(ctx, m, ev.UserID, "order_on_the_way", args, data)
	case "order.delivered":
		return s.add(ctx, m, ev.UserID, "order_delivered", args, data)
	case "order.cancelled":
		if ev.Reason == "customer_cancelled" {
			return toShop("order_cancelled_by_customer")
		}
		kind := "order_cancelled_" + ev.Reason
		if _, ok := texts["en"][kind+".body"]; !ok {
			kind = "order_cancelled"
		}
		note := strings.TrimSpace(ev.Note)
		if note != "" && !strings.ContainsAny(note[len(note)-1:], ".!?") && !strings.HasSuffix(note, "።") {
			note += "."
		}
		args["note"] = note
		return s.add(ctx, m, ev.UserID, kind, args, data)
	}
	return nil
}
