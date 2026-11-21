package httpapi

import (
	"errors"
	"math"
	"net"
	"net/http"
	"strconv"
	"strings"

	"github.com/google/uuid"

	"superapp/backend/internal/identity"
	"superapp/backend/internal/platform"
)

func (s *Server) requestOTP(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Phone   string `json:"phone"`
		Purpose string `json:"purpose"`
		Locale  string `json:"locale"`
	}
	if !decode(w, r, &body) {
		return
	}
	ch, err := s.Identity.RequestOTP(r.Context(), identity.OTPRequest{
		Phone: body.Phone, Purpose: body.Purpose, Locale: body.Locale, ClientIP: s.clientIP(r),
	})
	if err != nil {
		s.failAuth(w, err)
		return
	}
	platform.WriteJSON(w, http.StatusAccepted, map[string]any{
		"challenge_id": ch.ID, "expires_at": ch.ExpiresAt, "resend_after_seconds": int(ch.ResendAfter.Seconds()),
	})
}

func (s *Server) verifyOTP(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ChallengeID uuid.UUID `json:"challenge_id"`
		Code        string    `json:"code"`
		Device      struct {
			Platform           string `json:"platform"`
			Model              string `json:"model"`
			Attestation        string `json:"attestation"`
			BiometricPublicKey string `json:"biometric_public_key"`
		} `json:"device"`
	}
	if !decode(w, r, &body) {
		return
	}
	sess, err := s.Identity.VerifyOTP(r.Context(), body.ChallengeID, body.Code, identity.DeviceInfo{
		Platform: body.Device.Platform, Model: body.Device.Model,
		Attestation: body.Device.Attestation, BiometricPublicKey: body.Device.BiometricPublicKey,
	})
	if err != nil {
		s.failAuth(w, err)
		return
	}
	writeSession(w, sess)
}

func (s *Server) refreshToken(w http.ResponseWriter, r *http.Request) {
	var body struct {
		RefreshToken string `json:"refresh_token"`
	}
	if !decode(w, r, &body) {
		return
	}
	sess, err := s.Identity.Refresh(r.Context(), body.RefreshToken)
	if err != nil {
		s.failAuth(w, err)
		return
	}
	writeSession(w, sess)
}

func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	if err := s.Identity.Logout(r.Context(), platform.PrincipalFrom(r.Context())); err != nil {
		s.failAuth(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) setPIN(w http.ResponseWriter, r *http.Request) {
	var body struct {
		CurrentPIN string `json:"current_pin"`
		NewPIN     string `json:"new_pin"`
	}
	if !decode(w, r, &body) {
		return
	}
	if err := s.Identity.SetPIN(r.Context(), platform.PrincipalFrom(r.Context()), body.CurrentPIN, body.NewPIN); err != nil {
		s.failAuth(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) getMe(w http.ResponseWriter, r *http.Request) {
	p, err := s.Identity.Me(r.Context(), platform.UserFrom(r.Context()))
	if err != nil {
		s.failAuth(w, err)
		return
	}
	s.writeProfile(w, r, p)
}

func (s *Server) updateMe(w http.ResponseWriter, r *http.Request) {
	var body struct {
		DisplayName *string `json:"display_name"`
		Email       *string `json:"email"`
		Locale      *string `json:"locale"`
	}
	if !decode(w, r, &body) {
		return
	}
	p, err := s.Identity.UpdateProfile(r.Context(), platform.UserFrom(r.Context()), identity.ProfileUpdate{
		DisplayName: body.DisplayName, Email: body.Email, Locale: body.Locale,
	})
	if err != nil {
		s.failAuth(w, err)
		return
	}
	s.writeProfile(w, r, p)
}

// writeProfile adds the user's limits; "limits" is null until compliance fills identity.kyc_limits.
func (s *Server) writeProfile(w http.ResponseWriter, r *http.Request, p *identity.Profile) {
	var limits any
	if s.Payments != nil {
		st, err := s.Payments.LimitStatus(r.Context(), p.ID)
		if err != nil {
			s.fail(w, err)
			return
		}
		if st != nil {
			limits = map[string]any{
				"max_balance": st.MaxBalance, "daily_out_remaining": st.DailyRemaining, "monthly_out_remaining": st.MonthlyRemaining,
			}
		}
	}
	var email any
	if p.Email != "" {
		email = p.Email
	}
	platform.WriteJSON(w, http.StatusOK, map[string]any{
		"id": p.ID, "phone": p.Phone, "email": email, "display_name": p.DisplayName, "locale": p.Locale,
		"kyc_level": p.KYCLevel, "status": p.Status, "has_pin": p.HasPIN, "limits": limits,
	})
}

func writeSession(w http.ResponseWriter, s *identity.Session) {
	w.Header().Set("Cache-Control", "no-store")
	platform.WriteJSON(w, http.StatusOK, map[string]any{
		"access_token": s.AccessToken, "refresh_token": s.RefreshToken, "token_type": "Bearer",
		"expires_in": s.ExpiresIn, "user_id": s.UserID, "needs_pin": s.NeedsPIN,
	})
}

// clientIP uses the header named by ClientIPHeader when the API sits behind a proxy that sets it
// (and strips any client-supplied copy). Otherwise it uses the TCP peer address.
func (s *Server) clientIP(r *http.Request) string {
	if s.ClientIPHeader != "" {
		if v := strings.TrimSpace(strings.Split(r.Header.Get(s.ClientIPHeader), ",")[0]); v != "" {
			return v
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func (s *Server) failAuth(w http.ResponseWriter, err error) {
	var v *identity.ValidationError
	var rl *identity.RateLimitedError
	var bad *identity.OTPInvalidError
	var p *platform.Problem
	switch {
	case errors.As(err, &v):
		p = platform.NewProblem(http.StatusUnprocessableEntity, "validation_failed", "Check the details", v.Msg)
	case errors.As(err, &rl):
		secs := int(math.Ceil(rl.RetryAfter.Seconds()))
		if secs < 1 {
			secs = 1
		}
		w.Header().Set("Retry-After", strconv.Itoa(secs))
		p = platform.NewProblem(http.StatusTooManyRequests, "rate_limited", "Too many requests",
			"Please wait before asking for another code.")
	case errors.As(err, &bad):
		p = platform.NewProblem(http.StatusBadRequest, "otp_invalid", "Wrong code",
			"That code isn't right. "+strconv.Itoa(bad.AttemptsLeft)+" tries left.")
	case errors.Is(err, identity.ErrOTPExpired):
		p = platform.NewProblem(http.StatusBadRequest, "otp_expired", "Code expired", "Ask for a new code and try again.")
	case errors.Is(err, identity.ErrAttestationFailed):
		p = platform.NewProblem(http.StatusForbidden, "device_not_trusted", "Device check failed",
			"We couldn't verify this device. Update the app from the official store and try again.")
	case errors.Is(err, identity.ErrSessionInvalid):
		p = platform.NewProblem(http.StatusUnauthorized, "unauthenticated", "Sign in again", "Your session has ended.")
	case errors.Is(err, identity.ErrAccountClosed):
		p = platform.NewProblem(http.StatusForbidden, "permission_denied", "Account closed", "This account is closed. Contact support.")
	case errors.Is(err, identity.ErrCurrentPINRequired):
		p = platform.NewProblem(http.StatusBadRequest, "validation_failed", "Enter your current PIN",
			"Enter your current PIN, or reset it with a code sent to your phone.")
	case errors.Is(err, identity.ErrEmailInUse):
		p = platform.NewProblem(http.StatusConflict, "validation_failed", "Email already used",
			"That email belongs to another account.")
	case errors.Is(err, identity.ErrSMSUnavailable):
		p = platform.NewProblem(http.StatusServiceUnavailable, "sms_unavailable", "Couldn't send the code", "Please try again in a minute.")
	default:
		s.fail(w, err) // PIN errors and everything else
		return
	}
	platform.WriteProblem(w, p)
}
