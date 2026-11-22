package httpapi

import (
	"context"
	"errors"
	"net/http"

	"github.com/google/uuid"

	"superapp/backend/internal/identity"
	"superapp/backend/internal/platform"
)

func (s *Server) deviceRoutes(mux *http.ServeMux, authed func(http.HandlerFunc) http.Handler) {
	mux.Handle("GET /v1/me/devices", authed(s.listDevices))
	mux.Handle("POST /v1/me/devices/{id}/sign-out", authed(s.signOutDevice))
	mux.Handle("POST /v1/me/devices/sign-out-others", authed(s.signOutOtherDevices))
}

func (s *Server) listDevices(w http.ResponseWriter, r *http.Request) {
	list, err := s.Identity.Devices(r.Context(), platform.PrincipalFrom(r.Context()))
	if err != nil {
		s.failDevices(w, err)
		return
	}
	items := make([]map[string]any, len(list))
	for i, d := range list {
		items[i] = map[string]any{"id": d.ID, "platform": d.Platform, "model": d.Model, "first_seen_at": d.FirstSeen,
			"last_active_at": d.LastActive, "current": d.Current, "biometric": d.Biometric}
	}
	platform.WriteJSON(w, http.StatusOK, map[string]any{"items": items})
}

func (s *Server) signOutDevice(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	var body struct {
		PIN string `json:"pin"`
	}
	if !decode(w, r, &body) {
		return
	}
	if err := s.Identity.SignOutDevice(r.Context(), platform.PrincipalFrom(r.Context()), id, body.PIN); err != nil {
		s.failDevices(w, err)
		return
	}
	s.forgetPushTokens(r.Context(), []uuid.UUID{id})
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) signOutOtherDevices(w http.ResponseWriter, r *http.Request) {
	var body struct {
		PIN string `json:"pin"`
	}
	if !decode(w, r, &body) {
		return
	}
	ids, err := s.Identity.SignOutOtherDevices(r.Context(), platform.PrincipalFrom(r.Context()), body.PIN)
	if err != nil {
		s.failDevices(w, err)
		return
	}
	s.forgetPushTokens(r.Context(), ids)
	platform.WriteJSON(w, http.StatusOK, map[string]any{"signed_out": len(ids)})
}

// forgetPushTokens stops notifications going to signed-out devices. A failure only means a stale token
// stays until the push provider reports it gone, so it's logged rather than reported.
func (s *Server) forgetPushTokens(ctx context.Context, devices []uuid.UUID) {
	if s.Notify == nil {
		return
	}
	for _, d := range devices {
		if err := s.Notify.RemovePushToken(ctx, d); err != nil && s.Log != nil {
			s.Log.Warn("remove push token of signed-out device", "err", err)
		}
	}
}

func (s *Server) failDevices(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, identity.ErrDeviceNotFound):
		platform.WriteProblem(w, platform.NewProblem(http.StatusNotFound, "not_found", "Not found",
			"That device isn't signed in to your account."))
	case errors.Is(err, identity.ErrThisDevice):
		platform.WriteProblem(w, platform.NewProblem(http.StatusConflict, "this_device", "This is the device you're using",
			"To sign this device out, use Sign out at the bottom of your profile."))
	default:
		s.failAuth(w, err)
	}
}
