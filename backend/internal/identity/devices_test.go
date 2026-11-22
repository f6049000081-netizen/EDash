package identity_test

import (
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"superapp/backend/internal/identity"
	"superapp/backend/internal/platform"
)

const devicePIN = "204816"

func (e *env) withPIN(s *identity.Session) platform.Principal {
	e.t.Helper()
	p := e.principal(s)
	if err := e.svc.SetPIN(e.ctx, p, "", devicePIN); err != nil {
		e.t.Fatal(err)
	}
	return p
}

func (e *env) outbox(user uuid.UUID, topic string) int {
	var n int
	if err := pool.QueryRow(e.ctx, `SELECT count(*) FROM platform.outbox WHERE topic = $1 AND payload->>'user_id' = $2`,
		topic, user.String()).Scan(&n); err != nil {
		e.t.Fatal(err)
	}
	return n
}

func TestDevicesListAndSignOutOne(t *testing.T) {
	e := setup(t)
	phone := newPhone()
	phoneSession := e.signIn(phone, "signup", "phone")
	me := e.withPIN(phoneSession)
	e.advance(10 * time.Minute)
	tablet := e.signIn(phone, "login", "tablet")
	other := e.signIn(newPhone(), "signup", "stranger")

	list, err := e.svc.Devices(e.ctx, me)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 || !list[0].Current || list[0].ID != me.DeviceID || list[1].Current || list[1].Model != "Tecno Spark" ||
		!list[1].LastActive.After(list[0].LastActive) {
		t.Fatalf("devices: %+v", list)
	}
	tabletID := list[1].ID

	// This device signs out with Logout; strangers' devices aren't found; the PIN is needed.
	if err := e.svc.SignOutDevice(e.ctx, me, me.DeviceID, devicePIN); !errors.Is(err, identity.ErrThisDevice) {
		t.Errorf("this device: %v", err)
	}
	if err := e.svc.SignOutDevice(e.ctx, me, e.principal(other).DeviceID, devicePIN); !errors.Is(err, identity.ErrDeviceNotFound) {
		t.Errorf("stranger's device: %v", err)
	}
	if err := e.svc.SignOutDevice(e.ctx, me, tabletID, "111111"); !errors.Is(err, platform.ErrInvalidPIN) {
		t.Errorf("wrong PIN: %v", err)
	}
	e.principal(tablet)

	if err := e.svc.SignOutDevice(e.ctx, me, tabletID, devicePIN); err != nil {
		t.Fatal(err)
	}
	if _, err := e.auth(tablet.AccessToken); err == nil {
		t.Error("the tablet's access token still works")
	}
	if _, err := e.svc.Refresh(e.ctx, tablet.RefreshToken, identity.DPoPProof{}); err == nil {
		t.Error("the tablet can still refresh")
	}
	e.principal(phoneSession)
	e.principal(other)
	if n := e.outbox(me.UserID, "identity.device_signed_out"); n != 1 {
		t.Errorf("notifications: %d", n)
	}
	if err := e.svc.SignOutDevice(e.ctx, me, tabletID, devicePIN); !errors.Is(err, identity.ErrDeviceNotFound) {
		t.Errorf("signed out twice: %v", err)
	}
	if list, _ := e.svc.Devices(e.ctx, me); len(list) != 1 {
		t.Errorf("after sign-out: %+v", list)
	}

	// Signing in on it again works with an SMS code, as a device the account knows.
	again := e.signIn(phone, "login", "tablet")
	if p := e.principal(again); p.DeviceID != tabletID {
		t.Errorf("signed back in as a different device: %s", p.DeviceID)
	}
}

func TestSignOutOtherDevicesKeepsThisSession(t *testing.T) {
	e := setup(t)
	phone := newPhone()
	old := e.signIn(phone, "signup", "phone") // an earlier session on this phone, e.g. before a reinstall
	cur := e.signIn(phone, "login", "phone")
	me := e.withPIN(cur)
	a := e.signIn(phone, "login", "tablet")
	b := e.signIn(phone, "login", "laptop")
	if _, err := e.svc.SignOutOtherDevices(e.ctx, me, "000000"); !errors.Is(err, platform.ErrInvalidPIN) {
		t.Errorf("wrong PIN: %v", err)
	}
	ids, err := e.svc.SignOutOtherDevices(e.ctx, me, devicePIN)
	if err != nil || len(ids) != 2 {
		t.Fatalf("sign out others: %v %v", ids, err)
	}
	for name, s := range map[string]*identity.Session{"tablet": a, "laptop": b, "old session": old} {
		if _, err := e.auth(s.AccessToken); err == nil {
			t.Errorf("%s still signed in", name)
		}
	}
	e.principal(cur)
	if n := e.outbox(me.UserID, "identity.device_signed_out"); n != 2 {
		t.Errorf("notifications: %d", n)
	}
	if list, _ := e.svc.Devices(e.ctx, me); len(list) != 1 || !list[0].Current {
		t.Errorf("left: %+v", list)
	}
	if ids, err := e.svc.SignOutOtherDevices(e.ctx, me, devicePIN); err != nil || len(ids) != 0 {
		t.Errorf("again: %v %v", ids, err)
	}
}

func TestSigningOutADeviceRemovesItsBiometricKey(t *testing.T) {
	e := setup(t)
	phone := newPhone()
	me := e.withPIN(e.signIn(phone, "signup", "phone"))
	tablet := e.principal(e.signIn(phone, "login", "tablet"))
	if _, err := pool.Exec(e.ctx, `UPDATE identity.devices SET biometric_key_pub = 'k', biometric_enrolled_at = now() WHERE id = $1`,
		tablet.DeviceID); err != nil {
		t.Fatal(err)
	}
	if list, _ := e.svc.Devices(e.ctx, me); len(list) != 2 || !list[1].Biometric {
		t.Fatalf("devices: %+v", list)
	}
	if err := e.svc.SignOutDevice(e.ctx, me, tablet.DeviceID, devicePIN); err != nil {
		t.Fatal(err)
	}
	var key *string
	if err := pool.QueryRow(e.ctx, `SELECT biometric_key_pub FROM identity.devices WHERE id = $1`, tablet.DeviceID).Scan(&key); err != nil || key != nil {
		t.Errorf("biometric key kept: %v %v", key, err)
	}
}
