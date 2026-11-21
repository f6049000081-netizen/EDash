package recon_test

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"math/big"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"superapp/backend/internal/ledger"
	"superapp/backend/internal/money"
	"superapp/backend/internal/payments"
	"superapp/backend/internal/platform"
	"superapp/backend/internal/recon"
)

func TestParseCSV(t *testing.T) {
	good := "\uFEFFpartner_ref,amount,currency,occurred_at\nTB1,100.00,ETB,2026-11-20T09:15:00+03:00\nTB2, 5.5 ,ETB,2026-11-20T10:00:00Z\n"
	lines, err := recon.ParseCSV(strings.NewReader(good))
	if err != nil || len(lines) != 2 || lines[0].Amount != money.New(10000, money.ETB) || lines[1].Amount.AmountMinor != 550 {
		t.Fatalf("good file: %v %+v", err, lines)
	}
	for name, s := range map[string]string{
		"wrong header": "ref,amount,currency,occurred_at\n",
		"bad amount":   "partner_ref,amount,currency,occurred_at\nTB1,1.234,ETB,2026-11-20T09:15:00Z\n",
		"bad currency": "partner_ref,amount,currency,occurred_at\nTB1,1.00,XYZ,2026-11-20T09:15:00Z\n",
		"bad time":     "partner_ref,amount,currency,occurred_at\nTB1,1.00,ETB,20/11/2026\n",
		"short row":    "partner_ref,amount,currency,occurred_at\nTB1,1.00,ETB\n",
	} {
		if _, err := recon.ParseCSV(strings.NewReader(s)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

type rail struct {
	mu     sync.Mutex
	orders map[string]payments.RailOrder
}

func (r *rail) CreateCheckout(_ context.Context, id string, _ money.Money, _ string) (payments.RailCheckout, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.orders[id] = payments.RailOrder{State: payments.RailPending}
	return payments.RailCheckout{CheckoutURL: "https://pay.example/" + id}, nil
}

func (r *rail) QueryOrder(_ context.Context, id string) (payments.RailOrder, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.orders[id], nil
}

func TestImportMatchesBothWays(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()

	fake := &rail{orders: map[string]payments.RailOrder{}}
	svc := payments.NewService(pool, ledger.New(), payments.DefaultRules(), &platform.PINVerifier{Pool: pool})
	svc.Rails["telebirr"] = fake

	user := uuid.New()
	n, _ := rand.Int(rand.Reader, big.NewInt(100_000_000))
	if _, err := pool.Exec(ctx, `INSERT INTO identity.users (id, phone_e164, status) VALUES ($1, $2, 'active')`,
		user, fmt.Sprintf("+2517%08d", n.Int64())); err != nil {
		t.Fatal(err)
	}
	// A random past business day, so reruns against the same database never collide.
	d, _ := rand.Int(rand.Reader, big.NewInt(36_500))
	day := time.Date(2000, 1, 1, 0, 0, 0, 0, recon.EAT).AddDate(0, 0, int(d.Int64()))

	topUp := func(minor int64, paid int64) (uuid.UUID, string) {
		in, _, err := svc.StartTopUp(ctx, user, money.New(minor, money.ETB), "telebirr")
		if err != nil {
			t.Fatal(err)
		}
		ref := "TB-" + uuid.NewString()
		fake.mu.Lock()
		fake.orders[payments.OrderID(in.ID)] = payments.RailOrder{State: payments.RailSucceeded, PartnerRef: ref, Amount: money.New(paid, money.ETB)}
		fake.mu.Unlock()
		if _, err := svc.SettleTopUp(ctx, in.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, `UPDATE payments.intents SET completed_at = $2 WHERE id = $1 AND completed_at IS NOT NULL`, in.ID, day.Add(12*time.Hour)); err != nil {
			t.Fatal(err)
		}
		return in.ID, ref
	}
	_, refOK := topUp(10_000, 10_000)
	_, refWrongAmount := topUp(20_000, 20_000)
	_, refHeld := topUp(30_000, 3_000) // paid short: held for review, not succeeded
	missingAtPartner, _ := topUp(40_000, 40_000)

	at := day.Add(9 * time.Hour)
	lines := []recon.Line{
		{PartnerRef: refOK, Amount: money.New(10_000, money.ETB), OccurredAt: at},
		{PartnerRef: refWrongAmount, Amount: money.New(25_000, money.ETB), OccurredAt: at},
		{PartnerRef: refHeld, Amount: money.New(30_000, money.ETB), OccurredAt: at},
		{PartnerRef: "TB-we-never-heard-of-" + uuid.NewString(), Amount: money.New(5_000, money.ETB), OccurredAt: at},
	}
	sha := uuid.NewString()
	rep, err := recon.Import(ctx, pool, "telebirr", day, sha, lines)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Lines != 4 || rep.Matched != 1 || rep.AmountMismatch != 1 || rep.Unmatched != 1 || rep.MissingInternal != 1 {
		t.Errorf("report: %+v", rep)
	}
	if len(rep.MissingAtPartner) != 1 || rep.MissingAtPartner[0] != missingAtPartner {
		t.Errorf("missing at partner: %v, want [%v]", rep.MissingAtPartner, missingAtPartner)
	}
	if rep.Clean() || rep.StatementTotal != money.New(70_000, money.ETB) || rep.MatchedTotal != money.New(10_000, money.ETB) {
		t.Errorf("totals: %+v", rep)
	}
	var stored int
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM recon.statement_lines WHERE statement_id = $1`, rep.StatementID).Scan(&stored)
	if stored != 4 {
		t.Errorf("%d lines stored", stored)
	}

	if _, err := recon.Import(ctx, pool, "telebirr", day, uuid.NewString(), lines); !errors.Is(err, recon.ErrAlreadyImported) {
		t.Errorf("second statement for the same day: %v", err)
	}
	if _, err := recon.Import(ctx, pool, "telebirr", day.AddDate(0, 0, 1), sha, nil); !errors.Is(err, recon.ErrAlreadyImported) {
		t.Errorf("same file again: %v", err)
	}
	dup := append(lines[:1:1], lines[0])
	if _, err := recon.Import(ctx, pool, "telebirr", day.AddDate(0, 0, 2), uuid.NewString(), dup); err == nil {
		t.Error("statement with a repeated reference accepted")
	}
}
