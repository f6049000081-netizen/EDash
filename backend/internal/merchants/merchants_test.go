package merchants_test

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
	"github.com/jackc/pgx/v5/pgxpool"

	"superapp/backend/internal/merchants"
)

var (
	poolOnce sync.Once
	pool     *pgxpool.Pool
	poolErr  error
)

type env struct {
	t   *testing.T
	ctx context.Context
	svc *merchants.Service
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
	return &env{t: t, ctx: context.Background(), svc: &merchants.Service{Pool: pool}}
}

func (e *env) exec(sql string, args ...any) {
	e.t.Helper()
	if _, err := pool.Exec(e.ctx, sql, args...); err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) user(name, status string) uuid.UUID {
	e.t.Helper()
	id := uuid.New()
	e.exec(`INSERT INTO identity.users (id, phone_e164, display_name, status) VALUES ($1, $2, NULLIF($3, ''), $4)`,
		id, fmt.Sprintf("+2519%08d", binary.BigEndian.Uint64(id[:8])%100_000_000), name, status)
	return id
}

// tin is a fresh random TIN, since tests share one database.
func tin() string {
	id := uuid.New()
	return fmt.Sprintf("%010d", binary.BigEndian.Uint64(id[:8])%10_000_000_000)
}

func app(t string) merchants.Application {
	return merchants.Application{LegalName: "Selam Trading PLC", TradeName: "Selam Café", TIN: t, Licence: "AA/14/1234/2016",
		Category: "5812", City: "Addis Ababa"}
}

func TestApplyAndApprove(t *testing.T) {
	e := setup(t)
	owner := e.user("Tsion Alemu", "active")
	taxID := tin()

	var v *merchants.ValidationError
	for _, bad := range []func(*merchants.Application){
		func(a *merchants.Application) { a.TIN = "12345" },
		func(a *merchants.Application) { a.TradeName = "" },
		func(a *merchants.Application) { a.Category = "0000" },
		func(a *merchants.Application) { a.Licence = "<script>" },
		func(a *merchants.Application) { a.City = "" },
	} {
		a := app(taxID)
		bad(&a)
		if _, err := e.svc.Apply(e.ctx, owner, a); !errors.As(err, &v) {
			t.Errorf("%+v: %v", a, err)
		}
	}

	m, err := e.svc.Apply(e.ctx, owner, app(taxID))
	if err != nil || m.Status != "kyb_review" || m.Role != "owner" || m.MerchantNo != "" {
		t.Fatalf("apply: %+v %v", m, err)
	}
	// Sending it again (a retry) returns the same application; a different one waits for the decision.
	if again, err := e.svc.Apply(e.ctx, owner, app(taxID)); err != nil || again.ID != m.ID {
		t.Errorf("retry: %+v %v", again, err)
	}
	if _, err := e.svc.Apply(e.ctx, owner, app(tin())); !errors.Is(err, merchants.ErrConflict) {
		t.Errorf("second application: %v", err)
	}
	// Someone else can't register the same TIN, and a user without a PIN can't apply.
	if _, err := e.svc.Apply(e.ctx, e.user("Other", "active"), app(taxID)); !errors.Is(err, merchants.ErrConflict) {
		t.Errorf("same TIN: %v", err)
	}
	if _, err := e.svc.Apply(e.ctx, e.user("New", "pending"), app(tin())); !errors.Is(err, merchants.ErrConflict) {
		t.Errorf("pending user: %v", err)
	}
	// Not approved yet: no QR.
	if _, _, err := e.svc.QR(e.ctx, owner, m.ID); !errors.Is(err, merchants.ErrNotActive) {
		t.Errorf("QR before approval: %v", err)
	}

	approved, err := e.svc.Approve(e.ctx, m.ID, uuid.New())
	if err != nil || approved.Status != "active" || !merchants.ValidMerchantNo(approved.MerchantNo) || approved.ApprovedAt == nil {
		t.Fatalf("approve: %+v %v", approved, err)
	}
	payload, _, err := e.svc.QR(e.ctx, owner, m.ID)
	if err != nil || !strings.Contains(payload, approved.MerchantNo) || !strings.Contains(payload, "5909Selam Caf") {
		t.Fatalf("QR: %q %v", payload, err)
	}
	if again, err := e.svc.Approve(e.ctx, m.ID, uuid.New()); err != nil || again.MerchantNo != approved.MerchantNo {
		t.Errorf("approving twice: %+v %v", again, err)
	}
	var codes int
	_ = pool.QueryRow(e.ctx, `SELECT count(*) FROM payments.qr_codes WHERE owner_id = $1`, m.ID).Scan(&codes)
	if codes != 1 {
		t.Errorf("%d QR codes", codes)
	}

	// Strangers can't see it; a cashier sees it without the registration details.
	if _, err := e.svc.Get(e.ctx, e.user("Stranger", "active"), m.ID); !errors.Is(err, merchants.ErrNotFound) {
		t.Errorf("stranger: %v", err)
	}
	cashier := e.user("Kebede", "active")
	e.exec(`INSERT INTO merchant.members (merchant_id, user_id, role) VALUES ($1, $2, 'cashier')`, m.ID, cashier)
	if c, err := e.svc.Get(e.ctx, cashier, m.ID); err != nil || c.TIN != "" || c.TradeName != "Selam Café" {
		t.Errorf("cashier view: %+v %v", c, err)
	}
	mine, err := e.svc.Mine(e.ctx, cashier)
	if err != nil || len(mine) != 1 || mine[0].Role != "cashier" {
		t.Errorf("mine: %+v %v", mine, err)
	}
}

func TestSummaryAndPayments(t *testing.T) {
	e := setup(t)
	owner := e.user("Tsion Alemu", "active")
	m, err := e.svc.Apply(e.ctx, owner, app(tin()))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.Approve(e.ctx, m.ID, uuid.New()); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	e.svc.Now = func() time.Time { return now }
	pay := func(payer uuid.UUID, amount int64, at time.Time, status string) {
		e.exec(`INSERT INTO payments.intents (kind, payer_user_id, payee_type, payee_id, amount_minor, currency, rail, status, completed_at)
			VALUES ('qr_merchant', $1, 'merchant', $2, $3, 'ETB', 'wallet', $4, $5)`, payer, m.ID, amount, status, at)
	}
	abebe, anon := e.user("Abebe Kebede Tesfaye", "active"), e.user("", "active")
	pay(abebe, 225_00, now.Add(-time.Minute), "succeeded")
	pay(anon, 1140_00, now.Add(-2*time.Minute), "succeeded")
	pay(abebe, 99_00, now.Add(-3*time.Minute), "failed")
	pay(abebe, 50_00, now.Add(-30*time.Hour), "succeeded") // yesterday

	s, err := e.svc.Summary(e.ctx, owner, m.ID)
	if err != nil || s.SalesToday.AmountMinor != 1365_00 || s.PaymentsToday != 2 || s.AverageBasket.AmountMinor != 682_50 {
		t.Fatalf("summary: %+v %v", s, err)
	}
	cashier := e.user("Kebede", "active")
	e.exec(`INSERT INTO merchant.members (merchant_id, user_id, role) VALUES ($1, $2, 'cashier')`, m.ID, cashier)
	if s, err := e.svc.Summary(e.ctx, cashier, m.ID); err != nil || s.SalesToday.AmountMinor != 1365_00 || s.NextPayout != nil {
		t.Errorf("cashier summary: %+v %v", s, err)
	}

	list, err := e.svc.Payments(e.ctx, cashier, m.ID, nil, uuid.Nil, 2)
	if err != nil || len(list) != 2 || list[0].PayerName != "Abebe T." || list[1].PayerName != "Customer" {
		t.Fatalf("payments: %+v %v", list, err)
	}
	more, err := e.svc.Payments(e.ctx, cashier, m.ID, &list[1].CompletedAt, list[1].ID, 10)
	if err != nil || len(more) != 1 || more[0].Amount.AmountMinor != 50_00 {
		t.Errorf("next page: %+v %v", more, err)
	}
	if _, err := e.svc.Payments(e.ctx, e.user("x", "active"), m.ID, nil, uuid.Nil, 10); !errors.Is(err, merchants.ErrNotFound) {
		t.Errorf("stranger: %v", err)
	}
}

func TestQRPayload(t *testing.T) {
	q := merchants.StaticQR("12345674", "Buna House", "5812", "")
	want := "00020101021126300014one.gebeya.pay0108123456745204581253032305802ET" +
		"5910Buna House6011Addis Ababa6304"
	if !strings.HasPrefix(q, want) || len(q) != len(want)+4 {
		t.Errorf("payload %s", q)
	}
	// Ge'ez names keep a Latin fallback and go in the alternate-language template.
	am := merchants.StaticQR("12345674", "ቡና ቤት", "5812", "Adama")
	if !strings.Contains(am, "5919GEBEYA ONE MERCHANT") || !strings.Contains(am, "64150002am0105ቡና ቤት") {
		t.Errorf("amharic %s", am)
	}
	for range 50 {
		if n := merchants.NewMerchantNo(); !merchants.ValidMerchantNo(n) {
			t.Fatalf("invalid %s", n)
		}
	}
	if merchants.ValidMerchantNo("12345670") || !merchants.ValidMerchantNo("12345674") {
		t.Error("check digit")
	}
	if got := merchants.ShortName("  Liya  "); got != "Liya" {
		t.Error(got)
	}
}
