// Package ledger is the double-entry book of record. Every money movement is one journal entry whose
// postings balance per currency. Entries are append-only; mistakes are corrected with reversal entries.
//
// The ledger never opens its own transactions: callers pass a pgx.Tx so the ledger write commits or rolls
// back together with the business change that caused it (a payment intent, an order, a payout).
package ledger

import (
	"context"
	"errors"
	"fmt"
	"sort"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"superapp/backend/internal/money"
	"superapp/backend/internal/platform"
)

type Side string

const (
	Debit  Side = "D"
	Credit Side = "C"
)

type AccountKind string

const (
	UserWallet         AccountKind = "user_wallet"
	MerchantSettlement AccountKind = "merchant_settlement"
	PartnerClearing    AccountKind = "partner_clearing"
	FeeRevenue         AccountKind = "fee_revenue"
	CommissionRevenue  AccountKind = "commission_revenue"
	RewardsLiability   AccountKind = "rewards_liability"
	Suspense           AccountKind = "suspense"
	Hold               AccountKind = "hold"
	OrderEscrow        AccountKind = "order_escrow"
)

// accountRules: liabilities and income are credit-normal; assets are debit-normal.
// Only clearing and suspense accounts may go negative (they track timing differences with partners).
var accountRules = map[AccountKind]struct {
	normal        Side
	allowNegative bool
}{
	UserWallet:         {Credit, false},
	MerchantSettlement: {Credit, false},
	PartnerClearing:    {Debit, true},
	FeeRevenue:         {Credit, false},
	CommissionRevenue:  {Credit, false},
	RewardsLiability:   {Credit, false},
	Suspense:           {Debit, true},
	Hold:               {Credit, false},
	OrderEscrow:        {Credit, false},
}

type OwnerType string

const (
	OwnerUser     OwnerType = "user"
	OwnerMerchant OwnerType = "merchant"
	OwnerPartner  OwnerType = "partner"
	OwnerPlatform OwnerType = "platform"
)

type Posting struct {
	AccountID   uuid.UUID
	Side        Side
	AmountMinor int64
	Currency    money.Currency
}

type Entry struct {
	Kind       string    // p2p_transfer, qr_payment, topup, refund, reversal ...
	SourceType string    // the business object that caused it, e.g. "payments.intent"
	SourceID   uuid.UUID // its id; (SourceType, SourceID, Kind) is unique, so retries can't double-post
	Postings   []Posting
}

var (
	ErrUnbalanced    = errors.New("ledger: entry does not balance")
	ErrDuplicate     = errors.New("ledger: entry already posted for this source")
	ErrAccountClosed = errors.New("ledger: account is frozen or closed")
	ErrCurrency      = errors.New("ledger: posting currency differs from account currency")
)

// InsufficientFundsError is returned when a posting would take a non-negative account below zero.
type InsufficientFundsError struct {
	AccountID uuid.UUID
	Balance   int64
	Needed    int64
}

func (e *InsufficientFundsError) Error() string {
	return fmt.Sprintf("ledger: insufficient funds in %s: balance %d, needs %d", e.AccountID, e.Balance, e.Needed)
}

// Validate checks the entry on its own: at least two postings, positive amounts, valid currencies,
// and debits equal to credits in every currency. The database checks the same invariant again at commit.
func (e Entry) Validate() error {
	if e.Kind == "" || e.SourceType == "" || e.SourceID == uuid.Nil {
		return fmt.Errorf("ledger: entry needs kind, source type and source id")
	}
	if len(e.Postings) < 2 {
		return fmt.Errorf("%w: needs at least two postings", ErrUnbalanced)
	}
	net := map[money.Currency]int64{}
	for _, p := range e.Postings {
		if p.AmountMinor <= 0 || p.AmountMinor > money.MaxAmountMinor {
			return fmt.Errorf("ledger: posting amount %d out of range", p.AmountMinor)
		}
		if !p.Currency.Valid() {
			return fmt.Errorf("ledger: unknown currency %q", p.Currency)
		}
		switch p.Side {
		case Debit:
			net[p.Currency] += p.AmountMinor
		case Credit:
			net[p.Currency] -= p.AmountMinor
		default:
			return fmt.Errorf("ledger: bad side %q", p.Side)
		}
	}
	for c, n := range net {
		if n != 0 {
			return fmt.Errorf("%w: %s off by %d", ErrUnbalanced, c, n)
		}
	}
	return nil
}

type Ledger struct{}

func New() *Ledger { return &Ledger{} }

// EnsureAccount returns the account for (kind, owner, currency), creating it and its balance row if needed.
// ownerID is uuid.Nil for platform-owned accounts.
func (l *Ledger) EnsureAccount(ctx context.Context, tx pgx.Tx, kind AccountKind, ownerType OwnerType, ownerID uuid.UUID, cur money.Currency) (uuid.UUID, error) {
	rule, ok := accountRules[kind]
	if !ok {
		return uuid.Nil, fmt.Errorf("ledger: unknown account kind %q", kind)
	}
	owner := uuid.NullUUID{UUID: ownerID, Valid: ownerID != uuid.Nil}
	var id uuid.UUID
	err := tx.QueryRow(ctx, `
		INSERT INTO ledger.accounts (kind, owner_type, owner_id, currency, normal_side, allow_negative)
		VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (kind, owner_type, owner_id, currency) DO NOTHING
		RETURNING id`,
		string(kind), string(ownerType), owner, string(cur), string(rule.normal), rule.allowNegative).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		err = tx.QueryRow(ctx, `
			SELECT id FROM ledger.accounts
			WHERE kind = $1 AND owner_type = $2 AND owner_id IS NOT DISTINCT FROM $3 AND currency = $4`,
			string(kind), string(ownerType), owner, string(cur)).Scan(&id)
		return id, err
	}
	if err != nil {
		return uuid.Nil, err
	}
	_, err = tx.Exec(ctx, `INSERT INTO ledger.balances (account_id) VALUES ($1) ON CONFLICT DO NOTHING`, id)
	return id, err
}

// FindAccount looks an account up without creating it. ok is false if it doesn't exist yet.
func (l *Ledger) FindAccount(ctx context.Context, db platform.DB, kind AccountKind, ownerType OwnerType, ownerID uuid.UUID, cur money.Currency) (id uuid.UUID, ok bool, err error) {
	owner := uuid.NullUUID{UUID: ownerID, Valid: ownerID != uuid.Nil}
	err = db.QueryRow(ctx, `
		SELECT id FROM ledger.accounts
		WHERE kind = $1 AND owner_type = $2 AND owner_id IS NOT DISTINCT FROM $3 AND currency = $4`,
		string(kind), string(ownerType), owner, string(cur)).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, false, nil
	}
	return id, err == nil, err
}

// Post writes a journal entry and its postings and updates cached balances, all inside tx.
//
// Balance rows are locked in account-id order, so two transfers between the same pair of accounts
// in opposite directions cannot deadlock. If the entry would overdraw a non-negative account, Post
// returns *InsufficientFundsError and the caller should roll back (use a savepoint to keep the rest of tx).
func (l *Ledger) Post(ctx context.Context, tx pgx.Tx, e Entry) (uuid.UUID, error) {
	if err := e.Validate(); err != nil {
		return uuid.Nil, err
	}

	var entryID uuid.UUID
	err := tx.QueryRow(ctx, `
		INSERT INTO ledger.journal_entries (kind, source_type, source_id)
		VALUES ($1, $2, $3)
		ON CONFLICT (source_type, source_id, kind) DO NOTHING
		RETURNING id`, e.Kind, e.SourceType, e.SourceID).Scan(&entryID)
	if errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, ErrDuplicate
	}
	if err != nil {
		return uuid.Nil, err
	}

	// Net effect per account, in each account's normal direction.
	ids := make([]string, 0, len(e.Postings))
	seen := map[uuid.UUID]bool{}
	for _, p := range e.Postings {
		if !seen[p.AccountID] {
			seen[p.AccountID] = true
			ids = append(ids, p.AccountID.String())
		}
	}
	sort.Strings(ids)

	type acct struct {
		balance       int64
		normal        Side
		allowNegative bool
		status        string
		currency      money.Currency
	}
	accts := map[uuid.UUID]*acct{}
	rows, err := tx.Query(ctx, `
		SELECT b.account_id, b.balance_minor, a.normal_side, a.allow_negative, a.status, a.currency
		FROM ledger.balances b JOIN ledger.accounts a ON a.id = b.account_id
		WHERE b.account_id = ANY($1::uuid[])
		ORDER BY b.account_id
		FOR UPDATE OF b`, ids)
	if err != nil {
		return uuid.Nil, err
	}
	for rows.Next() {
		var id uuid.UUID
		var a acct
		var normal, cur string
		if err := rows.Scan(&id, &a.balance, &normal, &a.allowNegative, &a.status, &cur); err != nil {
			rows.Close()
			return uuid.Nil, err
		}
		a.normal, a.currency = Side(normal), money.Currency(cur)
		accts[id] = &a
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return uuid.Nil, err
	}

	delta := map[uuid.UUID]int64{}
	for _, p := range e.Postings {
		a, ok := accts[p.AccountID]
		if !ok {
			return uuid.Nil, fmt.Errorf("ledger: account %s not found", p.AccountID)
		}
		if a.status != "open" {
			return uuid.Nil, ErrAccountClosed
		}
		if a.currency != p.Currency {
			return uuid.Nil, ErrCurrency
		}
		if p.Side == a.normal {
			delta[p.AccountID] += p.AmountMinor
		} else {
			delta[p.AccountID] -= p.AmountMinor
		}
	}
	for id, d := range delta {
		a := accts[id]
		if !a.allowNegative && a.balance+d < 0 {
			return uuid.Nil, &InsufficientFundsError{AccountID: id, Balance: a.balance, Needed: -d}
		}
	}

	for _, p := range e.Postings {
		if _, err := tx.Exec(ctx, `
			INSERT INTO ledger.postings (entry_id, account_id, side, amount_minor, currency)
			VALUES ($1, $2, $3, $4, $5)`,
			entryID, p.AccountID, string(p.Side), p.AmountMinor, string(p.Currency)); err != nil {
			return uuid.Nil, err
		}
	}
	for id, d := range delta {
		if d == 0 {
			continue
		}
		if _, err := tx.Exec(ctx, `
			UPDATE ledger.balances SET balance_minor = balance_minor + $2, version = version + 1, updated_at = now()
			WHERE account_id = $1`, id, d); err != nil {
			return uuid.Nil, err
		}
	}
	return entryID, nil
}

// Balance returns the cached balance of an account in its normal direction.
func (l *Ledger) Balance(ctx context.Context, db platform.DB, accountID uuid.UUID) (int64, error) {
	var b int64
	err := db.QueryRow(ctx, `SELECT balance_minor FROM ledger.balances WHERE account_id = $1`, accountID).Scan(&b)
	return b, err
}

// RecomputedBalance sums the postings, ignoring the cache. Used by reconciliation and tests.
func (l *Ledger) RecomputedBalance(ctx context.Context, db platform.DB, accountID uuid.UUID) (int64, error) {
	var b int64
	err := db.QueryRow(ctx, `
		SELECT COALESCE(SUM(CASE WHEN p.side = a.normal_side THEN p.amount_minor ELSE -p.amount_minor END), 0)
		FROM ledger.postings p JOIN ledger.accounts a ON a.id = p.account_id
		WHERE p.account_id = $1`, accountID).Scan(&b)
	return b, err
}

// UserBalances lists a user's wallet balances by currency.
func (l *Ledger) UserBalances(ctx context.Context, db platform.DB, userID uuid.UUID) ([]money.Money, error) {
	rows, err := db.Query(ctx, `
		SELECT a.currency, b.balance_minor
		FROM ledger.accounts a JOIN ledger.balances b ON b.account_id = a.id
		WHERE a.kind = 'user_wallet' AND a.owner_type = 'user' AND a.owner_id = $1
		ORDER BY a.currency`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []money.Money
	for rows.Next() {
		var cur string
		var amt int64
		if err := rows.Scan(&cur, &amt); err != nil {
			return nil, err
		}
		out = append(out, money.New(amt, money.Currency(cur)))
	}
	return out, rows.Err()
}
