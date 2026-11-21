// Package httpapi exposes the auth, profile, wallet and payments endpoints from api/openapi.yaml.
package httpapi

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"superapp/backend/internal/identity"
	"superapp/backend/internal/ledger"
	"superapp/backend/internal/money"
	"superapp/backend/internal/payments"
	"superapp/backend/internal/platform"
)

type Server struct {
	Pool     *pgxpool.Pool
	Ledger   *ledger.Ledger
	Payments *payments.Service
	Identity *identity.Service
	Auth     platform.Authenticator // normally Identity; tests may substitute their own
	Log      *slog.Logger
	Sandbox  bool // enables POST /v1/sandbox/topups
	// Webhooks verifies partner notifications, keyed by rail. A rail without an entry answers 404.
	Webhooks map[string]NotificationVerifier
	// ClientIPHeader names a header set by a trusted proxy carrying the client address
	// (for example CF-Connecting-IP). Empty means use the TCP peer address.
	ClientIPHeader string
}

func (s *Server) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		if err := s.Pool.Ping(r.Context()); err != nil {
			http.Error(w, "db down", http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte("ok"))
	})

	authed := func(h http.HandlerFunc) http.Handler { return s.authenticate(h) }
	mutating := func(h http.HandlerFunc) http.Handler { return s.authenticate(platform.Idempotent(s.Pool, h)) }

	if s.Identity != nil {
		mux.HandleFunc("POST /v1/auth/otp", s.requestOTP)
		mux.HandleFunc("POST /v1/auth/otp/verify", s.verifyOTP)
		mux.HandleFunc("POST /v1/auth/token/refresh", s.refreshToken)
		mux.Handle("POST /v1/auth/logout", authed(s.logout))
		mux.Handle("PUT /v1/auth/pin", mutating(s.setPIN))
		mux.Handle("GET /v1/me", authed(s.getMe))
	}
	mux.Handle("POST /v1/topups", mutating(s.startTopUp))
	mux.HandleFunc("POST /v1/webhooks/rails/{rail}", s.railNotification)
	mux.Handle("GET /v1/wallet/balances", authed(s.getBalances))
	mux.Handle("POST /v1/qr/resolve", authed(s.resolveQR))
	mux.Handle("POST /v1/payment-intents", mutating(s.createIntent))
	mux.Handle("GET /v1/payment-intents/{id}", authed(s.getIntent))
	mux.Handle("POST /v1/payment-intents/{id}/confirm", mutating(s.confirmIntent))
	mux.Handle("POST /v1/payment-intents/{id}/cancel", mutating(s.cancelIntent))
	if s.Sandbox {
		mux.Handle("POST /v1/sandbox/topups", mutating(s.sandboxTopUp))
	}
	return s.logRequests(mux)
}

// ---------------------------------------------------------------------------
// middleware
// ---------------------------------------------------------------------------

func (s *Server) authenticate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p, err := s.Auth.Authenticate(r)
		if err != nil {
			if !errors.Is(err, platform.ErrUnauthenticated) {
				s.Log.Error("authenticate", "err", err)
				platform.WriteProblem(w, platform.NewProblem(http.StatusInternalServerError, "internal", "Something went wrong", "Please try again."))
				return
			}
			w.Header().Set("WWW-Authenticate", `Bearer realm="gebeya-one"`)
			platform.WriteProblem(w, platform.NewProblem(http.StatusUnauthorized, "unauthenticated", "Sign in required", ""))
			return
		}
		next.ServeHTTP(w, r.WithContext(platform.WithPrincipal(r.Context(), p)))
	})
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(s int) { w.status = s; w.ResponseWriter.WriteHeader(s) }

func (s *Server) logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(sw, r)
		// No bodies, PINs or user ids in logs.
		s.Log.Info("request", "method", r.Method, "path", r.URL.Path, "status", sw.status, "ms", time.Since(start).Milliseconds())
	})
}

// ---------------------------------------------------------------------------
// handlers
// ---------------------------------------------------------------------------

func (s *Server) getBalances(w http.ResponseWriter, r *http.Request) {
	user := platform.UserFrom(r.Context())
	bals, err := s.Ledger.UserBalances(r.Context(), s.Pool, user)
	if err != nil {
		s.fail(w, err)
		return
	}
	type bal struct {
		Available money.Money `json:"available"`
		Held      money.Money `json:"held"`
	}
	out := []bal{}
	for _, b := range bals {
		out = append(out, bal{Available: b, Held: money.New(0, b.Currency)})
	}
	if len(out) == 0 {
		out = append(out, bal{Available: money.New(0, money.ETB), Held: money.New(0, money.ETB)})
	}
	platform.WriteJSON(w, http.StatusOK, map[string]any{"balances": out, "points": 0, "as_of": time.Now().UTC()})
}

func (s *Server) resolveQR(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Payload string `json:"payload"`
	}
	if !decode(w, r, &body) {
		return
	}
	qr, err := s.Payments.ResolveQR(r.Context(), body.Payload)
	if err != nil {
		s.fail(w, err)
		return
	}
	platform.WriteJSON(w, http.StatusOK, map[string]any{
		"payee_type": "merchant", "payee_id": qr.MerchantID, "display_name": qr.MerchantName,
		"verified": true, "fixed_amount": qr.FixedAmount, "active_offer": nil,
	})
}

func (s *Server) createIntent(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Kind            payments.Kind `json:"kind"`
		Amount          *money.Money  `json:"amount"`
		PayeeUserID     *uuid.UUID    `json:"payee_user_id"`
		QRPayload       string        `json:"qr_payload"`
		FundingSourceID *uuid.UUID    `json:"funding_source_id"`
		Note            string        `json:"note"`
	}
	if !decode(w, r, &body) {
		return
	}
	in, err := s.Payments.CreateIntent(r.Context(), platform.UserFrom(r.Context()), payments.CreateRequest{
		Kind: body.Kind, Amount: body.Amount, PayeeUserID: body.PayeeUserID, QRPayload: body.QRPayload,
		FundingSourceID: body.FundingSourceID, Note: body.Note,
	})
	if err != nil {
		s.fail(w, err)
		return
	}
	platform.WriteJSON(w, http.StatusCreated, present(in))
}

func (s *Server) getIntent(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	in, err := s.Payments.Get(r.Context(), platform.UserFrom(r.Context()), id)
	if err != nil {
		s.fail(w, err)
		return
	}
	platform.WriteJSON(w, http.StatusOK, present(in))
}

func (s *Server) confirmIntent(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	var body struct {
		PIN                string `json:"pin"`
		BiometricSignature string `json:"biometric_signature"`
	}
	if !decode(w, r, &body) {
		return
	}
	if body.BiometricSignature != "" {
		platform.WriteProblem(w, platform.NewProblem(http.StatusBadRequest, "validation_failed", "Not supported yet",
			"Biometric confirmation isn't available yet. Use your PIN."))
		return
	}
	in, err := s.Payments.Confirm(r.Context(), platform.UserFrom(r.Context()), id, body.PIN)
	if err != nil {
		s.fail(w, err)
		return
	}
	platform.WriteJSON(w, http.StatusOK, present(in))
}

func (s *Server) cancelIntent(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	in, err := s.Payments.Cancel(r.Context(), platform.UserFrom(r.Context()), id)
	if err != nil {
		s.fail(w, err)
		return
	}
	platform.WriteJSON(w, http.StatusOK, present(in))
}

func (s *Server) sandboxTopUp(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Amount money.Money `json:"amount"`
		Rail   string      `json:"rail"`
	}
	if !decode(w, r, &body) {
		return
	}
	if body.Rail == "" {
		body.Rail = "telebirr"
	}
	in, err := s.Payments.SandboxTopUp(r.Context(), platform.UserFrom(r.Context()), body.Amount, body.Rail)
	if err != nil {
		s.fail(w, err)
		return
	}
	platform.WriteJSON(w, http.StatusCreated, present(in))
}

// ---------------------------------------------------------------------------
// presentation and errors
// ---------------------------------------------------------------------------

type intentJSON struct {
	ID     uuid.UUID `json:"id"`
	Kind   string    `json:"kind"`
	Status string    `json:"status"`
	Payee  struct {
		Type         string `json:"type"`
		DisplayName  string `json:"display_name"`
		VerifiedName bool   `json:"verified_name"`
	} `json:"payee"`
	Amount       money.Money       `json:"amount"`
	Fee          money.Money       `json:"fee"`
	Total        money.Money       `json:"total"`
	PointsToEarn int64             `json:"points_to_earn"`
	Rail         string            `json:"rail"`
	StepUp       string            `json:"step_up"`
	Failure      *platform.Problem `json:"failure"`
	ReceiptURL   *string           `json:"receipt_url"`
	CreatedAt    time.Time         `json:"created_at"`
}

func present(in *payments.Intent) intentJSON {
	j := intentJSON{
		ID: in.ID, Kind: string(in.Kind), Status: string(in.Status),
		Amount: in.Amount, Fee: money.New(in.FeeMinor, in.Amount.Currency), Total: in.Total(),
		PointsToEarn: in.PointsToEarn(), Rail: in.Rail, StepUp: "none", CreatedAt: in.CreatedAt,
	}
	j.Payee.Type, j.Payee.DisplayName, j.Payee.VerifiedName = in.PayeeType, in.PayeeName, in.PayeeVerified
	if in.Status == payments.StatusRequiresConfirmation {
		j.StepUp = "biometric_or_pin"
	}
	if in.FailureCode != "" {
		j.Failure = platform.NewProblem(http.StatusPaymentRequired, in.FailureCode, "Payment failed", "").NotCharged()
	}
	return j
}

func (s *Server) fail(w http.ResponseWriter, err error) {
	var v *payments.ValidationError
	var p *platform.Problem
	switch {
	case errors.As(err, &v):
		p = platform.NewProblem(http.StatusUnprocessableEntity, "validation_failed", "Check the details", v.Msg)
	case errors.Is(err, payments.ErrNotFound):
		p = platform.NewProblem(http.StatusNotFound, "not_found", "Not found", "")
	case errors.Is(err, payments.ErrPayeeNotFound):
		p = platform.NewProblem(http.StatusNotFound, "payee_not_found", "Payee not found", "We couldn't find that person or business.")
	case errors.Is(err, payments.ErrSelfPayment):
		p = platform.NewProblem(http.StatusUnprocessableEntity, "validation_failed", "Can't pay yourself", err.Error())
	case errors.Is(err, payments.ErrUnsupportedKind):
		p = platform.NewProblem(http.StatusUnprocessableEntity, "validation_failed", "Not available yet", err.Error())
	case errors.Is(err, payments.ErrRailError):
		s.Log.Error("rail error", "err", err)
		p = platform.NewProblem(http.StatusServiceUnavailable, "rail_unavailable", "Provider not responding",
			"The payment provider didn't respond. Nothing was charged. Please try again in a minute.").NotCharged()
	case errors.Is(err, payments.ErrTooManyPending):
		p = platform.NewProblem(http.StatusConflict, "invalid_state", "Unfinished top-ups",
			"Finish or wait for your other top-ups before starting a new one.")
	case errors.Is(err, payments.ErrRailUnavailable):
		p = platform.NewProblem(http.StatusUnprocessableEntity, "rail_unavailable", "Not available yet", err.Error())
	case errors.Is(err, payments.ErrInvalidState):
		p = platform.NewProblem(http.StatusConflict, "invalid_state", "Can't change this payment", err.Error())
	case errors.Is(err, payments.ErrInsufficientFunds):
		p = platform.NewProblem(http.StatusPaymentRequired, "insufficient_funds", "Not enough money",
			"Your balance is too low for this payment. Nothing was taken from your account.").NotCharged()
	case errors.Is(err, payments.ErrBlocked):
		p = platform.NewProblem(http.StatusForbidden, "payment_blocked", "Payment blocked",
			"We stopped this payment to protect your account. Nothing was taken.").NotCharged()
	case errors.Is(err, payments.ErrPayerInactive):
		p = platform.NewProblem(http.StatusForbidden, "permission_denied", "Account restricted", err.Error())
	case errors.Is(err, platform.ErrInvalidPIN):
		p = platform.NewProblem(http.StatusForbidden, "invalid_pin", "Wrong PIN", "That PIN isn't right. Try again.")
	case errors.Is(err, platform.ErrPINLocked):
		p = platform.NewProblem(http.StatusForbidden, "pin_locked", "PIN locked",
			"Too many wrong attempts. Try again in 30 minutes or reset your PIN.")
	case errors.Is(err, platform.ErrNoPIN):
		p = platform.NewProblem(http.StatusForbidden, "step_up_required", "Set a PIN first", "")
	default:
		s.Log.Error("unhandled error", "err", err)
		p = platform.NewProblem(http.StatusInternalServerError, "internal", "Something went wrong", "Please try again.")
	}
	platform.WriteProblem(w, p)
}

func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	dec := json.NewDecoder(io.LimitReader(r.Body, 1<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil && !errors.Is(err, io.EOF) {
		platform.WriteProblem(w, platform.NewProblem(http.StatusBadRequest, "validation_failed", "Invalid JSON", err.Error()))
		return false
	}
	return true
}

func pathID(w http.ResponseWriter, r *http.Request) (uuid.UUID, bool) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		platform.WriteProblem(w, platform.NewProblem(http.StatusNotFound, "not_found", "Not found", ""))
		return uuid.Nil, false
	}
	return id, true
}
