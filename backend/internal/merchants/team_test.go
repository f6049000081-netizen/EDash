package merchants_test

import (
	"encoding/binary"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"superapp/backend/internal/merchants"
)

func TestNormalizePhone(t *testing.T) {
	for in, want := range map[string]string{
		"+251 91 123 4567":   "+251911234567",
		"0911234567":         "+251911234567",
		"0711-234-567":       "+251711234567",
		"911234567":          "+251911234567",
		"251911234567":       "+251911234567",
		"00 44 20 7946 0958": "+442079460958",
		"+1 (202) 555-0143":  "+12025550143",
		"12345":              "",
		"abc":                "",
		"":                   "",
	} {
		if got := merchants.NormalizePhone(in); got != want {
			t.Errorf("%q: got %q, want %q", in, got, want)
		}
	}
}

// shop is an approved shop owned by a new user.
func (e *env) shop() (owner, merchant uuid.UUID) {
	e.t.Helper()
	owner, merchant = e.user("Tsion", "active"), uuid.New()
	e.exec(`INSERT INTO merchant.merchants (id, legal_name, trade_name, category_code, status) VALUES ($1, 'Selam Trading PLC', 'Selam Café', '5812', 'active')`, merchant)
	e.exec(`INSERT INTO merchant.members (merchant_id, user_id, role) VALUES ($1, $2, 'owner')`, merchant, owner)
	return owner, merchant
}

func (e *env) phone(user uuid.UUID) string {
	var p string
	if err := pool.QueryRow(e.ctx, `SELECT phone_e164 FROM identity.users WHERE id = $1`, user).Scan(&p); err != nil {
		e.t.Fatal(err)
	}
	return p
}

func (e *env) topics(merchant uuid.UUID) string {
	rows, _ := pool.Query(e.ctx, `SELECT topic FROM platform.outbox WHERE aggregate_id = $1 AND topic LIKE 'team.%' ORDER BY id`, merchant)
	var out []string
	for rows.Next() {
		var s string
		_ = rows.Scan(&s)
		out = append(out, s)
	}
	return strings.Join(out, " ")
}

func freshPhone() string {
	id := uuid.New()
	return fmt.Sprintf("+2519%08d", binary.BigEndian.Uint64(id[:8])%100_000_000)
}

func TestInviteAcceptChangeAndRemove(t *testing.T) {
	e := setup(t)
	owner, shop := e.shop()
	dawit := e.user("Dawit", "active")

	var invalid *merchants.ValidationError
	var conflict *merchants.ConflictError
	if _, err := e.svc.Invite(e.ctx, owner, shop, "12", "cashier"); !errors.As(err, &invalid) {
		t.Fatalf("bad number: %v", err)
	}
	if _, err := e.svc.Invite(e.ctx, owner, shop, e.phone(dawit), "owner"); !errors.As(err, &invalid) {
		t.Fatalf("an owner invited: %v", err)
	}
	if _, err := e.svc.Invite(e.ctx, owner, shop, e.phone(owner), "cashier"); !errors.As(err, &conflict) {
		t.Fatalf("a member invited: %v", err)
	}
	if _, err := e.svc.Invite(e.ctx, dawit, shop, freshPhone(), "cashier"); !errors.Is(err, merchants.ErrNotFound) {
		t.Fatalf("a stranger invited: %v", err)
	}

	// Inviting again replaces the first invitation.
	first, err := e.svc.Invite(e.ctx, owner, shop, e.phone(dawit), "accountant")
	if err != nil {
		t.Fatal(err)
	}
	inv, err := e.svc.Invite(e.ctx, owner, shop, "0"+e.phone(dawit)[4:], "cashier")
	if err != nil || inv.Role != "cashier" || inv.Shop != "Selam Café" || inv.InvitedBy != "Tsion" ||
		inv.ExpiresAt.Sub(inv.CreatedAt) != merchants.InviteFor {
		t.Fatalf("invite: %v %+v", err, inv)
	}
	mine, err := e.svc.MyInvitations(e.ctx, dawit)
	if err != nil || len(mine) != 1 || mine[0].ID != inv.ID {
		t.Fatalf("Dawit's invitations: %v %+v", err, mine)
	}
	if _, err := e.svc.AnswerInvitation(e.ctx, dawit, first.ID, true); !errors.Is(err, merchants.ErrInvitationGone) {
		t.Fatalf("the replaced invitation: %v", err)
	}
	// Nobody else can answer it.
	someone := e.user("Someone", "active")
	if _, err := e.svc.AnswerInvitation(e.ctx, someone, inv.ID, true); !errors.Is(err, merchants.ErrNotFound) {
		t.Fatalf("someone else accepted: %v", err)
	}

	m, err := e.svc.AnswerInvitation(e.ctx, dawit, inv.ID, true)
	if err != nil || m.Role != "cashier" || m.ID != shop {
		t.Fatalf("accept: %v %+v", err, m)
	}
	if _, err := e.svc.AnswerInvitation(e.ctx, dawit, inv.ID, true); !errors.Is(err, merchants.ErrInvitationGone) {
		t.Fatalf("accepted twice: %v", err)
	}
	// A cashier doesn't see the team; the owner does, with masked numbers.
	if _, err := e.svc.Team(e.ctx, dawit, shop); !errors.Is(err, merchants.ErrForbidden) {
		t.Fatalf("cashier sees the team: %v", err)
	}
	team, err := e.svc.Team(e.ctx, owner, shop)
	if err != nil || len(team.Members) != 2 || team.Members[0].Role != "owner" || team.Members[1].UserID != dawit ||
		!strings.HasPrefix(team.Members[1].Phone, "••") || len(team.Invitations) != 0 {
		t.Fatalf("team: %v %+v", err, team)
	}

	// Roles: only owners change them, never an owner's.
	if err := e.svc.SetRole(e.ctx, dawit, shop, dawit, "manager"); !errors.Is(err, merchants.ErrForbidden) {
		t.Fatalf("cashier promoted themselves: %v", err)
	}
	if err := e.svc.SetRole(e.ctx, owner, shop, owner, "cashier"); !errors.As(err, &conflict) {
		t.Fatalf("owner demoted: %v", err)
	}
	if err := e.svc.SetRole(e.ctx, owner, shop, dawit, "owner"); !errors.As(err, &invalid) {
		t.Fatalf("made an owner: %v", err)
	}
	if err := e.svc.SetRole(e.ctx, owner, shop, dawit, "manager"); err != nil {
		t.Fatal(err)
	}
	// A manager sees the team but can't change it.
	if team, err := e.svc.Team(e.ctx, dawit, shop); err != nil || team.Invitations != nil {
		t.Fatalf("manager's view: %v %+v", err, team)
	}
	if _, err := e.svc.Invite(e.ctx, dawit, shop, freshPhone(), "cashier"); !errors.Is(err, merchants.ErrForbidden) {
		t.Fatalf("manager invited: %v", err)
	}
	if err := e.svc.RemoveMember(e.ctx, dawit, shop, owner); !errors.As(err, &conflict) {
		t.Fatalf("manager removed the owner: %v", err)
	}
	if err := e.svc.RemoveMember(e.ctx, owner, shop, owner); !errors.As(err, &conflict) {
		t.Fatalf("owner left: %v", err)
	}

	// A browser signed in for the shop ends when they're removed.
	session := uuid.New()
	e.exec(`INSERT INTO merchant.web_sessions (id, token_hash, user_id, merchant_id, csrf_token, expires_at)
		VALUES ($1, $2, $3, $4, 'x', now() + interval '1 hour')`, session, session.String(), dawit, shop)
	if err := e.svc.RemoveMember(e.ctx, owner, shop, dawit); err != nil {
		t.Fatal(err)
	}
	var revoked bool
	_ = pool.QueryRow(e.ctx, `SELECT revoked_at IS NOT NULL FROM merchant.web_sessions WHERE id = $1`, session).Scan(&revoked)
	if !revoked {
		t.Error("the removed member's browser is still signed in")
	}
	if _, err := e.svc.Get(e.ctx, dawit, shop); !errors.Is(err, merchants.ErrNotFound) {
		t.Errorf("removed member still sees the shop: %v", err)
	}
	if got := e.topics(shop); got != "team.invited team.invited team.joined team.role_changed team.removed" {
		t.Errorf("events: %s", got)
	}
}

func TestInvitationsWaitForNewAccountsAndCanBeDeclinedOrCancelled(t *testing.T) {
	e := setup(t)
	owner, shop := e.shop()

	// A number not on EDash yet: the invitation is there when they sign up.
	later := freshPhone()
	inv, err := e.svc.Invite(e.ctx, owner, shop, later, "accountant")
	if err != nil {
		t.Fatal(err)
	}
	var payload string
	_ = pool.QueryRow(e.ctx, `SELECT payload::text FROM platform.outbox WHERE aggregate_id = $1 AND topic = 'team.invited'`, shop).Scan(&payload)
	if !strings.Contains(payload, `"user_id": null`) {
		t.Errorf("invite of an unknown number: %s", payload)
	}
	newcomer := uuid.New()
	e.exec(`INSERT INTO identity.users (id, phone_e164, display_name, status) VALUES ($1, $2, 'Hana', 'active')`, newcomer, later)
	if mine, _ := e.svc.MyInvitations(e.ctx, newcomer); len(mine) != 1 || mine[0].ID != inv.ID || mine[0].Role != "accountant" {
		t.Fatalf("newcomer's invitations: %+v", mine)
	}
	if m, err := e.svc.AnswerInvitation(e.ctx, newcomer, inv.ID, false); err != nil || m != nil {
		t.Fatalf("decline: %v", err)
	}
	if _, err := e.svc.Get(e.ctx, newcomer, shop); !errors.Is(err, merchants.ErrNotFound) {
		t.Error("declined but joined")
	}

	// Cancelled by the owner.
	abel := e.user("Abel", "active")
	inv2, err := e.svc.Invite(e.ctx, owner, shop, e.phone(abel), "manager")
	if err != nil {
		t.Fatal(err)
	}
	if err := e.svc.CancelInvitation(e.ctx, owner, shop, inv2.ID); err != nil {
		t.Fatal(err)
	}
	if mine, _ := e.svc.MyInvitations(e.ctx, abel); len(mine) != 0 {
		t.Fatalf("cancelled invitation still listed: %+v", mine)
	}
	if err := e.svc.CancelInvitation(e.ctx, owner, shop, inv2.ID); !errors.Is(err, merchants.ErrInvitationGone) {
		t.Fatalf("cancelled twice: %v", err)
	}

	// Expired.
	sara := e.user("Sara", "active")
	inv3, err := e.svc.Invite(e.ctx, owner, shop, e.phone(sara), "cashier")
	if err != nil {
		t.Fatal(err)
	}
	e.exec(`UPDATE merchant.invitations SET expires_at = now() - interval '1 minute' WHERE id = $1`, inv3.ID)
	if mine, _ := e.svc.MyInvitations(e.ctx, sara); len(mine) != 0 {
		t.Fatalf("expired invitation listed: %+v", mine)
	}
	if _, err := e.svc.AnswerInvitation(e.ctx, sara, inv3.ID, true); !errors.Is(err, merchants.ErrInvitationGone) {
		t.Fatalf("accepted an expired invitation: %v", err)
	}

	// Someone without a PIN yet can't join.
	pending := e.user("Lidya", "pending")
	inv4, err := e.svc.Invite(e.ctx, owner, shop, e.phone(pending), "cashier")
	if err != nil {
		t.Fatal(err)
	}
	var conflict *merchants.ConflictError
	if _, err := e.svc.AnswerInvitation(e.ctx, pending, inv4.ID, true); !errors.As(err, &conflict) {
		t.Fatalf("joined without a PIN: %v", err)
	}
}

func TestMembersLeaveAndSuspendedShopsCantChange(t *testing.T) {
	e := setup(t)
	owner, shop := e.shop()
	abel := e.user("Abel", "active")
	e.exec(`INSERT INTO merchant.members (merchant_id, user_id, role) VALUES ($1, $2, 'accountant')`, shop, abel)
	if err := e.svc.RemoveMember(e.ctx, abel, shop, abel); err != nil {
		t.Fatalf("leave: %v", err)
	}
	if got := e.topics(shop); got != "team.left" {
		t.Errorf("events: %s", got)
	}

	e.exec(`UPDATE merchant.merchants SET status = 'suspended' WHERE id = $1`, shop)
	var conflict *merchants.ConflictError
	if _, err := e.svc.Invite(e.ctx, owner, shop, freshPhone(), "cashier"); !errors.As(err, &conflict) {
		t.Fatalf("invited while suspended: %v", err)
	}
}

func TestTheTeamLimitHoldsUnderConcurrentInvites(t *testing.T) {
	e := setup(t)
	owner, shop := e.shop()
	// 48 members with the owner: room for one more.
	for i := 0; i < 47; i++ {
		e.exec(`INSERT INTO merchant.members (merchant_id, user_id, role) VALUES ($1, $2, 'cashier')`, shop, e.user("", "active"))
	}
	e.exec(`INSERT INTO merchant.invitations (merchant_id, phone_e164, role, invited_by, expires_at) VALUES ($1, $2, 'cashier', $3, $4)`,
		shop, freshPhone(), owner, time.Now().Add(time.Hour))
	var wg sync.WaitGroup
	errs := make([]error, 5)
	for i := range errs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = e.svc.Invite(e.ctx, owner, shop, freshPhone(), "cashier")
		}(i)
	}
	wg.Wait()
	ok := 0
	for _, err := range errs {
		var conflict *merchants.ConflictError
		switch {
		case err == nil:
			ok++
		case !errors.As(err, &conflict):
			t.Fatal(err)
		}
	}
	if ok != 1 {
		t.Fatalf("%d invitations sent with room for one", ok)
	}
}
