package bizweb_test

import (
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"

	"github.com/google/uuid"
)

func (e *env) phone(user uuid.UUID) string {
	var p string
	if err := pool.QueryRow(e.ctx, `SELECT phone_e164 FROM identity.users WHERE id = $1`, user).Scan(&p); err != nil {
		e.t.Fatal(err)
	}
	return p
}

func regexpFind(t *testing.T, s, re string) string {
	t.Helper()
	m := regexp.MustCompile(re).FindStringSubmatch(s)
	if m == nil {
		t.Fatalf("no match for %s", re)
	}
	return m[1]
}

func (e *env) role(user, merchant uuid.UUID) string {
	var r string
	_ = pool.QueryRow(e.ctx, `SELECT role FROM merchant.members WHERE merchant_id = $1 AND user_id = $2`, merchant, user).Scan(&r)
	return r
}

func TestOwnersRunTheTeamFromTheConsole(t *testing.T) {
	e := setup(t)
	owner, cashier, merchant, _ := e.shop()
	b := e.browser()
	b.signIn(owner, merchant)
	st, page, _ := b.get("/business/team")
	if st != http.StatusOK || !strings.Contains(page, `href="/business/team" aria-current="page"`) || !strings.Contains(page, "Dawit") ||
		!strings.Contains(page, "Send invitation") {
		t.Fatalf("team page: %d", st)
	}
	if strings.Contains(page, e.phone(cashier)[1:]) {
		t.Error("the cashier's full number is shown")
	}
	csrf := b.csrf("/business/team")

	// The cashier is signed in on another computer.
	cb := e.browser()
	cb.signIn(cashier, merchant)
	if st, page, _ := cb.get("/business/"); st != http.StatusOK || strings.Contains(page, `href="/business/team"`) {
		t.Fatalf("cashier's home: %d, team link shown: %v", st, strings.Contains(page, `href="/business/team"`))
	}
	if st, _, _ := cb.get("/business/team"); st != http.StatusForbidden {
		t.Errorf("cashier opened the team page: %d", st)
	}

	// Refusals keep the form.
	newcomer := e.user("Hana")
	local := "0" + e.phone(newcomer)[4:]
	if st, page, _ := b.post("/business/team/invite", url.Values{"csrf": {csrf}, "phone": {local}, "role": {"owner"}}); st != http.StatusUnprocessableEntity ||
		!strings.Contains(page, "Owners can&#39;t be invited") || !strings.Contains(page, `value="`+local+`"`) {
		t.Fatalf("owner invite: %d", st)
	}
	if _, _, loc := b.post("/business/team/invite", url.Values{"csrf": {csrf}, "phone": {local}, "role": {"accountant"}}); loc != "/business/team?msg=invited" {
		t.Fatalf("invite: %q", loc)
	}
	_, page, _ = b.get("/business/team?msg=invited")
	if !strings.Contains(page, "Invitation sent.") || !strings.Contains(page, e.phone(newcomer)[1:]) { // html/template writes + as &#43;
		t.Fatal("the open invitation isn't listed")
	}
	inv := regexpFind(t, page, `/business/team/invitations/([0-9a-f-]{36})/cancel`)
	if _, _, loc := b.post("/business/team/invitations/"+inv+"/cancel", url.Values{"csrf": {csrf}}); loc != "/business/team?msg=invite_cancelled" {
		t.Fatalf("cancel: %q", loc)
	}

	// Role change, then removal: the cashier's computer is signed out at once.
	path := "/business/team/members/" + cashier.String()
	if _, _, loc := b.post(path+"/role", url.Values{"csrf": {csrf}, "role": {"manager"}}); loc != "/business/team?msg=role_changed" || e.role(cashier, merchant) != "manager" {
		t.Fatalf("role: %q %s", loc, e.role(cashier, merchant))
	}
	if st, page, _ := cb.get("/business/team"); st != http.StatusOK || strings.Contains(page, "Send invitation") {
		t.Errorf("as manager: %d, invite form shown: %v", st, strings.Contains(page, "Send invitation"))
	}
	if _, _, loc := b.post("/business/team/members/"+owner.String()+"/remove", url.Values{"csrf": {csrf}}); loc != "" {
		t.Errorf("removed themselves: %q", loc)
	}
	if _, _, loc := b.post(path+"/remove", url.Values{"csrf": {csrf}}); loc != "/business/team?msg=member_removed" {
		t.Fatalf("remove: %q", loc)
	}
	if st, _, _ := cb.get("/business/"); st != http.StatusUnauthorized {
		t.Errorf("removed member still signed in: %d", st)
	}
}
