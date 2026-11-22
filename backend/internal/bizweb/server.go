// Package bizweb is the web business console: the shop's orders, products, storefront and money on a
// computer. People sign in without typing a password or PIN into the browser: the page shows a
// one-time code, and a member of the shop approves it in the app with their PIN. The session belongs to
// that person and that shop, and what it can do follows their role, checked by the same services the
// app's API uses. Pages carry no scripts, so a strict Content Security Policy applies, and every form is
// protected by a per-session CSRF token and a same-origin check.
package bizweb

import (
	"embed"
	"errors"
	"fmt"
	"html/template"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"superapp/backend/internal/commerce"
	"superapp/backend/internal/merchants"
	"superapp/backend/internal/money"
	"superapp/backend/internal/payouts"
)

//go:embed web
var webFS embed.FS

const (
	sessionCookie = "__Host-g1biz"
	loginCookie   = "__Host-g1biz-login"
	langCookie    = "g1biz-lang"
)

var eat = time.FixedZone("EAT", 3*60*60)

type Server struct {
	Store     *Store
	Merchants *merchants.Service
	Shops     *commerce.Service
	Payouts   *payouts.Service
	Log       *slog.Logger
	Now       func() time.Time

	base   *url.URL
	tpl    map[string]*template.Template
	limits *limiter
}

// New prepares the console. baseURL is where people reach it, e.g. https://business.gebeya.one.
func New(baseURL string, store *Store, m *merchants.Service, shops *commerce.Service, p *payouts.Service, log *slog.Logger) (*Server, error) {
	base, err := url.Parse(strings.TrimRight(baseURL, "/"))
	if err != nil || base.Host == "" {
		return nil, fmt.Errorf("bizweb: bad base URL %q", baseURL)
	}
	s := &Server{Store: store, Merchants: m, Shops: shops, Payouts: p, Log: log, base: base,
		limits: &limiter{max: 20, window: 10 * time.Minute, seen: map[string][]time.Time{}}}
	if err := s.parseTemplates(); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *Server) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	static, _ := fs.Sub(webFS, "web/static")
	mux.Handle("GET /business/static/", http.StripPrefix("/business/static/", http.FileServerFS(static)))
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) })
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, "/business/", http.StatusFound) })
	mux.HandleFunc("GET /business/login", s.login)
	mux.HandleFunc("GET /business/lang", s.setLang)

	mux.Handle("POST /business/logout", s.authed(s.logout))
	mux.Handle("GET /business/{$}", s.authed(s.home))
	mux.Handle("GET /business/orders", s.authed(s.orders))
	mux.Handle("GET /business/orders/{id}", s.authed(s.order))
	mux.Handle("POST /business/orders/{id}/{action}", s.authed(s.orderAction))
	mux.Handle("GET /business/products", s.authed(s.products))
	mux.Handle("GET /business/products/new", s.authed(s.productForm))
	mux.Handle("GET /business/products/{id}", s.authed(s.productForm))
	mux.Handle("POST /business/products", s.authed(s.productSave))
	mux.Handle("POST /business/products/{id}", s.authed(s.productSave))
	mux.Handle("GET /business/storefront", s.authed(s.storefront))
	mux.Handle("POST /business/storefront", s.authed(s.storefrontSave))
	mux.Handle("POST /business/storefront/open", s.authed(s.storefrontOpen))
	mux.Handle("GET /business/money", s.authed(s.money))
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

func setCookie(w http.ResponseWriter, name, value string, maxAge int) {
	http.SetCookie(w, &http.Cookie{Name: name, Value: value, Path: "/", MaxAge: maxAge, HttpOnly: true, Secure: true,
		SameSite: http.SameSiteStrictMode})
}

// limiter caps new sign-in codes per IP, so nobody can fill the table.
type limiter struct {
	mu     sync.Mutex
	max    int
	window time.Duration
	seen   map[string][]time.Time
}

func (l *limiter) allow(key string, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.seen) > 10_000 {
		l.seen = map[string][]time.Time{}
	}
	recent := l.seen[key][:0]
	for _, t := range l.seen[key] {
		if now.Sub(t) < l.window {
			recent = append(recent, t)
		}
	}
	if len(recent) >= l.max {
		l.seen[key] = recent
		return false
	}
	l.seen[key] = append(recent, now)
	return true
}

// ---------------------------------------------------------------------------
// Language
// ---------------------------------------------------------------------------

func validLang(l string) bool { return l == "am" || l == "en" || l == "om" || l == "ti" }

// lang is the language chosen in the console, else the person's language in the app, else English.
func lang(r *http.Request, sess *Session) string {
	if c, err := r.Cookie(langCookie); err == nil && validLang(c.Value) {
		return c.Value
	}
	if sess != nil && validLang(sess.Locale) {
		return sess.Locale
	}
	return "en"
}

func safeNext(next string) string {
	if !strings.HasPrefix(next, "/business/") || strings.HasPrefix(next, "//") || strings.ContainsAny(next, "\\\r\n") {
		return "/business/"
	}
	return next
}

func (s *Server) setLang(w http.ResponseWriter, r *http.Request) {
	if l := r.URL.Query().Get("l"); validLang(l) {
		http.SetCookie(w, &http.Cookie{Name: langCookie, Value: l, Path: "/", MaxAge: 365 * 24 * 3600, Secure: true,
			HttpOnly: true, SameSite: http.SameSiteLaxMode})
	}
	http.Redirect(w, r, safeNext(r.URL.Query().Get("next")), http.StatusSeeOther)
}

// ---------------------------------------------------------------------------
// Sign-in
// ---------------------------------------------------------------------------

func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if c, err := r.Cookie(sessionCookie); err == nil {
		if _, err := s.Store.SessionFrom(ctx, c.Value); err == nil {
			http.Redirect(w, r, "/business/", http.StatusSeeOther)
			return
		}
	}
	var secret, code string
	if c, err := r.Cookie(loginCookie); err == nil && r.URL.Query().Get("new") == "" {
		secret, code, _ = strings.Cut(c.Value, ".")
	}
	state, token, err := s.Store.PollLogin(ctx, secret, code, clientIP(r), r.UserAgent())
	if err != nil {
		s.serverError(w, r, nil, err)
		return
	}
	switch state {
	case LoginApproved:
		setCookie(w, loginCookie, "", -1)
		setCookie(w, sessionCookie, token, int(SessionAbsolute.Seconds()))
		http.Redirect(w, r, "/business/", http.StatusSeeOther)
		return
	case LoginGone:
		if !s.limits.allow(clientIP(r), s.now()) {
			http.Error(w, "Too many sign-in codes from this network. Wait 10 minutes and try again.", http.StatusTooManyRequests)
			return
		}
		l, err := s.Store.StartLogin(ctx, clientIP(r), r.UserAgent())
		if err != nil {
			s.serverError(w, r, nil, err)
			return
		}
		secret, code = l.Secret, l.Code
		setCookie(w, loginCookie, secret+"."+code, int(LoginTTL.Seconds())+120)
	}
	s.renderPage(w, r, http.StatusOK, "login", nil, page{Refresh: 3, Data: map[string]any{"Code": FormatCode(code)}})
}

type ctxKey struct{}

// authed requires a live session; POSTs also need the session's CSRF token and a same-origin request.
func (s *Server) authed(h func(http.ResponseWriter, *http.Request, *Session)) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var token string
		if c, err := r.Cookie(sessionCookie); err == nil {
			token = c.Value
		}
		sess, err := s.Store.SessionFrom(r.Context(), token)
		if err != nil {
			if !errors.Is(err, ErrSignedOut) {
				s.serverError(w, r, nil, err)
				return
			}
			setCookie(w, sessionCookie, "", -1)
			if r.Method == http.MethodGet {
				s.render(w, r, http.StatusUnauthorized, "signed_out", nil, nil)
			} else {
				http.Error(w, translate(lang(r, nil), "signed_out")+" "+translate(lang(r, nil), "signed_out_body"), http.StatusUnauthorized)
			}
			return
		}
		if r.Method == http.MethodPost {
			if origin := r.Header.Get("Origin"); origin != "" && origin != s.base.Scheme+"://"+s.base.Host {
				http.Error(w, "Cross-site request refused.", http.StatusForbidden)
				return
			}
			r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
			if !tokensEqual(r.PostFormValue("csrf"), sess.CSRF) {
				http.Error(w, translate(lang(r, sess), "m_expired_form"), http.StatusForbidden)
				return
			}
		}
		h(w, r, sess)
	})
}

func (s *Server) logout(w http.ResponseWriter, r *http.Request, sess *Session) {
	if c, err := r.Cookie(sessionCookie); err == nil {
		_ = s.Store.EndSession(r.Context(), c.Value)
	}
	setCookie(w, sessionCookie, "", -1)
	s.render(w, r, http.StatusOK, "signed_out", nil, map[string]any{"Bye": true})
}

// ---------------------------------------------------------------------------
// Rendering
// ---------------------------------------------------------------------------

type page struct {
	S       *Session
	Lang    string
	Langs   any
	Nav     string
	Path    string
	Message string
	Error   string
	Live    int
	Refresh int // seconds; 0: none
	Data    any
}

// T translates key; args are name, value pairs ("n", 3).
func (p page) T(key string, args ...any) string {
	ss := make([]string, len(args))
	for i, a := range args {
		ss[i] = fmt.Sprint(a)
	}
	return translate(p.Lang, key, ss...)
}

func (s *Server) render(w http.ResponseWriter, r *http.Request, status int, name string, sess *Session, data any) {
	s.renderPage(w, r, status, name, sess, page{Data: data})
}

func (s *Server) renderPage(w http.ResponseWriter, r *http.Request, status int, name string, sess *Session, p page) {
	p.S, p.Lang, p.Langs, p.Path = sess, lang(r, sess), languages, r.URL.RequestURI()
	if p.Nav == "" {
		p.Nav = name
	}
	if code := r.URL.Query().Get("msg"); code != "" && p.Message == "" {
		if _, ok := texts["en"]["m_"+code]; ok {
			p.Message = translate(p.Lang, "m_"+code)
		}
	}
	if sess != nil {
		if live, err := s.Shops.ShopOrders(r.Context(), sess.UserID, sess.MerchantID, true, 100); err == nil {
			p.Live = len(live)
		}
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	if err := s.tpl[name].ExecuteTemplate(w, "layout", p); err != nil && s.Log != nil {
		s.Log.Error("bizweb render", "page", name, "err", err)
	}
}

func (s *Server) serverError(w http.ResponseWriter, r *http.Request, sess *Session, err error) {
	if s.Log != nil {
		s.Log.Error("business console", "path", r.URL.Path, "err", err)
	}
	http.Error(w, translate(lang(r, sess), "m_error"), http.StatusInternalServerError)
}

// failed shows a refusal from a service as a page message, or reports it if it's a fault.
func (s *Server) code(err error) string {
	var v *commerce.ValidationError
	var c *commerce.ConflictError
	switch {
	case err == nil:
		return ""
	case errors.Is(err, commerce.ErrForbidden), errors.Is(err, merchants.ErrForbidden):
		return "forbidden"
	case errors.Is(err, commerce.ErrNotFound), errors.Is(err, merchants.ErrNotFound):
		return "notfound"
	case errors.Is(err, commerce.ErrWrongCode):
		return "wrong_code"
	case errors.Is(err, commerce.ErrHandoverLocked):
		return "locked"
	case errors.As(err, &v):
		return "invalid"
	case errors.As(err, &c):
		return "conflict"
	}
	return ""
}

func (s *Server) parseTemplates() error {
	funcs := template.FuncMap{
		"etb": func(m money.Money) string { return m.Display() },
		"when": func(t any) string {
			var v time.Time
			switch x := t.(type) {
			case time.Time:
				v = x
			case *time.Time:
				if x == nil {
					return "—"
				}
				v = *x
			default:
				return ""
			}
			v = v.In(eat)
			now := s.now().In(eat)
			if v.Year() == now.Year() && v.YearDay() == now.YearDay() {
				return v.Format("15:04")
			}
			return v.Format("2 Jan 15:04")
		},
		"day": func(t time.Time) string { return t.In(eat).Format("2 Jan 2006") },
		"clock": func(t *time.Time) string {
			if t == nil {
				return ""
			}
			return t.In(eat).Format("15:04")
		},
		"pill": func(k string) string {
			switch k {
			case "delivered", "settled", "active", "sent":
				return "ok"
			case "cancelled", "archived", "sold_out":
				return "bad"
			case "placed", "on_hold", "hidden", "low":
				return "warn"
			}
			return "info"
		},
		"itemCount": func(o commerce.Order) int {
			n := 0
			for _, i := range o.Items {
				n += i.Quantity
			}
			return n
		},
		"low":  func(p commerce.Product) bool { return p.Stock != nil && *p.Stock <= 5 && p.Status == "active" },
		"has":  func(list []string, v string) bool { return slices.Contains(list, v) },
		"list": func(v ...string) []string { return v },
		"dict": func(kv ...any) map[string]any {
			m := map[string]any{}
			for i := 0; i+1 < len(kv); i += 2 {
				m[fmt.Sprint(kv[i])] = kv[i+1]
			}
			return m
		},
		"initial": func(name string) string {
			for _, r := range name {
				return strings.ToUpper(string(r))
			}
			return ""
		},
		"stock": func(p commerce.Product) string {
			if p.Stock == nil {
				return ""
			}
			return strconv.Itoa(*p.Stock)
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
			return fmt.Errorf("bizweb: %s: %w", name, err)
		}
		s.tpl[name] = t
	}
	return nil
}

func parseID(r *http.Request) (uuid.UUID, bool) {
	id, err := uuid.Parse(r.PathValue("id"))
	return id, err == nil
}
