package bizweb

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"net/netip"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"superapp/backend/internal/platform"
)

const (
	LoginTTL        = 5 * time.Minute // how long a code shown in the browser can be approved
	SessionIdle     = 2 * time.Hour   // signed out after this long without a page load
	SessionAbsolute = 12 * time.Hour  // and after a working day at most
)

var (
	ErrNotFound  = errors.New("bizweb: not found")
	ErrForbidden = errors.New("bizweb: not allowed")
	ErrSignedOut = errors.New("bizweb: signed out")
)

// Store keeps browser sign-ins and sessions. The API server uses it to approve a sign-in from the app;
// the console uses it for everything else.
type Store struct {
	Pool *pgxpool.Pool
	PIN  *platform.PINVerifier
	Now  func() time.Time
}

func (s *Store) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func hash(v string) string {
	h := sha256.Sum256([]byte(v))
	return hex.EncodeToString(h[:])
}

func randomToken() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic(err) // the system's random source failing is not recoverable
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

// codeAlphabet leaves out 0, O, 1 and I, which people confuse when copying a code.
const codeAlphabet = "ABCDEFGHJKLMNPQRSTUVWXYZ23456789"

func newCode() string {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	for i := range b {
		b[i] = codeAlphabet[int(b[i])%len(codeAlphabet)] // 256 is a multiple of 32: no bias
	}
	return string(b)
}

// NormalizeCode accepts what people type: lower case, spaces and the dash in "ABCD-EFGH".
func NormalizeCode(c string) string {
	c = strings.ToUpper(strings.NewReplacer(" ", "", "-", "").Replace(strings.TrimSpace(c)))
	if len(c) != 8 || strings.Trim(c, codeAlphabet) != "" {
		return ""
	}
	return c
}

// FormatCode shows a code as "ABCD-EFGH".
func FormatCode(c string) string {
	if len(c) != 8 {
		return c
	}
	return c[:4] + "-" + c[4:]
}

func inet(ip string) *netip.Addr {
	a, err := netip.ParseAddr(ip)
	if err != nil {
		return nil
	}
	return &a
}

// Login is a sign-in waiting in a browser. Secret goes in the browser's cookie; Code is shown on screen.
type Login struct {
	Secret    string
	Code      string
	ExpiresAt time.Time
}

func (s *Store) StartLogin(ctx context.Context, ip, userAgent string) (*Login, error) {
	l := &Login{Secret: randomToken(), Code: newCode(), ExpiresAt: s.now().Add(LoginTTL)}
	_, err := s.Pool.Exec(ctx, `
		INSERT INTO merchant.web_logins (secret_hash, code_hash, ip, user_agent, expires_at) VALUES ($1, $2, $3, $4, $5)`,
		hash(l.Secret), hash(l.Code), inet(ip), truncate(userAgent, 300), l.ExpiresAt)
	if err != nil {
		return nil, err
	}
	// Old sign-ins are of no use to anyone; keep the table small.
	_, _ = s.Pool.Exec(ctx, `DELETE FROM merchant.web_logins WHERE expires_at < $1`, s.now().Add(-24*time.Hour))
	return l, nil
}

type LoginState int

const (
	LoginWaiting LoginState = iota
	LoginGone               // expired, already used, or unknown: start again
	LoginApproved
)

// PollLogin is the browser checking whether its code was approved. When it was, the browser gets its
// session token, once.
func (s *Store) PollLogin(ctx context.Context, secret, code, ip, userAgent string) (LoginState, string, error) {
	if secret == "" || code == "" {
		return LoginGone, "", nil
	}
	var state LoginState
	var token string
	err := pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		var id uuid.UUID
		var codeHash string
		var expires time.Time
		var approvedBy, merchant *uuid.UUID
		var used *time.Time
		err := tx.QueryRow(ctx, `
			SELECT id, code_hash, expires_at, approved_by, merchant_id, used_at FROM merchant.web_logins
			WHERE secret_hash = $1 FOR UPDATE`, hash(secret)).Scan(&id, &codeHash, &expires, &approvedBy, &merchant, &used)
		if errors.Is(err, pgx.ErrNoRows) {
			state = LoginGone
			return nil
		}
		if err != nil {
			return err
		}
		switch {
		case codeHash != hash(code) || used != nil:
			state = LoginGone
		case approvedBy != nil:
			// An approved login can be picked up a little after the code's own expiry: the person may have
			// approved it in the last seconds.
			if s.now().After(expires.Add(time.Minute)) {
				state = LoginGone
				return nil
			}
			token = randomToken()
			now := s.now()
			if _, err := tx.Exec(ctx, `UPDATE merchant.web_logins SET used_at = $2 WHERE id = $1`, id, now); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `
				INSERT INTO merchant.web_sessions (token_hash, user_id, merchant_id, csrf_token, ip, user_agent, created_at, last_seen_at, expires_at)
				VALUES ($1, $2, $3, $4, $5, $6, $7, $7, $8)`,
				hash(token), *approvedBy, *merchant, randomToken(), inet(ip), truncate(userAgent, 300), now, now.Add(SessionAbsolute)); err != nil {
				return err
			}
			state = LoginApproved
		case s.now().After(expires):
			state = LoginGone
		default:
			state = LoginWaiting
		}
		return nil
	})
	return state, token, err
}

// LoginInfo is what the app shows before the person approves: which browser is asking.
type LoginInfo struct {
	Browser   string
	IP        string
	CreatedAt time.Time
	ExpiresAt time.Time
}

func (s *Store) waitingLogin(ctx context.Context, q interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}, code string, lock bool) (uuid.UUID, *LoginInfo, error) {
	code = NormalizeCode(code)
	if code == "" {
		return uuid.Nil, nil, ErrNotFound
	}
	sql := `SELECT id, coalesce(user_agent, ''), coalesce(host(ip), ''), created_at, expires_at FROM merchant.web_logins
		WHERE code_hash = $1 AND approved_at IS NULL AND used_at IS NULL AND expires_at > $2`
	if lock {
		sql += ` FOR UPDATE`
	}
	var id uuid.UUID
	var ua string
	info := &LoginInfo{}
	err := q.QueryRow(ctx, sql, hash(code), s.now()).Scan(&id, &ua, &info.IP, &info.CreatedAt, &info.ExpiresAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, nil, ErrNotFound
	}
	if err != nil {
		return uuid.Nil, nil, err
	}
	info.Browser = DescribeBrowser(ua)
	return id, info, nil
}

// LookupLogin tells the app which browser a code belongs to, so the person can check before approving.
func (s *Store) LookupLogin(ctx context.Context, user uuid.UUID, code string) (*LoginInfo, error) {
	_, info, err := s.waitingLogin(ctx, s.Pool, code, false)
	return info, err
}

// ApproveLogin signs the browser showing code in to merchant as user. Any member of an approved shop may
// do it, with their PIN; what they can do there still follows their role.
func (s *Store) ApproveLogin(ctx context.Context, user, merchant uuid.UUID, code, pin string) (*LoginInfo, error) {
	var shop string
	err := s.Pool.QueryRow(ctx, `
		SELECT m.trade_name FROM merchant.members mm JOIN merchant.merchants m ON m.id = mm.merchant_id
		WHERE mm.user_id = $1 AND mm.merchant_id = $2 AND m.status = 'active'`, user, merchant).Scan(&shop)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrForbidden
	}
	if err != nil {
		return nil, err
	}
	if _, _, err := s.waitingLogin(ctx, s.Pool, code, false); err != nil {
		return nil, err // checked before the PIN, so a mistyped code doesn't cost a PIN try
	}
	if err := s.PIN.Verify(ctx, user, pin); err != nil {
		return nil, err
	}
	var info *LoginInfo
	err = pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		id, i, err := s.waitingLogin(ctx, tx, code, true)
		if err != nil {
			return err
		}
		info = i
		if _, err := tx.Exec(ctx, `
			UPDATE merchant.web_logins SET approved_by = $2, merchant_id = $3, approved_at = $4 WHERE id = $1`,
			id, user, merchant, s.now()); err != nil {
			return err
		}
		return platform.WriteOutbox(ctx, tx, "identity.web_signed_in", user, map[string]any{
			"user_id": user, "merchant_id": merchant, "shop": shop, "browser": i.Browser,
		})
	})
	return info, err
}

// Session is a signed-in browser. Role, names and the shop's status are read afresh on every request, so
// a change of role or a suspension applies at once.
type Session struct {
	ID         uuid.UUID
	UserID     uuid.UUID
	MerchantID uuid.UUID
	CSRF       string
	Name       string
	Locale     string
	Role       string
	Shop       string
	ExpiresAt  time.Time
}

func (s *Session) Manages() bool { return s.Role == "owner" || s.Role == "manager" }
func (s *Session) Handles() bool { return s.Role != "accountant" }
func (s *Session) SeesMoney() bool {
	return s.Role == "owner" || s.Role == "manager" || s.Role == "accountant"
}

// SessionFrom checks a session cookie. It fails with ErrSignedOut when the session has ended for any
// reason: expired, idle, revoked, the person left the shop or was frozen, or the shop isn't active.
func (s *Store) SessionFrom(ctx context.Context, token string) (*Session, error) {
	if token == "" {
		return nil, ErrSignedOut
	}
	now := s.now()
	sess := &Session{}
	var lastSeen time.Time
	err := s.Pool.QueryRow(ctx, `
		SELECT ws.id, ws.user_id, ws.merchant_id, ws.csrf_token, ws.expires_at, ws.last_seen_at,
		       coalesce(u.display_name, ''), u.locale::text, mm.role, m.trade_name
		FROM merchant.web_sessions ws
		JOIN identity.users u ON u.id = ws.user_id
		JOIN merchant.members mm ON mm.merchant_id = ws.merchant_id AND mm.user_id = ws.user_id
		JOIN merchant.merchants m ON m.id = ws.merchant_id
		WHERE ws.token_hash = $1 AND ws.revoked_at IS NULL AND ws.expires_at > $2
		  AND u.status NOT IN ('frozen', 'closed') AND m.status = 'active'`,
		hash(token), now).Scan(&sess.ID, &sess.UserID, &sess.MerchantID, &sess.CSRF, &sess.ExpiresAt, &lastSeen,
		&sess.Name, &sess.Locale, &sess.Role, &sess.Shop)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrSignedOut
	}
	if err != nil {
		return nil, err
	}
	if now.Sub(lastSeen) > SessionIdle {
		_, _ = s.Pool.Exec(ctx, `UPDATE merchant.web_sessions SET revoked_at = $2 WHERE id = $1`, sess.ID, now)
		return nil, ErrSignedOut
	}
	if now.Sub(lastSeen) > time.Minute {
		if _, err := s.Pool.Exec(ctx, `UPDATE merchant.web_sessions SET last_seen_at = $2 WHERE id = $1`, sess.ID, now); err != nil {
			return nil, err
		}
	}
	return sess, nil
}

func (s *Store) EndSession(ctx context.Context, token string) error {
	_, err := s.Pool.Exec(ctx, `UPDATE merchant.web_sessions SET revoked_at = $2 WHERE token_hash = $1 AND revoked_at IS NULL`,
		hash(token), s.now())
	return err
}

// SessionInfo is a signed-in browser as the app lists it.
type SessionInfo struct {
	ID         uuid.UUID
	MerchantID uuid.UUID
	Shop       string
	Browser    string
	IP         string
	CreatedAt  time.Time
	LastSeenAt time.Time
}

// Sessions lists the person's own browsers that are still signed in, for any shop.
func (s *Store) Sessions(ctx context.Context, user uuid.UUID) ([]SessionInfo, error) {
	now := s.now()
	rows, err := s.Pool.Query(ctx, `
		SELECT ws.id, ws.merchant_id, m.trade_name, coalesce(ws.user_agent, ''), coalesce(host(ws.ip), ''), ws.created_at, ws.last_seen_at
		FROM merchant.web_sessions ws JOIN merchant.merchants m ON m.id = ws.merchant_id
		WHERE ws.user_id = $1 AND ws.revoked_at IS NULL AND ws.expires_at > $2 AND ws.last_seen_at > $3
		ORDER BY ws.last_seen_at DESC LIMIT 50`, user, now, now.Add(-SessionIdle))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []SessionInfo{}
	for rows.Next() {
		var i SessionInfo
		var ua string
		if err := rows.Scan(&i.ID, &i.MerchantID, &i.Shop, &ua, &i.IP, &i.CreatedAt, &i.LastSeenAt); err != nil {
			return nil, err
		}
		i.Browser = DescribeBrowser(ua)
		out = append(out, i)
	}
	return out, rows.Err()
}

// Revoke signs one of the person's own browsers out.
func (s *Store) Revoke(ctx context.Context, user, id uuid.UUID) error {
	tag, err := s.Pool.Exec(ctx, `
		UPDATE merchant.web_sessions SET revoked_at = $3 WHERE id = $1 AND user_id = $2 AND revoked_at IS NULL`, id, user, s.now())
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// DescribeBrowser turns a User-Agent into "Chrome on Windows" for people to recognise.
func DescribeBrowser(ua string) string {
	has := func(s string) bool { return strings.Contains(ua, s) }
	browser := "A browser"
	switch {
	case has("Edg/"):
		browser = "Edge"
	case has("OPR/"), has("Opera"):
		browser = "Opera"
	case has("Firefox/"):
		browser = "Firefox"
	case has("Chrome/"), has("CriOS/"):
		browser = "Chrome"
	case has("Safari/"):
		browser = "Safari"
	}
	osName := ""
	switch {
	case has("Windows"):
		osName = "Windows"
	case has("Android"):
		osName = "Android"
	case has("iPhone"), has("iPad"):
		osName = "iPhone or iPad"
	case has("Mac OS X"), has("Macintosh"):
		osName = "Mac"
	case has("CrOS"):
		osName = "Chromebook"
	case has("Linux"):
		osName = "Linux"
	}
	if osName == "" {
		return browser
	}
	return browser + " on " + osName
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
