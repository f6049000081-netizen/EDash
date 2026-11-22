package bizweb_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"superapp/backend/internal/bizweb"
	"superapp/backend/internal/commerce"
	"superapp/backend/internal/ledger"
	"superapp/backend/internal/merchants"
	"superapp/backend/internal/money"
	"superapp/backend/internal/payments"
	"superapp/backend/internal/payouts"
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
	t     *testing.T
	ctx   context.Context
	pay   *payments.Service
	shops *commerce.Service
	store *bizweb.Store
	srv   *httptest.Server
	now   time.Time
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
	pv := &platform.PINVerifier{Pool: pool}
	rules := payments.DefaultRules()
	rules.VelocityMax = 0
	e := &env{t: t, ctx: context.Background(), now: time.Now()}
	e.pay = payments.NewService(pool, ledger.New(), rules, pv)
	e.shops = commerce.New(pool, e.pay)
	e.store = &bizweb.Store{Pool: pool, PIN: pv, Now: func() time.Time { return e.now }}
	e.srv = httptest.NewUnstartedServer(nil)
	e.srv.StartTLS()
	web, err := bizweb.New(e.srv.URL, e.store, &merchants.Service{Pool: pool}, e.shops, payouts.NewService(pool, ledger.New(), pv), nil)
	if err != nil {
		t.Fatal(err)
	}
	e.srv.Config.Handler = web.Handler()
	t.Cleanup(e.srv.Close)
	return e
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
	e.exec(`INSERT INTO identity.users (id, phone_e164, display_name, status, kyc_level, locale) VALUES ($1, $2, $3, 'active', 'L2', 'en')`,
		id, fmt.Sprintf("+2519%08d", id.ID()%100_000_000), name)
	e.exec(`INSERT INTO identity.credentials (user_id, pin_hash) VALUES ($1, $2)`, id, pinHash)
	return id
}

// shop: an approved merchant with an owner and a cashier, open, selling injera (stock 3).
func (e *env) shop() (owner, cashier, merchant, injera uuid.UUID) {
	e.t.Helper()
	owner, cashier, merchant = e.user("Tsion"), e.user("Dawit"), uuid.New()
	e.exec(`INSERT INTO merchant.merchants (id, legal_name, trade_name, category_code, status) VALUES ($1, 'Merkato Fresh PLC', 'Merkato Fresh', '5411', 'active')`, merchant)
	e.exec(`INSERT INTO merchant.members (merchant_id, user_id, role) VALUES ($1, $2, 'owner'), ($1, $3, 'cashier')`, merchant, owner, cashier)
	three := 3
	p, err := e.shops.CreateProduct(e.ctx, owner, merchant, commerce.ProductInput{Title: "Injera, 10 pcs", Category: "Bakery",
		Price: money.New(32000, money.ETB), Stock: &three})
	if err != nil {
		e.t.Fatal(err)
	}
	if _, err := e.shops.UpdateShop(e.ctx, owner, merchant, commerce.ShopUpdate{Open: true, Pickup: true, Delivery: true,
		DeliveryFee: money.New(4000, money.ETB), DeliveryArea: "Bole"}); err != nil {
		e.t.Fatal(err)
	}
	return owner, cashier, merchant, p.ID
}

// paidOrder: a customer orders two injera for pickup and pays.
func (e *env) paidOrder(merchant, injera uuid.UUID) (customer uuid.UUID, o *commerce.Order) {
	e.t.Helper()
	customer = e.user("Hana")
	if _, err := e.pay.SandboxTopUp(e.ctx, customer, money.New(500000, money.ETB)); err != nil {
		e.t.Fatal(err)
	}
	o, err := e.shops.Place(e.ctx, customer, commerce.PlaceRequest{MerchantID: merchant, Channel: "pickup",
		Items: []commerce.LineRequest{{ProductID: injera, Quantity: 2}}, Note: "Extra bag please"})
	if err != nil {
		e.t.Fatal(err)
	}
	if _, err := e.pay.Confirm(e.ctx, customer, *o.PaymentIntentID, pin); err != nil {
		e.t.Fatal(err)
	}
	if o, err = e.shops.Order(e.ctx, customer, o.ID); err != nil {
		e.t.Fatal(err)
	}
	return customer, o
}

// browser is a person's web browser: it keeps cookies and doesn't follow redirects.
type browser struct {
	e *env
	c *http.Client
}

func (e *env) browser() *browser {
	jar, _ := cookiejar.New(nil)
	c := new(http.Client)
	*c = *e.srv.Client() // a copy: Client() returns the same one each time, and each browser needs its own cookies
	c.Jar = jar
	c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &browser{e: e, c: c}
}

func (b *browser) get(path string) (int, string, string) {
	b.e.t.Helper()
	res, err := b.c.Get(b.e.srv.URL + path)
	if err != nil {
		b.e.t.Fatal(err)
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(res.Body)
	return res.StatusCode, string(body), res.Header.Get("Location")
}

func (b *browser) post(path string, form url.Values) (int, string, string) {
	b.e.t.Helper()
	res, err := b.c.PostForm(b.e.srv.URL+path, form)
	if err != nil {
		b.e.t.Fatal(err)
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(res.Body)
	return res.StatusCode, string(body), res.Header.Get("Location")
}

var (
	codeRE = regexp.MustCompile(`class="code"[^>]*>([A-Z0-9]{4}-[A-Z0-9]{4})<`)
	csrfRE = regexp.MustCompile(`name="csrf" value="([^"]+)"`)
)

// signIn shows a code in the browser and approves it in "the app" as user, for merchant.
func (b *browser) signIn(user, merchant uuid.UUID) {
	b.e.t.Helper()
	st, page, _ := b.get("/business/login")
	m := codeRE.FindStringSubmatch(page)
	if st != http.StatusOK || m == nil {
		b.e.t.Fatalf("login page: %d %.300s", st, page)
	}
	if _, err := b.e.store.ApproveLogin(b.e.ctx, user, merchant, strings.ToLower(m[1]), pin); err != nil {
		b.e.t.Fatalf("approve: %v", err)
	}
	if st, _, loc := b.get("/business/login"); st != http.StatusSeeOther || loc != "/business/" {
		b.e.t.Fatalf("after approval the login page should move on, got %d %q", st, loc)
	}
}

func (b *browser) csrf(path string) string {
	b.e.t.Helper()
	_, page, _ := b.get(path)
	m := csrfRE.FindStringSubmatch(page)
	if m == nil {
		b.e.t.Fatalf("no CSRF token on %s", path)
	}
	return m[1]
}

func TestSignInIsApprovedInTheAppWithThePIN(t *testing.T) {
	e := setup(t)
	owner, _, merchant, _ := e.shop()
	stranger := e.user("Stranger")
	b := e.browser()

	if st, page, _ := b.get("/business/"); st != http.StatusUnauthorized || !strings.Contains(page, "signed out") {
		t.Fatalf("no session: %d", st)
	}
	_, page, _ := b.get("/business/login")
	code := codeRE.FindStringSubmatch(page)[1]
	if !strings.Contains(page, `http-equiv="refresh"`) {
		t.Error("the sign-in page should check back by itself")
	}
	// Reloading keeps the same code.
	if _, again, _ := b.get("/business/login"); codeRE.FindStringSubmatch(again)[1] != code {
		t.Error("the code changed on reload")
	}

	info, err := e.store.LookupLogin(e.ctx, owner, code)
	if err != nil || info.Browser == "" {
		t.Fatalf("lookup: %v %+v", err, info)
	}
	if _, err := e.store.LookupLogin(e.ctx, owner, "ABCD-EFGH"); !errors.Is(err, bizweb.ErrNotFound) {
		t.Errorf("unknown code: %v", err)
	}
	if _, err := e.store.ApproveLogin(e.ctx, stranger, merchant, code, pin); !errors.Is(err, bizweb.ErrForbidden) {
		t.Errorf("someone outside the shop approved: %v", err)
	}
	if _, err := e.store.ApproveLogin(e.ctx, owner, merchant, code, "000000"); !errors.Is(err, platform.ErrInvalidPIN) {
		t.Errorf("wrong PIN: %v", err)
	}
	if st, _, _ := b.get("/business/login"); st != http.StatusOK {
		t.Error("the browser got in before approval")
	}
	if _, err := e.store.ApproveLogin(e.ctx, owner, merchant, code, pin); err != nil {
		t.Fatal(err)
	}
	if _, err := e.store.ApproveLogin(e.ctx, owner, merchant, code, pin); !errors.Is(err, bizweb.ErrNotFound) {
		t.Errorf("a code was approved twice: %v", err)
	}
	var events int
	_ = pool.QueryRow(e.ctx, `SELECT count(*) FROM platform.outbox WHERE topic = 'identity.web_signed_in' AND aggregate_id = $1`, owner).Scan(&events)
	if events != 1 {
		t.Errorf("sign-in events: %d", events)
	}
	if st, _, loc := b.get("/business/login"); st != http.StatusSeeOther || loc != "/business/" {
		t.Fatalf("approved browser: %d %q", st, loc)
	}
	if st, page, _ := b.get("/business/"); st != http.StatusOK || !strings.Contains(page, "Merkato Fresh") || !strings.Contains(page, "Hello, Tsion") {
		t.Fatalf("home: %d", st)
	}

	// A second browser can't reuse the first one's code.
	other := e.browser()
	if _, err := e.store.ApproveLogin(e.ctx, owner, merchant, code, pin); err == nil {
		t.Error("used code approved again")
	}
	if st, _, _ := other.get("/business/"); st != http.StatusUnauthorized {
		t.Error("a browser without a session got in")
	}

	// The app lists the browser and can sign it out.
	list, err := e.store.Sessions(e.ctx, owner)
	if err != nil || len(list) != 1 || list[0].Shop != "Merkato Fresh" {
		t.Fatalf("sessions: %v %+v", err, list)
	}
	if err := e.store.Revoke(e.ctx, stranger, list[0].ID); !errors.Is(err, bizweb.ErrNotFound) {
		t.Errorf("someone else signed it out: %v", err)
	}
	if err := e.store.Revoke(e.ctx, owner, list[0].ID); err != nil {
		t.Fatal(err)
	}
	if st, _, _ := b.get("/business/"); st != http.StatusUnauthorized {
		t.Error("revoked session still works")
	}
}

func TestCodesExpireAndSessionsEnd(t *testing.T) {
	e := setup(t)
	owner, cashier, merchant, _ := e.shop()

	b := e.browser()
	_, page, _ := b.get("/business/login")
	code := codeRE.FindStringSubmatch(page)[1]
	e.now = e.now.Add(bizweb.LoginTTL + time.Second)
	if _, err := e.store.ApproveLogin(e.ctx, owner, merchant, code, pin); !errors.Is(err, bizweb.ErrNotFound) {
		t.Errorf("expired code approved: %v", err)
	}
	if _, page, _ := b.get("/business/login"); codeRE.FindStringSubmatch(page)[1] == code {
		t.Error("an expired code is still shown")
	}

	// Idle for too long.
	b = e.browser()
	b.signIn(owner, merchant)
	e.now = e.now.Add(bizweb.SessionIdle + time.Minute)
	if st, _, _ := b.get("/business/"); st != http.StatusUnauthorized {
		t.Error("idle session still works")
	}

	// Leaving the shop ends the session at once; so does a suspension.
	c := e.browser()
	c.signIn(cashier, merchant)
	e.exec(`DELETE FROM merchant.members WHERE merchant_id = $1 AND user_id = $2`, merchant, cashier)
	if st, _, _ := c.get("/business/orders"); st != http.StatusUnauthorized {
		t.Error("removed member still signed in")
	}
	o := e.browser()
	o.signIn(owner, merchant)
	e.exec(`UPDATE merchant.merchants SET status = 'suspended' WHERE id = $1`, merchant)
	if st, _, _ := o.get("/business/"); st != http.StatusUnauthorized {
		t.Error("suspended shop still signed in")
	}
	if _, err := e.store.ApproveLogin(e.ctx, owner, merchant, "ABCDEFGH", pin); !errors.Is(err, bizweb.ErrForbidden) {
		t.Errorf("sign-in approved for a suspended shop: %v", err)
	}

	// Signing out.
	e.exec(`UPDATE merchant.merchants SET status = 'active' WHERE id = $1`, merchant)
	o = e.browser()
	o.signIn(owner, merchant)
	if st, _, _ := o.post("/business/logout", url.Values{"csrf": {o.csrf("/business/")}}); st != http.StatusOK {
		t.Fatalf("logout: %d", st)
	}
	if st, _, _ := o.get("/business/"); st != http.StatusUnauthorized {
		t.Error("signed-out session still works")
	}
}

func TestACashierHandlesAnOrderFromAcceptToHandover(t *testing.T) {
	e := setup(t)
	_, cashier, merchant, injera := e.shop()
	_, order := e.paidOrder(merchant, injera)
	b := e.browser()
	b.signIn(cashier, merchant)

	_, home, _ := b.get("/business/")
	for _, want := range []string{order.Number, "Hana", "Accept", "ETB 640.00", "1 waiting to be accepted"} {
		if !strings.Contains(home, want) {
			t.Errorf("home is missing %q", want)
		}
	}
	if strings.Contains(home, "Next payout") {
		t.Error("a cashier sees payouts")
	}
	path := "/business/orders/" + order.ID.String()
	_, page, _ := b.get(path)
	if !strings.Contains(page, "Extra bag please") || strings.Contains(page, order.HandoverCode) {
		t.Error("the order page should show the note and never the customer's code")
	}
	csrf := b.csrf(path)

	if st, _, _ := b.post(path+"/accept", url.Values{"csrf": {"wrong"}}); st != http.StatusForbidden {
		t.Errorf("accept without the CSRF token: %d", st)
	}
	if st, _, loc := b.post(path+"/accept", url.Values{"csrf": {csrf}, "return": {"list"}}); st != http.StatusSeeOther || loc != "/business/orders?msg=accepted" {
		t.Fatalf("accept: %d %q", st, loc)
	}
	if _, _, loc := b.post(path+"/ready", url.Values{"csrf": {csrf}}); loc != path+"?msg=ready" {
		t.Fatalf("ready: %q", loc)
	}
	if _, page, _ := b.get(path + "?msg=ready"); !strings.Contains(page, "Marked ready") || !strings.Contains(page, `id="handover"`) {
		t.Error("ready order page")
	}
	wrong := "0000"
	if order.HandoverCode == wrong {
		wrong = "1111"
	}
	if _, _, loc := b.post(path+"/hand_over", url.Values{"csrf": {csrf}, "code": {wrong}}); loc != path+"?msg=wrong_code" {
		t.Errorf("wrong code: %q", loc)
	}
	if _, _, loc := b.post(path+"/hand_over", url.Values{"csrf": {csrf}, "code": {"12a4"}}); loc != path+"?msg=code_format" {
		t.Errorf("malformed code: %q", loc)
	}
	if _, _, loc := b.post(path+"/hand_over", url.Values{"csrf": {csrf}, "code": {order.HandoverCode}}); loc != "/business/orders?msg=handed_over" {
		t.Fatalf("hand over: %q", loc)
	}
	var status string
	_ = pool.QueryRow(e.ctx, `SELECT status FROM commerce.orders WHERE id = $1`, order.ID).Scan(&status)
	if status != "delivered" {
		t.Errorf("order is %s", status)
	}
	if _, done, _ := b.get("/business/orders?state=done"); !strings.Contains(done, order.Number) || !strings.Contains(done, "Handed over") {
		t.Error("the finished order isn't listed")
	}

	// A cashier can't change products or the storefront.
	if st, page, _ := b.post("/business/products", url.Values{"csrf": {csrf}, "title": {"Teff"}, "price": {"100"}}); st != http.StatusUnprocessableEntity || !strings.Contains(page, "Your role can&#39;t do that") {
		t.Errorf("cashier added a product: %d", st)
	}
	if st, _, _ := b.post("/business/storefront", url.Values{"csrf": {csrf}, "pickup": {"1"}}); st != http.StatusUnprocessableEntity {
		t.Errorf("cashier changed the storefront: %d", st)
	}
}

func TestTheShopDeclinesWithANoteAndTheCustomerIsRefunded(t *testing.T) {
	e := setup(t)
	owner, _, merchant, injera := e.shop()
	customer, order := e.paidOrder(merchant, injera)
	b := e.browser()
	b.signIn(owner, merchant)
	path := "/business/orders/" + order.ID.String()
	csrf := b.csrf(path)
	if _, _, loc := b.post(path+"/cancel", url.Values{"csrf": {csrf}, "note": {"  "}}); loc != path+"?msg=note" {
		t.Errorf("decline without a note: %q", loc)
	}
	if _, _, loc := b.post(path+"/cancel", url.Values{"csrf": {csrf}, "note": {"Sorry, no injera today"}}); loc != "/business/orders?msg=cancelled" {
		t.Fatalf("decline: %q", loc)
	}
	o, err := e.shops.Order(e.ctx, customer, order.ID)
	if err != nil || o.Status != "cancelled" || o.CancelReason != "shop_declined" || o.CancelNote != "Sorry, no injera today" || o.PaymentStatus != "reversed" {
		t.Fatalf("after decline: %v %+v", err, o)
	}
	if _, page, _ := b.get(path); !strings.Contains(page, "You declined it.") {
		t.Error("the order page should say why it was cancelled")
	}
}

func TestOwnersManageProductsAndTheStorefront(t *testing.T) {
	e := setup(t)
	owner, _, merchant, injera := e.shop()
	b := e.browser()
	b.signIn(owner, merchant)
	csrf := b.csrf("/business/products")

	if st, page, _ := b.post("/business/products", url.Values{"csrf": {csrf}, "title": {"Teff, 5 kg"}, "price": {"12.345"}}); st != http.StatusUnprocessableEntity || !strings.Contains(page, "Enter a price like 180") {
		t.Errorf("bad price: %d", st)
	}
	if _, _, loc := b.post("/business/products", url.Values{"csrf": {csrf}, "title": {"Teff, 5 kg"}, "price": {"1,450.50"},
		"category": {"Grain"}, "stock": {"12"}}); loc != "/business/products?msg=saved" {
		t.Fatalf("add product: %q", loc)
	}
	_, list, _ := b.get("/business/products?msg=saved")
	for _, want := range []string{"Saved.", "Teff, 5 kg", "ETB 1,450.50", "Grain", "Injera, 10 pcs"} {
		if !strings.Contains(list, want) {
			t.Errorf("products page is missing %q", want)
		}
	}
	// Edit injera: hide it and count 2.
	edit := "/business/products/" + injera.String()
	if _, form, _ := b.get(edit); !strings.Contains(form, `value="320.00"`) {
		t.Error("the edit form should show the current price")
	}
	if _, _, loc := b.post(edit, url.Values{"csrf": {csrf}, "title": {"Injera, 10 pcs"}, "price": {"330"}, "stock": {"2"}, "hidden": {"1"}}); loc != "/business/products?msg=saved" {
		t.Fatalf("edit: %q", loc)
	}
	var status string
	var price int64
	_ = pool.QueryRow(e.ctx, `SELECT status, price_minor FROM commerce.products WHERE id = $1`, injera).Scan(&status, &price)
	if status != "hidden" || price != 33000 {
		t.Errorf("after edit: %s %d", status, price)
	}
	if _, _, loc := b.post(edit, url.Values{"csrf": {csrf}, "title": {"Injera, 10 pcs"}, "price": {"330"}, "archive": {"1"}}); loc != "/business/products?msg=saved" {
		t.Fatalf("archive: %q", loc)
	}
	if _, list, _ := b.get("/business/products"); strings.Contains(list, "Injera, 10 pcs") {
		t.Error("a removed product is still listed")
	}

	// Storefront: delivery off, then the home switch closes the shop.
	if _, _, loc := b.post("/business/storefront", url.Values{"csrf": {csrf}, "open": {"1"}, "pickup": {"1"}, "fee": {""},
		"about": {"Fresh every morning"}}); loc != "/business/storefront?msg=saved" {
		t.Fatalf("storefront: %q", loc)
	}
	if st, page, _ := b.post("/business/storefront", url.Values{"csrf": {csrf}, "open": {"1"}}); st != http.StatusUnprocessableEntity || !strings.Contains(page, "Offer pickup, delivery or both.") {
		t.Errorf("storefront with neither pickup nor delivery: %d", st)
	}
	if _, _, loc := b.post("/business/storefront/open", url.Values{"csrf": {csrf}, "open": {"0"}}); loc != "/business/?msg=saved" {
		t.Fatalf("close: %q", loc)
	}
	shop, err := e.shops.ShopSettings(e.ctx, owner, merchant)
	if err != nil || shop.Open || shop.Delivery || !shop.Pickup || shop.About != "Fresh every morning" {
		t.Fatalf("shop after saving: %v %+v", err, shop)
	}
}

func TestEveryPageRendersInEnglishAndAmharic(t *testing.T) {
	e := setup(t)
	owner, cashier, merchant, injera := e.shop()
	_, order := e.paidOrder(merchant, injera)
	pages := []string{"/business/", "/business/orders", "/business/orders?state=done", "/business/orders/" + order.ID.String(),
		"/business/products", "/business/products/new", "/business/products/" + injera.String(), "/business/storefront", "/business/money"}
	for _, who := range []uuid.UUID{owner, cashier} {
		b := e.browser()
		b.signIn(who, merchant)
		for _, lang := range []string{"en", "am"} {
			if st, _, loc := b.get("/business/lang?l=" + lang + "&next=/business/orders"); st != http.StatusSeeOther || loc != "/business/orders" {
				t.Fatalf("language switch: %d %q", st, loc)
			}
			all := pages
			if who == owner {
				all = append(all[:len(all):len(all)], "/business/team") // cashiers don't see the team
			}
			for _, p := range all {
				st, page, _ := b.get(p)
				if st != http.StatusOK || !strings.Contains(page, "</html>") {
					t.Errorf("%s (%s): %d %.200s", p, lang, st, page)
				}
				if lang == "am" && !strings.Contains(page, "ትዕዛዞች") {
					t.Errorf("%s isn't in Amharic", p)
				}
			}
		}
	}
	// Redirects after a language switch stay on the console.
	b := e.browser()
	if _, _, loc := b.get("/business/lang?l=am&next=https://evil.example/"); loc != "/business/" {
		t.Errorf("open redirect: %q", loc)
	}
}
