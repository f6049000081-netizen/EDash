package httpapi

import (
	"fmt"
	"net/http"

	"superapp/backend/internal/commerce"
	"superapp/backend/internal/platform"
)

// slotBody is a shop's delivery-time rules as the apps send them: times as "HH:MM", Ethiopian time.
type slotBody struct {
	Minutes  int    `json:"minutes"`
	From     string `json:"from"`
	To       string `json:"to"`
	Capacity int    `json:"capacity"`
	Days     int    `json:"days"`
	Lead     int    `json:"lead_minutes"`
}

func (b *slotBody) rules() (*commerce.SlotRules, error) {
	from, ok1 := clockMinutes(b.From)
	to, ok2 := clockMinutes(b.To)
	if !ok1 || !ok2 {
		return nil, &commerce.ValidationError{Msg: "Write delivery hours as HH:MM, for example 09:00 and 18:00."}
	}
	return &commerce.SlotRules{Minutes: b.Minutes, From: from, To: to, Capacity: b.Capacity, Days: b.Days, Lead: b.Lead}, nil
}

// clockMinutes reads "HH:MM" (00:00 to 24:00) as minutes after midnight.
func clockMinutes(v string) (int, bool) {
	var h, m int
	if n, err := fmt.Sscanf(v, "%d:%d", &h, &m); err != nil || n != 2 || len(v) != 5 || h < 0 || m < 0 || m > 59 || h*60+m > 24*60 {
		return 0, false
	}
	return h*60 + m, true
}

func clock(minutes int) string { return fmt.Sprintf("%02d:%02d", minutes/60, minutes%60) }

func slotRulesJSON(r *commerce.SlotRules) any {
	if r == nil {
		return nil
	}
	return map[string]any{"minutes": r.Minutes, "from": clock(r.From), "to": clock(r.To), "capacity": r.Capacity,
		"days": r.Days, "lead_minutes": r.Lead, "time_zone": "Africa/Addis_Ababa"}
}

// getSlots lists the delivery times a customer can book now; an empty list when the shop has none.
func (s *Server) getSlots(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	list, err := s.Commerce.Slots(r.Context(), id)
	if err != nil {
		s.failCommerce(w, err)
		return
	}
	items := make([]map[string]any, 0, len(list))
	for _, sl := range list {
		items = append(items, map[string]any{"start": sl.Start, "end": sl.End, "left": sl.Left, "full": sl.Left == 0})
	}
	platform.WriteJSON(w, http.StatusOK, map[string]any{"uses_slots": list != nil, "items": items})
}
