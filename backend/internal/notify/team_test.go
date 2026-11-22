package notify_test

import (
	"testing"

	"github.com/google/uuid"
)

func TestTeamNotifications(t *testing.T) {
	e := setup(t)
	shop := e.merchant("Selam Café")
	owner, manager := e.user("Tsion", "en"), e.user("Meron", "en")
	dawit := e.user("Dawit", "am")
	e.exec(`INSERT INTO merchant.members (merchant_id, user_id, role) VALUES ($1, $2, 'owner'), ($1, $3, 'manager')`, shop, owner, manager)
	inv := uuid.New()

	// The invited person is told, in their language, with the role in it.
	e.handle(event("team.invited", shop, map[string]any{"merchant_id": shop, "invitation_id": inv, "user_id": dawit, "by": owner, "role": "cashier"}))
	got := e.list(dawit)
	if len(got) != 1 || got[0].Kind != "team_invited" || got[0].Title != "የSelam Café ቡድንን ይቀላቀላሉ?" ||
		got[0].Body != "Tsion በገንዘብ ተቀባይነት ጋብዘዎታል። ለመቀበል ወይም ላለመቀበል መገለጫ → ንግድን ይክፈቱ። ግብዣው ለ7 ቀናት ይቆያል።" ||
		got[0].Data["invitation_id"] != inv.String() {
		t.Fatalf("invited: %+v", got)
	}
	// An invitation to a number not on EDash notifies nobody.
	e.handle(event("team.invited", shop, map[string]any{"merchant_id": shop, "invitation_id": uuid.New(), "user_id": nil, "by": owner, "role": "manager"}))

	// Joining tells the owners, not the managers.
	e.handle(event("team.joined", shop, map[string]any{"merchant_id": shop, "invitation_id": inv, "user_id": dawit, "role": "cashier"}))
	e.exec(`INSERT INTO merchant.members (merchant_id, user_id, role) VALUES ($1, $2, 'cashier')`, shop, dawit)
	if o := e.list(owner); len(o) != 1 || o[0].Title != "Dawit joined Selam Café" || o[0].Body != "They accepted your invitation and joined as cashier." {
		t.Fatalf("owner: %+v", o)
	}
	if m := e.list(manager); len(m) != 0 {
		t.Fatalf("manager: %+v", m)
	}

	e.handle(event("team.role_changed", shop, map[string]any{"merchant_id": shop, "user_id": dawit, "by": owner, "role": "accountant"}))
	e.handle(event("team.removed", shop, map[string]any{"merchant_id": shop, "user_id": dawit, "by": owner, "role": "accountant"}))
	got = e.list(dawit)
	if len(got) != 3 || got[1].Kind != "team_role_changed" || got[1].Body != "አሁን ሂሳብ ሠራተኛ ነዎት። በንግድ ውስጥ የሚያዩትና የሚሠሩት አብሮ ተቀይሯል።" ||
		got[0].Kind != "team_removed" {
		t.Fatalf("Dawit: %+v", got)
	}

	e.handle(event("team.left", shop, map[string]any{"merchant_id": shop, "user_id": manager, "by": manager, "role": "manager"}))
	if o := e.list(owner); len(o) != 2 || o[0].Title != "Meron left Selam Café" || o[0].Body != "They were on the team as manager. Their computer sign-ins for the shop have ended." {
		t.Fatalf("owner after leaving: %+v", o)
	}
}
