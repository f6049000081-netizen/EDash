package opsweb_test

import (
	"context"
	"crypto/rand"
	"math/big"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"superapp/backend/internal/money"
	"superapp/backend/internal/recon"
)

func TestReconciliationExceptionsAreResolved(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	// A random business day in the 1990s: other tests use later days, and reruns never collide.
	d, _ := rand.Int(rand.Reader, big.NewInt(3_650))
	day := time.Date(1990, 1, 1, 0, 0, 0, 0, recon.EAT).AddDate(0, 0, int(d.Int64()))
	ref := "TB-unknown-" + uuid.NewString()[:8]
	rep, err := recon.Import(ctx, pool, "telebirr", day, uuid.NewString(), []recon.Line{
		{PartnerRef: ref, Amount: money.New(12_345, money.ETB), OccurredAt: day.Add(10 * time.Hour)},
	})
	if err != nil || rep.MissingInternal != 1 {
		t.Fatalf("import: %+v %v", rep, err)
	}
	var exID uuid.UUID
	if err := pool.QueryRow(ctx, `SELECT id FROM recon.exceptions WHERE statement_id = $1`, rep.StatementID).Scan(&exID); err != nil {
		t.Fatal(err)
	}

	// A risk analyst has no reconciliation grant.
	analyst, _, _ := e.signIn(claimsFor(e.invite("Yonas", "risk_analyst")))
	if res, html := analyst.do("GET", "/ops/recon", nil, ""); res.StatusCode != http.StatusForbidden || strings.Contains(html, ref) {
		t.Fatalf("analyst: %d", res.StatusCode)
	}

	fin, _, _ := e.signIn(claimsFor(e.invite("Tigist", "finance_officer")))
	_, html := fin.do("GET", "/ops/recon", nil, "")
	for _, want := range []string{ref, "We have no record", "ETB 123.45", day.Format("2 Jan 2006"), "Reconciliation</span><span class=\"count\">"} {
		if !strings.Contains(html, want) {
			t.Fatalf("recon page lacks %q:\n%s", want, html)
		}
	}
	csrf := csrfRE.FindStringSubmatch(html)[1]
	resolve := func(reason, note string) string {
		res, _ := fin.do("POST", "/ops/recon/"+exID.String()+"/resolve", url.Values{"csrf": {csrf}, "reason": {reason}, "note": {note}}, "")
		return res.Header.Get("Location")
	}
	if loc := resolve("", "telebirr confirmed a test payment"); loc != "/ops/recon?msg=conflict" {
		t.Errorf("no reason: %s", loc)
	}
	if loc := resolve("partner_corrected", "short"); loc != "/ops/recon?msg=note" {
		t.Errorf("short note: %s", loc)
	}
	if loc := resolve("partner_corrected", "telebirr confirmed it was a test payment and removed it"); loc != "/ops/recon?msg=applied" {
		t.Fatalf("resolve: %s", loc)
	}
	if loc := resolve("other", "trying to resolve it a second time"); loc != "/ops/recon?msg=conflict" {
		t.Errorf("twice: %s", loc)
	}
	_, html = fin.do("GET", "/ops/recon?show=resolved", nil, "")
	for _, want := range []string{ref, "The partner corrected their statement", "Tigist", "removed it"} {
		if !strings.Contains(html, want) {
			t.Fatalf("resolved list lacks %q", want)
		}
	}
	var audited int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM admin.audit_log WHERE action = 'recon.resolve' AND target_id = $1 AND result = 'allowed'
		AND detail->'params'->>'reason' = 'partner_corrected'`, exID).Scan(&audited); err != nil || audited != 1 {
		t.Errorf("audit: %d %v", audited, err)
	}
}
