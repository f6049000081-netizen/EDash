// Package merchants is the business side of the app: applying as a merchant (KYB), approval, the shop's
// payment QR, and the figures a shop sees through the day.
//
// An application never becomes a working merchant on its own. Operations staff review the business
// documents and approve it (Approve); only then does the shop get a merchant number and a QR code that
// customers can pay.
package merchants

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"math/big"
	"regexp"
	"strings"
	"time"
	"unicode"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"superapp/backend/internal/money"
	"superapp/backend/internal/platform"
)

// EAT is Ethiopian time (UTC+3 all year). "Today" for a shop is the day in Addis Ababa.
var EAT = time.FixedZone("EAT", 3*60*60)

var (
	ErrNotFound  = errors.New("merchants: not found")
	ErrForbidden = errors.New("merchants: not allowed for this role")
	ErrConflict  = errors.New("merchants: conflict")
	ErrNotActive = errors.New("merchants: not approved yet")
)

type ValidationError struct{ Msg string }

func (e *ValidationError) Error() string { return e.Msg }

// ConflictError explains a refused application.
type ConflictError struct{ Msg string }

func (e *ConflictError) Error() string { return e.Msg }
func (e *ConflictError) Unwrap() error { return ErrConflict }

// Categories a shop can choose when applying, by MCC-style code.
var Categories = map[string]string{
	"5411": "Groceries and supermarkets",
	"5812": "Restaurants and cafés",
	"5814": "Fast food",
	"5912": "Pharmacies",
	"5651": "Clothing",
	"5732": "Electronics",
	"5251": "Hardware and building supplies",
	"7230": "Beauty and barber",
	"4121": "Taxis and transport",
	"8299": "Schools and training",
	"5999": "Other shops",
}

type Merchant struct {
	ID         uuid.UUID
	LegalName  string
	TradeName  string
	TIN        string
	Licence    string
	Category   string
	City       string
	Status     string
	MerchantNo string
	Role       string // the caller's role
	PayoutPlan string
	CreatedAt  time.Time
	ApprovedAt *time.Time
}

type Service struct {
	Pool *pgxpool.Pool
	Now  func() time.Time
}

func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

// ---------------------------------------------------------------------------
// Applying
// ---------------------------------------------------------------------------

type Application struct {
	LegalName string
	TradeName string
	TIN       string
	Licence   string
	Category  string
	City      string
}

var (
	tinRE     = regexp.MustCompile(`^[0-9]{10}$`)
	licenceRE = regexp.MustCompile(`^[A-Za-z0-9/\-]{4,40}$`)
)

func clean(s string) string { return strings.Join(strings.Fields(s), " ") }

// Apply records a merchant application from a signed-in user, who becomes its owner. The shop can't take
// payments until operations approve it. Sending the same application again returns the first one.
func (s *Service) Apply(ctx context.Context, user uuid.UUID, a Application) (*Merchant, error) {
	a.LegalName, a.TradeName, a.City = clean(a.LegalName), clean(a.TradeName), clean(a.City)
	a.TIN, a.Licence = strings.TrimSpace(a.TIN), strings.TrimSpace(a.Licence)
	switch {
	case len([]rune(a.LegalName)) < 2 || len([]rune(a.LegalName)) > 120:
		return nil, &ValidationError{"Enter the business's registered name."}
	case len([]rune(a.TradeName)) < 2 || len([]rune(a.TradeName)) > 60:
		return nil, &ValidationError{"Enter the name customers know the shop by (up to 60 characters)."}
	case !tinRE.MatchString(a.TIN):
		return nil, &ValidationError{"The TIN is the 10-digit taxpayer number on the business's registration."}
	case a.Licence != "" && !licenceRE.MatchString(a.Licence):
		return nil, &ValidationError{"Enter the trade licence number as it is printed on the licence."}
	case Categories[a.Category] == "":
		return nil, &ValidationError{"Choose what the business sells."}
	case len([]rune(a.City)) < 2 || len([]rune(a.City)) > 40:
		return nil, &ValidationError{"Enter the town or city the business is in."}
	}
	var out *Merchant
	err := pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		// One application at a time per person, so a stolen phone can't flood operations.
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('merchant-apply:' || $1::text, 0))`, user); err != nil {
			return err
		}
		var userStatus string
		if err := tx.QueryRow(ctx, `SELECT status FROM identity.users WHERE id = $1`, user).Scan(&userStatus); err != nil {
			return err
		}
		if userStatus != "active" {
			return &ConflictError{"Set up your PIN before applying."}
		}
		var existing uuid.UUID
		var existingTIN string
		err := tx.QueryRow(ctx, `
			SELECT m.id, COALESCE(m.tin, '') FROM merchant.merchants m JOIN merchant.members mm ON mm.merchant_id = m.id
			WHERE mm.user_id = $1 AND mm.role = 'owner' AND m.status IN ('applied', 'kyb_review')`, user).Scan(&existing, &existingTIN)
		if err == nil {
			if existingTIN == a.TIN {
				out, err = s.get(ctx, tx, user, existing)
				return err
			}
			return &ConflictError{"You already have an application being reviewed. You can apply for another business once it's decided."}
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		now := s.now()
		id, err := uuid.NewV7()
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `
			INSERT INTO merchant.merchants (id, legal_name, trade_name, tin, business_licence, category_code, city, status,
				applied_by, created_at, updated_at)
			VALUES ($1, $2, $3, $4, NULLIF($5, ''), $6, $7, 'kyb_review', $8, $9, $9)`,
			id, a.LegalName, a.TradeName, a.TIN, a.Licence, a.Category, a.City, user, now)
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return &ConflictError{"A business with this TIN is already registered. If it's yours, ask its owner to add you, or contact support."}
		}
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO merchant.members (merchant_id, user_id, role, created_at) VALUES ($1, $2, 'owner', $3)`,
			id, user, now); err != nil {
			return err
		}
		if err := platform.WriteOutbox(ctx, tx, "merchant.applied", id, map[string]any{"merchant_id": id, "applied_by": user}); err != nil {
			return err
		}
		out, err = s.get(ctx, tx, user, id)
		return err
	})
	return out, err
}

// Approve activates a reviewed merchant: it gets a merchant number and a static payment QR. The
// operations console calls it after a second person has approved (maker-checker); it is safe to call
// again, and fills in whatever an active merchant is missing.
func (s *Service) Approve(ctx context.Context, merchant, approvedBy uuid.UUID) (*Merchant, error) {
	err := pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		var status, name, category, city string
		var no *string
		err := tx.QueryRow(ctx, `
			SELECT status, trade_name, category_code, COALESCE(city, ''), merchant_no FROM merchant.merchants WHERE id = $1 FOR UPDATE`,
			merchant).Scan(&status, &name, &category, &city, &no)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if status != "applied" && status != "kyb_review" && status != "active" {
			return &ConflictError{"Only applications under review can be approved; this merchant is " + status + "."}
		}
		now := s.now()
		if no == nil {
			for range 5 {
				n := NewMerchantNo()
				tag, err := tx.Exec(ctx, `
					UPDATE merchant.merchants SET merchant_no = $2 WHERE id = $1
					AND NOT EXISTS (SELECT 1 FROM merchant.merchants WHERE merchant_no = $2)`, merchant, n)
				if err != nil {
					return err
				}
				if tag.RowsAffected() == 1 {
					no = &n
					break
				}
			}
			if no == nil {
				return errors.New("merchants: no free merchant number")
			}
		}
		if status != "active" {
			if _, err := tx.Exec(ctx, `
				UPDATE merchant.merchants SET status = 'active', approved_by = $2, approved_at = $3, updated_at = $3 WHERE id = $1`,
				merchant, approvedBy, now); err != nil {
				return err
			}
			if err := platform.WriteOutbox(ctx, tx, "merchant.approved", merchant, map[string]any{"merchant_id": merchant}); err != nil {
				return err
			}
		}
		payload := StaticQR(*no, name, category, city)
		_, err = tx.Exec(ctx, `
			INSERT INTO payments.qr_codes (owner_type, owner_id, mode, payload, created_at)
			SELECT 'merchant', $1, 'static', $2, $3
			WHERE NOT EXISTS (SELECT 1 FROM payments.qr_codes WHERE payload = $2)`, merchant, payload, now)
		return err
	})
	if err != nil {
		return nil, err
	}
	return s.get(ctx, s.Pool, uuid.Nil, merchant)
}

// NewMerchantNo is a random 8-digit merchant number whose last digit is a Luhn check digit, so a
// mistyped number is caught. It's random rather than sequential so it doesn't reveal how many shops exist.
func NewMerchantNo() string {
	n, err := rand.Int(rand.Reader, big.NewInt(9_000_000))
	if err != nil {
		panic(err)
	}
	body := fmt.Sprintf("%07d", n.Int64()+1_000_000)
	return body + string(rune('0'+luhn(body)))
}

func luhn(digits string) int {
	sum := 0
	double := true
	for i := len(digits) - 1; i >= 0; i-- {
		d := int(digits[i] - '0')
		if double {
			d *= 2
			if d > 9 {
				d -= 9
			}
		}
		sum += d
		double = !double
	}
	return (10 - sum%10) % 10
}

// ValidMerchantNo checks the length and check digit.
func ValidMerchantNo(s string) bool {
	if len(s) != 8 || strings.Trim(s, "0123456789") != "" {
		return false
	}
	return luhn(s[:7]) == int(s[7]-'0')
}

// ---------------------------------------------------------------------------
// QR
// ---------------------------------------------------------------------------

// QRGUID identifies Gebeya One in the merchant account template. Other wallets can read the code's
// format, name and amount, but paying through them needs the interoperability scheme (EthSwitch) to
// register this template; until then, only Gebeya One resolves it.
const QRGUID = "one.gebeya.pay"

// StaticQR builds an EMVCo merchant-presented QR payload (static: the payer enters the amount).
func StaticQR(merchantNo, name, category, city string) string {
	if city == "" {
		city = "Addis Ababa"
	}
	var b strings.Builder
	tlv(&b, "00", "01")
	tlv(&b, "01", "11")
	var acct strings.Builder
	tlv(&acct, "00", QRGUID)
	tlv(&acct, "01", merchantNo)
	tlv(&b, "26", acct.String())
	tlv(&b, "52", category)
	tlv(&b, "53", "230") // ETB
	tlv(&b, "58", "ET")
	tlv(&b, "59", latin(name, 25, "GEBEYA ONE MERCHANT"))
	tlv(&b, "60", latin(city, 15, "ADDIS ABABA"))
	if latin(name, 99, "") != name {
		// Names in Ge'ez script go in the alternate-language template, so wallets can show them.
		var lang strings.Builder
		tlv(&lang, "00", "am")
		tlv(&lang, "01", truncate(name, 25))
		tlv(&b, "64", lang.String())
	}
	b.WriteString("6304")
	return b.String() + fmt.Sprintf("%04X", crc16(b.String()))
}

func tlv(b *strings.Builder, tag, v string) {
	fmt.Fprintf(b, "%s%02d%s", tag, len([]rune(v)), v)
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) > n {
		r = r[:n]
	}
	return string(r)
}

// latin keeps the printable ASCII of s (EMVCo's alphanumeric special set), or fallback when none is left.
func latin(s string, n int, fallback string) string {
	var b strings.Builder
	for _, r := range s {
		if r < 128 && unicode.IsPrint(r) {
			b.WriteRune(r)
		}
	}
	out := strings.TrimSpace(clean(b.String()))
	if out == "" {
		return fallback
	}
	return truncate(out, n)
}

// crc16 is CRC-16/CCITT-FALSE, as EMVCo requires.
func crc16(s string) uint16 {
	crc := uint16(0xFFFF)
	for _, c := range []byte(s) {
		crc ^= uint16(c) << 8
		for range 8 {
			if crc&0x8000 != 0 {
				crc = crc<<1 ^ 0x1021
			} else {
				crc <<= 1
			}
		}
	}
	return crc
}

// QR returns the shop's payment QR payload. Any member may show it.
func (s *Service) QR(ctx context.Context, user, merchant uuid.UUID) (string, *Merchant, error) {
	m, err := s.Get(ctx, user, merchant)
	if err != nil {
		return "", nil, err
	}
	if m.Status != "active" {
		return "", nil, ErrNotActive
	}
	var payload string
	err = s.Pool.QueryRow(ctx, `
		SELECT payload FROM payments.qr_codes
		WHERE owner_type = 'merchant' AND owner_id = $1 AND mode = 'static' AND (expires_at IS NULL OR expires_at > now())
		ORDER BY (payload LIKE '%' || $2 || '%') DESC, created_at DESC LIMIT 1`, merchant, QRGUID).Scan(&payload)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil, ErrNotActive
	}
	return payload, m, err
}

// ---------------------------------------------------------------------------
// Reading
// ---------------------------------------------------------------------------

func (s *Service) role(ctx context.Context, db platform.DB, user, merchant uuid.UUID) (string, error) {
	var role string
	err := db.QueryRow(ctx, `SELECT role FROM merchant.members WHERE merchant_id = $1 AND user_id = $2`, merchant, user).Scan(&role)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrNotFound
	}
	return role, err
}

const merchantCols = `m.id, m.legal_name, m.trade_name, COALESCE(m.tin, ''), COALESCE(m.business_licence, ''), m.category_code,
	COALESCE(m.city, ''), m.status, COALESCE(m.merchant_no, ''), m.payout_schedule, m.created_at, m.approved_at`

func scanMerchant(row pgx.Row, extra ...any) (*Merchant, error) {
	m := &Merchant{}
	dest := append([]any{&m.ID, &m.LegalName, &m.TradeName, &m.TIN, &m.Licence, &m.Category, &m.City, &m.Status,
		&m.MerchantNo, &m.PayoutPlan, &m.CreatedAt, &m.ApprovedAt}, extra...)
	return m, row.Scan(dest...)
}

func (s *Service) get(ctx context.Context, db platform.DB, user, id uuid.UUID) (*Merchant, error) {
	var role *string
	m, err := scanMerchant(db.QueryRow(ctx, `
		SELECT `+merchantCols+`, mm.role FROM merchant.merchants m
		LEFT JOIN merchant.members mm ON mm.merchant_id = m.id AND mm.user_id = $2
		WHERE m.id = $1`, id, user), &role)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if role != nil {
		m.Role = *role
	}
	return m, err
}

// Get returns a merchant the user belongs to. Strangers get ErrNotFound.
func (s *Service) Get(ctx context.Context, user, id uuid.UUID) (*Merchant, error) {
	m, err := s.get(ctx, s.Pool, user, id)
	if err != nil {
		return nil, err
	}
	if m.Role == "" {
		return nil, ErrNotFound
	}
	if m.Role == "cashier" { // cashiers take payments; the business's registration details aren't theirs to see
		m.LegalName, m.TIN, m.Licence = "", "", ""
	}
	return m, nil
}

// Mine lists the businesses the user belongs to, with their role, oldest first.
func (s *Service) Mine(ctx context.Context, user uuid.UUID) ([]Merchant, error) {
	rows, err := s.Pool.Query(ctx, `
		SELECT m.id FROM merchant.members mm JOIN merchant.merchants m ON m.id = mm.merchant_id
		WHERE mm.user_id = $1 AND m.status <> 'closed' ORDER BY mm.created_at, m.id`, user)
	if err != nil {
		return nil, err
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
	if err != nil {
		return nil, err
	}
	out := make([]Merchant, 0, len(ids))
	for _, id := range ids {
		m, err := s.Get(ctx, user, id)
		if err != nil {
			return nil, err
		}
		out = append(out, *m)
	}
	return out, nil
}

type Summary struct {
	SalesToday    money.Money
	PaymentsToday int
	AverageBasket money.Money
	// Money earned but not yet paid out: unbatched sales plus payouts waiting to be sent.
	Pending    money.Money
	NextPayout *NextPayout
}

type NextPayout struct {
	ID           uuid.UUID
	Net          money.Money
	Status       string
	HoldReason   string
	ScheduledFor time.Time
}

// Summary is today's figures for the shop's home screen. Cashiers see sales; payout figures are for
// owners, managers and accountants.
func (s *Service) Summary(ctx context.Context, user, merchant uuid.UUID) (*Summary, error) {
	role, err := s.role(ctx, s.Pool, user, merchant)
	if err != nil {
		return nil, err
	}
	local := s.now().In(EAT)
	start := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, EAT)
	var gross int64
	var n int
	if err := s.Pool.QueryRow(ctx, `
		SELECT COALESCE(sum(amount_minor), 0), count(*) FROM payments.intents
		WHERE payee_type = 'merchant' AND payee_id = $1 AND status = 'succeeded' AND currency = 'ETB'
		  AND completed_at >= $2`, merchant, start).Scan(&gross, &n); err != nil {
		return nil, err
	}
	out := &Summary{SalesToday: money.New(gross, money.ETB), PaymentsToday: n, AverageBasket: money.New(0, money.ETB)}
	if n > 0 {
		out.AverageBasket = money.New(gross/int64(n), money.ETB)
	}
	if role == "cashier" {
		return out, nil
	}
	var pending int64
	if err := s.Pool.QueryRow(ctx, `
		SELECT COALESCE(sum(b.balance_minor), 0) FROM ledger.accounts a JOIN ledger.balances b ON b.account_id = a.id
		WHERE a.owner_type = 'merchant' AND a.owner_id = $1 AND a.kind IN ('merchant_settlement', 'hold') AND a.currency = 'ETB'`,
		merchant).Scan(&pending); err != nil {
		return nil, err
	}
	out.Pending = money.New(pending, money.ETB)
	np := &NextPayout{}
	var net int64
	err = s.Pool.QueryRow(ctx, `
		SELECT id, net_minor, status, COALESCE(hold_reason, ''), scheduled_for FROM merchant.payouts
		WHERE merchant_id = $1 AND status IN ('scheduled', 'sent', 'on_hold')
		ORDER BY scheduled_for, id LIMIT 1`, merchant).Scan(&np.ID, &net, &np.Status, &np.HoldReason, &np.ScheduledFor)
	if err == nil {
		np.Net = money.New(net, money.ETB)
		out.NextPayout = np
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return nil, err
	}
	return out, nil
}

type Payment struct {
	ID          uuid.UUID
	PayerName   string // first name and initial only
	Amount      money.Money
	CompletedAt time.Time
}

// Payments lists the shop's received payments, newest first. Every member sees it: it's how a cashier
// checks that a customer has paid. Payers are shown by first name and initial.
func (s *Service) Payments(ctx context.Context, user, merchant uuid.UUID, before *time.Time, beforeID uuid.UUID, limit int) ([]Payment, error) {
	if _, err := s.role(ctx, s.Pool, user, merchant); err != nil {
		return nil, err
	}
	if limit <= 0 || limit > 100 {
		limit = 30
	}
	t := time.Date(9999, 1, 1, 0, 0, 0, 0, time.UTC)
	if before != nil {
		t = *before
	} else {
		beforeID = uuid.Max
	}
	rows, err := s.Pool.Query(ctx, `
		SELECT i.id, COALESCE(u.display_name, ''), i.amount_minor, i.currency::text, i.completed_at
		FROM payments.intents i LEFT JOIN identity.users u ON u.id = i.payer_user_id
		WHERE i.payee_type = 'merchant' AND i.payee_id = $1 AND i.status = 'succeeded'
		  AND (i.completed_at, i.id) < ($2, $3)
		ORDER BY i.completed_at DESC, i.id DESC LIMIT $4`, merchant, t, beforeID, limit)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Payment, error) {
		var p Payment
		var name, cur string
		var amt int64
		err := r.Scan(&p.ID, &name, &amt, &cur, &p.CompletedAt)
		p.PayerName, p.Amount = ShortName(name), money.New(amt, money.Currency(cur))
		return p, err
	})
}

// ShortName turns "Abebe Kebede" into "Abebe K." and an empty name into "Customer".
func ShortName(name string) string {
	f := strings.Fields(name)
	switch len(f) {
	case 0:
		return "Customer"
	case 1:
		return f[0]
	default:
		return f[0] + " " + string([]rune(f[len(f)-1])[:1]) + "."
	}
}
