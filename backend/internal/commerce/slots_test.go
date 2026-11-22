package commerce_test

import (
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"superapp/backend/internal/commerce"
	"superapp/backend/internal/money"
)

// slotShop turns on delivery times at a new shop: hourly, all day, two days ahead, no notice, capacity 2,
// so there are always free times whatever the hour the test runs.
func (e *env) slotShop() (owner, shop, berbere uuid.UUID) {
	e.t.Helper()
	owner, shop, _, berbere = e.shop()
	if _, err := e.svc.UpdateShop(e.ctx, owner, shop, commerce.ShopUpdate{Open: true, Pickup: true, Delivery: true,
		DeliveryFee: money.New(4000, money.ETB), DeliveryArea: "Bole",
		Slots: &commerce.SlotRules{Minutes: 60, From: 0, To: 24 * 60, Capacity: 2, Days: 2, Lead: 0}}); err != nil {
		e.t.Fatal(err)
	}
	return owner, shop, berbere
}

func (e *env) slots(shop uuid.UUID) []commerce.Slot {
	e.t.Helper()
	list, err := e.svc.Slots(e.ctx, shop)
	if err != nil {
		e.t.Fatal(err)
	}
	return list
}

func (e *env) left(shop uuid.UUID, start time.Time) int {
	e.t.Helper()
	for _, s := range e.slots(shop) {
		if s.Start.Equal(start) {
			return s.Left
		}
	}
	e.t.Fatalf("no slot at %v", start)
	return 0
}

func (e *env) deliver(user, shop, product uuid.UUID, slot *time.Time) (*commerce.Order, error) {
	return e.svc.Place(e.ctx, user, commerce.PlaceRequest{MerchantID: shop, Channel: "delivery", DeliveryAddress: "Bole, near Edna Mall",
		SlotStart: slot, Items: []commerce.LineRequest{{ProductID: product, Quantity: 1}}})
}

func TestDeliveryTimesFillUpAndFreeAgain(t *testing.T) {
	e := setup(t)
	owner, shop, berbere := e.slotShop()

	settings, err := e.svc.ShopSettings(e.ctx, owner, shop)
	if err != nil || settings.Slots == nil || settings.Slots.Capacity != 2 {
		t.Fatalf("settings: %v %+v", err, settings)
	}
	var invalid *commerce.ValidationError
	if _, err := e.svc.UpdateShop(e.ctx, owner, shop, commerce.ShopUpdate{Open: true, Pickup: true, Delivery: true,
		DeliveryFee: money.New(0, money.ETB), DeliveryArea: "Bole",
		Slots: &commerce.SlotRules{Minutes: 90, From: 540, To: 1080, Capacity: 2, Days: 2}}); !errors.As(err, &invalid) {
		t.Fatalf("90-minute times: %v", err)
	}

	list := e.slots(shop)
	if len(list) < 24 {
		t.Fatalf("only %d times over two days", len(list))
	}
	last := list[len(list)-1]
	if last.Left != 2 || last.End.Sub(last.Start) != time.Hour {
		t.Fatalf("last time: %+v", last)
	}
	start := last.Start

	hana, abel, sara := e.user("Hana"), e.user("Abel"), e.user("Sara")
	if _, err := e.deliver(hana, shop, berbere, nil); !errors.As(err, &invalid) {
		t.Fatalf("delivery without a time: %v", err)
	}
	if _, err := e.svc.Place(e.ctx, hana, commerce.PlaceRequest{MerchantID: shop, Channel: "pickup", SlotStart: &start,
		Items: []commerce.LineRequest{{ProductID: berbere, Quantity: 1}}}); !errors.As(err, &invalid) {
		t.Fatalf("pickup with a delivery time: %v", err)
	}
	var conflict *commerce.ConflictError
	offGrid := start.Add(time.Minute)
	if _, err := e.deliver(hana, shop, berbere, &offGrid); !errors.As(err, &conflict) {
		t.Fatalf("a time not offered: %v", err)
	}

	first, err := e.deliver(hana, shop, berbere, &start)
	if err != nil || first.SlotStart == nil || !first.SlotStart.Equal(start) || first.SlotEnd == nil || !first.SlotEnd.Equal(last.End) {
		t.Fatalf("first: %v %+v", err, first)
	}
	if _, err := e.deliver(abel, shop, berbere, &start); err != nil {
		t.Fatal(err)
	}
	if n := e.left(shop, start); n != 0 {
		t.Fatalf("left after two: %d", n)
	}
	if _, err := e.deliver(sara, shop, berbere, &start); !errors.As(err, &conflict) {
		t.Fatalf("third order in a full time: %v", err)
	}
	// An unpaid order the customer lets go frees its place.
	if _, err := e.svc.Cancel(e.ctx, hana, first.ID); err != nil {
		t.Fatal(err)
	}
	if n := e.left(shop, start); n != 1 {
		t.Fatalf("left after a cancel: %d", n)
	}
	if _, err := e.deliver(sara, shop, berbere, &start); err != nil {
		t.Fatalf("after the cancel: %v", err)
	}

	// Turning delivery off drops the times; turning it back on without them leaves delivery as soon as possible.
	if _, err := e.svc.UpdateShop(e.ctx, owner, shop, commerce.ShopUpdate{Open: true, Pickup: true, Delivery: false,
		DeliveryFee: money.New(0, money.ETB), Slots: &commerce.SlotRules{Minutes: 60, From: 0, To: 1440, Capacity: 2, Days: 2}}); err != nil {
		t.Fatal(err)
	}
	if got, _ := e.svc.ShopSettings(e.ctx, owner, shop); got.Slots != nil {
		t.Fatalf("times kept without delivery: %+v", got.Slots)
	}
	if list := e.slots(shop); list != nil {
		t.Fatalf("slots listed without delivery: %d", len(list))
	}
}

func TestShopsWithoutDeliveryTimesRefuseOne(t *testing.T) {
	e := setup(t)
	_, shop, _, berbere := e.shop()
	if list := e.slots(shop); list != nil {
		t.Fatalf("slots at a shop without times: %v", list)
	}
	hana := e.user("Hana")
	at := time.Now().Add(3 * time.Hour).Truncate(time.Hour)
	var conflict *commerce.ConflictError
	if _, err := e.deliver(hana, shop, berbere, &at); !errors.As(err, &conflict) {
		t.Fatalf("a time at a shop without times: %v", err)
	}
	o, err := e.deliver(hana, shop, berbere, nil)
	if err != nil || o.SlotStart != nil {
		t.Fatalf("as soon as possible: %v %+v", err, o)
	}
}

func TestTheLastPlaceGoesToOneCustomer(t *testing.T) {
	e := setup(t)
	_, shop, berbere := e.slotShop()
	list := e.slots(shop)
	start := list[len(list)-2].Start

	const tries = 6
	users := make([]uuid.UUID, tries)
	for i := range users {
		users[i] = e.user("Customer")
	}
	var wg sync.WaitGroup
	errs := make([]error, tries)
	for i := range users {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = e.deliver(users[i], shop, berbere, &start)
		}(i)
	}
	wg.Wait()
	ok, full := 0, 0
	for _, err := range errs {
		var conflict *commerce.ConflictError
		switch {
		case err == nil:
			ok++
		case errors.As(err, &conflict):
			full++
		default:
			t.Fatal(err)
		}
	}
	if ok != 2 || full != tries-2 {
		t.Fatalf("%d booked, %d refused; capacity is 2", ok, full)
	}
}

func TestRemindersWaitForTheDeliveryTime(t *testing.T) {
	e := setup(t)
	owner, shop, berbere := e.slotShop()
	list := e.slots(shop)
	start := list[len(list)-1].Start // tomorrow evening
	hana := e.user("Hana")
	e.fund(hana, 500000)
	o, err := e.deliver(hana, shop, berbere, &start)
	if err != nil {
		t.Fatal(err)
	}
	e.checkout(hana, o)
	if _, err := e.svc.Advance(e.ctx, owner, shop, o.ID, "accept"); err != nil {
		t.Fatal(err)
	}
	// Accepted two days ago, but it isn't due until tomorrow evening: no reminder yet.
	e.exec(`UPDATE commerce.orders SET accepted_at = now() - interval '48 hours' WHERE id = $1`, o.ID)
	if _, _, err := e.svc.FlagStalled(e.ctx); err != nil {
		t.Fatal(err)
	}
	if got := e.order(hana, o.ID); got.Overdue {
		t.Fatal("reminded before the delivery time")
	}
	// A day after the delivery time has passed, it is.
	e.exec(`UPDATE commerce.orders SET slot_start = now() - interval '26 hours', slot_end = now() - interval '25 hours' WHERE id = $1`, o.ID)
	if _, _, err := e.svc.FlagStalled(e.ctx); err != nil {
		t.Fatal(err)
	}
	if got := e.order(hana, o.ID); !got.Overdue || got.InReview {
		t.Fatalf("a day after the delivery time: overdue %v, review %v", got.Overdue, got.InReview)
	}
}
