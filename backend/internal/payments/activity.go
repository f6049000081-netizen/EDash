package payments

import (
	"context"
	"encoding/base64"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"superapp/backend/internal/money"
)

// ActivityItem is one line in the user's money history.
type ActivityItem struct {
	ID          uuid.UUID
	Type        string // payment, transfer_in, transfer_out, topup
	Title       string // counterparty: merchant, person, or the rail for top-ups
	Subtitle    string
	Direction   string // in, out
	Amount      money.Money
	Status      string // pending, completed, failed, refunded
	Charged     *bool  // outgoing only: did money leave the wallet
	FailureCode string
	OccurredAt  time.Time
}

// ActivityPage is newest first. NextCursor is empty on the last page.
type ActivityPage struct {
	Items      []ActivityItem
	NextCursor string
}

var railNames = map[string]string{
	"telebirr": "telebirr", "cbe_birr": "CBE Birr", "mpesa_et": "M-PESA", "ethswitch": "Bank transfer",
	"card": "Card", "remittance": "Money from abroad", "wallet": "Wallet", "sandbox": "Test money (sandbox)",
}

// Activity lists what the user sent, received and topped up. Intents that were never confirmed or
// were cancelled are left out: no money moved and the user didn't finish them. filter is "all",
// "money" or "orders" (no orders exist yet, so that page is empty).
func (s *Service) Activity(ctx context.Context, user uuid.UUID, filter, cursor string, limit int) (*ActivityPage, error) {
	switch filter {
	case "", "all", "money":
	case "orders":
		return &ActivityPage{Items: []ActivityItem{}}, nil
	default:
		return nil, &ValidationError{"filter must be all, money or orders"}
	}
	if limit <= 0 {
		limit = 25
	}
	if limit > 100 {
		limit = 100
	}
	beforeTime, beforeID := time.Date(9999, 1, 1, 0, 0, 0, 0, time.UTC), uuid.Max
	if cursor != "" {
		var err error
		if beforeTime, beforeID, err = decodeCursor(cursor); err != nil {
			return nil, &ValidationError{"That page link is no longer valid. Reload the list."}
		}
	}

	// Two index-friendly branches (sent by me, received by me), each limited, then merged.
	rows, err := s.Pool.Query(ctx, `
		WITH mine AS (
			(SELECT i.id, i.kind, i.status, i.amount_minor, i.fee_minor, i.currency, i.rail, i.failure_code,
			        i.created_at, i.completed_at, i.payee_type, i.payee_id AS other, 'out' AS dir
			 FROM payments.intents i
			 WHERE i.payer_user_id = $1
			   AND i.status NOT IN ('created', 'requires_confirmation', 'cancelled')
			   AND (i.created_at, i.id) < ($2, $3)
			 ORDER BY i.created_at DESC, i.id DESC LIMIT $4)
			UNION ALL
			(SELECT i.id, i.kind, i.status, i.amount_minor, 0, i.currency, i.rail, NULL,
			        i.created_at, i.completed_at, 'user', i.payer_user_id, 'in'
			 FROM payments.intents i
			 WHERE i.payee_type = 'user' AND i.payee_id = $1 AND i.kind = 'p2p'
			   AND i.status IN ('succeeded', 'reversed')
			   AND (i.created_at, i.id) < ($2, $3)
			 ORDER BY i.created_at DESC, i.id DESC LIMIT $4)
		)
		SELECT m.id, m.kind, m.status::text, m.amount_minor, m.fee_minor, m.currency::text, m.rail::text,
		       COALESCE(m.failure_code, ''), m.created_at, m.completed_at, m.dir,
		       COALESCE(u.display_name, ''), COALESCE(right(u.phone_e164, 4), ''), COALESCE(mc.trade_name, '')
		FROM mine m
		LEFT JOIN identity.users u ON m.payee_type = 'user' AND u.id = m.other
		LEFT JOIN merchant.merchants mc ON m.payee_type = 'merchant' AND mc.id = m.other
		ORDER BY m.created_at DESC, m.id DESC
		LIMIT $4`, user, beforeTime, beforeID, limit+1)
	if err != nil {
		return nil, err
	}
	type row struct {
		id                          uuid.UUID
		kind, status, cur, rail     string
		failure, dir                string
		amount, fee                 int64
		created                     time.Time
		completed                   *time.Time
		personName, last4, merchant string
	}
	list, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (row, error) {
		var x row
		err := r.Scan(&x.id, &x.kind, &x.status, &x.amount, &x.fee, &x.cur, &x.rail, &x.failure,
			&x.created, &x.completed, &x.dir, &x.personName, &x.last4, &x.merchant)
		return x, err
	})
	if err != nil {
		return nil, err
	}

	page := &ActivityPage{Items: make([]ActivityItem, 0, len(list))}
	if len(list) > limit {
		last := list[limit-1]
		page.NextCursor = encodeCursor(last.created, last.id)
		list = list[:limit]
	}
	for _, x := range list {
		it := ActivityItem{ID: x.id, Direction: x.dir, FailureCode: x.failure, OccurredAt: x.created}
		if x.completed != nil {
			it.OccurredAt = *x.completed
		}
		person := x.personName
		if person == "" && x.last4 != "" {
			person = "•••• " + x.last4
		}
		switch {
		case x.kind == string(KindTopUp):
			it.Type, it.Direction, it.Subtitle = "topup", "in", "Top-up"
			it.Title = railNames[x.rail]
		case x.kind == string(KindQRMerchant):
			it.Type, it.Title, it.Subtitle = "payment", x.merchant, "QR payment"
		case x.dir == "in":
			it.Type, it.Title, it.Subtitle = "transfer_in", person, "Received"
		default:
			it.Type, it.Title, it.Subtitle = "transfer_out", person, "Sent"
		}
		amount := x.amount
		if it.Direction == "out" {
			amount += x.fee // what left the wallet
		}
		it.Amount = money.New(amount, money.Currency(x.cur))
		switch x.status {
		case "succeeded":
			it.Status = "completed"
		case "failed":
			it.Status = "failed"
		case "reversed":
			it.Status = "refunded"
		default: // processing, held_for_review
			it.Status = "pending"
		}
		if it.Direction == "out" {
			charged := it.Status == "completed"
			it.Charged = &charged
		}
		page.Items = append(page.Items, it)
	}
	return page, nil
}

// The cursor is opaque to clients: base64url of "<created_at unix nanos>.<intent id>".
func encodeCursor(t time.Time, id uuid.UUID) string {
	return base64.RawURLEncoding.EncodeToString([]byte(strconv.FormatInt(t.UnixNano(), 10) + "." + id.String()))
}

func decodeCursor(c string) (time.Time, uuid.UUID, error) {
	raw, err := base64.RawURLEncoding.DecodeString(c)
	if err != nil {
		return time.Time{}, uuid.Nil, err
	}
	ns, idStr, ok := strings.Cut(string(raw), ".")
	if !ok {
		return time.Time{}, uuid.Nil, fmt.Errorf("bad cursor")
	}
	n, err := strconv.ParseInt(ns, 10, 64)
	if err != nil {
		return time.Time{}, uuid.Nil, err
	}
	id, err := uuid.Parse(idStr)
	if err != nil {
		return time.Time{}, uuid.Nil, err
	}
	return time.Unix(0, n).UTC(), id, nil
}
