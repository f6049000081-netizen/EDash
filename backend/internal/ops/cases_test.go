package ops_test

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/google/uuid"

	"superapp/backend/internal/ledger"
	"superapp/backend/internal/merchants"
	"superapp/backend/internal/money"
	"superapp/backend/internal/ops"
	"superapp/backend/internal/payments"
	"superapp/backend/internal/platform"
)

func (e *env) withPayments() *payments.Service {
	if e.svc.Payments == nil {
		e.svc.Payments = payments.NewService(pool, ledger.New(), payments.DefaultRules(), &platform.PINVerifier{Pool: pool})
	}
	return e.svc.Payments
}

func (e *env) customer(name string) uuid.UUID {
	e.t.Helper()
	id := uuid.New()
	e.exec(`INSERT INTO identity.users (id, phone_e164, display_name, status) VALUES ($1, $2, $3, 'active')`,
		id, fmt.Sprintf("+2519%08d", uint32(id.ID())%100_000_000), name)
	return id
}

func (e *env) wallet(user uuid.UUID) int64 {
	var b int64
	_ = pool.QueryRow(e.ctx, `SELECT COALESCE((SELECT b.balance_minor FROM ledger.accounts a JOIN ledger.balances b ON b.account_id = a.id
		WHERE a.owner_type = 'user' AND a.owner_id = $1 AND a.kind = 'user_wallet'), 0)`, user).Scan(&b)
	return b
}

// heldCase makes held payments from payer to payee and a case holding them.
func (e *env) heldCase(payer, payee uuid.UUID, amounts ...int64) (uuid.UUID, []uuid.UUID) {
	e.t.Helper()
	c := uuid.New()
	e.exec(`INSERT INTO risk.cases (id, subject_type, subject_id, summary, risk_score, amount_minor, currency, status, sla_due_at)
		VALUES ($1, 'user', $2, 'Payment above the review threshold', 50, 0, 'ETB', 'unassigned', now() + interval '4 hours')`, c, payer)
	var ids []uuid.UUID
	for _, a := range amounts {
		id := uuid.New()
		e.exec(`INSERT INTO payments.intents (id, kind, payer_user_id, payee_type, payee_id, amount_minor, currency, rail, status, confirmed_at)
			VALUES ($1, 'p2p', $2, 'user', $3, $4, 'ETB', 'wallet', 'held_for_review', now())`, id, payer, payee, a)
		e.exec(`INSERT INTO risk.case_intents (case_id, intent_id) VALUES ($1, $2)`, c, id)
		ids = append(ids, id)
	}
	return c, ids
}

func (e *env) intentStatus(id uuid.UUID) string {
	var s string
	_ = pool.QueryRow(e.ctx, `SELECT status::text || COALESCE(':' || failure_code, '') FROM payments.intents WHERE id = $1`, id).Scan(&s)
	return s
}

func (e *env) caseStatus(id uuid.UUID) string {
	var s string
	_ = pool.QueryRow(e.ctx, `SELECT status FROM risk.cases WHERE id = $1`, id).Scan(&s)
	return s
}

func TestWorkingARiskCase(t *testing.T) {
	e := setup(t)
	pay := e.withPayments()
	payer, payee := e.customer("Kebede"), e.customer("Almaz")
	if _, err := pay.SandboxTopUp(e.ctx, payer, money.New(10_000_000, money.ETB)); err != nil {
		t.Fatal(err)
	}
	c, ids := e.heldCase(payer, payee, 6_000_000, 2_000_000)
	analyst := e.staff("selam", "risk_analyst")
	finance := e.staff("abel", "finance_officer")
	support := e.staff("hana", "support_agent")
	admin := e.staff("tsion", "super_admin")

	if err := e.svc.TakeCase(e.ctx, support, c); !errors.Is(err, ops.ErrForbidden) {
		t.Errorf("support took a case: %v", err)
	}
	if err := e.svc.TakeCase(e.ctx, analyst, c); err != nil || e.caseStatus(c) != "in_review" {
		t.Fatalf("take: %v %s", err, e.caseStatus(c))
	}
	if err := e.svc.NoteCase(e.ctx, analyst, c, "Called the customer: says it's rent for a shop in Merkato."); err != nil {
		t.Fatal(err)
	}
	detail, err := e.svc.Case(e.ctx, c)
	if err != nil || len(detail.Notes) != 1 || detail.Assignee != "selam" || len(detail.Intents) != 2 ||
		!strings.HasPrefix(detail.Subject.Phone, "••") || detail.Subject.Balance.AmountMinor != 10_000_000 {
		t.Fatalf("case detail: %v %+v", err, detail)
	}

	// Releasing moves money, so the analyst's request waits for someone else.
	release := ops.Action{Kind: "payment.release", Target: ids[0], Note: "Customer confirmed the rent payment by phone"}
	out, err := e.svc.Do(e.ctx, analyst, release)
	if err != nil || out.Applied || out.ApprovalID == uuid.Nil || e.intentStatus(ids[0]) != "held_for_review" {
		t.Fatalf("release request: %v %+v %s", err, out, e.intentStatus(ids[0]))
	}
	if err := e.svc.Decide(e.ctx, analyst, out.ApprovalID, true, "Approving my own request"); !errors.Is(err, ops.ErrOwnRequest) {
		t.Errorf("own approval: %v", err)
	}
	if err := e.svc.Decide(e.ctx, finance, out.ApprovalID, true, "Matches the customer's history"); err != nil {
		t.Fatal(err)
	}
	if e.intentStatus(ids[0]) != "succeeded" || e.wallet(payee) != 6_000_000 || e.wallet(payer) != 4_000_000 {
		t.Fatalf("after release: %s %d %d", e.intentStatus(ids[0]), e.wallet(payer), e.wallet(payee))
	}

	// The case can't be resolved while a payment is still held.
	resolve := ops.Action{Kind: "case.resolve", Target: c, Note: "Legitimate rent payments; customer verified"}
	var conflict *merchants.ConflictError
	if _, err := e.svc.Do(e.ctx, analyst, resolve); !errors.As(err, &conflict) {
		t.Errorf("resolve with a held payment: %v", err)
	}
	// Declining moves nothing, so the analyst does it alone.
	if out, err := e.svc.Do(e.ctx, analyst, ops.Action{Kind: "payment.decline", Target: ids[1], Note: "Duplicate of the rent payment"}); err != nil || !out.Applied {
		t.Fatalf("decline: %v %+v", err, out)
	}
	if e.intentStatus(ids[1]) != "failed:declined_after_review" || e.wallet(payer) != 4_000_000 {
		t.Errorf("after decline: %s %d", e.intentStatus(ids[1]), e.wallet(payer))
	}
	if out, err := e.svc.Do(e.ctx, analyst, resolve); err != nil || !out.Applied || e.caseStatus(c) != "resolved" {
		t.Fatalf("resolve: %v %s", err, e.caseStatus(c))
	}

	// Freezing the customer needs a second person; they're told their account is on hold.
	freeze := ops.Action{Kind: "user.freeze", Target: payer, Note: "Pattern matches a known mule account"}
	out, err = e.svc.Do(e.ctx, analyst, freeze)
	if err != nil || out.Applied {
		t.Fatalf("freeze request: %v %+v", err, out)
	}
	if err := e.svc.Decide(e.ctx, admin, out.ApprovalID, true, "Agreed, freeze while we check"); err != nil {
		t.Fatal(err)
	}
	var status string
	_ = pool.QueryRow(e.ctx, `SELECT status FROM identity.users WHERE id = $1`, payer).Scan(&status)
	var told int
	_ = pool.QueryRow(e.ctx, `SELECT count(*) FROM platform.outbox WHERE topic = 'identity.account_frozen' AND aggregate_id = $1`, payer).Scan(&told)
	if status != "frozen" || told != 1 {
		t.Errorf("freeze: %s, %d events", status, told)
	}
	rows, _ := e.svc.Approvals(e.ctx, false, payer, 5)
	if len(rows) != 1 || rows[0].TargetName != "Kebede" || rows[0].Decision != "approved" {
		t.Errorf("approval row: %+v", rows)
	}
}

func TestManagingStaffNeedsASecondSuperAdmin(t *testing.T) {
	e := setup(t)
	a := e.staff("aster", "super_admin")
	b := e.staff("bereket", "super_admin")
	analyst := e.staff("chala", "risk_analyst")

	if _, err := e.svc.Do(e.ctx, a, ops.Action{Kind: "staff.roles", Target: a.Staff.ID, Note: "Give myself more access please",
		Params: map[string]string{"roles": "super_admin,finance_officer"}}); !errors.Is(err, ops.ErrSelf) {
		t.Errorf("changing own roles: %v", err)
	}
	if _, err := e.svc.Do(e.ctx, analyst, ops.Action{Kind: "staff.suspend", Target: a.Staff.ID, Note: "Analysts can't manage staff"}); !errors.Is(err, ops.ErrForbidden) {
		t.Errorf("analyst managing staff: %v", err)
	}

	// Invite: the details travel with the request, so the checker approves exactly what's applied.
	newID := uuid.New()
	email := "dagim." + newID.String()[:8] + "@gebeya.test"
	out, err := e.svc.Do(e.ctx, a, ops.Action{Kind: "staff.invite", Target: newID, Note: "New analyst on the night shift",
		Params: map[string]string{"email": email, "name": "Dagim Tesfaye", "roles": "risk_analyst"}})
	if err != nil || out.Applied {
		t.Fatalf("invite request: %v %+v", err, out)
	}
	pending, _ := e.svc.Approvals(e.ctx, true, newID, 5)
	if len(pending) != 1 || pending[0].Params["email"] != email || pending[0].TargetName != "Dagim Tesfaye" {
		t.Fatalf("pending invite: %+v", pending)
	}
	if err := e.svc.Decide(e.ctx, b, out.ApprovalID, true, "Confirmed with HR, start date today"); err != nil {
		t.Fatal(err)
	}
	st, err := e.svc.LoadStaff(e.ctx, newID)
	if err != nil || st.RoleList() != "risk_analyst" || st.Grant("payments.release") != ops.Approve {
		t.Fatalf("invited: %v %+v", err, st)
	}
	if id, err := e.svc.StaffForLogin(e.ctx, "idp|"+newID.String(), email); err != nil || id != newID {
		t.Errorf("first sign-in: %v %s", err, id)
	}

	// Change roles, then suspend.
	out, _ = e.svc.Do(e.ctx, a, ops.Action{Kind: "staff.roles", Target: analyst.Staff.ID, Note: "Moving to the support team",
		Params: map[string]string{"roles": "support_agent"}})
	if err := e.svc.Decide(e.ctx, b, out.ApprovalID, true, "Agreed with the team lead"); err != nil {
		t.Fatal(err)
	}
	if st, _ := e.svc.LoadStaff(e.ctx, analyst.Staff.ID); st.RoleList() != "support_agent" {
		t.Errorf("roles now %s", st.RoleList())
	}
	sessionToken, _ := e.svc.StartSession(e.ctx, analyst.Staff.ID, "10.0.0.9", "browser")
	out, _ = e.svc.Do(e.ctx, a, ops.Action{Kind: "staff.suspend", Target: analyst.Staff.ID, Note: "Left the company today"})
	if err := e.svc.Decide(e.ctx, b, out.ApprovalID, true, "HR confirmed the leaving date"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.SessionFrom(e.ctx, sessionToken); !errors.Is(err, ops.ErrForbidden) {
		t.Errorf("suspended staff still signed in: %v", err)
	}

	// Nobody approves a change to themselves either: the request stays open for someone else.
	out, _ = e.svc.Do(e.ctx, a, ops.Action{Kind: "staff.suspend", Target: b.Staff.ID, Note: "Testing the self-approval rule"})
	if err := e.svc.Decide(e.ctx, b, out.ApprovalID, true, "Approving my own suspension"); !errors.Is(err, ops.ErrSelf) {
		t.Errorf("approving own suspension: %v", err)
	}
	if p, _ := e.svc.Approvals(e.ctx, true, b.Staff.ID, 5); len(p) != 1 {
		t.Errorf("request should still be open: %+v", p)
	}
	if err := e.svc.Decide(e.ctx, a, out.ApprovalID, false, ""); !errors.Is(err, ops.ErrOwnRequest) {
		t.Errorf("deciding own request: %v", err)
	}
}
