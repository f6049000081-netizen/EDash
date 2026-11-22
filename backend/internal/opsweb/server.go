// Package opsweb is the operations console: server-rendered pages for staff, on its own server that is
// reachable only from the company network. Staff sign in through the company's identity provider
// (OpenID Connect); the console keeps no passwords. Pages carry no scripts, so a strict Content Security
// Policy applies, and every form is protected by a per-session CSRF token and a same-origin check.
package opsweb

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"embed"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"github.com/google/uuid"
	"golang.org/x/oauth2"

	"superapp/backend/internal/kyb"
	"superapp/backend/internal/merchants"
	"superapp/backend/internal/money"
	"superapp/backend/internal/ops"
	"superapp/backend/internal/payments"
	"superapp/backend/internal/payouts"
)

//go:embed web
var webFS embed.FS

type Config struct {
	Issuer       string // the company identity provider
	ClientID     string
	ClientSecret string
	BaseURL      string // where staff reach the console, e.g. https://ops.internal.gebeya.one
	// RequiredAMR lists authentication methods of which the ID token's "amr" must contain at least one
	// (e.g. "hwk" for a hardware key). Empty skips the check: local development only.
	RequiredAMR []string
	SessionKey  []byte // signs the short-lived sign-in cookie; 32 random bytes
}

type Server struct {
	Ops  *ops.Service
	Log  *slog.Logger
	cfg  Config
	oidc *oidc.IDTokenVerifier
	oa   *oauth2.Config
	tpl  map[string]*template.Template
	base *url.URL
}

const (
	sessionCookie = "__Host-g1ops"
	loginCookie   = "__Host-g1ops-login"
)

func New(ctx context.Context, svc *ops.Service, log *slog.Logger, cfg Config) (*Server, error) {
	if len(cfg.SessionKey) < 32 {
		return nil, errors.New("opsweb: SessionKey must be at least 32 bytes")
	}
	base, err := url.Parse(strings.TrimRight(cfg.BaseURL, "/"))
	if err != nil || base.Host == "" {
		return nil, fmt.Errorf("opsweb: bad BaseURL %q", cfg.BaseURL)
	}
	provider, err := oidc.NewProvider(ctx, cfg.Issuer)
	if err != nil {
		return nil, fmt.Errorf("opsweb: identity provider: %w", err)
	}
	s := &Server{Ops: svc, Log: log, cfg: cfg, base: base,
		oidc: provider.Verifier(&oidc.Config{ClientID: cfg.ClientID}),
		oa: &oauth2.Config{ClientID: cfg.ClientID, ClientSecret: cfg.ClientSecret, Endpoint: provider.Endpoint(),
			RedirectURL: base.String() + "/ops/callback", Scopes: []string{oidc.ScopeOpenID, "email", "profile"}},
	}
	if err := s.parseTemplates(); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	static, _ := fs.Sub(webFS, "web/static")
	mux.Handle("GET /ops/static/", http.StripPrefix("/ops/static/", http.FileServerFS(static)))
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) })
	mux.HandleFunc("GET /ops/login", s.login)
	mux.HandleFunc("GET /ops/callback", s.callback)
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, "/ops/", http.StatusFound) })

	mux.Handle("POST /ops/logout", s.authed(s.logout))
	mux.Handle("GET /ops/{$}", s.authed(s.dashboard))
	mux.Handle("GET /ops/merchants", s.authed(s.merchantList))
	mux.Handle("GET /ops/merchants/{id}", s.authed(s.merchantPage))
	mux.Handle("POST /ops/merchants/{id}/action", s.authed(s.merchantAction))
	mux.Handle("GET /ops/documents/{id}", s.authed(s.documentFile))
	mux.Handle("POST /ops/documents/{id}/review", s.authed(s.documentReview))
	mux.Handle("GET /ops/payouts", s.authed(s.payoutList))
	mux.Handle("POST /ops/payouts/{id}/release", s.authed(s.payoutRelease))
	mux.Handle("GET /ops/cases", s.authed(s.caseList))
	mux.Handle("GET /ops/cases/{id}", s.authed(s.casePage))
	mux.Handle("POST /ops/cases/{id}/take", s.authed(s.caseTake))
	mux.Handle("POST /ops/cases/{id}/note", s.authed(s.caseNote))
	mux.Handle("POST /ops/cases/{id}/action", s.authed(s.caseAction))
	mux.Handle("GET /ops/payments/{id}", s.authed(s.goToCase))
	mux.Handle("GET /ops/users/{id}", s.authed(s.goToCase))
	mux.Handle("GET /ops/staff", s.authed(s.staffPage))
	mux.Handle("POST /ops/staff", s.authed(s.staffInvite))
	mux.Handle("POST /ops/staff/{id}/action", s.authed(s.staffAction))
	mux.Handle("GET /ops/approvals", s.authed(s.approvalList))
	mux.Handle("POST /ops/approvals/{id}", s.authed(s.approvalDecide))
	mux.Handle("GET /ops/audit", s.authed(s.auditPage))
	return securityHeaders(mux)
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", "default-src 'none'; style-src 'self'; img-src 'self'; form-action 'self'; frame-ancestors 'none'; base-uri 'none'")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Cache-Control", "no-store")
		h.Set("Cross-Origin-Opener-Policy", "same-origin")
		next.ServeHTTP(w, r)
	})
}

func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return ""
	}
	return host
}

func actor(r *http.Request, st *ops.Staff) ops.Actor {
	return ops.Actor{Staff: st, IP: clientIP(r), Device: r.UserAgent()}
}

// ---------------------------------------------------------------------------
// Sign-in
// ---------------------------------------------------------------------------

type loginState struct {
	State    string    `json:"s"`
	Nonce    string    `json:"n"`
	Verifier string    `json:"v"`
	Expires  time.Time `json:"e"`
}

func (s *Server) sign(b []byte) string {
	m := hmac.New(sha256.New, s.cfg.SessionKey)
	m.Write(b)
	return base64.RawURLEncoding.EncodeToString(m.Sum(nil))
}

func setCookie(w http.ResponseWriter, name, value string, maxAge int) {
	http.SetCookie(w, &http.Cookie{Name: name, Value: value, Path: "/", MaxAge: maxAge, HttpOnly: true, Secure: true,
		SameSite: http.SameSiteStrictMode})
}

func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	st := loginState{State: oauth2.GenerateVerifier(), Nonce: oauth2.GenerateVerifier(), Verifier: oauth2.GenerateVerifier(),
		Expires: time.Now().Add(10 * time.Minute)}
	b, _ := json.Marshal(st)
	enc := base64.RawURLEncoding.EncodeToString(b)
	// SameSite=Lax would be needed for the redirect back from the identity provider to carry it; Strict
	// drops it on that cross-site navigation, so the sign-in cookie alone is Lax. It holds no authority.
	http.SetCookie(w, &http.Cookie{Name: loginCookie, Value: enc + "." + s.sign([]byte(enc)), Path: "/", MaxAge: 600,
		HttpOnly: true, Secure: true, SameSite: http.SameSiteLaxMode})
	http.Redirect(w, r, s.oa.AuthCodeURL(st.State, oidc.Nonce(st.Nonce), oauth2.S256ChallengeOption(st.Verifier)), http.StatusFound)
}

func (s *Server) readLogin(r *http.Request) (*loginState, error) {
	c, err := r.Cookie(loginCookie)
	if err != nil {
		return nil, err
	}
	enc, sig, ok := strings.Cut(c.Value, ".")
	if !ok || !hmac.Equal([]byte(sig), []byte(s.sign([]byte(enc)))) {
		return nil, errors.New("bad sign-in cookie")
	}
	raw, err := base64.RawURLEncoding.DecodeString(enc)
	if err != nil {
		return nil, err
	}
	var st loginState
	if err := json.Unmarshal(raw, &st); err != nil {
		return nil, err
	}
	if time.Now().After(st.Expires) {
		return nil, errors.New("sign-in took too long")
	}
	return &st, nil
}

func (s *Server) callback(w http.ResponseWriter, r *http.Request) {
	fail := func(reason string, detail map[string]any) {
		if detail == nil {
			detail = map[string]any{}
		}
		detail["reason"] = reason
		_ = s.Ops.AuditSystem(r.Context(), ops.Actor{IP: clientIP(r), Device: r.UserAgent()}, "staff.sign_in", "denied", detail)
		setCookie(w, loginCookie, "", -1)
		s.render(w, r, http.StatusForbidden, "signin_failed", nil, map[string]any{"Reason": reason})
	}
	st, err := s.readLogin(r)
	if err != nil || r.URL.Query().Get("state") != st.State {
		fail("The sign-in link expired or was opened in another browser. Start again.", nil)
		return
	}
	setCookie(w, loginCookie, "", -1)
	if e := r.URL.Query().Get("error"); e != "" {
		fail("The identity provider refused the sign-in ("+e+").", nil)
		return
	}
	tok, err := s.oa.Exchange(r.Context(), r.URL.Query().Get("code"), oauth2.VerifierOption(st.Verifier))
	if err != nil {
		fail("The identity provider didn't confirm the sign-in.", nil)
		return
	}
	raw, _ := tok.Extra("id_token").(string)
	idt, err := s.oidc.Verify(r.Context(), raw)
	if err != nil || idt.Nonce != st.Nonce {
		fail("The identity token didn't check out.", nil)
		return
	}
	var claims struct {
		Email         string   `json:"email"`
		EmailVerified bool     `json:"email_verified"`
		AMR           []string `json:"amr"`
	}
	if err := idt.Claims(&claims); err != nil || claims.Email == "" || !claims.EmailVerified {
		fail("The identity provider didn't send a verified email address.", nil)
		return
	}
	if len(s.cfg.RequiredAMR) > 0 && !slices.ContainsFunc(claims.AMR, func(m string) bool { return slices.Contains(s.cfg.RequiredAMR, m) }) {
		fail("Sign in with your hardware security key.", map[string]any{"amr": claims.AMR})
		return
	}
	subject := idt.Issuer + "|" + idt.Subject
	id, err := s.Ops.StaffForLogin(r.Context(), subject, claims.Email)
	if err != nil {
		fail("This account isn't set up for the operations console. Ask a super admin.", map[string]any{"email": claims.Email})
		return
	}
	staff, err := s.Ops.LoadStaff(r.Context(), id)
	if err != nil {
		fail("This staff account is suspended or has no role.", map[string]any{"email": claims.Email})
		return
	}
	token, err := s.Ops.StartSession(r.Context(), id, clientIP(r), r.UserAgent())
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	_ = s.Ops.AuditSystem(r.Context(), actor(r, staff), "staff.sign_in", "allowed", map[string]any{"amr": claims.AMR})
	setCookie(w, sessionCookie, token, int(ops.SessionAbsolute.Seconds()))
	// A SameSite=Strict cookie isn't sent on a redirect that the identity provider's site started, so this
	// page moves on to the console from our own origin.
	s.render(w, r, http.StatusOK, "signed_in", nil, map[string]any{"Name": staff.Name})
}

type sessionKey struct{}

// authed requires a live session; POSTs also need the session's CSRF token and a same-origin request.
func (s *Server) authed(h func(http.ResponseWriter, *http.Request, *ops.Session)) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var token string
		if c, err := r.Cookie(sessionCookie); err == nil {
			token = c.Value
		}
		sess, err := s.Ops.SessionFrom(r.Context(), token)
		if err != nil {
			if !errors.Is(err, ops.ErrForbidden) {
				s.serverError(w, r, err)
				return
			}
			setCookie(w, sessionCookie, "", -1)
			if r.Method == http.MethodGet {
				s.render(w, r, http.StatusUnauthorized, "signed_out", nil, nil)
			} else {
				http.Error(w, "Your session ended. Sign in again.", http.StatusUnauthorized)
			}
			return
		}
		if r.Method == http.MethodPost {
			if origin := r.Header.Get("Origin"); origin != "" && origin != s.base.Scheme+"://"+s.base.Host {
				http.Error(w, "Cross-site request refused.", http.StatusForbidden)
				return
			}
			r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
			if subtle.ConstantTimeCompare([]byte(r.PostFormValue("csrf")), []byte(sess.CSRF)) != 1 {
				http.Error(w, "This form expired. Go back, reload the page and try again.", http.StatusForbidden)
				return
			}
		}
		h(w, r, sess)
	})
}

func (s *Server) logout(w http.ResponseWriter, r *http.Request, sess *ops.Session) {
	if c, err := r.Cookie(sessionCookie); err == nil {
		_ = s.Ops.EndSession(r.Context(), c.Value)
	}
	_ = s.Ops.AuditSystem(r.Context(), actor(r, sess.Staff), "staff.sign_out", "allowed", map[string]any{})
	setCookie(w, sessionCookie, "", -1)
	s.render(w, r, http.StatusOK, "signed_out", nil, map[string]any{"Bye": true})
}

// ---------------------------------------------------------------------------
// Pages
// ---------------------------------------------------------------------------

// Messages shown after a form, by code (codes only in URLs, never free text).
var messages = map[string]string{
	"applied":      "Done.",
	"requested":    "Sent for approval. Someone else with this permission has to approve it before it happens.",
	"approved":     "Approved and applied.",
	"rejected":     "Request rejected. Nothing changed.",
	"forbidden":    "Your role doesn't allow that. The attempt was recorded.",
	"own":          "You can't approve your own request. Someone else has to.",
	"note":         "Write a note of at least 10 characters saying why.",
	"duplicate":    "The same request is already waiting for approval.",
	"decided":      "That request was already decided or has lapsed.",
	"notfound":     "Not found.",
	"conflict":     "That can't be done in the current state. Reload the page.",
	"unavailable":  "Something went wrong. Nothing was changed. Try again.",
	"taken":        "The case is yours.",
	"noted":        "Note added.",
	"note_empty":   "Write something in the note first.",
	"self":         "Nobody can change their own access. Ask another super admin.",
	"doc_accepted": "Document accepted.",
	"doc_rejected": "Document rejected. The owner was told why and can send a new one.",
	"doc_decided":  "That document was already decided or replaced.",
	"doc_note":     "Say why the document can't be accepted (at least 10 characters).",
}

func (s *Server) back(w http.ResponseWriter, r *http.Request, path, code string) {
	http.Redirect(w, r, path+"?msg="+code, http.StatusSeeOther)
}

// outcomeCode turns an action's result into a message code. Validation messages from the services are
// shown inline instead (see conflictText).
func outcomeCode(out ops.Outcome, err error) string {
	switch {
	case err == nil && out.Applied:
		return "applied"
	case err == nil:
		return "requested"
	case errors.Is(err, ops.ErrForbidden):
		return "forbidden"
	case errors.Is(err, ops.ErrOwnRequest):
		return "own"
	case errors.Is(err, ops.ErrSelf):
		return "self"
	case errors.Is(err, ops.ErrNoteMissing):
		return "note"
	case errors.Is(err, ops.ErrDuplicate):
		return "duplicate"
	case errors.Is(err, ops.ErrDecided):
		return "decided"
	case errors.Is(err, ops.ErrNotFound), errors.Is(err, merchants.ErrNotFound), errors.Is(err, payouts.ErrNotFound):
		return "notfound"
	}
	var c *merchants.ConflictError
	var v *payouts.ValidationError
	if errors.As(err, &c) || errors.As(err, &v) {
		return "conflict"
	}
	return "unavailable"
}

type page struct {
	Session *ops.Session
	Counts  ops.Counts
	Message string
	Data    any
	Nav     string
}

func (s *Server) render(w http.ResponseWriter, r *http.Request, status int, name string, sess *ops.Session, data any) {
	p := page{Session: sess, Data: data, Nav: name, Message: messages[r.URL.Query().Get("msg")]}
	if sess != nil {
		c, err := s.Ops.Counts(r.Context(), sess.Staff.ID)
		if err == nil {
			p.Counts = c
		}
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	if err := s.tpl[name].ExecuteTemplate(w, "layout", p); err != nil && s.Log != nil {
		s.Log.Error("render", "page", name, "err", err)
	}
}

func (s *Server) serverError(w http.ResponseWriter, r *http.Request, err error) {
	if s.Log != nil {
		s.Log.Error("ops console", "path", r.URL.Path, "err", err)
	}
	http.Error(w, "Something went wrong. Nothing was changed.", http.StatusInternalServerError)
}

func (s *Server) dashboard(w http.ResponseWriter, r *http.Request, sess *ops.Session) {
	s.render(w, r, http.StatusOK, "dashboard", sess, nil)
}

var listStatuses = map[string][]string{
	"review":    {"applied", "kyb_review"},
	"active":    {"active"},
	"suspended": {"suspended"},
	"closed":    {"closed"},
}

func (s *Server) merchantList(w http.ResponseWriter, r *http.Request, sess *ops.Session) {
	which := r.URL.Query().Get("status")
	if listStatuses[which] == nil {
		which = "review"
	}
	list, err := s.Ops.MerchantList(r.Context(), listStatuses[which], 200)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	s.render(w, r, http.StatusOK, "merchants", sess, map[string]any{"Which": which, "List": list})
}

func (s *Server) merchantPage(w http.ResponseWriter, r *http.Request, sess *ops.Session) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	m, err := s.Ops.Merchant(r.Context(), id)
	if errors.Is(err, ops.ErrNotFound) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	pending, err := s.Ops.Approvals(r.Context(), true, id, 20)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	pays, err := s.Ops.PayoutList(r.Context(), "", id, 20)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	audit, err := s.Ops.Audit(r.Context(), id, 50)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	data := map[string]any{"M": m, "Pending": pending, "Payouts": pays, "Audit": audit, "Docs": true}
	if s.Ops.KYB == nil {
		data["Docs"] = false
	} else {
		docs, err := s.Ops.KYB.ForMerchant(r.Context(), id)
		if err != nil {
			s.serverError(w, r, err)
			return
		}
		missing, err := kyb.MissingTx(r.Context(), s.Ops.Pool, id)
		if err != nil {
			s.serverError(w, r, err)
			return
		}
		data["Documents"], data["Missing"] = docs, missing
	}
	s.render(w, r, http.StatusOK, "merchant", sess, data)
}

func (s *Server) merchantAction(w http.ResponseWriter, r *http.Request, sess *ops.Session) {
	id, err := uuid.Parse(r.PathValue("id"))
	kind := r.PostFormValue("kind")
	if err != nil || !strings.HasPrefix(kind, "merchant.") || ops.Permission(kind) == "" {
		http.NotFound(w, r)
		return
	}
	out, err := s.Ops.Do(r.Context(), actor(r, sess.Staff), ops.Action{Kind: kind, Target: id, Note: r.PostFormValue("note")})
	s.logUnexpected(r, err)
	s.back(w, r, "/ops/merchants/"+id.String(), outcomeCode(out, err))
}

// documentFile shows a document to staff. It's served from our own origin, sandboxed, and never cached.
func (s *Server) documentFile(w http.ResponseWriter, r *http.Request, sess *ops.Session) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil || s.Ops.KYB == nil {
		http.NotFound(w, r)
		return
	}
	d, data, err := s.Ops.OpenDocument(r.Context(), actor(r, sess.Staff), id)
	switch {
	case errors.Is(err, ops.ErrForbidden):
		s.render(w, r, http.StatusForbidden, "forbidden", sess, nil)
		return
	case errors.Is(err, kyb.ErrNotFound):
		http.NotFound(w, r)
		return
	case err != nil:
		s.serverError(w, r, err)
		return
	}
	h := w.Header()
	h.Set("Content-Type", d.ContentType)
	h.Set("Content-Security-Policy", "default-src 'none'; img-src 'self'; style-src 'unsafe-inline'; sandbox")
	disposition := "inline"
	if d.ContentType == "application/pdf" {
		disposition = "attachment" // opened in the staff member's own PDF reader, not in the console's origin
	}
	ext := map[string]string{"image/jpeg": "jpg", "image/png": "png", "application/pdf": "pdf"}[d.ContentType]
	h.Set("Content-Disposition", fmt.Sprintf(`%s; filename="%s-%s.%s"`, disposition, d.Kind, d.ID.String()[:8], ext))
	_, _ = w.Write(data)
}

func (s *Server) documentReview(w http.ResponseWriter, r *http.Request, sess *ops.Session) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil || s.Ops.KYB == nil {
		http.NotFound(w, r)
		return
	}
	accept := r.PostFormValue("decision") == "accept"
	d, err := s.Ops.ReviewDocument(r.Context(), actor(r, sess.Staff), id, accept, r.PostFormValue("note"))
	var v *kyb.ValidationError
	switch {
	case err == nil:
		s.back(w, r, "/ops/merchants/"+d.MerchantID.String(), map[bool]string{true: "doc_accepted", false: "doc_rejected"}[accept])
	case errors.As(err, &v):
		var merchant uuid.UUID
		_ = s.Ops.Pool.QueryRow(r.Context(), `SELECT merchant_id FROM merchant.documents WHERE id = $1`, id).Scan(&merchant)
		code := "doc_decided"
		if !accept && strings.Contains(v.Msg, "why") {
			code = "doc_note"
		}
		s.back(w, r, "/ops/merchants/"+merchant.String(), code)
	case errors.Is(err, ops.ErrForbidden):
		s.render(w, r, http.StatusForbidden, "forbidden", sess, nil)
	case errors.Is(err, kyb.ErrNotFound):
		http.NotFound(w, r)
	default:
		s.serverError(w, r, err)
	}
}

func (s *Server) payoutList(w http.ResponseWriter, r *http.Request, sess *ops.Session) {
	list, err := s.Ops.PayoutList(r.Context(), "on_hold", uuid.Nil, 200)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	s.render(w, r, http.StatusOK, "payouts", sess, map[string]any{"List": list})
}

func (s *Server) payoutRelease(w http.ResponseWriter, r *http.Request, sess *ops.Session) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	out, err := s.Ops.Do(r.Context(), actor(r, sess.Staff), ops.Action{Kind: "payout.release", Target: id, Note: r.PostFormValue("note")})
	s.logUnexpected(r, err)
	s.back(w, r, "/ops/payouts", outcomeCode(out, err))
}

func (s *Server) approvalList(w http.ResponseWriter, r *http.Request, sess *ops.Session) {
	pending, err := s.Ops.Approvals(r.Context(), true, uuid.Nil, 100)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	done, err := s.Ops.Approvals(r.Context(), false, uuid.Nil, 30)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	s.render(w, r, http.StatusOK, "approvals", sess, map[string]any{"Pending": pending, "Done": done})
}

func (s *Server) approvalDecide(w http.ResponseWriter, r *http.Request, sess *ops.Session) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	approve := r.PostFormValue("decision") == "approve"
	err = s.Ops.Decide(r.Context(), actor(r, sess.Staff), id, approve, r.PostFormValue("note"))
	s.logUnexpected(r, err)
	code := outcomeCode(ops.Outcome{Applied: true}, err)
	if err == nil {
		code = map[bool]string{true: "approved", false: "rejected"}[approve]
	}
	s.back(w, r, "/ops/approvals", code)
}

func (s *Server) auditPage(w http.ResponseWriter, r *http.Request, sess *ops.Session) {
	if !sess.Staff.Can("audit.view", ops.View) {
		s.render(w, r, http.StatusForbidden, "forbidden", sess, nil)
		return
	}
	rows, err := s.Ops.Audit(r.Context(), uuid.Nil, 200)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	n, broken, err := s.Ops.VerifyAudit(r.Context())
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	s.render(w, r, http.StatusOK, "audit", sess, map[string]any{"Rows": rows, "Checked": n, "Broken": broken})
}

func (s *Server) logUnexpected(r *http.Request, err error) {
	if err != nil && outcomeCode(ops.Outcome{}, err) == "unavailable" && s.Log != nil {
		s.Log.Error("ops action", "path", r.URL.Path, "err", err)
	}
}

// ---------------------------------------------------------------------------
// Templates
// ---------------------------------------------------------------------------

var eat = time.FixedZone("EAT", 3*60*60)

var labels = map[string]string{
	"merchant.approve": "Approve application", "merchant.reject": "Reject application",
	"merchant.suspend": "Suspend merchant", "merchant.reinstate": "Reinstate merchant", "payout.release": "Release payout",
	"no_destination": "No payout account", "merchant_suspended": "Merchant suspended",
	"rejected_by_partner": "Rejected by the partner", "too_many_failures": "Kept failing",
	"applied": "Applied", "kyb_review": "In review", "active": "Active", "suspended": "Suspended", "closed": "Closed",
	"scheduled": "Scheduled", "sent": "Sent", "settled": "Paid", "on_hold": "On hold",
	"allowed": "Allowed", "denied": "Denied", "pending": "Awaiting approval", "failed": "Didn't go through",
	"approved": "Approved", "rejected": "Rejected", "lapsed": "Lapsed",
	"staff.sign_in": "Signed in", "staff.sign_out": "Signed out", "staff.invite": "Invited staff",
	"approval.approve": "Approved a request", "approval.reject": "Rejected a request",
	"document.view": "Opened a document", "document.accept": "Accepted a document", "document.reject": "Rejected a document",
	"submitted": "To check", "accepted": "Accepted", "replaced": "Replaced",
	"payment.release": "Release held payment", "payment.decline": "Decline held payment",
	"user.freeze": "Freeze account", "user.unfreeze": "Unfreeze account",
	"case.escalate": "Escalate to AML compliance", "case.resolve": "Resolve case", "case.take": "Took the case",
	"case.note":   "Added a note",
	"staff.roles": "Change roles", "staff.suspend": "Suspend staff member", "staff.reinstate": "Reinstate staff member",
	"unassigned": "New", "in_review": "In review", "awaiting_approval": "Awaiting approval", "resolved": "Resolved",
	"escalated_aml": "With AML compliance", "held_for_review": "Held", "succeeded": "Went through",
	"frozen": "Frozen", "restricted": "Restricted", "pending_user": "Pending",
	"p2p": "Send money", "qr_merchant": "QR payment", "topup": "Top-up",
	"insufficient_funds": "not enough money", "declined_after_review": "declined after review",
	"limit_exceeded": "over a limit",
	"super_admin":    "Super admin", "finance_officer": "Finance officer", "risk_analyst": "Risk analyst",
	"support_agent": "Support agent", "merchant_ops": "Merchant operations", "content_editor": "Content editor",
	"left": "Left",
}

func (s *Server) parseTemplates() error {
	funcs := template.FuncMap{
		"when": func(t any) string {
			switch v := t.(type) {
			case time.Time:
				return v.In(eat).Format("2 Jan 2006 15:04")
			case *time.Time:
				if v == nil {
					return "—"
				}
				return v.In(eat).Format("2 Jan 2006 15:04")
			}
			return ""
		},
		"etb":      func(m money.Money) string { return m.Display() },
		"label":    func(k string) string { return or(labels[k], k) },
		"category": func(c string) string { return or(merchants.Categories[c], c) },
		"dockind":  func(k string) string { return or(kyb.Kinds[k], k) },
		"size": func(n int) string {
			if n >= 1<<20 {
				return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
			}
			return fmt.Sprintf("%d KB", (n+1023)/1024)
		},
		"can": func(st *ops.Staff, kind string, at string) bool {
			return st.Can(ops.Permission(kind), map[string]ops.Grant{"view": ops.View, "approve": ops.Approve, "full": ops.Full}[at])
		},
		"needsSecond": func(st *ops.Staff, kind string) bool { return st.Grant(ops.Permission(kind)) == ops.Approve },
		"short":       func(id uuid.UUID) string { return id.String()[:8] },
		"pill": func(k string) string {
			switch k {
			case "active", "allowed", "approved", "settled", "accepted", "succeeded", "resolved":
				return "ok"
			case "denied", "suspended", "rejected", "closed", "frozen", "escalated_aml":
				return "bad"
			case "on_hold", "pending", "kyb_review", "applied", "lapsed", "submitted", "failed", "held_for_review", "unassigned", "awaiting_approval":
				return "warn"
			}
			return "info"
		},
		"list": func(v ...string) []string { return v },
		"has":  func(list []string, v string) bool { return slices.Contains(list, v) },
		"roleNames": func(csv string) string {
			var out []string
			for _, r := range strings.Split(csv, ",") {
				out = append(out, or(labels[r], r))
			}
			return strings.Join(out, ", ")
		},
		"rules": func(rs []string) string {
			out := make([]string, 0, len(rs))
			for _, r := range rs {
				out = append(out, or(payments.RuleText[r], r))
			}
			return strings.Join(out, "; ")
		},
		"targetLink": func(t, id string) string {
			switch t {
			case "merchant":
				return "/ops/merchants/" + id
			case "payout":
				return "/ops/payouts"
			case "case":
				return "/ops/cases/" + id
			case "payment":
				return "/ops/payments/" + id
			case "user":
				return "/ops/users/" + id
			}
			return "/ops/staff"
		},
	}
	s.tpl = map[string]*template.Template{}
	pages, err := fs.Glob(webFS, "web/pages/*.html")
	if err != nil {
		return err
	}
	for _, p := range pages {
		name := strings.TrimSuffix(strings.TrimPrefix(p, "web/pages/"), ".html")
		t, err := template.New(name).Funcs(funcs).ParseFS(webFS, "web/layout.html", p)
		if err != nil {
			return fmt.Errorf("opsweb: %s: %w", name, err)
		}
		s.tpl[name] = t
	}
	return nil
}

func or(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
