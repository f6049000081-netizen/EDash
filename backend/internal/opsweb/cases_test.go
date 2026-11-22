package opsweb_test

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/google/uuid"
)

func TestRiskCasesAndStaffPages(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatal(err)
		}
	}
	payer, payee, c, held := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	for _, u := range []uuid.UUID{payer, payee} {
		exec(`INSERT INTO identity.users (id, phone_e164, display_name, status) VALUES ($1, $2, 'Kebede', 'active')`,
			u, fmt.Sprintf("+2519%08d", u.ID()%100_000_000))
	}
	exec(`INSERT INTO risk.cases (id, subject_type, subject_id, summary, risk_score, amount_minor, currency, status, sla_due_at)
		VALUES ($1, 'user', $2, 'Payment above the review threshold', 50, 6000000, 'ETB', 'unassigned', now() - interval '1 hour')`, c, payer)
	exec(`INSERT INTO payments.intents (id, kind, payer_user_id, payee_type, payee_id, amount_minor, currency, rail, status, confirmed_at)
		VALUES ($1, 'p2p', $2, 'user', $3, 6000000, 'ETB', 'wallet', 'held_for_review', now())`, held, payer, payee)
	exec(`INSERT INTO risk.case_intents (case_id, intent_id) VALUES ($1, $2)`, c, held)

	analyst, _, _ := e.signIn(claimsFor(e.invite("Selam", "risk_analyst")))
	_, list := analyst.do("GET", "/ops/cases", nil, "")
	if !strings.Contains(list, "/ops/cases/"+c.String()) || !strings.Contains(list, "Late") {
		t.Fatalf("case list:\n%s", list)
	}
	page := "/ops/cases/" + c.String()
	res, html := analyst.do("GET", page, nil, "")
	if res.StatusCode != http.StatusOK || !strings.Contains(html, "Ask to release") || !strings.Contains(html, ">Decline<") ||
		!strings.Contains(html, "Ask to freeze") || strings.Contains(html, "+2519") {
		t.Fatalf("case page: %d\n%s", res.StatusCode, html)
	}
	csrf := csrfRE.FindStringSubmatch(html)[1]
	post := func(path string, form url.Values) string {
		form.Set("csrf", csrf)
		res, _ := analyst.do("POST", path, form, "")
		return res.Header.Get("Location")
	}
	if loc := post(page+"/take", url.Values{}); loc != page+"?msg=taken" {
		t.Errorf("take: %s", loc)
	}
	if loc := post(page+"/note", url.Values{"body": {"Customer says it's rent."}}); loc != page+"?msg=noted" {
		t.Errorf("note: %s", loc)
	}
	// A form pointed at a payment outside the case gets nowhere.
	if res, _ := analyst.do("POST", page+"/action", url.Values{"csrf": {csrf}, "kind": {"payment.decline"},
		"target": {uuid.NewString()}, "note": {"Someone else's payment"}}, ""); res.StatusCode != http.StatusNotFound {
		t.Errorf("foreign payment: %d", res.StatusCode)
	}
	if loc := post(page+"/action", url.Values{"kind": {"payment.release"}, "target": {held.String()},
		"note": {"Customer confirmed by phone"}}); loc != page+"?msg=requested" {
		t.Errorf("release: %s", loc)
	}
	_, html = analyst.do("GET", page, nil, "")
	for _, want := range []string{"Customer says it&#39;s rent.", "Waiting for approval: <strong>Release held payment</strong>", "Took the case",
		"<td>Release held payment</td>"} {
		if !strings.Contains(html, want) {
			t.Errorf("case page lacks %q", want)
		}
	}
	// The approvals page links the request back to the case.
	_, html = analyst.do("GET", "/ops/approvals", nil, "")
	if !strings.Contains(html, `href="/ops/payments/`+held.String()+`"`) {
		t.Errorf("approvals page:\n%s", html)
	}
	if res, _ := analyst.do("GET", "/ops/payments/"+held.String(), nil, ""); res.Header.Get("Location") != page {
		t.Errorf("payment link: %v", res.Header)
	}
	if res, _ := analyst.do("GET", "/ops/staff", nil, ""); res.StatusCode != http.StatusForbidden {
		t.Errorf("analyst opened staff: %d", res.StatusCode)
	}

	// Staff: a super admin asks to invite someone; the request shows the details.
	admin, _, _ := e.signIn(claimsFor(e.invite("Tsion", "super_admin")))
	_, html = admin.do("GET", "/ops/staff", nil, "")
	if !strings.Contains(html, "Ask to invite") || !strings.Contains(html, "Selam") {
		t.Fatalf("staff page:\n%s", html)
	}
	csrf = csrfRE.FindStringSubmatch(html)[1]
	email := "new." + uuid.NewString()[:8] + "@gebeya.test"
	res, _ = admin.do("POST", "/ops/staff", url.Values{"csrf": {csrf}, "name": {"Dagim Tesfaye"}, "email": {email},
		"roles": {"risk_analyst", "support_agent"}, "note": {"Night shift analyst starting Monday"}}, "")
	if loc := res.Header.Get("Location"); loc != "/ops/staff?msg=requested" {
		t.Errorf("invite: %s", loc)
	}
	_, html = admin.do("GET", "/ops/staff", nil, "")
	if !strings.Contains(html, "Dagim Tesfaye (Risk analyst, Support agent)") {
		t.Errorf("pending invite not shown:\n%s", html)
	}
	if strings.Contains(e.logs.String(), "level=ERROR") {
		t.Errorf("server errors: %s", e.logs.String())
	}
}
