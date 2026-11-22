package opsweb_test

import (
	"context"
	"fmt"
	"net/url"
	"strings"
	"testing"

	"github.com/google/uuid"

	"superapp/backend/internal/commerce"
	"superapp/backend/internal/money"
	"superapp/backend/internal/platform"
)

func TestUnfinishedOrdersAreSettledByTwoPeople(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatal(err)
		}
	}
	hash, _ := platform.HashPIN("482913")
	user := func(name string) uuid.UUID {
		id := uuid.New()
		exec(`INSERT INTO identity.users (id, phone_e164, display_name, status, kyc_level) VALUES ($1, $2, $3, 'active', 'L2')`,
			id, fmt.Sprintf("+2519%08d", id.ID()%100_000_000), name)
		exec(`INSERT INTO identity.credentials (user_id, pin_hash) VALUES ($1, $2)`, id, hash)
		return id
	}
	owner, hana, shop := user("Owner"), user("Hana Girma"), uuid.New()
	exec(`INSERT INTO merchant.merchants (id, legal_name, trade_name, category_code, status) VALUES ($1, 'Abol Coffee PLC', 'Abol Coffee', '5411', 'active')`, shop)
	exec(`INSERT INTO merchant.members (merchant_id, user_id, role) VALUES ($1, $2, 'owner')`, shop, owner)
	shops := e.svc.Commerce
	p, err := shops.CreateProduct(ctx, owner, shop, commerce.ProductInput{Title: "Coffee beans, 1 kg", Category: "Coffee",
		Price: money.New(95000, money.ETB)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := shops.UpdateShop(ctx, owner, shop, commerce.ShopUpdate{Open: true, Pickup: true, DeliveryFee: money.New(0, money.ETB)}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.Payments.SandboxTopUp(ctx, hana, money.New(200000, money.ETB)); err != nil {
		t.Fatal(err)
	}
	o, err := shops.Place(ctx, hana, commerce.PlaceRequest{MerchantID: shop, Channel: "pickup",
		Items: []commerce.LineRequest{{ProductID: p.ID, Quantity: 2}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.Payments.Confirm(ctx, hana, *o.PaymentIntentID, "482913"); err != nil {
		t.Fatal(err)
	}
	if _, err := shops.Advance(ctx, owner, shop, o.ID, "accept"); err != nil {
		t.Fatal(err)
	}
	exec(`UPDATE commerce.orders SET accepted_at = now() - interval '73 hours' WHERE id = $1`, o.ID)
	if _, _, err := shops.FlagStalled(ctx); err != nil {
		t.Fatal(err)
	}

	agent, _, _ := e.signIn(claimsFor(e.invite("Meron", "support_agent")))
	_, html := agent.do("GET", "/ops/orders", nil, "")
	for _, want := range []string{o.Number, "Abol Coffee", "Hana Girma", "2 × Coffee beans, 1 kg", "Ask to refund", "Ask to pay the shop",
		"Unfinished orders</span><span class=\"count\">"} {
		if !strings.Contains(html, want) {
			t.Fatalf("orders page lacks %q:\n%s", want, html)
		}
	}
	if strings.Contains(html, "+2519") {
		t.Fatal("orders page shows a full phone number")
	}
	csrf := csrfRE.FindStringSubmatch(html)[1]
	res, _ := agent.do("POST", "/ops/orders/"+o.ID.String()+"/action", url.Values{"csrf": {csrf}, "kind": {"order.refund"},
		"note": {"Shop says the customer never came; customer agrees"}}, "")
	if loc := res.Header.Get("Location"); loc != "/ops/orders?msg=requested" {
		t.Fatalf("refund request: %s", loc)
	}
	_, html = agent.do("GET", "/ops/orders", nil, "")
	if !strings.Contains(html, "Refund the customer: awaiting approval") {
		t.Fatalf("pending request not shown:\n%s", html)
	}

	// The second person sees the order number on the approvals page and approves: the customer is refunded.
	finance, _, _ := e.signIn(claimsFor(e.invite("Dawit", "finance_officer")))
	_, html = finance.do("GET", "/ops/approvals", nil, "")
	if !strings.Contains(html, o.Number) || !strings.Contains(html, `href="/ops/orders"`) {
		t.Fatalf("approvals page:\n%s", html)
	}
	var approval uuid.UUID
	if err := pool.QueryRow(ctx, `SELECT id FROM admin.approvals WHERE target_id = $1 AND decision IS NULL`, o.ID).Scan(&approval); err != nil {
		t.Fatal(err)
	}
	csrf = csrfRE.FindStringSubmatch(html)[1]
	res, _ = finance.do("POST", "/ops/approvals/"+approval.String(), url.Values{"csrf": {csrf}, "decision": {"approve"},
		"note": {"Checked the call notes"}}, "")
	if loc := res.Header.Get("Location"); loc != "/ops/approvals?msg=approved" {
		t.Fatalf("approve: %s", loc)
	}
	after, err := shops.Order(ctx, hana, o.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.Status != "cancelled" || after.CancelReason != "not_completed" || after.PaymentStatus != "reversed" {
		t.Fatalf("after approval: %s %s %s", after.Status, after.CancelReason, after.PaymentStatus)
	}
	_, html = agent.do("GET", "/ops/orders", nil, "")
	if strings.Contains(html, o.Number) {
		t.Fatal("a settled order is still listed")
	}

	// Risk analysts can look but not act; content editors don't see the page.
	analyst, _, _ := e.signIn(claimsFor(e.invite("Selam", "risk_analyst")))
	if res, html := analyst.do("GET", "/ops/orders", nil, ""); res.StatusCode != 200 || strings.Contains(html, "Ask to refund") {
		t.Fatalf("analyst: %d", res.StatusCode)
	}
	editor, _, _ := e.signIn(claimsFor(e.invite("Lily", "content_editor")))
	if res, _ := editor.do("GET", "/ops/orders", nil, ""); res.StatusCode != 403 {
		t.Fatalf("editor: %d", res.StatusCode)
	}
	if strings.Contains(e.logs.String(), "level=ERROR") {
		t.Errorf("server errors: %s", e.logs.String())
	}
}
