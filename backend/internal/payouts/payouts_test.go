package payouts_test

// Integration tests against PostgreSQL loaded with db/schema.sql (set TEST_DATABASE_URL). Each test makes
// its own merchant and people. Sales are posted to the ledger the way a QR payment posts them.

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"superapp/backend/internal/ledger"
	"superapp/backend/internal/money"
	"superapp/backend/internal/payouts"
	"superapp/backend/internal/platform"
)

var (
	poolOnce sync.Once
	pool     *pgxpool.Pool
	poolErr  error
)

const pin = "582047"

var pinHash = func() string { h, _ := platform.HashPIN(pin); return h }()

type env struct {
	t        *testing.T
	ctx      context.Context
	svc      *payouts.Service
	l        *ledger.Ledger
	now      time.Time
	merchant uuid.UUID
	owner    uuid.UUID
	rail     *fakeRail
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
	e := &env{t: t, ctx: context.Background(), l: ledger.New(), now: time.Now()}
	e.svc = payouts.NewService(pool, e.l, &platform.PINVerifier{Pool: pool})
	e.svc.Now = func() time.Time { return e.now }
	e.rail = &fakeRail{script: map[string][]payouts.RailResult{}, sent: map[uuid.UUID][]string{}}
	e.merchant = uuid.New()
	e.exec(`INSERT INTO merchant.merchants (id, legal_name, trade_name, category_code, status, payout_schedule)
		VALUES ($1, 'Test PLC', 'Selam Café', '5812', 'active', 'instant')`, e.merchant)
	e.owner = e.member("owner")
	return e
}

func (e *env) exec(sql string, args ...any) {
	e.t.Helper()
	if _, err := pool.Exec(e.ctx, sql, args...); err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) member(role string) uuid.UUID {
	e.t.Helper()
	id := uuid.New()
	e.exec(`INSERT INTO identity.users (id, phone_e164, status) VALUES ($1, $2, 'active')`,
		id, fmt.Sprintf("+2519%08d", binary.BigEndian.Uint64(id[:8])%100_000_000))
	e.exec(`INSERT INTO identity.credentials (user_id, pin_hash) VALUES ($1, $2)`, id, pinHash)
	e.exec(`INSERT INTO merchant.members (merchant_id, user_id, role) VALUES ($1, $2, $3)`, e.merchant, id, role)
	return id
}

// sale posts a QR payment of gross with the given commission, as payments does.
func (e *env) sale(gross, commission int64) uuid.UUID {
	e.t.Helper()
	id := uuid.New()
	err := pgx.BeginFunc(e.ctx, pool, func(tx pgx.Tx) error {
		cur := money.ETB
		payer, err := e.l.EnsureAccount(e.ctx, tx, ledger.Suspense, ledger.OwnerPlatform, uuid.Nil, cur)
		if err != nil {
			return err
		}
		settle, err := e.l.EnsureAccount(e.ctx, tx, ledger.MerchantSettlement, ledger.OwnerMerchant, e.merchant, cur)
		if err != nil {
			return err
		}
		rev, err := e.l.EnsureAccount(e.ctx, tx, ledger.CommissionRevenue, ledger.OwnerPlatform, uuid.Nil, cur)
		if err != nil {
			return err
		}
		ps := []ledger.Posting{
			{AccountID: payer, Side: ledger.Debit, AmountMinor: gross, Currency: cur},
			{AccountID: settle, Side: ledger.Credit, AmountMinor: gross - commission, Currency: cur},
		}
		if commission > 0 {
			ps = append(ps, ledger.Posting{AccountID: rev, Side: ledger.Credit, AmountMinor: commission, Currency: cur})
		}
		_, err = e.l.Post(e.ctx, tx, ledger.Entry{Kind: "qr_payment", SourceType: "payments.intent", SourceID: id, Postings: ps})
		return err
	})
	if err != nil {
		e.t.Fatal(err)
	}
	return id
}

func (e *env) balance(kind ledger.AccountKind, owner ledger.OwnerType, id uuid.UUID) int64 {
	e.t.Helper()
	acct, ok, err := e.l.FindAccount(e.ctx, pool, kind, owner, id, money.ETB)
	if err != nil {
		e.t.Fatal(err)
	}
	if !ok {
		return 0
	}
	b, err := e.l.Balance(e.ctx, pool, acct)
	if err != nil {
		e.t.Fatal(err)
	}
	return b
}

func (e *env) payout(id uuid.UUID) payouts.Payout {
	e.t.Helper()
	list, err := e.svc.List(e.ctx, e.owner, e.merchant, nil, uuid.Nil, 100)
	if err != nil {
		e.t.Fatal(err)
	}
	for _, p := range list {
		if p.ID == id {
			return p
		}
	}
	e.t.Fatalf("payout %s not listed", id)
	return payouts.Payout{}
}

func (e *env) events() []string {
	e.t.Helper()
	rows, _ := pool.Query(e.ctx, `SELECT topic FROM platform.outbox WHERE aggregate_id = $1 ORDER BY id`, e.merchant)
	topics, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		e.t.Fatal(err)
	}
	return topics
}

// fakeRail stands in for a licensed payout partner. Each request id gets the next scripted answer.
type fakeRail struct {
	mu       sync.Mutex
	script   map[string][]payouts.RailResult // by payout id; consumed in order
	failSend map[uuid.UUID]int               // fail this many sends of a payout with an unknown outcome
	sent     map[uuid.UUID][]string          // request ids seen per payout
	queried  []string
}

func (f *fakeRail) next(payout string) payouts.RailResult {
	q := f.script[payout]
	if len(q) == 0 {
		return payouts.RailResult{State: payouts.RailPaid, Ref: "TX-" + payout[:8]}
	}
	f.script[payout] = q[1:]
	return q[0]
}

func (f *fakeRail) Send(_ context.Context, t payouts.Transfer) (payouts.RailResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sent[t.PayoutID] = append(f.sent[t.PayoutID], t.RequestID)
	if f.failSend[t.PayoutID] > 0 {
		f.failSend[t.PayoutID]--
		return payouts.RailResult{}, errors.New("timeout")
	}
	return f.next(t.PayoutID.String()), nil
}

func (f *fakeRail) Query(_ context.Context, requestID string) (payouts.RailResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.queried = append(f.queried, requestID)
	return f.next(requestID[:36]), nil
}

func (e *env) setDestination() {
	e.t.Helper()
	if _, err := e.svc.SetDestination(e.ctx, e.owner, e.merchant, "telebirr", "", "+251911223344", pin); err != nil {
		e.t.Fatal(err)
	}
}

func TestPayoutLifecycle(t *testing.T) {
	e := setup(t)
	e.sale(100_00, 50)  // ETB 100, 0.5% commission
	e.sale(250_00, 125) // ETB 250
	time.Sleep(10 * time.Millisecond)

	// No payout account yet: the money is reserved and the payout waits for one.
	p, err := e.svc.Batch(e.ctx, e.merchant, time.Now())
	if err != nil || p == nil {
		t.Fatalf("batch: %v %v", p, err)
	}
	if p.Status != payouts.OnHold || p.HoldReason != payouts.HoldNoDestination ||
		p.Gross.AmountMinor != 350_00 || p.Commission.AmountMinor != 175 || p.Net.AmountMinor != 348_25 {
		t.Fatalf("payout %+v", p)
	}
	if s, h := e.balance(ledger.MerchantSettlement, ledger.OwnerMerchant, e.merchant), e.balance(ledger.Hold, ledger.OwnerMerchant, e.merchant); s != 0 || h != 348_25 {
		t.Fatalf("settlement %d hold %d", s, h)
	}
	if again, err := e.svc.Batch(e.ctx, e.merchant, time.Now()); again != nil || err != nil {
		t.Fatalf("sales paid out twice: %v %v", again, err)
	}

	// Setting the account starts the 48-hour cooling-off; nothing is sent before it ends.
	e.svc.Rails["telebirr"] = e.rail
	e.setDestination()
	if got := e.payout(p.ID); got.Status != payouts.Scheduled || !got.ScheduledFor.Equal(e.now.Add(48*time.Hour).Truncate(time.Microsecond)) && got.ScheduledFor.Sub(e.now.Add(48*time.Hour)).Abs() > time.Millisecond {
		t.Fatalf("after setting a destination: %+v", got)
	}
	if err := e.svc.SendDue(e.ctx); err != nil {
		t.Fatal(err)
	}
	if len(e.rail.sent[p.ID]) != 0 {
		t.Fatal("sent during the cooling-off period")
	}

	// After it: sent, the partner is still processing, then it's paid.
	e.now = e.now.Add(49 * time.Hour)
	e.rail.script[p.ID.String()] = []payouts.RailResult{{State: payouts.RailPending, Ref: "TB-1"}, {State: payouts.RailPaid}}
	if err := e.svc.SendDue(e.ctx); err != nil {
		t.Fatal(err)
	}
	if got := e.payout(p.ID); got.Status != payouts.Sent || got.PartnerRef != "TB-1" || got.DestinationName != "telebirr ••3344" {
		t.Fatalf("after sending: %+v", got)
	}
	e.now = e.now.Add(5 * time.Minute)
	if err := e.svc.CheckSent(e.ctx); err != nil {
		t.Fatal(err)
	}
	got := e.payout(p.ID)
	if got.Status != payouts.Settled || got.SettledAt == nil {
		t.Fatalf("after the partner paid: %+v", got)
	}
	if h := e.balance(ledger.Hold, ledger.OwnerMerchant, e.merchant); h != 0 {
		t.Errorf("hold after settling: %d", h)
	}
	if want := fmt.Sprintf("%s-1", p.ID); strings.Join(e.rail.sent[p.ID], ",") != want {
		t.Errorf("request ids %v", e.rail.sent[p.ID])
	}
	if ev := strings.Join(e.events(), ","); ev != "payout.on_hold,merchant.payout_destination_changed,payout.settled" {
		t.Errorf("events: %s", ev)
	}

	// The next sale is a payout of its own, with only that sale in it.
	e.sale(40_00, 20)
	time.Sleep(10 * time.Millisecond)
	p2, err := e.svc.Batch(e.ctx, e.merchant, time.Now())
	if err != nil || p2 == nil || p2.Net.AmountMinor != 39_80 || p2.Status != payouts.Scheduled {
		t.Fatalf("second payout: %+v %v", p2, err)
	}
}

func TestSmallBalancesAndLaterSalesWait(t *testing.T) {
	e := setup(t)
	e.setDestination()
	e.sale(5_00, 0) // below the ETB 10 minimum
	time.Sleep(10 * time.Millisecond)
	if p, err := e.svc.Batch(e.ctx, e.merchant, time.Now()); p != nil || err != nil {
		t.Fatalf("below the minimum: %+v %v", p, err)
	}
	cutoff := time.Now()
	time.Sleep(10 * time.Millisecond)
	e.sale(20_00, 0) // after the cutoff
	p, err := e.svc.Batch(e.ctx, e.merchant, cutoff)
	if p != nil || err != nil {
		t.Fatalf("a sale after the cutoff was included: %+v %v", p, err)
	}
	p, err = e.svc.Batch(e.ctx, e.merchant, time.Now())
	if err != nil || p == nil || p.Net.AmountMinor != 25_00 {
		t.Fatalf("carried over: %+v %v", p, err)
	}
}

func TestFailuresKeepTheMoneyReserved(t *testing.T) {
	e := setup(t)
	e.svc.Rails["telebirr"] = e.rail
	e.setDestination()
	e.now = e.now.Add(49 * time.Hour)
	e.sale(80_00, 0)
	time.Sleep(10 * time.Millisecond)
	p, err := e.svc.Batch(e.ctx, e.merchant, time.Now())
	if err != nil || p == nil {
		t.Fatal(p, err)
	}

	// A timeout: unknown outcome, so the same request id is retried.
	e.rail.failSend = map[uuid.UUID]int{p.ID: 1}
	if err := e.svc.SendDue(e.ctx); err != nil {
		t.Fatal(err)
	}
	if got := e.payout(p.ID); got.Status != payouts.Scheduled || got.LastError != "send_error" {
		t.Fatalf("after a timeout: %+v", got)
	}
	// A definite, temporary failure: retried later under a new request id.
	e.rail.script[p.ID.String()] = []payouts.RailResult{{State: payouts.RailFailed, Reason: "SYSTEM_BUSY"},
		{State: payouts.RailFailed, Permanent: true, Reason: "ACCOUNT_CLOSED"}}
	e.now = e.now.Add(11 * time.Minute)
	if err := e.svc.SendDue(e.ctx); err != nil {
		t.Fatal(err)
	}
	if got := e.payout(p.ID); got.Status != payouts.Scheduled || got.LastError != "partner: SYSTEM_BUSY" {
		t.Fatalf("after a temporary failure: %+v", got)
	}
	// A permanent one: on hold, and the merchant's money is still reserved for them.
	e.now = e.now.Add(time.Hour)
	if err := e.svc.SendDue(e.ctx); err != nil {
		t.Fatal(err)
	}
	got := e.payout(p.ID)
	if got.Status != payouts.OnHold || got.HoldReason != payouts.HoldRejected {
		t.Fatalf("after a permanent failure: %+v", got)
	}
	if ids := strings.Join(e.rail.sent[p.ID], ","); ids != fmt.Sprintf("%[1]s-1,%[1]s-1,%[1]s-2", p.ID) {
		t.Errorf("request ids: %s", ids)
	}
	if h := e.balance(ledger.Hold, ledger.OwnerMerchant, e.merchant); h != 80_00 {
		t.Errorf("reserved: %d", h)
	}
}

func TestNoRailMeansNothingIsSent(t *testing.T) {
	e := setup(t)
	e.setDestination() // no rail configured
	e.now = e.now.Add(49 * time.Hour)
	e.sale(30_00, 0)
	time.Sleep(10 * time.Millisecond)
	p, _ := e.svc.Batch(e.ctx, e.merchant, time.Now())
	if err := e.svc.SendDue(e.ctx); err != nil {
		t.Fatal(err)
	}
	if got := e.payout(p.ID); got.Status != payouts.Scheduled || got.LastError != "rail_unavailable" {
		t.Fatalf("without a licensed partner: %+v", got)
	}
}

func TestSuspendedMerchantIsHeld(t *testing.T) {
	e := setup(t)
	e.svc.Rails["telebirr"] = e.rail
	e.setDestination()
	e.now = e.now.Add(49 * time.Hour)
	e.sale(30_00, 0)
	time.Sleep(10 * time.Millisecond)
	p, _ := e.svc.Batch(e.ctx, e.merchant, time.Now())
	e.exec(`UPDATE merchant.merchants SET status = 'suspended' WHERE id = $1`, e.merchant)
	if err := e.svc.SendDue(e.ctx); err != nil {
		t.Fatal(err)
	}
	if got := e.payout(p.ID); got.Status != payouts.OnHold || got.HoldReason != payouts.HoldMerchantSuspended || len(e.rail.sent[p.ID]) != 0 {
		t.Fatalf("suspended: %+v", got)
	}
}

func TestOnlyOwnersChangeTheDestination(t *testing.T) {
	e := setup(t)
	cashier := e.member("cashier")
	accountant := e.member("accountant")
	if _, err := e.svc.SetDestination(e.ctx, cashier, e.merchant, "telebirr", "", "+251911223344", pin); !errors.Is(err, payouts.ErrForbidden) {
		t.Errorf("cashier: %v", err)
	}
	if _, err := e.svc.SetDestination(e.ctx, accountant, e.merchant, "telebirr", "", "+251911223344", pin); !errors.Is(err, payouts.ErrForbidden) {
		t.Errorf("accountant: %v", err)
	}
	if _, err := e.svc.SetDestination(e.ctx, uuid.New(), e.merchant, "telebirr", "", "+251911223344", pin); !errors.Is(err, payouts.ErrNotFound) {
		t.Errorf("stranger: %v", err)
	}
	if _, err := e.svc.SetDestination(e.ctx, e.owner, e.merchant, "telebirr", "", "+251911223344", "000000"); !errors.Is(err, platform.ErrInvalidPIN) {
		t.Errorf("wrong PIN: %v", err)
	}
	var v *payouts.ValidationError
	for _, c := range [][3]string{{"telebirr", "", "0911"}, {"ethswitch", "", "1000123456789"}, {"ethswitch", "CBETETAA", "12"}, {"card", "", "4111"}} {
		if _, err := e.svc.SetDestination(e.ctx, e.owner, e.merchant, c[0], c[1], c[2], pin); !errors.As(err, &v) {
			t.Errorf("%v: %v", c, err)
		}
	}
	d, err := e.svc.SetDestination(e.ctx, e.owner, e.merchant, "ethswitch", "cbetetaa", "1000 1234 5678", pin)
	if err != nil || d.Label != "CBETETAA ••5678" {
		t.Fatalf("bank: %+v %v", d, err)
	}
	if cur, err := e.svc.CurrentDestination(e.ctx, accountant, e.merchant); err != nil || cur.ID != d.ID {
		t.Errorf("accountant sees the destination: %+v %v", cur, err)
	}
	if _, err := e.svc.List(e.ctx, cashier, e.merchant, nil, uuid.Nil, 10); !errors.Is(err, payouts.ErrForbidden) {
		t.Errorf("cashier sees payouts: %v", err)
	}
}

func TestConcurrentBatchesClaimEachSaleOnce(t *testing.T) {
	e := setup(t)
	for i := 0; i < 5; i++ {
		e.sale(20_00, 10)
	}
	time.Sleep(10 * time.Millisecond)
	cutoff := time.Now()
	var wg sync.WaitGroup
	var mu sync.Mutex
	var total int64
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			p, err := e.svc.Batch(e.ctx, e.merchant, cutoff)
			if err != nil {
				t.Error(err)
				return
			}
			if p != nil {
				mu.Lock()
				total += p.Net.AmountMinor
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if total != 5*(20_00-10) {
		t.Errorf("paid out %d", total)
	}
}

func TestCutoff(t *testing.T) {
	// Wednesday 2026-11-25 02:30 in Addis Ababa (Tuesday 23:30 UTC).
	now := time.Date(2026, 11, 24, 23, 30, 0, 0, time.UTC)
	if got := payouts.Cutoff("daily", now); !got.Equal(time.Date(2026, 11, 25, 0, 0, 0, 0, payouts.EAT)) {
		t.Errorf("daily: %v", got)
	}
	if got := payouts.Cutoff("weekly", now); !got.Equal(time.Date(2026, 11, 23, 0, 0, 0, 0, payouts.EAT)) {
		t.Errorf("weekly: %v", got)
	}
	if got := payouts.Cutoff("instant", now); !got.Equal(now) {
		t.Errorf("instant: %v", got)
	}
}
