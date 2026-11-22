package ops_test

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"superapp/backend/internal/kyb"
	"superapp/backend/internal/kyb/kybtest"
	"superapp/backend/internal/ledger"
	"superapp/backend/internal/merchants"
	"superapp/backend/internal/ops"
	"superapp/backend/internal/payouts"
	"superapp/backend/internal/platform"
)

var (
	poolOnce sync.Once
	pool     *pgxpool.Pool
	poolErr  error
)

type env struct {
	t   *testing.T
	ctx context.Context
	svc *ops.Service
	now time.Time
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
	e := &env{t: t, ctx: context.Background(), now: time.Now()}
	m := &merchants.Service{Pool: pool}
	p := payouts.NewService(pool, ledger.New(), &platform.PINVerifier{Pool: pool})
	e.svc = &ops.Service{Pool: pool, Merchants: m, Payouts: p, Now: func() time.Time { return e.now }}
	m.Now, p.Now = e.svc.Now, e.svc.Now
	return e
}

func (e *env) exec(sql string, args ...any) {
	e.t.Helper()
	if _, err := pool.Exec(e.ctx, sql, args...); err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) staff(name string, roles ...string) ops.Actor {
	e.t.Helper()
	email := fmt.Sprintf("%s.%s@gebeya.test", name, uuid.NewString()[:8])
	id, err := e.svc.Invite(e.ctx, email, name, roles, ops.Bootstrap)
	if err != nil {
		e.t.Fatal(err)
	}
	st, err := e.svc.LoadStaff(e.ctx, id)
	if err != nil {
		e.t.Fatal(err)
	}
	return ops.Actor{Staff: st, IP: "10.0.0.7", Device: "test browser"}
}

func (e *env) merchant(status string) uuid.UUID {
	e.t.Helper()
	owner, id := uuid.New(), uuid.New()
	e.exec(`INSERT INTO identity.users (id, phone_e164, display_name, status) VALUES ($1, $2, 'Tsion Alemu', 'active')`,
		owner, fmt.Sprintf("+2519%08d", binary.BigEndian.Uint64(owner[:8])%100_000_000))
	e.exec(`INSERT INTO merchant.merchants (id, legal_name, trade_name, tin, category_code, city, status, applied_by)
		VALUES ($1, 'Selam PLC', 'Selam Café', $2, '5812', 'Addis Ababa', $3, $4)`,
		id, fmt.Sprintf("%010d", binary.BigEndian.Uint64(id[:8])%10_000_000_000), status, owner)
	e.exec(`INSERT INTO merchant.members (merchant_id, user_id, role) VALUES ($1, $2, 'owner')`, id, owner)
	return id
}

// fakePayout adds a payout with no money reserved behind it, and takes it out of the sending queue when
// the test ends: payout sending is global, so another package's test would otherwise try to pay it.
func (e *env) fakePayout(shop uuid.UUID, net int64, reason string, attempts int) uuid.UUID {
	e.t.Helper()
	id := uuid.New()
	e.exec(`INSERT INTO merchant.payouts (id, merchant_id, period_start, period_end, gross_minor, commission_minor, fees_minor,
			refunds_minor, net_minor, currency, status, hold_reason, attempts, scheduled_for)
		VALUES ($1, $2, now(), now(), $3, 0, 0, 0, $3, 'ETB', 'on_hold', $4, $5, now())`, id, shop, net, reason, attempts)
	e.t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `UPDATE merchant.payouts SET status = 'on_hold', hold_reason = 'too_many_failures' WHERE id = $1`, id)
	})
	return id
}

func (e *env) status(merchant uuid.UUID) string {
	var s string
	if err := pool.QueryRow(e.ctx, `SELECT status FROM merchant.merchants WHERE id = $1`, merchant).Scan(&s); err != nil {
		e.t.Fatal(err)
	}
	return s
}

func (e *env) auditResults(target uuid.UUID) []string {
	rows, err := e.svc.Audit(e.ctx, target, 50)
	if err != nil {
		e.t.Fatal(err)
	}
	var out []string
	for i := len(rows) - 1; i >= 0; i-- {
		out = append(out, rows[i].Action+":"+rows[i].Result)
	}
	return out
}

const note = "TIN and licence checked against the registry"

func TestMerchantOpsApproveAloneOthersCannot(t *testing.T) {
	e := setup(t)
	mops, risk := e.staff("Yonas", "merchant_ops"), e.staff("Selam", "risk_analyst")
	app := e.merchant("kyb_review")

	if _, err := e.svc.Do(e.ctx, risk, ops.Action{Kind: "merchant.approve", Target: app, Note: note}); !errors.Is(err, ops.ErrForbidden) {
		t.Fatalf("risk analyst (view only): %v", err)
	}
	if _, err := e.svc.Do(e.ctx, mops, ops.Action{Kind: "merchant.approve", Target: app, Note: "ok"}); !errors.Is(err, ops.ErrNoteMissing) {
		t.Fatalf("no note: %v", err)
	}
	out, err := e.svc.Do(e.ctx, mops, ops.Action{Kind: "merchant.approve", Target: app, Note: note})
	if err != nil || !out.Applied || e.status(app) != "active" {
		t.Fatalf("merchant ops: %+v %v", out, err)
	}
	var no, by string
	_ = pool.QueryRow(e.ctx, `SELECT merchant_no, approved_by::text FROM merchant.merchants WHERE id = $1`, app).Scan(&no, &by)
	if !merchants.ValidMerchantNo(no) || by != mops.Staff.ID.String() {
		t.Errorf("approval details: %s %s", no, by)
	}
	if got := fmt.Sprint(e.auditResults(app)); got != "[merchant.approve:denied merchant.approve:allowed]" {
		t.Errorf("audit: %s", got)
	}
	// Approving twice is refused (it's no longer under review).
	if _, err := e.svc.Do(e.ctx, mops, ops.Action{Kind: "merchant.approve", Target: app, Note: note}); err == nil {
		t.Error("approved an active merchant again")
	}

	rejected := e.merchant("kyb_review")
	if _, err := e.svc.Do(e.ctx, mops, ops.Action{Kind: "merchant.reject", Target: rejected, Note: "The TIN belongs to another business."}); err != nil {
		t.Fatal(err)
	}
	if e.status(rejected) != "closed" {
		t.Errorf("rejected: %s", e.status(rejected))
	}
}

func TestSuspendNeedsASecondPerson(t *testing.T) {
	e := setup(t)
	maker, checker := e.staff("Selam", "risk_analyst"), e.staff("Dawit", "risk_analyst")
	finance := e.staff("Biniam", "finance_officer")
	shop := e.merchant("active")
	reason := "Card-testing pattern: 40 payments of ETB 1 in 5 minutes"

	// Can't ask for something that can't happen.
	if _, err := e.svc.Do(e.ctx, maker, ops.Action{Kind: "merchant.reinstate", Target: shop, Note: reason}); err == nil {
		t.Fatal("asked to reinstate an active merchant")
	}
	out, err := e.svc.Do(e.ctx, maker, ops.Action{Kind: "merchant.suspend", Target: shop, Note: reason})
	if err != nil || out.Applied || out.ApprovalID == uuid.Nil || e.status(shop) != "active" {
		t.Fatalf("request: %+v %v", out, err)
	}
	if _, err := e.svc.Do(e.ctx, checker, ops.Action{Kind: "merchant.suspend", Target: shop, Note: reason}); !errors.Is(err, ops.ErrDuplicate) {
		t.Errorf("second identical request: %v", err)
	}
	if err := e.svc.Decide(e.ctx, maker, out.ApprovalID, true, "approving my own request"); !errors.Is(err, ops.ErrOwnRequest) {
		t.Errorf("own request: %v", err)
	}
	if err := e.svc.Decide(e.ctx, finance, out.ApprovalID, true, "looks right to me, go"); !errors.Is(err, ops.ErrForbidden) {
		t.Errorf("no grant: %v", err)
	}
	if err := e.svc.Decide(e.ctx, checker, out.ApprovalID, true, "Confirmed the pattern in the case"); err != nil {
		t.Fatal(err)
	}
	if e.status(shop) != "suspended" {
		t.Fatalf("after approval: %s", e.status(shop))
	}
	if err := e.svc.Decide(e.ctx, checker, out.ApprovalID, true, "Confirmed the pattern in the case"); !errors.Is(err, ops.ErrDecided) {
		t.Errorf("deciding twice: %v", err)
	}
	var told string
	_ = pool.QueryRow(e.ctx, `SELECT payload->>'reason' FROM platform.outbox WHERE aggregate_id = $1 AND topic = 'merchant.suspended'`, shop).Scan(&told)
	if told != reason {
		t.Errorf("merchant told %q", told)
	}
	want := "[merchant.reinstate:failed merchant.suspend:pending merchant.suspend:failed approval.approve:denied approval.approve:denied approval.approve:allowed]"
	if got := fmt.Sprint(e.auditResults(shop)); got != want {
		t.Errorf("audit: %s", got)
	}

	// A rejected request changes nothing.
	out, err = e.svc.Do(e.ctx, maker, ops.Action{Kind: "merchant.reinstate", Target: shop, Note: "Customer verified, false positive"})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.svc.Decide(e.ctx, checker, out.ApprovalID, false, ""); err != nil || e.status(shop) != "suspended" {
		t.Errorf("rejected request: %v %s", err, e.status(shop))
	}
	// Requests lapse.
	out, _ = e.svc.Do(e.ctx, maker, ops.Action{Kind: "merchant.reinstate", Target: shop, Note: "Customer verified, false positive"})
	e.now = e.now.Add(73 * time.Hour)
	if err := e.svc.Decide(e.ctx, checker, out.ApprovalID, true, "Customer verified, ok"); !errors.Is(err, ops.ErrDecided) {
		t.Errorf("lapsed: %v", err)
	}
}

func TestReleasingAHeldPayout(t *testing.T) {
	e := setup(t)
	f1, f2 := e.staff("Biniam", "finance_officer"), e.staff("Hana", "finance_officer")
	shop := e.merchant("active")
	owner := uuid.Nil
	_ = pool.QueryRow(e.ctx, `SELECT user_id FROM merchant.members WHERE merchant_id = $1`, shop).Scan(&owner)
	e.exec(`INSERT INTO merchant.payout_destinations (merchant_id, rail, account_ref, label, created_by)
		VALUES ($1, 'telebirr', '+251911223344', 'telebirr ••3344', $2)`, shop, owner)
	payout := e.fakePayout(shop, 10000, "rejected_by_partner", 2)

	out, err := e.svc.Do(e.ctx, f1, ops.Action{Kind: "payout.release", Target: payout, Note: "Owner fixed the wallet number"})
	if err != nil || out.ApprovalID == uuid.Nil {
		t.Fatalf("request: %+v %v", out, err)
	}
	if err := e.svc.Decide(e.ctx, f2, out.ApprovalID, true, "Checked the new account with the owner"); err != nil {
		t.Fatal(err)
	}
	var status string
	var attempts, base int
	_ = pool.QueryRow(e.ctx, `SELECT status, attempts, attempt_base FROM merchant.payouts WHERE id = $1`, payout).Scan(&status, &attempts, &base)
	if status != "scheduled" || base != 2 || attempts != 2 {
		t.Errorf("released: %s attempts %d base %d", status, attempts, base)
	}
	// Nothing to release any more.
	if _, err := e.svc.Do(e.ctx, f1, ops.Action{Kind: "payout.release", Target: payout, Note: "Owner fixed the wallet number"}); err == nil {
		t.Error("released a scheduled payout")
	}
}

func TestReinstatingResumesPayoutsHeldForTheSuspension(t *testing.T) {
	e := setup(t)
	admin1, admin2 := e.staff("Tigist", "super_admin"), e.staff("Abel", "super_admin")
	shop := e.merchant("suspended")
	payout := e.fakePayout(shop, 5000, "merchant_suspended", 0)
	out, err := e.svc.Do(e.ctx, admin1, ops.Action{Kind: "merchant.reinstate", Target: shop, Note: "Review closed, no fraud found"})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.svc.Decide(e.ctx, admin2, out.ApprovalID, true, "Agreed after reading the case"); err != nil {
		t.Fatal(err)
	}
	var status string
	_ = pool.QueryRow(e.ctx, `SELECT status FROM merchant.payouts WHERE id = $1`, payout).Scan(&status)
	if e.status(shop) != "active" || status != "scheduled" {
		t.Errorf("shop %s payout %s", e.status(shop), status)
	}
}

func TestAuditChainHoldsUnderConcurrentWrites(t *testing.T) {
	e := setup(t)
	a := e.staff("Tigist", "super_admin")
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := e.svc.AuditSystem(e.ctx, a, "staff.sign_in", "allowed", map[string]any{"i": i, "note": "ünïcode ✓"}); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	n, broken, err := e.svc.VerifyAudit(e.ctx)
	if err != nil || broken != 0 || n < 8 {
		t.Fatalf("chain: %d rows, broken at %d, %v", n, broken, err)
	}
	if _, err := pool.Exec(e.ctx, `UPDATE admin.audit_log SET result = 'allowed' WHERE result = 'denied'`); err == nil {
		t.Error("the audit log accepted an update")
	}
}

func TestSessionsAndSignIn(t *testing.T) {
	e := setup(t)
	email := fmt.Sprintf("hana.%s@gebeya.test", uuid.NewString()[:8])
	id, err := e.svc.Invite(e.ctx, email, "Hana", []string{"support_agent"}, ops.Bootstrap)
	if err != nil {
		t.Fatal(err)
	}
	// Only the invited email can bind; after that only the subject counts.
	if _, err := e.svc.StaffForLogin(e.ctx, "idp|someone-else", "other@gebeya.test"); !errors.Is(err, ops.ErrForbidden) {
		t.Errorf("stranger: %v", err)
	}
	subject := "idp|" + uuid.NewString()
	if got, err := e.svc.StaffForLogin(e.ctx, subject, email); err != nil || got != id {
		t.Fatalf("first sign-in: %v %v", got, err)
	}
	if _, err := e.svc.StaffForLogin(e.ctx, "idp|attacker", email); !errors.Is(err, ops.ErrForbidden) {
		t.Errorf("email reused by another subject: %v", err)
	}

	token, err := e.svc.StartSession(e.ctx, id, "10.0.0.7", "test")
	if err != nil {
		t.Fatal(err)
	}
	s, err := e.svc.SessionFrom(e.ctx, token)
	if err != nil || s.Staff.ID != id || len(s.CSRF) != 64 || !s.Staff.Can("refunds.small", ops.Full) || s.Staff.Can("merchants.approve", ops.View) {
		t.Fatalf("session: %+v %v", s, err)
	}
	e.now = e.now.Add(31 * time.Minute)
	if _, err := e.svc.SessionFrom(e.ctx, token); !errors.Is(err, ops.ErrForbidden) {
		t.Errorf("idle session: %v", err)
	}
	e.now = time.Now()
	token, _ = e.svc.StartSession(e.ctx, id, "", "")
	e.exec(`UPDATE admin.staff SET status = 'suspended' WHERE id = $1`, id)
	if _, err := e.svc.SessionFrom(e.ctx, token); !errors.Is(err, ops.ErrForbidden) {
		t.Errorf("suspended staff: %v", err)
	}
}

func TestApprovalWaitsForAcceptedDocuments(t *testing.T) {
	e := setup(t)
	key := make([]byte, 32)
	_, _ = rand.Read(key)
	sealer, _ := kyb.ParseKeys("k1:" + base64.StdEncoding.EncodeToString(key))
	e.svc.KYB = &kyb.Service{Pool: pool, Store: kybtest.NewMemStore(), Sealer: sealer}
	shop := e.merchant("kyb_review")
	var owner uuid.UUID
	_ = pool.QueryRow(e.ctx, `SELECT user_id FROM merchant.members WHERE merchant_id = $1`, shop).Scan(&owner)
	yonas := e.staff("yonas", "merchant_ops")
	support := e.staff("hana", "support_agent")
	approve := ops.Action{Kind: "merchant.approve", Target: shop, Note: "TIN and licence checked with the registries"}

	var c *merchants.ConflictError
	if _, err := e.svc.Do(e.ctx, yonas, approve); !errors.As(err, &c) || c.Msg != "Accept these documents first: TIN certificate, Owner's ID (Fayda, kebele ID or passport)." {
		t.Fatalf("approve without documents: %v", err)
	}
	tin, _ := e.svc.KYB.Upload(e.ctx, owner, shop, "tin_certificate", kybtest.Photo(uuid.NewString()))
	id, _ := e.svc.KYB.Upload(e.ctx, owner, shop, "owner_id", kybtest.Photo(uuid.NewString()))

	// Support staff can't open or decide documents; refusals are recorded.
	if _, _, err := e.svc.OpenDocument(e.ctx, support, tin.ID); !errors.Is(err, ops.ErrForbidden) {
		t.Errorf("support opened a document: %v", err)
	}
	if _, err := e.svc.ReviewDocument(e.ctx, support, tin.ID, true, ""); !errors.Is(err, ops.ErrForbidden) {
		t.Errorf("support accepted a document: %v", err)
	}
	if _, data, err := e.svc.OpenDocument(e.ctx, yonas, tin.ID); err != nil || len(data) != tin.Size {
		t.Fatalf("open: %v", err)
	}
	for _, d := range []uuid.UUID{tin.ID, id.ID} {
		if _, err := e.svc.ReviewDocument(e.ctx, yonas, d, true, ""); err != nil {
			t.Fatal(err)
		}
	}
	if out, err := e.svc.Do(e.ctx, yonas, approve); err != nil || !out.Applied || e.status(shop) != "active" {
		t.Fatalf("approve: %v %+v %s", err, out, e.status(shop))
	}
	var got []string
	rows, _ := pool.Query(e.ctx, `SELECT action || ':' || result FROM admin.audit_log
		WHERE target_id = $1 OR detail->>'merchant_id' = $1::text OR target_id IN ($2, $3) ORDER BY id`, shop, tin.ID, id.ID)
	for rows.Next() {
		var s string
		_ = rows.Scan(&s)
		got = append(got, s)
	}
	want := []string{"merchant.approve:failed", "document.view:denied", "document.accept:denied", "document.view:allowed",
		"document.accept:allowed", "document.accept:allowed", "merchant.approve:allowed"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("audit\n got %v\nwant %v", got, want)
	}
}
