package commerce_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"superapp/backend/internal/commerce"
	"superapp/backend/internal/ledger"
	"superapp/backend/internal/money"
	"superapp/backend/internal/payments"
	"superapp/backend/internal/platform"
)

const pin = "482913"

var (
	poolOnce sync.Once
	pool     *pgxpool.Pool
	poolErr  error
	pinOnce  sync.Once
	pinHash  string
)

type env struct {
	t   *testing.T
	ctx context.Context
	pay *payments.Service
	svc *commerce.Service
}

func setup(t *testing.T) *env {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}
	poolOnce.Do(func() { pool, poolErr = pgxpool.New(context.Background(), dsn) })
	if poolErr != nil {
		t.Fatal(poolErr)
	}
	rules := payments.DefaultRules()
	rules.VelocityMax = 0
	pay := payments.NewService(pool, ledger.New(), rules, &platform.PINVerifier{Pool: pool})
	return &env{t: t, ctx: context.Background(), pay: pay, svc: commerce.New(pool, pay)}
}

func (e *env) exec(sql string, args ...any) {
	e.t.Helper()
	if _, err := pool.Exec(e.ctx, sql, args...); err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) user(name string) uuid.UUID {
	e.t.Helper()
	pinOnce.Do(func() { pinHash, _ = platform.HashPIN(pin) })
	id := uuid.New()
	e.exec(`INSERT INTO identity.users (id, phone_e164, display_name, status, kyc_level) VALUES ($1, $2, $3, 'active', 'L2')`,
		id, fmt.Sprintf("+2519%08d", id.ID()%100_000_000), name)
	e.exec(`INSERT INTO identity.credentials (user_id, pin_hash) VALUES ($1, $2)`, id, pinHash)
	return id
}

func (e *env) fund(user uuid.UUID, minor int64) {
	e.t.Helper()
	if _, err := e.pay.SandboxTopUp(e.ctx, user, money.New(minor, money.ETB)); err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) balance(kind ledger.AccountKind, owner ledger.OwnerType, id uuid.UUID) int64 {
	var b int64
	_ = pool.QueryRow(e.ctx, `SELECT COALESCE((SELECT b.balance_minor FROM ledger.accounts a JOIN ledger.balances b ON b.account_id = a.id
		WHERE a.kind = $1 AND a.owner_type = $2 AND a.owner_id IS NOT DISTINCT FROM $3 AND a.currency = 'ETB'), 0)`,
		string(kind), string(owner), uuid.NullUUID{UUID: id, Valid: id != uuid.Nil}).Scan(&b)
	return b
}

func (e *env) wallet(u uuid.UUID) int64 { return e.balance(ledger.UserWallet, ledger.OwnerUser, u) }

// shop makes an approved merchant owned by a new user, open for pickup and delivery (ETB 40 fee), with
// injera (counted stock 3) and berbere (not counted), and a 1% commission.
func (e *env) shop() (owner, merchant, injera, berbere uuid.UUID) {
	e.t.Helper()
	owner, merchant = e.user("Owner"), uuid.New()
	e.exec(`INSERT INTO merchant.merchants (id, legal_name, trade_name, category_code, status) VALUES ($1, 'Merkato Fresh PLC', 'Merkato Fresh', '5411', 'active')`, merchant)
	e.exec(`INSERT INTO merchant.members (merchant_id, user_id, role) VALUES ($1, $2, 'owner')`, merchant, owner)
	e.exec(`INSERT INTO merchant.commission_plans (merchant_id, channel, rate_bps, valid_from, approved_by) VALUES ($1, 'qr', 100, now() - interval '1 day', $2)`, merchant, uuid.New())
	three := 3
	p1, err := e.svc.CreateProduct(e.ctx, owner, merchant, commerce.ProductInput{Title: "Injera, 10 pcs", Category: "Bakery",
		Price: money.New(32000, money.ETB), Stock: &three})
	if err != nil {
		e.t.Fatal(err)
	}
	p2, err := e.svc.CreateProduct(e.ctx, owner, merchant, commerce.ProductInput{Title: "Berbere, 500 g", Category: "Spices",
		Price: money.New(21000, money.ETB)})
	if err != nil {
		e.t.Fatal(err)
	}
	if _, err := e.svc.UpdateShop(e.ctx, owner, merchant, commerce.ShopUpdate{Open: true, Pickup: true, Delivery: true,
		DeliveryFee: money.New(4000, money.ETB), DeliveryArea: "Bole and Kazanchis"}); err != nil {
		e.t.Fatal(err)
	}
	return owner, merchant, p1.ID, p2.ID
}

func (e *env) checkout(user uuid.UUID, o *commerce.Order) *payments.Intent {
	e.t.Helper()
	in, err := e.pay.Confirm(e.ctx, user, *o.PaymentIntentID, pin)
	if err != nil {
		e.t.Fatalf("confirm: %v", err)
	}
	return in
}

func (e *env) order(user, id uuid.UUID) *commerce.Order {
	e.t.Helper()
	o, err := e.svc.Order(e.ctx, user, id)
	if err != nil {
		e.t.Fatal(err)
	}
	return o
}

func (e *env) stock(product uuid.UUID) int {
	var n int
	_ = pool.QueryRow(e.ctx, `SELECT stock FROM commerce.products WHERE id = $1`, product).Scan(&n)
	return n
}

func TestPickupOrderFromBasketToHandover(t *testing.T) {
	e := setup(t)
	owner, shop, injera, berbere := e.shop()
	hana := e.user("Hana")
	e.fund(hana, 500000)

	list, err := e.svc.Shops(e.ctx, 100)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, s := range list {
		found = found || (s.MerchantID == shop && s.ProductCount == 2 && s.Open)
	}
	if !found {
		t.Fatalf("shop not listed")
	}

	o, err := e.svc.Place(e.ctx, hana, commerce.PlaceRequest{MerchantID: shop, Channel: "pickup", Note: "No plastic bag",
		Items: []commerce.LineRequest{{ProductID: injera, Quantity: 2}, {ProductID: berbere, Quantity: 1}}})
	if err != nil {
		t.Fatal(err)
	}
	if o.Status != "pending_payment" || o.Total.AmountMinor != 85000 || o.DeliveryFee.AmountMinor != 0 || len(o.Items) != 2 ||
		o.HandoverCode != "" || e.stock(injera) != 1 {
		t.Fatalf("placed: %+v stock %d", o, e.stock(injera))
	}
	// The shop doesn't see unpaid orders.
	if got, _ := e.svc.ShopOrders(e.ctx, owner, shop, true, 50); len(got) != 0 {
		t.Fatalf("shop sees an unpaid order")
	}
	if in := e.checkout(hana, o); in.Status != payments.StatusSucceeded {
		t.Fatalf("payment %s", in.Status)
	}
	escrowBefore := e.balance(ledger.OrderEscrow, ledger.OwnerPlatform, uuid.Nil)
	if e.wallet(hana) != 415000 || e.order(hana, o.ID).Status != "placed" || len(e.order(hana, o.ID).HandoverCode) != 4 {
		t.Fatalf("after paying: wallet %d, %s", e.wallet(hana), e.order(hana, o.ID).Status)
	}

	got, err := e.svc.ShopOrders(e.ctx, owner, shop, true, 50)
	if err != nil || len(got) != 1 || got[0].HandoverCode != "" || got[0].CustomerName != "Hana" || !strings.HasPrefix(got[0].CustomerPhone, "••") ||
		got[0].Note != "No plastic bag" || got[0].AcceptBy == nil {
		t.Fatalf("shop's view: %v %+v", err, got)
	}
	for _, step := range []string{"accept", "ready"} {
		if _, err := e.svc.Advance(e.ctx, owner, shop, o.ID, step); err != nil {
			t.Fatalf("%s: %v", step, err)
		}
	}
	if _, err := e.svc.Advance(e.ctx, owner, shop, o.ID, "on_the_way"); err == nil {
		t.Error("a pickup order went on the way")
	}
	// The customer can't cancel once the shop has started.
	var conflict *commerce.ConflictError
	if _, err := e.svc.Cancel(e.ctx, hana, o.ID); !errors.As(err, &conflict) {
		t.Errorf("late cancel: %v", err)
	}

	code := e.order(hana, o.ID).HandoverCode
	wrong := "0000"
	if code == wrong {
		wrong = "1111"
	}
	if _, err := e.svc.HandOver(e.ctx, owner, shop, o.ID, wrong); !errors.Is(err, commerce.ErrWrongCode) {
		t.Fatalf("wrong code: %v", err)
	}
	done, err := e.svc.HandOver(e.ctx, owner, shop, o.ID, code)
	if err != nil || done.Status != "delivered" {
		t.Fatalf("handover: %v %+v", err, done)
	}
	// Escrow paid the shop, less 1% commission.
	if s := e.balance(ledger.MerchantSettlement, ledger.OwnerMerchant, shop); s != 84150 {
		t.Errorf("shop settlement %d", s)
	}
	if d := escrowBefore - e.balance(ledger.OrderEscrow, ledger.OwnerPlatform, uuid.Nil); d != 85000 {
		t.Errorf("escrow released %d", d)
	}
	if e.order(hana, o.ID).HandoverCode != "" {
		t.Error("code still shown after handover")
	}
	var topics []string
	rows, _ := pool.Query(e.ctx, `SELECT topic FROM platform.outbox WHERE aggregate_id = $1 ORDER BY id`, o.ID)
	for rows.Next() {
		var s string
		_ = rows.Scan(&s)
		topics = append(topics, s)
	}
	if fmt.Sprint(topics) != "[order.placed order.accepted order.ready order.delivered]" {
		t.Errorf("events %v", topics)
	}
	// The customer's activity shows the order.
	page, err := e.pay.Activity(e.ctx, hana, "orders", "", 10)
	if err != nil || len(page.Items) != 1 || page.Items[0].Title != "Merkato Fresh" || page.Items[0].Type != "order" {
		t.Errorf("activity: %v %+v", err, page)
	}
}

func TestCancelledOrdersAreRefundedAndStockReturns(t *testing.T) {
	e := setup(t)
	owner, shop, injera, _ := e.shop()
	hana := e.user("Hana")
	e.fund(hana, 500000)
	place := func(channel string, qty int) *commerce.Order {
		o, err := e.svc.Place(e.ctx, hana, commerce.PlaceRequest{MerchantID: shop, Channel: channel, DeliveryAddress: "Bole, near Edna Mall",
			Items: []commerce.LineRequest{{ProductID: injera, Quantity: qty}}})
		if err != nil {
			t.Fatal(err)
		}
		return o
	}

	// Customer cancels before the shop accepts: refunded at once.
	a := place("delivery", 1)
	if a.Total.AmountMinor != 36000 {
		t.Fatalf("delivery total %d", a.Total.AmountMinor)
	}
	e.checkout(hana, a)
	if _, err := e.svc.Cancel(e.ctx, hana, a.ID); err != nil {
		t.Fatal(err)
	}
	if e.wallet(hana) != 500000 || e.stock(injera) != 3 {
		t.Fatalf("after cancel: wallet %d stock %d", e.wallet(hana), e.stock(injera))
	}
	page, _ := e.pay.Activity(e.ctx, hana, "orders", "", 10)
	if len(page.Items) != 1 || page.Items[0].Status != "refunded" {
		t.Errorf("activity: %+v", page.Items)
	}

	// Not enough stock.
	var conflict *commerce.ConflictError
	if _, err := e.svc.Place(e.ctx, hana, commerce.PlaceRequest{MerchantID: shop, Channel: "pickup",
		Items: []commerce.LineRequest{{ProductID: injera, Quantity: 4}}}); !errors.As(err, &conflict) {
		t.Errorf("over stock: %v", err)
	}

	// The shop cancels an accepted delivery order with a note.
	b := place("delivery", 2)
	e.checkout(hana, b)
	if _, err := e.svc.Advance(e.ctx, owner, shop, b.ID, "accept"); err != nil {
		t.Fatal(err)
	}
	shopView, _ := e.svc.ShopOrders(e.ctx, owner, shop, true, 10)
	if len(shopView) != 1 || shopView[0].CustomerPhone[:5] != "+2519" || shopView[0].DeliveryAddress != "Bole, near Edna Mall" {
		t.Errorf("delivery order view: %+v", shopView)
	}
	if _, err := e.svc.CancelByShop(e.ctx, owner, shop, b.ID, ""); err == nil {
		t.Error("cancel without a note")
	}
	if _, err := e.svc.CancelByShop(e.ctx, owner, shop, b.ID, "Our oven broke today, sorry"); err != nil {
		t.Fatal(err)
	}
	if got := e.order(hana, b.ID); got.Status != "cancelled" || got.CancelReason != "shop_cancelled" ||
		got.CancelNote != "Our oven broke today, sorry" || e.wallet(hana) != 500000 || e.stock(injera) != 3 {
		t.Fatalf("shop cancel: %+v wallet %d", got, e.wallet(hana))
	}

	// Never paid: the sweep cancels it and the stock returns; a paid order the shop ignores is refunded.
	c := place("pickup", 1)
	d := place("pickup", 1)
	e.checkout(hana, d)
	e.exec(`UPDATE commerce.orders SET created_at = now() - interval '1 hour', placed_at = now() - interval '1 hour' WHERE id = ANY($1)`,
		[]uuid.UUID{c.ID, d.ID})
	e.exec(`UPDATE commerce.orders SET placed_at = NULL WHERE id = $1`, c.ID)
	if _, err := e.svc.RunOnce(e.ctx); err != nil {
		t.Fatal(err)
	}
	if got := e.order(hana, c.ID); got.Status != "cancelled" || got.CancelReason != "payment_not_finished" {
		t.Errorf("unpaid: %+v", got)
	}
	if got := e.order(hana, d.ID); got.Status != "cancelled" || got.CancelReason != "no_response" {
		t.Errorf("ignored: %+v", got)
	}
	if e.wallet(hana) != 500000 || e.stock(injera) != 3 {
		t.Errorf("after sweep: wallet %d stock %d", e.wallet(hana), e.stock(injera))
	}
	if _, err := e.pay.Confirm(e.ctx, hana, *c.PaymentIntentID, pin); err != nil {
		in, _ := e.pay.Get(e.ctx, hana, *c.PaymentIntentID)
		if in.Status != payments.StatusCancelled {
			t.Errorf("paying a cancelled order: %v %s", err, in.Status)
		}
	}
	if e.wallet(hana) != 500000 {
		t.Errorf("charged for a cancelled order: %d", e.wallet(hana))
	}
}

func TestFailedPaymentCancelsTheOrderAndRulesHold(t *testing.T) {
	e := setup(t)
	owner, shop, injera, berbere := e.shop()
	hana := e.user("Hana")
	e.fund(hana, 30000) // not enough
	o, err := e.svc.Place(e.ctx, hana, commerce.PlaceRequest{MerchantID: shop, Channel: "pickup",
		Items: []commerce.LineRequest{{ProductID: injera, Quantity: 1}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.pay.Confirm(e.ctx, hana, *o.PaymentIntentID, pin); !errors.Is(err, payments.ErrInsufficientFunds) {
		t.Fatalf("want insufficient funds, got %v", err)
	}
	if got := e.order(hana, o.ID); got.Status != "cancelled" || got.CancelReason != "payment_failed" || e.stock(injera) != 3 {
		t.Errorf("after failed payment: %+v stock %d", got, e.stock(injera))
	}
	orders, _ := e.svc.Orders(e.ctx, hana, 10)
	if len(orders) != 0 {
		t.Errorf("unpaid orders listed: %+v", orders)
	}

	// Owners can't buy from their own shop; staff without the role can't change the catalog.
	var conflict *commerce.ConflictError
	if _, err := e.svc.Place(e.ctx, owner, commerce.PlaceRequest{MerchantID: shop, Channel: "pickup",
		Items: []commerce.LineRequest{{ProductID: berbere, Quantity: 1}}}); !errors.As(err, &conflict) {
		t.Errorf("own shop: %v", err)
	}
	cashier := e.user("Cashier")
	e.exec(`INSERT INTO merchant.members (merchant_id, user_id, role) VALUES ($1, $2, 'cashier')`, shop, cashier)
	if _, err := e.svc.CreateProduct(e.ctx, cashier, shop, commerce.ProductInput{Title: "Shiro", Price: money.New(100, money.ETB)}); !errors.Is(err, commerce.ErrForbidden) {
		t.Errorf("cashier added a product: %v", err)
	}
	if _, err := e.svc.ShopOrders(e.ctx, hana, shop, true, 10); !errors.Is(err, commerce.ErrNotFound) {
		t.Errorf("stranger read orders: %v", err)
	}
	// A closed shop takes no orders.
	if _, err := e.svc.UpdateShop(e.ctx, owner, shop, commerce.ShopUpdate{Pickup: true, DeliveryFee: money.New(0, money.ETB)}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.Place(e.ctx, hana, commerce.PlaceRequest{MerchantID: shop, Channel: "pickup",
		Items: []commerce.LineRequest{{ProductID: berbere, Quantity: 1}}}); !errors.As(err, &conflict) {
		t.Errorf("closed shop: %v", err)
	}

}

func TestCustomerConfirmsWhenTheCodeIsLockedOut(t *testing.T) {
	e := setup(t)
	owner, shop, _, berbere := e.shop()
	hana := e.user("Hana")
	e.fund(hana, 100000)
	o, _ := e.svc.Place(e.ctx, hana, commerce.PlaceRequest{MerchantID: shop, Channel: "delivery", DeliveryAddress: "Kazanchis, blue gate",
		Items: []commerce.LineRequest{{ProductID: berbere, Quantity: 1}}})
	e.checkout(hana, o)
	for _, step := range []string{"accept", "ready", "on_the_way"} {
		if _, err := e.svc.Advance(e.ctx, owner, shop, o.ID, step); err != nil {
			t.Fatal(step, err)
		}
	}
	code := e.order(hana, o.ID).HandoverCode
	bad := fmt.Sprintf("%04d", (int(code[3]-'0')+1)%10+int(code[0]-'0')*1000)
	if bad == code {
		bad = "9999"
	}
	for i := 0; i < commerce.MaxHandoverTries; i++ {
		_, _ = e.svc.HandOver(e.ctx, owner, shop, o.ID, bad)
	}
	var conflict *commerce.ConflictError
	if _, err := e.svc.HandOver(e.ctx, owner, shop, o.ID, code); !errors.As(err, &conflict) {
		t.Fatalf("right code after lockout: %v", err)
	}
	got, err := e.svc.Received(e.ctx, hana, o.ID)
	if err != nil || got.Status != "delivered" {
		t.Fatalf("received: %v %+v", err, got)
	}
	if s := e.balance(ledger.MerchantSettlement, ledger.OwnerMerchant, shop); s != 24750 { // 25,000 less 1%... of 250.00
		t.Errorf("settlement %d", s)
	}
}
