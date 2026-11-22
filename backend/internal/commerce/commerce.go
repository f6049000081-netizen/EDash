// Package commerce is ordering from shops in the app: each shop's catalog and ordering settings,
// customers' orders, and the shop's side of handling them.
//
// An order is paid like any payment (PIN or biometric, risk checks, limits; see payments.OrderHook),
// but the money waits in order escrow. The shop is paid when the order is handed over: the shop enters
// the customer's four-digit handover code, or the customer confirms in the app. If the order is
// cancelled first, by the customer before the shop accepts it, by the shop, or because the shop
// didn't respond in time, the customer gets the money back in their wallet at once.
package commerce

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"superapp/backend/internal/money"
	"superapp/backend/internal/payments"
	"superapp/backend/internal/platform"
)

type Service struct {
	Pool     *pgxpool.Pool
	Payments *payments.Service

	// PayWithin: orders not paid by then are cancelled (the customer left checkout).
	PayWithin time.Duration
	// AcceptWithin: paid orders the shop hasn't accepted by then are cancelled and refunded.
	AcceptWithin time.Duration

	now func() time.Time
}

// New wires the service into payments, so order payments move orders along.
func New(pool *pgxpool.Pool, pay *payments.Service) *Service {
	s := &Service{Pool: pool, Payments: pay, PayWithin: 30 * time.Minute, AcceptWithin: 20 * time.Minute}
	pay.Orders = s
	return s
}

func (s *Service) clock() time.Time {
	if s.now != nil {
		return s.now()
	}
	return time.Now()
}

var (
	ErrNotFound  = errors.New("commerce: not found")
	ErrForbidden = errors.New("commerce: role not allowed")
)

// ValidationError is the caller's input; ConflictError is the order's or shop's state.
type ValidationError struct{ Msg string }

func (e *ValidationError) Error() string { return e.Msg }

type ConflictError struct{ Msg string }

func (e *ConflictError) Error() string { return e.Msg }

// Shop roles. Owners and managers run the shop's catalog and settings; cashiers also handle orders;
// accountants only see them.
var (
	manageRoles = []string{"owner", "manager"}
	handleRoles = []string{"owner", "manager", "cashier"}
	viewRoles   = []string{"owner", "manager", "cashier", "accountant"}
)

func (s *Service) requireRole(ctx context.Context, db platform.DB, user, merchant uuid.UUID, allowed []string) error {
	var role string
	err := db.QueryRow(ctx, `SELECT role FROM merchant.members WHERE merchant_id = $1 AND user_id = $2`, merchant, user).Scan(&role)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	for _, r := range allowed {
		if r == role {
			return nil
		}
	}
	return ErrForbidden
}

// ---------------------------------------------------------------------------
// Shops and products
// ---------------------------------------------------------------------------

type Shop struct {
	MerchantID   uuid.UUID
	Name         string
	Category     string
	City         string
	Open         bool
	Pickup       bool
	Delivery     bool
	DeliveryFee  money.Money
	DeliveryArea string
	About        string
	Products     []Product // Shop and ShopSettings only
	ProductCount int
}

type Product struct {
	ID          uuid.UUID
	Title       string
	Description string
	Category    string
	Price       money.Money
	Stock       *int // nil: not counted
	Status      string
	UpdatedAt   time.Time
}

func (p Product) SoldOut() bool { return p.Stock != nil && *p.Stock == 0 }

const shopCols = `m.id, m.trade_name, m.category_code, COALESCE(m.city, ''), s.open, s.pickup, s.delivery,
	s.delivery_fee_minor, COALESCE(s.delivery_area, ''), COALESCE(s.about, '')`

func scanShop(row pgx.Row, extra ...any) (*Shop, error) {
	sh := &Shop{}
	var fee int64
	err := row.Scan(append([]any{&sh.MerchantID, &sh.Name, &sh.Category, &sh.City, &sh.Open, &sh.Pickup, &sh.Delivery,
		&fee, &sh.DeliveryArea, &sh.About}, extra...)...)
	sh.DeliveryFee = money.New(fee, money.ETB)
	return sh, err
}

// Shops lists shops customers can order from: approved, set up for orders, with something to sell.
// Open shops come first.
func (s *Service) Shops(ctx context.Context, limit int) ([]Shop, error) {
	if limit <= 0 || limit > 100 {
		limit = 50
	}
	rows, err := s.Pool.Query(ctx, `
		SELECT `+shopCols+`, (SELECT count(*) FROM commerce.products p WHERE p.merchant_id = m.id AND p.status = 'active')
		FROM commerce.shops s JOIN merchant.merchants m ON m.id = s.merchant_id
		WHERE m.status = 'active'
		  AND EXISTS (SELECT 1 FROM commerce.products p WHERE p.merchant_id = m.id AND p.status = 'active')
		ORDER BY s.open DESC, m.trade_name LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Shop{}
	for rows.Next() {
		var count int
		sh, err := scanShop(rows, &count)
		if err != nil {
			return nil, err
		}
		sh.ProductCount = count
		out = append(out, *sh)
	}
	return out, rows.Err()
}

// Shop is one shop's page for customers: its settings and the products on sale.
func (s *Service) Shop(ctx context.Context, merchant uuid.UUID) (*Shop, error) {
	sh, err := scanShop(s.Pool.QueryRow(ctx, `
		SELECT `+shopCols+` FROM commerce.shops s JOIN merchant.merchants m ON m.id = s.merchant_id
		WHERE m.id = $1 AND m.status = 'active'`, merchant))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if sh.Products, err = s.products(ctx, s.Pool, merchant, false); err != nil {
		return nil, err
	}
	sh.ProductCount = len(sh.Products)
	return sh, nil
}

func (s *Service) products(ctx context.Context, db platform.DB, merchant uuid.UUID, all bool) ([]Product, error) {
	rows, err := db.Query(ctx, `
		SELECT id, title, COALESCE(description, ''), category, price_minor, currency, stock, status, updated_at
		FROM commerce.products
		WHERE merchant_id = $1 AND (status = 'active' OR ($2 AND status <> 'archived'))
		ORDER BY category, title`, merchant, all)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Product, error) {
		var p Product
		var cur string
		err := r.Scan(&p.ID, &p.Title, &p.Description, &p.Category, &p.Price.AmountMinor, &cur, &p.Stock, &p.Status, &p.UpdatedAt)
		p.Price.Currency = money.Currency(cur)
		return p, err
	})
}

// ShopSettings is the shop's own view: settings (defaults if never saved) and every product not archived.
func (s *Service) ShopSettings(ctx context.Context, user, merchant uuid.UUID) (*Shop, error) {
	if err := s.requireRole(ctx, s.Pool, user, merchant, viewRoles); err != nil {
		return nil, err
	}
	sh, err := scanShop(s.Pool.QueryRow(ctx, `
		SELECT m.id, m.trade_name, m.category_code, COALESCE(m.city, ''), COALESCE(s.open, false), COALESCE(s.pickup, true),
		       COALESCE(s.delivery, false), COALESCE(s.delivery_fee_minor, 0), COALESCE(s.delivery_area, ''), COALESCE(s.about, '')
		FROM merchant.merchants m LEFT JOIN commerce.shops s ON s.merchant_id = m.id WHERE m.id = $1`, merchant))
	if err != nil {
		return nil, err
	}
	if sh.Products, err = s.products(ctx, s.Pool, merchant, true); err != nil {
		return nil, err
	}
	sh.ProductCount = len(sh.Products)
	return sh, nil
}

type ShopUpdate struct {
	Open, Pickup, Delivery bool
	DeliveryFee            money.Money
	DeliveryArea, About    string
}

// UpdateShop saves the shop's ordering settings. Opening needs an approved shop and something on sale.
func (s *Service) UpdateShop(ctx context.Context, user, merchant uuid.UUID, u ShopUpdate) (*Shop, error) {
	u.DeliveryArea, u.About = strings.TrimSpace(u.DeliveryArea), strings.TrimSpace(u.About)
	switch {
	case !u.Pickup && !u.Delivery:
		return nil, &ValidationError{"Offer pickup, delivery or both."}
	case u.DeliveryFee.Currency != money.ETB || u.DeliveryFee.AmountMinor < 0 || u.DeliveryFee.AmountMinor > 1_000_000:
		return nil, &ValidationError{"The delivery fee must be in ETB, from 0 to 10,000."}
	case len([]rune(u.DeliveryArea)) > 200:
		return nil, &ValidationError{"Describe the delivery area in up to 200 characters."}
	case len([]rune(u.About)) > 500:
		return nil, &ValidationError{"The shop description can be up to 500 characters."}
	case u.Delivery && u.DeliveryArea == "":
		return nil, &ValidationError{"Say where you deliver, for example \"Bole and Kazanchis\"."}
	}
	err := pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		if err := s.requireRole(ctx, tx, user, merchant, manageRoles); err != nil {
			return err
		}
		if u.Open {
			var status string
			var products int
			if err := tx.QueryRow(ctx, `SELECT status, (SELECT count(*) FROM commerce.products WHERE merchant_id = $1 AND status = 'active')
				FROM merchant.merchants WHERE id = $1`, merchant).Scan(&status, &products); err != nil {
				return err
			}
			if status != "active" {
				return &ConflictError{"Your shop can take orders once its application is approved."}
			}
			if products == 0 {
				return &ConflictError{"Add at least one product before opening for orders."}
			}
		}
		_, err := tx.Exec(ctx, `
			INSERT INTO commerce.shops (merchant_id, open, pickup, delivery, delivery_fee_minor, delivery_area, about, updated_at)
			VALUES ($1, $2, $3, $4, $5, NULLIF($6, ''), NULLIF($7, ''), now())
			ON CONFLICT (merchant_id) DO UPDATE SET open = $2, pickup = $3, delivery = $4, delivery_fee_minor = $5,
				delivery_area = NULLIF($6, ''), about = NULLIF($7, ''), updated_at = now()`,
			merchant, u.Open, u.Pickup, u.Delivery, u.DeliveryFee.AmountMinor, u.DeliveryArea, u.About)
		return err
	})
	if err != nil {
		return nil, err
	}
	return s.ShopSettings(ctx, user, merchant)
}

type ProductInput struct {
	Title, Description, Category string
	Price                        money.Money
	Stock                        *int
	Status                       string // active, hidden, archived
}

func (in *ProductInput) check() error {
	in.Title, in.Description, in.Category = strings.TrimSpace(in.Title), strings.TrimSpace(in.Description), strings.TrimSpace(in.Category)
	if in.Status == "" {
		in.Status = "active"
	}
	switch {
	case in.Title == "" || len([]rune(in.Title)) > 80:
		return &ValidationError{"Give the product a name of up to 80 characters."}
	case len([]rune(in.Description)) > 500:
		return &ValidationError{"The description can be up to 500 characters."}
	case len([]rune(in.Category)) > 40:
		return &ValidationError{"The shelf name can be up to 40 characters."}
	case in.Price.Currency != money.ETB || in.Price.AmountMinor <= 0 || in.Price.AmountMinor > 100_000_000:
		return &ValidationError{"Set a price in ETB, from 0.01 to 1,000,000."}
	case in.Stock != nil && (*in.Stock < 0 || *in.Stock > 100_000):
		return &ValidationError{"Stock must be from 0 to 100,000, or left empty if you don't count it."}
	case in.Status != "active" && in.Status != "hidden" && in.Status != "archived":
		return &ValidationError{"status must be active, hidden or archived"}
	}
	return nil
}

func (s *Service) CreateProduct(ctx context.Context, user, merchant uuid.UUID, in ProductInput) (*Product, error) {
	if err := in.check(); err != nil {
		return nil, err
	}
	var id uuid.UUID
	err := pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		if err := s.requireRole(ctx, tx, user, merchant, manageRoles); err != nil {
			return err
		}
		var n int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM commerce.products WHERE merchant_id = $1 AND status <> 'archived'`, merchant).Scan(&n); err != nil {
			return err
		}
		if n >= 500 {
			return &ConflictError{"A shop can have up to 500 products. Archive some first."}
		}
		return tx.QueryRow(ctx, `
			INSERT INTO commerce.products (merchant_id, title, description, category, price_minor, currency, stock, status)
			VALUES ($1, $2, NULLIF($3, ''), $4, $5, 'ETB', $6, $7) RETURNING id`,
			merchant, in.Title, in.Description, in.Category, in.Price.AmountMinor, in.Stock, in.Status).Scan(&id)
	})
	if err != nil {
		return nil, err
	}
	return s.product(ctx, merchant, id)
}

// UpdateProduct replaces a product's details. Orders keep the title and price they were placed with.
func (s *Service) UpdateProduct(ctx context.Context, user, merchant, id uuid.UUID, in ProductInput) (*Product, error) {
	if err := in.check(); err != nil {
		return nil, err
	}
	err := pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		if err := s.requireRole(ctx, tx, user, merchant, manageRoles); err != nil {
			return err
		}
		tag, err := tx.Exec(ctx, `
			UPDATE commerce.products SET title = $3, description = NULLIF($4, ''), category = $5, price_minor = $6,
				stock = $7, status = $8, updated_at = now()
			WHERE id = $1 AND merchant_id = $2 AND status <> 'archived'`,
			id, merchant, in.Title, in.Description, in.Category, in.Price.AmountMinor, in.Stock, in.Status)
		if err == nil && tag.RowsAffected() == 0 {
			return ErrNotFound
		}
		return err
	})
	if err != nil {
		return nil, err
	}
	return s.product(ctx, merchant, id)
}

func (s *Service) product(ctx context.Context, merchant, id uuid.UUID) (*Product, error) {
	var p Product
	var cur string
	err := s.Pool.QueryRow(ctx, `
		SELECT id, title, COALESCE(description, ''), category, price_minor, currency, stock, status, updated_at
		FROM commerce.products WHERE id = $1 AND merchant_id = $2`, id, merchant).
		Scan(&p.ID, &p.Title, &p.Description, &p.Category, &p.Price.AmountMinor, &cur, &p.Stock, &p.Status, &p.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	p.Price.Currency = money.Currency(cur)
	return &p, err
}

// handoverCode is four random digits.
func handoverCode() (string, error) {
	n, err := rand.Int(rand.Reader, big.NewInt(10000))
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%04d", n.Int64()), nil
}

func codesEqual(a, b string) bool { return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1 }
