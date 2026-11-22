package commerce

import (
	"testing"
	"time"
)

func TestSlotWindowsFollowAddisTimeAndLead(t *testing.T) {
	r := SlotRules{Minutes: 120, From: 9 * 60, To: 18 * 60, Capacity: 3, Days: 2, Lead: 60}
	// 10:30 in Addis Ababa (07:30 UTC). The 11:00 slot is within the hour's notice, so 13:00 comes first.
	now := time.Date(2026, 11, 23, 7, 30, 0, 0, time.UTC)
	var got []string
	for _, w := range r.windows(now) {
		got = append(got, w.Start.In(EAT).Format("Jan 2 15:04")+"-"+w.End.In(EAT).Format("15:04"))
	}
	want := []string{
		"Nov 23 13:00-15:00", "Nov 23 15:00-17:00",
		"Nov 24 09:00-11:00", "Nov 24 11:00-13:00", "Nov 24 13:00-15:00", "Nov 24 15:00-17:00",
	}
	if len(got) != len(want) {
		t.Fatalf("got %v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("slot %d: got %v, want %v", i, got, want)
		}
	}
	// 17:00 doesn't fit a whole two hours before 18:00, so it's never offered.

	// 22:30 UTC is already the next day in Addis Ababa: "today" there is the 24th.
	late := time.Date(2026, 11, 23, 22, 30, 0, 0, time.UTC)
	if first := r.windows(late)[0].Start.In(EAT); first.Day() != 24 || first.Hour() != 9 {
		t.Fatalf("first slot after midnight in Addis: %v", first)
	}

	// A slot running to midnight ends at the next day's 00:00.
	night := SlotRules{Minutes: 60, From: 23 * 60, To: 24 * 60, Capacity: 1, Days: 1, Lead: 0}
	if w := night.windows(time.Date(2026, 11, 23, 6, 0, 0, 0, time.UTC)); len(w) != 1 || w[0].End.In(EAT).Format("2 15:04") != "24 00:00" {
		t.Fatalf("night slot: %+v", w)
	}
}

func TestSlotRulesAreChecked(t *testing.T) {
	ok := SlotRules{Minutes: 60, From: 8 * 60, To: 20 * 60, Capacity: 5, Days: 3, Lead: 30}
	if err := ok.check(); err != nil {
		t.Fatal(err)
	}
	for name, bad := range map[string]SlotRules{
		"90 minutes":     {Minutes: 90, From: 480, To: 1200, Capacity: 5, Days: 3},
		"quarter past":   {Minutes: 60, From: 495, To: 1200, Capacity: 5, Days: 3},
		"after midnight": {Minutes: 60, From: 480, To: 1470, Capacity: 5, Days: 3},
		"too short":      {Minutes: 240, From: 480, To: 600, Capacity: 5, Days: 3},
		"no places":      {Minutes: 60, From: 480, To: 1200, Capacity: 0, Days: 3},
		"too many":       {Minutes: 60, From: 480, To: 1200, Capacity: 51, Days: 3},
		"two weeks":      {Minutes: 60, From: 480, To: 1200, Capacity: 5, Days: 14},
		"two days' lead": {Minutes: 60, From: 480, To: 1200, Capacity: 5, Days: 3, Lead: 2880},
		"not a number":   {Minutes: 60, From: -1, To: 1200, Capacity: 5, Days: 3},
	} {
		if err := bad.check(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}
