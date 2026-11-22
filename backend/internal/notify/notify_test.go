package notify_test

// Integration tests against PostgreSQL loaded with db/schema.sql (set TEST_DATABASE_URL).
// Every test makes its own users, intents and outbox ids, because the database is shared across runs
// and across packages running in parallel.

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"superapp/backend/internal/notify"
	"superapp/backend/internal/outbox"
)

var (
	poolOnce sync.Once
	pool     *pgxpool.Pool
	poolErr  error
)

type env struct {
	t   *testing.T
	ctx context.Context
	svc *notify.Service
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
	return &env{t: t, ctx: context.Background(), svc: &notify.Service{Pool: pool}}
}

func (e *env) exec(sql string, args ...any) {
	e.t.Helper()
	if _, err := pool.Exec(e.ctx, sql, args...); err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) user(name, locale string) uuid.UUID {
	e.t.Helper()
	id := uuid.New()
	phone := fmt.Sprintf("+2519%08d", binary.BigEndian.Uint64(id[:8])%100_000_000)
	e.exec(`INSERT INTO identity.users (id, phone_e164, display_name, status, locale) VALUES ($1, $2, NULLIF($3, ''), 'active', $4)`,
		id, phone, name, locale)
	return id
}

func (e *env) merchant(name string) uuid.UUID {
	e.t.Helper()
	id := uuid.New()
	e.exec(`INSERT INTO merchant.merchants (id, legal_name, trade_name, category_code, status) VALUES ($1, $2, $2, '5812', 'active')`, id, name)
	return id
}

// intent records a payment and the status changes it went through, as payments does.
func (e *env) intent(kind string, payer uuid.UUID, payeeType string, payee uuid.UUID, minor int64, rail string, statuses ...string) uuid.UUID {
	e.t.Helper()
	id := uuid.New()
	e.exec(`INSERT INTO payments.intents (id, kind, payer_user_id, payee_type, payee_id, amount_minor, currency, rail, status)
		VALUES ($1, $2, $3, $4, $5, $6, 'ETB', $7, $8)`, id, kind, payer, payeeType, payee, minor, rail, statuses[len(statuses)-1])
	from := ""
	for _, to := range statuses {
		e.exec(`INSERT INTO payments.intent_events (intent_id, from_status, to_status, actor)
			VALUES ($1, NULLIF($2, '')::payments.intent_status, $3, 'system')`, id, from, to)
		from = to
	}
	return id
}

var nextEvent = rand.Int64N(1 << 50)
var eventMu sync.Mutex

// event builds an outbox message with an id no other test uses.
func event(topic string, aggregate uuid.UUID, payload map[string]any) outbox.Message {
	eventMu.Lock()
	nextEvent++
	id := nextEvent + 1<<52
	eventMu.Unlock()
	b, _ := json.Marshal(payload)
	return outbox.Message{ID: id, Topic: topic, AggregateID: aggregate, Payload: b, CreatedAt: time.Now()}
}

func paymentEvent(topic string, intent uuid.UUID, kind string, payer uuid.UUID, payeeType string, payee uuid.UUID, minor int64, status, rail string) outbox.Message {
	return event(topic, intent, map[string]any{
		"intent_id": intent, "kind": kind, "payer_user_id": payer, "payee_type": payeeType, "payee_id": payee,
		"amount": map[string]any{"amount_minor": minor, "currency": "ETB"}, "status": status, "failure_code": "", "rail": rail,
	})
}

func (e *env) handle(m outbox.Message) {
	e.t.Helper()
	if err := e.svc.Handle(e.ctx, m); err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) list(user uuid.UUID) []notify.Notification {
	e.t.Helper()
	p, err := e.svc.List(e.ctx, user, "", 100)
	if err != nil {
		e.t.Fatal(err)
	}
	return p.Items
}

func TestTransferNotifiesPayeeNotPayer(t *testing.T) {
	e := setup(t)
	alem, bekele := e.user("Alem", "en"), e.user("", "am")
	in := e.intent("p2p", alem, "user", bekele, 150_000, "wallet", "requires_confirmation", "succeeded")
	m := paymentEvent("payment.succeeded", in, "p2p", alem, "user", bekele, 150_000, "succeeded", "wallet")
	e.handle(m)
	e.handle(m) // the relay delivers at least once

	got := e.list(bekele)
	if len(got) != 1 || got[0].Kind != "money_received" || got[0].Title != "ገንዘብ ደርሶዎታል" ||
		got[0].Body != "Alem ETB 1,500.00 ልከውልዎታል።" || got[0].Data["intent_id"] != in.String() || got[0].Read {
		t.Fatalf("payee: %+v", got)
	}
	if got := e.list(alem); len(got) != 0 {
		t.Errorf("the payer saw the result on screen and needs no notification: %+v", got)
	}

	// A sender with no name is shown by the last four digits of their number.
	carol := e.user("", "en")
	in2 := e.intent("p2p", carol, "user", alem, 5_000, "wallet", "requires_confirmation", "succeeded")
	e.handle(paymentEvent("payment.succeeded", in2, "p2p", carol, "user", alem, 5_000, "succeeded", "wallet"))
	if got := e.list(alem); len(got) != 1 || !strings.HasPrefix(got[0].Body, "•••• ") || !strings.HasSuffix(got[0].Body, "sent you ETB 50.00.") {
		t.Errorf("unnamed sender: %+v", got)
	}
}

func TestLaterOutcomesNotifyPayer(t *testing.T) {
	e := setup(t)
	u, friend := e.user("Hana", "en"), e.user("Dawit", "en")
	shop := e.merchant("Kaldi's Coffee")

	tu := e.intent("topup", u, "user", u, 25_000, "telebirr", "processing", "succeeded")
	e.handle(paymentEvent("payment.succeeded", tu, "topup", u, "user", u, 25_000, "succeeded", "telebirr"))

	held := e.intent("topup", u, "user", u, 90_000, "telebirr", "processing", "held_for_review")
	e.handle(paymentEvent("payment.held", held, "topup", u, "user", u, 90_000, "held_for_review", "telebirr"))

	expired := e.intent("topup", u, "user", u, 1_000, "telebirr", "processing", "failed")
	e.handle(paymentEvent("payment.failed", expired, "topup", u, "user", u, 1_000, "failed", "telebirr"))

	// A transfer held for review and then released: both sides hear about it.
	p2p := e.intent("p2p", u, "user", friend, 70_000, "wallet", "requires_confirmation", "held_for_review", "succeeded")
	e.handle(paymentEvent("payment.held", p2p, "p2p", u, "user", friend, 70_000, "held_for_review", "wallet"))
	e.handle(paymentEvent("payment.succeeded", p2p, "p2p", u, "user", friend, 70_000, "succeeded", "wallet"))

	qr := e.intent("qr_merchant", u, "merchant", shop, 4_500, "wallet", "requires_confirmation", "held_for_review", "failed")
	e.handle(paymentEvent("payment.failed", qr, "qr_merchant", u, "merchant", shop, 4_500, "failed", "wallet"))

	// Decided while confirming: no notification for the payer.
	onSpot := e.intent("qr_merchant", u, "merchant", shop, 3_000, "wallet", "requires_confirmation", "failed")
	e.handle(paymentEvent("payment.failed", onSpot, "qr_merchant", u, "merchant", shop, 3_000, "failed", "wallet"))

	want := []struct{ kind, body string }{
		{"payment_failed", "Your payment of ETB 45.00 to Kaldi's Coffee didn't go through. Your money wasn't taken."},
		{"payment_succeeded", "Your payment of ETB 700.00 to Dawit went through."},
		{"topup_failed", "Your top-up of ETB 10.00 from telebirr didn't complete, and nothing was added to your wallet."},
		{"topup_held", "Your top-up of ETB 900.00 from telebirr is being reviewed. We'll tell you when it's done."},
		{"topup_succeeded", "ETB 250.00 from telebirr was added to your wallet."},
	}
	got := e.list(u)
	if len(got) != len(want) {
		t.Fatalf("%d notifications: %+v", len(got), got)
	}
	for i, w := range want {
		if got[i].Kind != w.kind || got[i].Body != w.body {
			t.Errorf("%d: %s %q, want %s %q", i, got[i].Kind, got[i].Body, w.kind, w.body)
		}
	}
	if got := e.list(friend); len(got) != 1 || got[0].Kind != "money_received" {
		t.Errorf("payee of the released transfer: %+v", got)
	}
}

func (e *env) device(user uuid.UUID, platform string) uuid.UUID {
	e.t.Helper()
	id := uuid.New()
	e.exec(`INSERT INTO identity.devices (id, user_id, fingerprint_hash, platform) VALUES ($1, $2, $3, $4)`, id, user, uuid.NewString(), platform)
	return id
}

func (e *env) session(user, device uuid.UUID, live bool) {
	e.t.Helper()
	e.exec(`INSERT INTO identity.sessions (user_id, device_id, refresh_token_hash, family_id, expires_at, revoked_at)
		VALUES ($1, $2, $3, gen_random_uuid(), now() + interval '30 days', CASE WHEN $4 THEN NULL ELSE now() END)`,
		user, device, uuid.NewString(), live)
}

func TestSecurityAlerts(t *testing.T) {
	e := setup(t)
	u := e.user("Selam", "en")
	first := e.device(u, "android")
	signIn := func(dev uuid.UUID, platform string, isNew bool) outbox.Message {
		return event("identity.signed_in", u, map[string]any{"user_id": u, "device_id": dev, "new_device": isNew, "platform": platform, "purpose": "login"})
	}
	e.exec(`UPDATE identity.devices SET first_seen_at = now() - interval '1 day' WHERE id = $1`, first)
	second := e.device(u, "ios")
	// The sign-up event is handled late, after the second device exists: still not a warning.
	e.handle(signIn(first, "android", true))
	e.handle(signIn(first, "android", false))
	if got := e.list(u); len(got) != 0 {
		t.Fatalf("first device or a known device: %+v", got)
	}
	e.handle(signIn(second, "ios", true))
	e.handle(event("identity.pin_reset", u, map[string]any{"user_id": u, "device_id": second, "sessions_revoked": 1}))
	e.handle(event("identity.refresh_token_reused", u, map[string]any{"user_id": u, "device_id": first}))
	e.handle(event("identity.pin_set", u, map[string]any{"user_id": u, "device_id": first}))
	e.handle(event("identity.biometric_enrolled", u, map[string]any{"user_id": u, "device_id": first}))

	got := e.list(u)
	kinds := []string{}
	for _, n := range got {
		kinds = append(kinds, n.Kind)
	}
	if strings.Join(kinds, ",") != "biometric_enrolled,session_revoked,pin_reset,new_device" {
		t.Fatalf("kinds %v", kinds)
	}
	if got[3].Body != "Your account was signed in on a new iPhone device. If this wasn't you, reset your PIN now." {
		t.Errorf("new device: %q", got[3].Body)
	}
}

func TestMalformedEventIsSkippedNotRetried(t *testing.T) {
	e := setup(t)
	m := outbox.Message{ID: 1 << 53, Topic: "payment.succeeded", Payload: []byte(`{"amount": "lots"}`)}
	if err := e.svc.Handle(e.ctx, m); err != nil {
		t.Errorf("a malformed event must not block the ones behind it: %v", err)
	}
}

func TestListAndMarkRead(t *testing.T) {
	e := setup(t)
	u, other := e.user("Meron", "en"), e.user("Yonas", "en")
	payer := e.user("Sara", "en")
	for i := 0; i < 5; i++ {
		in := e.intent("p2p", payer, "user", u, int64(100*(i+1)), "wallet", "requires_confirmation", "succeeded")
		e.handle(paymentEvent("payment.succeeded", in, "p2p", payer, "user", u, int64(100*(i+1)), "succeeded", "wallet"))
	}
	oin := e.intent("p2p", payer, "user", other, 100, "wallet", "requires_confirmation", "succeeded")
	e.handle(paymentEvent("payment.succeeded", oin, "p2p", payer, "user", other, 100, "succeeded", "wallet"))

	var seen []uuid.UUID
	cursor := ""
	for {
		p, err := e.svc.List(e.ctx, u, cursor, 2)
		if err != nil {
			t.Fatal(err)
		}
		if p.Unread != 5 {
			t.Fatalf("unread %d", p.Unread)
		}
		for _, n := range p.Items {
			seen = append(seen, n.ID)
		}
		if p.NextCursor == "" {
			break
		}
		cursor = p.NextCursor
	}
	if len(seen) != 5 {
		t.Fatalf("paged through %d", len(seen))
	}

	otherItems := e.list(other)
	n, err := e.svc.MarkRead(e.ctx, u, []uuid.UUID{seen[0], otherItems[0].ID})
	if err != nil || n != 1 {
		t.Fatalf("mark one (another user's id is ignored): %d %v", n, err)
	}
	if e.list(other)[0].Read {
		t.Error("marked another user's notification read")
	}
	if n, _ := e.svc.MarkRead(e.ctx, u, nil); n != 4 {
		t.Errorf("mark all: %d", n)
	}
	if p, _ := e.svc.List(e.ctx, u, "", 1); p.Unread != 0 || !p.Items[0].Read {
		t.Errorf("after mark all: %+v", p)
	}
	if _, err := e.svc.List(e.ctx, u, "garbage", 10); !errors.Is(err, notify.ErrBadCursor) {
		t.Errorf("bad cursor: %v", err)
	}
}

type fakePusher struct {
	mu      sync.Mutex
	sent    map[string][]notify.Push
	invalid map[string]bool
}

func (f *fakePusher) Push(_ context.Context, token string, p notify.Push) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.invalid[token] {
		return notify.ErrTokenInvalid
	}
	f.sent[token] = append(f.sent[token], p)
	return nil
}

func TestPushGoesOnlyToSignedInDevices(t *testing.T) {
	e := setup(t)
	fp := &fakePusher{sent: map[string][]notify.Push{}, invalid: map[string]bool{}}
	e.svc.Pusher = fp
	u, payer := e.user("Lidya", "en"), e.user("Tesfaye", "en")
	phone, tablet, old, stale := e.device(u, "android"), e.device(u, "ios"), e.device(u, "android"), e.device(u, "android")
	e.session(u, phone, true)
	e.session(u, tablet, true)
	e.session(u, old, false) // signed out by a PIN reset
	e.session(u, stale, true)
	tok := func() string { return "tok-" + uuid.NewString() }
	tPhone, tTablet, tOld, tStale := tok(), tok(), tok(), tok()
	for dev, tk := range map[uuid.UUID]string{phone: tPhone, tablet: tTablet, old: tOld, stale: tStale} {
		if err := e.svc.RegisterPushToken(e.ctx, u, dev, tk); err != nil {
			t.Fatal(err)
		}
	}
	fp.invalid[tStale] = true // the app was uninstalled

	in := e.intent("p2p", payer, "user", u, 12_345, "wallet", "requires_confirmation", "succeeded")
	e.handle(paymentEvent("payment.succeeded", in, "p2p", payer, "user", u, 12_345, "succeeded", "wallet"))

	if len(fp.sent[tPhone]) != 1 || len(fp.sent[tTablet]) != 1 {
		t.Fatalf("signed-in devices: %v", fp.sent)
	}
	if len(fp.sent[tOld]) != 0 {
		t.Error("pushed to a signed-out device")
	}
	p := fp.sent[tPhone][0]
	if p.Title != "Money received" || p.Body != "Tesfaye sent you ETB 123.45." || p.Data["intent_id"] != in.String() || p.Data["notification_id"] == "" {
		t.Errorf("push: %+v", p)
	}
	var left int
	_ = pool.QueryRow(e.ctx, `SELECT count(*) FROM notify.push_tokens WHERE token = $1`, tStale).Scan(&left)
	if left != 0 {
		t.Error("an invalid token should be forgotten")
	}
	var pushed bool
	_ = pool.QueryRow(e.ctx, `SELECT pushed_at IS NOT NULL FROM notify.notifications WHERE user_id = $1`, u).Scan(&pushed)
	if !pushed {
		t.Error("pushed_at not set")
	}
}

func TestRegisterPushToken(t *testing.T) {
	e := setup(t)
	a, b := e.user("A", "en"), e.user("B", "en")
	da, db, web := e.device(a, "android"), e.device(b, "android"), e.device(a, "web")
	tk := "tok-" + uuid.NewString()
	if err := e.svc.RegisterPushToken(e.ctx, a, da, tk); err != nil {
		t.Fatal(err)
	}
	// The same phone signs in as someone else: the token follows the device to the new account.
	if err := e.svc.RegisterPushToken(e.ctx, b, db, tk); err != nil {
		t.Fatal(err)
	}
	var owner uuid.UUID
	_ = pool.QueryRow(e.ctx, `SELECT user_id FROM notify.push_tokens WHERE token = $1`, tk).Scan(&owner)
	if owner != b {
		t.Errorf("token owner %s, want %s", owner, b)
	}
	var v *notify.ValidationError
	if err := e.svc.RegisterPushToken(e.ctx, a, web, "tok-web"); !errors.As(err, &v) {
		t.Errorf("web device: %v", err)
	}
	if err := e.svc.RegisterPushToken(e.ctx, a, db, "tok-x"); !errors.As(err, &v) {
		t.Errorf("someone else's device: %v", err)
	}
	if err := e.svc.RegisterPushToken(e.ctx, a, da, "  "); !errors.As(err, &v) {
		t.Errorf("empty token: %v", err)
	}
}

func TestMerchantNotificationsGoToOwnersAndAccountants(t *testing.T) {
	e := setup(t)
	shop := e.merchant("Tomoca Bole")
	owner, accountant, cashier := e.user("Owner", "en"), e.user("Accountant", "am"), e.user("Cashier", "en")
	for u, role := range map[uuid.UUID]string{owner: "owner", accountant: "accountant", cashier: "cashier"} {
		e.exec(`INSERT INTO merchant.members (merchant_id, user_id, role) VALUES ($1, $2, $3)`, shop, u, role)
	}
	payout := uuid.New()
	net := map[string]any{"amount_minor": 348_25, "currency": "ETB"}
	e.handle(event("payout.settled", shop, map[string]any{"merchant_id": shop, "payout_id": payout, "net": net, "destination": "telebirr ••3344"}))
	e.handle(event("payout.on_hold", shop, map[string]any{"merchant_id": shop, "payout_id": payout, "net": net, "reason": "no_destination"}))
	e.handle(event("merchant.payout_destination_changed", shop, map[string]any{
		"merchant_id": shop, "label": "CBETETAA ••5678", "effective_at": "2026-11-24T09:00:00Z"}))

	got := e.list(owner)
	if len(got) != 3 || got[0].Kind != "payout_destination_changed" || got[1].Kind != "payout_on_hold_no_destination" || got[2].Kind != "payout_settled" {
		t.Fatalf("owner: %+v", got)
	}
	if got[2].Body != "ETB 348.25 from Tomoca Bole's sales was paid to telebirr ••3344." {
		t.Errorf("settled: %q", got[2].Body)
	}
	if got[0].Body != "Payouts for Tomoca Bole will go to CBETETAA ••5678 from 24 Nov 2026, 12:00. If you didn't make this change, contact support now." {
		t.Errorf("destination changed: %q", got[0].Body)
	}
	if a := e.list(accountant); len(a) != 3 || a[2].Title != "ክፍያ ተልኳል" {
		t.Errorf("accountant (Amharic): %+v", a)
	}
	if c := e.list(cashier); len(c) != 0 {
		t.Errorf("cashier: %+v", c)
	}
}

func TestMerchantDecisionsTellTheOwnerWhy(t *testing.T) {
	e := setup(t)
	shop := e.merchant("Kaldi Piassa")
	owner := e.user("Owner", "en")
	e.exec(`INSERT INTO merchant.members (merchant_id, user_id, role) VALUES ($1, $2, 'owner')`, shop, owner)
	e.handle(event("merchant.suspended", shop, map[string]any{"merchant_id": shop, "reason": "Unusual refunds under review"}))
	got := e.list(owner)
	if len(got) != 1 || got[0].Title != "Kaldi Piassa is paused" ||
		got[0].Body != "Your shop can't take payments for now. Reason: Unusual refunds under review. Money already earned is kept for you." {
		t.Fatalf("%+v", got)
	}
}

func TestARejectedDocumentTellsTheOwnerWhatToResend(t *testing.T) {
	e := setup(t)
	shop := e.merchant("Abay Kiosk")
	owner := e.user("Owner", "en")
	e.exec(`INSERT INTO merchant.members (merchant_id, user_id, role) VALUES ($1, $2, 'owner')`, shop, owner)
	e.handle(event("merchant.document_rejected", shop, map[string]any{"merchant_id": shop, "document": "owner_id",
		"reason": "The photo is cut off at the bottom"}))
	got := e.list(owner)
	if len(got) != 1 || got[0].Title != "Abay Kiosk: send your ID again" ||
		got[0].Body != "We couldn't accept it. The photo is cut off at the bottom. Add a new photo in Profile → Business." {
		t.Fatalf("%+v", got)
	}
}
