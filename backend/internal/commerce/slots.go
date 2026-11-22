package commerce

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// EAT is Ethiopian time (UTC+3, no daylight saving). Delivery times are always in it, wherever the
// customer is: a diaspora customer books the time it arrives in Addis.
var EAT = time.FixedZone("EAT", 3*60*60)

// SlotRules are a shop's delivery times: slots of Minutes from From to To (minutes after midnight, EAT),
// each taking up to Capacity orders, bookable for Days days (today included) and at least Lead minutes
// ahead.
type SlotRules struct {
	Minutes  int
	From, To int
	Capacity int
	Days     int
	Lead     int
}

// SlotLengths are the slot lengths a shop can choose, in minutes.
var SlotLengths = []int{60, 120, 180, 240}

func (r *SlotRules) check() error {
	okLen := false
	for _, m := range SlotLengths {
		okLen = okLen || r.Minutes == m
	}
	switch {
	case !okLen:
		return &ValidationError{"Delivery times can be 1, 2, 3 or 4 hours long."}
	case r.From < 0 || r.From%30 != 0 || r.To > 24*60 || r.To%30 != 0:
		return &ValidationError{"Delivery hours start and end on the hour or half past."}
	case r.To-r.From < r.Minutes:
		return &ValidationError{"The delivery hours must fit at least one delivery time."}
	case r.Capacity < 1 || r.Capacity > 50:
		return &ValidationError{"Each delivery time can take from 1 to 50 orders."}
	case r.Days < 1 || r.Days > 7:
		return &ValidationError{"Customers can book from 1 to 7 days ahead."}
	case r.Lead < 0 || r.Lead > 24*60:
		return &ValidationError{"The time you need before a delivery can be up to 24 hours."}
	}
	return nil
}

// Slot is one delivery time and how many more orders it can take.
type Slot struct {
	Start, End time.Time
	Left       int
}

// windows lists the slots bookable at now, without how full they are.
func (r *SlotRules) windows(now time.Time) []Slot {
	local := now.In(EAT)
	midnight := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, EAT)
	earliest := now.Add(time.Duration(r.Lead) * time.Minute)
	var out []Slot
	for d := 0; d < r.Days; d++ {
		day := midnight.AddDate(0, 0, d)
		for m := r.From; m+r.Minutes <= r.To; m += r.Minutes {
			start := day.Add(time.Duration(m) * time.Minute)
			if start.Before(earliest) {
				continue
			}
			out = append(out, Slot{Start: start, End: start.Add(time.Duration(r.Minutes) * time.Minute), Left: r.Capacity})
		}
	}
	return out
}

// slotRules reads a shop's delivery-time rules; nil when it has none.
func slotRules(ctx context.Context, db interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}, merchant uuid.UUID) (*SlotRules, error) {
	var mins, from, to, capacity, days, lead *int
	err := db.QueryRow(ctx, `SELECT slot_minutes, slot_from, slot_to, slot_capacity, slot_days, slot_lead_minutes
		FROM commerce.shops WHERE merchant_id = $1`, merchant).Scan(&mins, &from, &to, &capacity, &days, &lead)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && mins == nil) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &SlotRules{Minutes: *mins, From: *from, To: *to, Capacity: *capacity, Days: *days, Lead: *lead}, nil
}

// slotsTaken counts the orders holding each slot start: every order not cancelled, including those
// still being paid for (they lapse within PayWithin and free the place).
const slotsTaken = `
	SELECT slot_start, count(*) FROM commerce.orders
	WHERE merchant_id = $1 AND slot_start >= $2 AND slot_start < $3 AND status NOT IN ('cancelled', 'refunded')
	GROUP BY slot_start`

// Slots lists the delivery times a customer can book at a shop now, with the places left in each (full
// ones included, with Left 0). Nil when the shop doesn't deliver or doesn't use delivery times.
func (s *Service) Slots(ctx context.Context, merchant uuid.UUID) ([]Slot, error) {
	var delivery bool
	err := s.Pool.QueryRow(ctx, `SELECT s.delivery FROM commerce.shops s JOIN merchant.merchants m ON m.id = s.merchant_id
		WHERE s.merchant_id = $1 AND m.status = 'active'`, merchant).Scan(&delivery)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	rules, err := slotRules(ctx, s.Pool, merchant)
	if err != nil || rules == nil || !delivery {
		return nil, err
	}
	list := rules.windows(s.clock())
	if len(list) == 0 {
		return []Slot{}, nil
	}
	rows, err := s.Pool.Query(ctx, slotsTaken, merchant, list[0].Start, list[len(list)-1].End)
	if err != nil {
		return nil, err
	}
	taken := map[int64]int{}
	for rows.Next() {
		var at time.Time
		var n int
		if err := rows.Scan(&at, &n); err != nil {
			rows.Close()
			return nil, err
		}
		taken[at.Unix()] = n
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for i := range list {
		list[i].Left = max(0, rules.Capacity-taken[list[i].Start.Unix()])
	}
	return list, nil
}

// bookSlot checks a delivery order's chosen time inside the order's transaction and returns its end.
// An advisory lock per shop and slot makes two customers taking the last place wait for each other.
func (s *Service) bookSlot(ctx context.Context, tx pgx.Tx, merchant uuid.UUID, start *time.Time) (*time.Time, error) {
	rules, err := slotRules(ctx, tx, merchant)
	if err != nil {
		return nil, err
	}
	if rules == nil {
		if start != nil {
			return nil, &ConflictError{"This shop doesn't take delivery times. Order without one."}
		}
		return nil, nil
	}
	if start == nil {
		return nil, &ValidationError{"Choose a delivery time."}
	}
	var slot *Slot
	for _, w := range rules.windows(s.clock()) {
		if w.Start.Equal(*start) {
			slot = &w
			break
		}
	}
	if slot == nil {
		return nil, &ConflictError{"That delivery time can't be booked any more. Choose another."}
	}
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('commerce.slot:' || $1::text || ':' || ($2::bigint)::text, 0))`,
		merchant, slot.Start.Unix()); err != nil {
		return nil, err
	}
	var taken int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM commerce.orders WHERE merchant_id = $1 AND slot_start = $2
		AND status NOT IN ('cancelled', 'refunded')`, merchant, slot.Start).Scan(&taken); err != nil {
		return nil, err
	}
	if taken >= rules.Capacity {
		return nil, &ConflictError{"That delivery time just filled up. Choose another."}
	}
	return &slot.End, nil
}
