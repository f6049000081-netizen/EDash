package bizweb_test

import (
	"net/http"
	"net/url"
	"strings"
	"testing"

	"superapp/backend/internal/commerce"
	"superapp/backend/internal/money"
)

func TestOwnersSetDeliveryTimesAndTheSwitchKeepsThem(t *testing.T) {
	e := setup(t)
	owner, _, merchant, _ := e.shop()
	b := e.browser()
	b.signIn(owner, merchant)
	csrf := b.csrf("/business/storefront")

	form := func(extra url.Values) url.Values {
		v := url.Values{"csrf": {csrf}, "open": {"1"}, "pickup": {"1"}, "delivery": {"1"}, "fee": {"40"}, "area": {"Bole"},
			"slot_from": {"540"}, "slot_to": {"1080"}, "slot_minutes": {"120"}, "slot_capacity": {"4"}, "slot_days": {"3"},
			"slot_lead": {"60"}}
		for k, x := range extra {
			v[k] = x
		}
		return v
	}
	// Times that don't fit are refused with the reason, and the form keeps what was chosen.
	st, page, _ := b.post("/business/storefront", form(url.Values{"slots": {"1"}, "slot_to": {"600"}, "slot_minutes": {"240"}}))
	if st != http.StatusUnprocessableEntity || !strings.Contains(page, "must fit at least one delivery time") ||
		!strings.Contains(page, `<option value="240" selected>`) {
		t.Fatalf("times that don't fit: %d", st)
	}
	if _, _, loc := b.post("/business/storefront", form(url.Values{"slots": {"1"}})); loc != "/business/storefront?msg=saved" {
		t.Fatalf("save: %q", loc)
	}
	want := commerce.SlotRules{Minutes: 120, From: 540, To: 1080, Capacity: 4, Days: 3, Lead: 60}
	shop, err := e.shops.ShopSettings(e.ctx, owner, merchant)
	if err != nil || shop.Slots == nil || *shop.Slots != want {
		t.Fatalf("saved times: %v %+v", err, shop.Slots)
	}
	if _, page, _ := b.get("/business/storefront"); !strings.Contains(page, `name="slots" value="1" checked`) ||
		!strings.Contains(page, `<option value="540" selected>09:00</option>`) {
		t.Error("the form doesn't show the saved times")
	}

	// The home page's open switch leaves them alone.
	if _, _, loc := b.post("/business/storefront/open", url.Values{"csrf": {csrf}, "open": {"0"}}); loc != "/business/?msg=saved" {
		t.Fatalf("close: %q", loc)
	}
	if shop, _ := e.shops.ShopSettings(e.ctx, owner, merchant); shop.Open || shop.Slots == nil || *shop.Slots != want {
		t.Fatalf("after the switch: %+v", shop)
	}

	// Unticking them goes back to delivering as soon as possible.
	if _, _, loc := b.post("/business/storefront", form(nil)); loc != "/business/storefront?msg=saved" {
		t.Fatalf("save without times: %q", loc)
	}
	if shop, _ := e.shops.ShopSettings(e.ctx, owner, merchant); shop.Slots != nil {
		t.Fatalf("times kept: %+v", shop.Slots)
	}
}

func TestBookedDeliveryTimesShowOnOrders(t *testing.T) {
	e := setup(t)
	owner, _, merchant, injera := e.shop()
	if _, err := e.shops.UpdateShop(e.ctx, owner, merchant, commerce.ShopUpdate{Open: true, Pickup: true, Delivery: true,
		DeliveryFee: money.New(0, money.ETB), DeliveryArea: "Bole",
		Slots: &commerce.SlotRules{Minutes: 60, From: 0, To: 1440, Capacity: 5, Days: 2, Lead: 0}}); err != nil {
		t.Fatal(err)
	}
	list, err := e.shops.Slots(e.ctx, merchant)
	if err != nil || len(list) == 0 {
		t.Fatalf("slots: %v", err)
	}
	slot := list[len(list)-1]
	customer := e.user("Hana")
	if _, err := e.pay.SandboxTopUp(e.ctx, customer, money.New(500000, money.ETB)); err != nil {
		t.Fatal(err)
	}
	o, err := e.shops.Place(e.ctx, customer, commerce.PlaceRequest{MerchantID: merchant, Channel: "delivery",
		DeliveryAddress: "Bole", SlotStart: &slot.Start, Items: []commerce.LineRequest{{ProductID: injera, Quantity: 1}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.pay.Confirm(e.ctx, customer, *o.PaymentIntentID, pin); err != nil {
		t.Fatal(err)
	}

	b := e.browser()
	b.signIn(owner, merchant)
	when := slot.Start.In(commerce.EAT).Format("Mon 2 Jan, 15:04") + "–"
	if _, page, _ := b.get("/business/orders"); !strings.Contains(page, when) {
		t.Errorf("the order list doesn't show %q", when)
	}
	if _, page, _ := b.get("/business/orders/" + o.ID.String()); !strings.Contains(page, "Deliver "+when) {
		t.Errorf("the order page doesn't show %q", when)
	}
}
