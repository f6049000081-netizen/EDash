package opsweb_test

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"superapp/backend/internal/ledger"
	"superapp/backend/internal/merchants"
	"superapp/backend/internal/ops"
	"superapp/backend/internal/opsweb"
	"superapp/backend/internal/payouts"
	"superapp/backend/internal/platform"
)

// fakeIdP is a stand-in for the company identity provider, for tests only: discovery, keys, and a token
// endpoint that checks the PKCE verifier and returns an RS256 ID token.
type fakeIdP struct {
	srv   *httptest.Server
	key   *rsa.PrivateKey
	mu    sync.Mutex
	codes map[string]grant
}

type grant struct {
	nonce, challenge string
	claims           map[string]any
}

func newIdP(t *testing.T) *fakeIdP {
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeIdP{key: k, codes: map[string]grant{}}
	mux := http.NewServeMux()
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	b64 := base64.RawURLEncoding.EncodeToString
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"issuer": f.srv.URL, "authorization_endpoint": f.srv.URL + "/auth", "token_endpoint": f.srv.URL + "/token",
			"jwks_uri": f.srv.URL + "/keys", "id_token_signing_alg_values_supported": []string{"RS256"},
			"response_types_supported": []string{"code"}, "subject_types_supported": []string{"public"},
		})
	})
	mux.HandleFunc("/keys", func(w http.ResponseWriter, _ *http.Request) {
		e := big.NewInt(int64(k.E)).Bytes()
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": []any{map[string]any{
			"kty": "RSA", "kid": "k1", "alg": "RS256", "use": "sig", "n": b64(k.N.Bytes()), "e": b64(e)}}})
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		f.mu.Lock()
		g, ok := f.codes[r.PostForm.Get("code")]
		delete(f.codes, r.PostForm.Get("code"))
		f.mu.Unlock()
		sum := sha256.Sum256([]byte(r.PostForm.Get("code_verifier")))
		if !ok || b64(sum[:]) != g.challenge {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":"invalid_grant"}`))
			return
		}
		claims := map[string]any{"iss": f.srv.URL, "aud": "g1ops", "nonce": g.nonce,
			"iat": time.Now().Unix(), "exp": time.Now().Add(5 * time.Minute).Unix()}
		for k, v := range g.claims {
			claims[k] = v
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "at", "token_type": "Bearer", "id_token": f.sign(claims)})
	})
	return f
}

func (f *fakeIdP) sign(claims map[string]any) string {
	h, _ := json.Marshal(map[string]any{"alg": "RS256", "kid": "k1", "typ": "JWT"})
	c, _ := json.Marshal(claims)
	in := base64.RawURLEncoding.EncodeToString(h) + "." + base64.RawURLEncoding.EncodeToString(c)
	sum := sha256.Sum256([]byte(in))
	sig, _ := rsa.SignPKCS1v15(rand.Reader, f.key, crypto.SHA256, sum[:])
	return in + "." + base64.RawURLEncoding.EncodeToString(sig)
}

// authorize plays the user signing in at the provider: it reads our redirect and returns the callback URL.
func (f *fakeIdP) authorize(t *testing.T, location string, claims map[string]any) (code, state string) {
	u, err := url.Parse(location)
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	if q.Get("code_challenge_method") != "S256" || q.Get("client_id") != "g1ops" {
		t.Fatalf("authorization request: %s", location)
	}
	code = uuid.NewString()
	f.mu.Lock()
	f.codes[code] = grant{nonce: q.Get("nonce"), challenge: q.Get("code_challenge"), claims: claims}
	f.mu.Unlock()
	return code, q.Get("state")
}

// browser keeps cookies by hand: httptest serves plain HTTP, and the console's cookies are Secure.
type browser struct {
	t       *testing.T
	base    string
	cookies map[string]string
	c       *http.Client
}

func (b *browser) do(method, path string, form url.Values, origin string) (*http.Response, string) {
	b.t.Helper()
	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	req, _ := http.NewRequest(method, b.base+path, body)
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	if origin != "" {
		req.Header.Set("Origin", origin)
	}
	for k, v := range b.cookies {
		req.AddCookie(&http.Cookie{Name: k, Value: v})
	}
	res, err := b.c.Do(req)
	if err != nil {
		b.t.Fatal(err)
	}
	defer res.Body.Close()
	for _, c := range res.Cookies() {
		if c.MaxAge < 0 {
			delete(b.cookies, c.Name)
		} else {
			if !c.Secure || !c.HttpOnly || !strings.HasPrefix(c.Name, "__Host-") {
				b.t.Errorf("cookie %s isn't locked down: %+v", c.Name, c)
			}
			b.cookies[c.Name] = c.Value
		}
	}
	text, _ := io.ReadAll(res.Body)
	return res, string(text)
}

var csrfRE = regexp.MustCompile(`name="csrf" value="([0-9a-f]{64})"`)

type env struct {
	t       *testing.T
	idp     *fakeIdP
	svc     *ops.Service
	console *httptest.Server
	logs    *bytes.Buffer
}

var (
	poolOnce sync.Once
	pool     *pgxpool.Pool
	poolErr  error
)

func setup(t *testing.T, amr ...string) *env {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}
	poolOnce.Do(func() { pool, poolErr = pgxpool.New(context.Background(), dsn) })
	if poolErr != nil {
		t.Fatal(poolErr)
	}
	e := &env{t: t, idp: newIdP(t), logs: &bytes.Buffer{}}
	e.svc = &ops.Service{Pool: pool, Merchants: &merchants.Service{Pool: pool},
		Payouts: payouts.NewService(pool, ledger.New(), &platform.PINVerifier{Pool: pool})}
	e.console = httptest.NewUnstartedServer(nil)
	key := make([]byte, 32)
	_, _ = rand.Read(key)
	web, err := opsweb.New(context.Background(), e.svc, slog.New(slog.NewTextHandler(e.logs, nil)), opsweb.Config{
		Issuer: e.idp.srv.URL, ClientID: "g1ops", ClientSecret: "secret", BaseURL: "http://" + e.console.Listener.Addr().String(),
		RequiredAMR: amr, SessionKey: key,
	})
	if err != nil {
		t.Fatal(err)
	}
	e.console.Config.Handler = web.Handler()
	e.console.Start()
	t.Cleanup(e.console.Close)
	return e
}

func (e *env) browser() *browser {
	return &browser{t: e.t, base: e.console.URL, cookies: map[string]string{},
		c: &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
}

func (e *env) invite(name string, roles ...string) (email string) {
	email = fmt.Sprintf("%s.%s@gebeya.test", strings.ToLower(name), uuid.NewString()[:8])
	if _, err := e.svc.Invite(context.Background(), email, name, roles, ops.Bootstrap); err != nil {
		e.t.Fatal(err)
	}
	return email
}

// signIn goes through the whole sign-in and returns the browser and the last page.
func (e *env) signIn(claims map[string]any) (*browser, *http.Response, string) {
	b := e.browser()
	res, _ := b.do("GET", "/ops/login", nil, "")
	if res.StatusCode != http.StatusFound {
		e.t.Fatalf("login: %d", res.StatusCode)
	}
	code, state := e.idp.authorize(e.t, res.Header.Get("Location"), claims)
	res, page := b.do("GET", "/ops/callback?code="+code+"&state="+url.QueryEscape(state), nil, "")
	return b, res, page
}

func claimsFor(email string) map[string]any {
	return map[string]any{"sub": uuid.NewString(), "email": email, "email_verified": true}
}

func TestSignInAndPages(t *testing.T) {
	e := setup(t)
	email := e.invite("Yonas", "merchant_ops")
	b, res, page := e.signIn(claimsFor(email))
	if res.StatusCode != http.StatusOK || !strings.Contains(page, "Signed in as Yonas") || !strings.Contains(page, `http-equiv="refresh"`) {
		t.Fatalf("callback: %d %s", res.StatusCode, page)
	}
	if !strings.Contains(res.Header.Get("Content-Security-Policy"), "default-src 'none'") {
		t.Error("no CSP")
	}

	// An application to decide.
	owner, shop := uuid.New(), uuid.New()
	ctx := context.Background()
	_, _ = pool.Exec(ctx, `INSERT INTO identity.users (id, phone_e164, display_name, status) VALUES ($1, $2, 'Tsion Alemu', 'active')`,
		owner, fmt.Sprintf("+2519%08d", binary.BigEndian.Uint64(owner[:8])%100_000_000))
	_, _ = pool.Exec(ctx, `INSERT INTO merchant.merchants (id, legal_name, trade_name, tin, category_code, city, status, applied_by)
		VALUES ($1, 'Selam PLC', 'Selam <b>Café</b>', $2, '5812', 'Adama', 'kyb_review', $3)`,
		shop, fmt.Sprintf("%010d", binary.BigEndian.Uint64(shop[:8])%10_000_000_000), owner)
	_, _ = pool.Exec(ctx, `INSERT INTO merchant.members (merchant_id, user_id, role) VALUES ($1, $2, 'owner')`, shop, owner)

	for _, p := range []string{"/ops/", "/ops/merchants?status=review", "/ops/merchants?status=active", "/ops/merchants/" + shop.String(),
		"/ops/payouts", "/ops/approvals"} {
		res, page := b.do("GET", p, nil, "")
		if res.StatusCode != http.StatusOK || !strings.Contains(page, "</html>") {
			t.Errorf("%s: %d", p, res.StatusCode)
		}
	}
	res, page = b.do("GET", "/ops/merchants/"+shop.String(), nil, "")
	if !strings.Contains(page, "Selam &lt;b&gt;Café&lt;/b&gt;") || strings.Contains(page, "<b>Café</b>") {
		t.Error("merchant name not escaped")
	}
	if strings.Contains(page, "+2519") {
		t.Error("full phone number on the page")
	}
	if res, _ := b.do("GET", "/ops/audit", nil, ""); res.StatusCode != http.StatusForbidden {
		t.Errorf("merchant ops opened the audit log: %d", res.StatusCode)
	}

	csrf := csrfRE.FindStringSubmatch(page)
	if csrf == nil {
		t.Fatal("no CSRF token on the page")
	}
	form := url.Values{"kind": {"merchant.approve"}, "note": {"TIN and licence checked with the registries"}}
	if res, _ := b.do("POST", "/ops/merchants/"+shop.String()+"/action", form, ""); res.StatusCode != http.StatusForbidden {
		t.Errorf("no CSRF token: %d", res.StatusCode)
	}
	form.Set("csrf", csrf[1])
	if res, _ := b.do("POST", "/ops/merchants/"+shop.String()+"/action", form, "https://evil.example"); res.StatusCode != http.StatusForbidden {
		t.Errorf("cross-site: %d", res.StatusCode)
	}
	res, _ = b.do("POST", "/ops/merchants/"+shop.String()+"/action", form, e.console.URL)
	if res.StatusCode != http.StatusSeeOther || !strings.HasSuffix(res.Header.Get("Location"), "?msg=applied") {
		t.Fatalf("approve: %d %s", res.StatusCode, res.Header.Get("Location"))
	}
	var status string
	_ = pool.QueryRow(ctx, `SELECT status FROM merchant.merchants WHERE id = $1`, shop).Scan(&status)
	if status != "active" {
		t.Errorf("status %s", status)
	}
	_, page = b.do("GET", "/ops/merchants/"+shop.String()+"?msg=applied", nil, "")
	if !strings.Contains(page, "Done.") || !strings.Contains(page, "Approve application") {
		t.Error("result and history not shown")
	}

	// Signing out ends the session for good.
	res, _ = b.do("POST", "/ops/logout", url.Values{"csrf": {csrf[1]}}, "")
	if res.StatusCode != http.StatusOK {
		t.Errorf("logout: %d", res.StatusCode)
	}
	if res, _ := b.do("GET", "/ops/", nil, ""); res.StatusCode != http.StatusUnauthorized {
		t.Errorf("after logout: %d", res.StatusCode)
	}
	if strings.Contains(e.logs.String(), "level=ERROR") {
		t.Errorf("server errors: %s", e.logs.String())
	}
}

func TestRefusedSignIns(t *testing.T) {
	e := setup(t)
	if _, res, _ := e.signIn(claimsFor("nobody@gebeya.test")); res.StatusCode != http.StatusForbidden {
		t.Errorf("not staff: %d", res.StatusCode)
	}
	email := e.invite("Hana", "support_agent")
	c := claimsFor(email)
	c["email_verified"] = false
	if _, res, _ := e.signIn(c); res.StatusCode != http.StatusForbidden {
		t.Errorf("unverified email: %d", res.StatusCode)
	}
	// A forged state, or a code replayed without the browser's sign-in cookie, gets nowhere.
	b := e.browser()
	res, _ := b.do("GET", "/ops/login", nil, "")
	code, _ := e.idp.authorize(t, res.Header.Get("Location"), claimsFor(email))
	if res, _ := b.do("GET", "/ops/callback?code="+code+"&state=forged", nil, ""); res.StatusCode != http.StatusForbidden {
		t.Errorf("forged state: %d", res.StatusCode)
	}
	other := e.browser()
	res, _ = b.do("GET", "/ops/login", nil, "")
	code, state := e.idp.authorize(t, res.Header.Get("Location"), claimsFor(email))
	if res, _ := other.do("GET", "/ops/callback?code="+code+"&state="+url.QueryEscape(state), nil, ""); res.StatusCode != http.StatusForbidden {
		t.Errorf("callback in another browser: %d", res.StatusCode)
	}
	if res, _ := e.browser().do("GET", "/ops/", nil, ""); res.StatusCode != http.StatusUnauthorized {
		t.Errorf("no session: %d", res.StatusCode)
	}
}

func TestHardwareKeyRequired(t *testing.T) {
	e := setup(t, "hwk")
	email := e.invite("Tigist", "super_admin")
	c := claimsFor(email)
	c["amr"] = []string{"pwd"}
	if _, res, page := e.signIn(c); res.StatusCode != http.StatusForbidden || !strings.Contains(page, "hardware security key") {
		t.Errorf("password only: %d", res.StatusCode)
	}
	c["amr"] = []string{"pwd", "hwk"}
	b, res, _ := e.signIn(c)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("with a hardware key: %d", res.StatusCode)
	}
	if res, page := b.do("GET", "/ops/audit", nil, ""); res.StatusCode != http.StatusOK || !strings.Contains(page, "Hash chain intact") {
		t.Errorf("audit page: %d", res.StatusCode)
	}
}
