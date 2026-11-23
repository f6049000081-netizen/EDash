package payments_test

import (
	"crypto/rand"
	"errors"
	"fmt"
	"math/big"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"superapp/backend/internal/money"
	"superapp/backend/internal/payments"
)

func randomPhone() string {
	n, _ := rand.Int(rand.Reader, big.NewInt(100_000_000))
	return fmt.Sprintf("+2519%08d", n.Int64())
}

func (e *env) phoneOf(user uuid.UUID) string {
	var p string
	if err := e.pool.QueryRow(e.ctx, `SELECT phone_e164 FROM identity.users WHERE id = $1`, user).Scan(&p); err != nil {
		e.t.Fatal(err)
	}
	return p
}

// byPhone starts a transfer to [phone] from [device], as the app does when someone types a number.
func (e *env) byPhone(from uuid.UUID, device *uuid.UUID, phone string) (*payments.Intent, error) {
	amt := money.New(1_000, money.ETB)
	return e.svc.CreateIntent(e.ctx, from, payments.CreateRequest{Kind: payments.KindP2P, Amount: &amt, PayeePhone: phone, DeviceID: device})
}

// lookedUpEarlier records lookups of [n] new numbers [ago] in the past.
func (e *env) lookedUpEarlier(user uuid.UUID, device *uuid.UUID, n int, found bool, ago time.Duration) {
	for i := 0; i < n; i++ {
		if _, err := e.pool.Exec(e.ctx, `
			INSERT INTO payments.payee_lookups (user_id, device_id, number_hash, found, created_at)
			VALUES ($1, $2, $3, $4, now() - $5::interval)`, user, device, uuid.NewString(), found, fmt.Sprintf("%d seconds", int(ago.Seconds()))); err != nil {
			e.t.Fatal(err)
		}
	}
}

func (e *env) openCase(user uuid.UUID) (string, bool) {
	var summary string
	err := e.pool.QueryRow(e.ctx, `SELECT summary FROM risk.cases WHERE subject_type = 'user' AND subject_id = $1
		AND status NOT IN ('resolved', 'escalated_aml')`, user).Scan(&summary)
	return summary, err == nil
}

func TestLookupsAreLimitedPerHourAndPerDay(t *testing.T) {
	e := setup(t)
	e.svc.Lookups = payments.LookupLimits{PerHour: 3, PerDay: 5, NotFoundPerDay: 2}
	abebe := e.user("Abebe")
	friends := []uuid.UUID{e.user("Sara"), e.user("Hana"), e.user("Dawit"), e.user("Tsion")}

	for _, f := range friends[:3] {
		if in, err := e.byPhone(abebe, nil, e.phoneOf(f)); err != nil || in.PayeeName == "" {
			t.Fatalf("lookup: %v", err)
		}
	}
	// The same number again is free; a fourth new number this hour isn't.
	if _, err := e.byPhone(abebe, nil, e.phoneOf(friends[0])); err != nil {
		t.Errorf("same number again: %v", err)
	}
	_, err := e.byPhone(abebe, nil, e.phoneOf(friends[3]))
	var le *payments.LookupLimitError
	if !errors.As(err, &le) || le.Daily || le.RetryAfter < 50*time.Minute {
		t.Fatalf("hourly limit: %v", err)
	}
	if _, open := e.openCase(abebe); open {
		t.Error("the hourly limit opened a risk case")
	}

	// Lookups from two hours ago count for the day, not the hour.
	carol := e.user("Carol")
	e.lookedUpEarlier(carol, nil, 4, true, 2*time.Hour)
	if _, err := e.byPhone(carol, nil, e.phoneOf(friends[0])); err != nil {
		t.Fatalf("fifth of the day: %v", err)
	}
	_, err = e.byPhone(carol, nil, e.phoneOf(friends[1]))
	if !errors.As(err, &le) || !le.Daily || le.RetryAfter < 21*time.Hour || le.RetryAfter > 22*time.Hour {
		t.Fatalf("daily limit: %v", err)
	}
	if summary, open := e.openCase(carol); !open || summary != "Looked up many phone numbers (possible number harvesting)" {
		t.Errorf("risk case: %q %v", summary, open)
	}
	_, _ = e.byPhone(carol, nil, e.phoneOf(friends[2])) // refused again: the case isn't duplicated
	var cases int
	if err := e.pool.QueryRow(e.ctx, `SELECT count(*) FROM risk.cases WHERE subject_id = $1`, carol).Scan(&cases); err != nil || cases != 1 {
		t.Errorf("cases: %d %v", cases, err)
	}

	// Yesterday's lookups no longer count.
	dan := e.user("Dan")
	e.lookedUpEarlier(dan, nil, 10, true, 25*time.Hour)
	if _, err := e.byPhone(dan, nil, e.phoneOf(friends[0])); err != nil {
		t.Errorf("after a day: %v", err)
	}
}

func TestNumbersWithoutAccountsHaveTheirOwnLimit(t *testing.T) {
	e := setup(t)
	e.svc.Lookups = payments.LookupLimits{PerHour: 10, PerDay: 10, NotFoundPerDay: 2}
	u := e.user("Selam")
	for i := 0; i < 2; i++ {
		if _, err := e.byPhone(u, nil, randomPhone()); !errors.Is(err, payments.ErrPayeeNotFound) {
			t.Fatalf("unknown number: %v", err)
		}
	}
	var le *payments.LookupLimitError
	if _, err := e.byPhone(u, nil, randomPhone()); !errors.As(err, &le) || !le.Daily {
		t.Fatalf("third unknown number: %v", err)
	}
	// Numbers that have accounts can still be looked up.
	if _, err := e.byPhone(u, nil, e.phoneOf(e.user("Meron"))); err != nil {
		t.Errorf("known account: %v", err)
	}
}

func TestPeopleYouHavePaidNeverCount(t *testing.T) {
	e := setup(t)
	e.svc.Lookups = payments.LookupLimits{PerHour: 1, PerDay: 1, NotFoundPerDay: 1}
	abebe, sara, hana := e.user("Abebe"), e.user("Sara"), e.user("Hana")
	e.fund(abebe, 100_000)
	e.fund(hana, 100_000)
	if _, err := e.pay(abebe, sara, 1_000); err != nil {
		t.Fatal(err)
	}
	if _, err := e.pay(hana, abebe, 1_000); err != nil { // received from counts too
		t.Fatal(err)
	}
	e.lookedUpEarlier(abebe, nil, 1, true, time.Minute) // the limit is used up
	for _, p := range []uuid.UUID{sara, hana, sara} {
		if _, err := e.byPhone(abebe, nil, e.phoneOf(p)); err != nil {
			t.Errorf("someone already paid: %v", err)
		}
	}
	if _, err := e.byPhone(abebe, nil, e.phoneOf(e.user("Stranger"))); err == nil {
		t.Error("a stranger got past a used-up limit")
	}
}

func TestOnePhoneIsLimitedAcrossAccounts(t *testing.T) {
	e := setup(t)
	e.svc.Lookups = payments.LookupLimits{PerHour: 3, PerDay: 10, NotFoundPerDay: 10}
	device := uuid.New()
	a, b := e.user("First account"), e.user("Second account")
	e.lookedUpEarlier(a, &device, 3, true, time.Minute)
	var le *payments.LookupLimitError
	if _, err := e.byPhone(b, &device, e.phoneOf(e.user("Target"))); !errors.As(err, &le) {
		t.Fatalf("second account on the same phone: %v", err)
	}
	if _, err := e.byPhone(b, nil, e.phoneOf(e.user("Target 2"))); err != nil {
		t.Errorf("the account itself is under its limit: %v", err)
	}
}

func TestParallelLookupsCantBeatTheLimit(t *testing.T) {
	e := setup(t)
	e.svc.Lookups = payments.LookupLimits{PerHour: 5, PerDay: 50, NotFoundPerDay: 50}
	u := e.user("Racer")
	targets := make([]string, 12)
	for i := range targets {
		targets[i] = e.phoneOf(e.user(fmt.Sprint("T", i)))
	}
	var wg sync.WaitGroup
	var mu sync.Mutex
	ok := 0
	for _, p := range targets {
		wg.Add(1)
		go func(p string) {
			defer wg.Done()
			if _, err := e.byPhone(u, nil, p); err == nil {
				mu.Lock()
				ok++
				mu.Unlock()
			}
		}(p)
	}
	wg.Wait()
	if ok != 5 {
		t.Errorf("%d lookups got through, want 5", ok)
	}
}
