package payments

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"superapp/backend/internal/ledger"
	"superapp/backend/internal/money"
	"superapp/backend/internal/platform"
)

// PresenceVerifier proves the payer is present (PIN now; device biometric signatures later).
type PresenceVerifier interface {
	Verify(ctx context.Context, userID uuid.UUID, pin string) error
}

type Service struct {
	Pool     *pgxpool.Pool
	Ledger   *ledger.Ledger
	Risk     RiskEngine
	Presence PresenceVerifier

	// DefaultMerchantBps applies when a merchant has no commission plan. 50 = 0.5%.
	DefaultMerchantBps int64

	// Rails collects top-ups from licensed partners, keyed by rail name ("telebirr"). A rail missing
	// from the map is refused with ErrRailUnavailable.
	Rails map[string]TopUpRail
	TopUp TopUpLimits

	// LimitsRequired makes a missing identity.kyc_limits row block payments (kyc_required) instead
	// of meaning "no limit". Turn it on in production once compliance has filled the table.
	LimitsRequired bool
}

func NewService(pool *pgxpool.Pool, l *ledger.Ledger, risk RiskEngine, presence PresenceVerifier) *Service {
	return &Service{Pool: pool, Ledger: l, Risk: risk, Presence: presence, DefaultMerchantBps: 50,
		Rails: map[string]TopUpRail{}, TopUp: DefaultTopUpLimits()}
}

// ---------------------------------------------------------------------------
// Create
// ---------------------------------------------------------------------------

func (s *Service) CreateIntent(ctx context.Context, payerID uuid.UUID, req CreateRequest) (*Intent, error) {
	if req.FundingSourceID != nil {
		return nil, ErrRailUnavailable
	}
	if len(req.Note) > 140 {
		return nil, &ValidationError{"Notes can be up to 140 characters."}
	}
	if err := s.requireActive(ctx, s.Pool, payerID); err != nil {
		return nil, err
	}

	in := &Intent{ID: uuid.New(), Kind: req.Kind, PayerID: payerID, Rail: "wallet", Status: StatusRequiresConfirmation, Note: req.Note}

	switch req.Kind {
	case KindP2P:
		if req.PayeeUserID == nil {
			return nil, &ValidationError{"Choose who to send money to."}
		}
		if *req.PayeeUserID == payerID {
			return nil, ErrSelfPayment
		}
		var name *string
		var status string
		err := s.Pool.QueryRow(ctx, `SELECT display_name, status FROM identity.users WHERE id = $1`, *req.PayeeUserID).Scan(&name, &status)
		if errors.Is(err, pgx.ErrNoRows) || (err == nil && status != "active") {
			return nil, ErrPayeeNotFound
		}
		if err != nil {
			return nil, err
		}
		in.PayeeType, in.PayeeID, in.PayeeVerified = "user", *req.PayeeUserID, true
		if name != nil {
			in.PayeeName = *name
		}
		if req.Amount == nil {
			return nil, &ValidationError{"Enter an amount."}
		}
		in.Amount = *req.Amount

	case KindQRMerchant:
		qr, err := s.ResolveQR(ctx, req.QRPayload)
		if err != nil {
			return nil, err
		}
		in.PayeeType, in.PayeeID, in.PayeeName, in.PayeeVerified = "merchant", qr.MerchantID, qr.MerchantName, true
		switch {
		case qr.FixedAmount != nil && req.Amount != nil && *req.Amount != *qr.FixedAmount:
			return nil, &ValidationError{"This QR code is for a fixed amount. Pay the amount shown."}
		case qr.FixedAmount != nil:
			in.Amount = *qr.FixedAmount
		case req.Amount == nil:
			return nil, &ValidationError{"Enter an amount."}
		default:
			in.Amount = *req.Amount
		}

	default:
		return nil, ErrUnsupportedKind
	}

	if !in.Amount.Currency.Valid() {
		return nil, &ValidationError{"Unsupported currency."}
	}
	if in.Amount.AmountMinor <= 0 || in.Amount.AmountMinor > money.MaxAmountMinor {
		return nil, &ValidationError{"Enter an amount greater than zero."}
	}
	in.FeeMinor = 0 // wallet transfers and QR payments are free for the payer; merchants pay commission

	// Tell the user about limits before they enter a PIN. Confirm checks again under a lock.
	if err := s.checkSpend(ctx, s.Pool, payerID, in.Total(), time.Now()); err != nil {
		return nil, err
	}

	err := pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		err := tx.QueryRow(ctx, `
			INSERT INTO payments.intents
				(id, kind, payer_user_id, payee_type, payee_id, amount_minor, currency, fee_minor, rail, status, note)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, NULLIF($11, ''))
			RETURNING created_at`,
			in.ID, string(in.Kind), in.PayerID, in.PayeeType, in.PayeeID, in.Amount.AmountMinor,
			string(in.Amount.Currency), in.FeeMinor, in.Rail, string(in.Status), in.Note).Scan(&in.CreatedAt)
		if err != nil {
			return err
		}
		return addEvent(ctx, tx, in.ID, "", StatusRequiresConfirmation, "created", "user:"+payerID.String())
	})
	if err != nil {
		return nil, err
	}
	return in, nil
}

type ResolvedQR struct {
	MerchantID   uuid.UUID
	MerchantName string
	FixedAmount  *money.Money
}

// ResolveQR maps a scanned payload to an active merchant, for the payee screen and for CreateIntent.
func (s *Service) ResolveQR(ctx context.Context, payload string) (*ResolvedQR, error) {
	payload = strings.TrimSpace(payload)
	if payload == "" {
		return nil, &ValidationError{"Scan a QR code."}
	}
	var ownerType, mode, cur, merchantStatus, name string
	var ownerID uuid.UUID
	var amount *int64
	var expires *time.Time
	err := s.Pool.QueryRow(ctx, `
		SELECT q.owner_type, q.owner_id, q.mode, q.amount_minor, q.currency, q.expires_at, m.status, m.trade_name
		FROM payments.qr_codes q JOIN merchant.merchants m ON m.id = q.owner_id
		WHERE q.payload = $1 AND q.owner_type = 'merchant'`, payload).
		Scan(&ownerType, &ownerID, &mode, &amount, &cur, &expires, &merchantStatus, &name)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrPayeeNotFound
	}
	if err != nil {
		return nil, err
	}
	if merchantStatus != "active" || (expires != nil && time.Now().After(*expires)) {
		return nil, ErrPayeeNotFound
	}
	r := &ResolvedQR{MerchantID: ownerID, MerchantName: name}
	if mode == "dynamic" && amount != nil {
		m := money.New(*amount, money.Currency(cur))
		r.FixedAmount = &m
	}
	return r, nil
}

// ---------------------------------------------------------------------------
// Confirm
// ---------------------------------------------------------------------------

// Confirm checks the PIN, runs risk checks and executes the payment in one transaction.
// Confirming an intent that already finished returns it unchanged, so client retries are safe.
// When funds are short it returns the failed intent together with ErrInsufficientFunds.
func (s *Service) Confirm(ctx context.Context, payerID, intentID uuid.UUID, pin string) (*Intent, error) {
	in, err := s.Get(ctx, payerID, intentID)
	if err != nil {
		return nil, err
	}
	if in.Status != StatusRequiresConfirmation {
		return in, nil
	}
	if err := s.Presence.Verify(ctx, payerID, pin); err != nil {
		return nil, err
	}

	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after Commit

	var status string
	if err := tx.QueryRow(ctx, `SELECT status FROM payments.intents WHERE id = $1 FOR UPDATE`, intentID).Scan(&status); err != nil {
		return nil, err
	}
	if Status(status) != StatusRequiresConfirmation {
		// Another request confirmed it while we checked the PIN.
		_ = tx.Rollback(ctx)
		return s.Get(ctx, payerID, intentID)
	}
	if err := s.requireActive(ctx, tx, payerID); err != nil {
		return nil, err
	}

	// Lock the payer so parallel payments are checked against limits one at a time.
	if _, err := tx.Exec(ctx, `SELECT 1 FROM identity.users WHERE id = $1 FOR UPDATE`, payerID); err != nil {
		return nil, err
	}
	if err := s.checkSpend(ctx, tx, payerID, in.Total(), time.Now()); err != nil {
		return s.failOnLimit(ctx, tx, in, err)
	}

	decision, err := s.Risk.Assess(ctx, tx, in)
	if err != nil {
		return nil, err
	}
	if err := recordDecision(ctx, tx, in.ID, decision); err != nil {
		return nil, err
	}
	actor := "user:" + payerID.String()

	switch decision.Action {
	case ActionHold:
		if err := s.finish(ctx, tx, in, StatusHeldForReview, "", decision, actor, "payment.held"); err != nil {
			return nil, err
		}
		return s.Get(ctx, payerID, intentID)
	case ActionBlock:
		if err := s.finish(ctx, tx, in, StatusFailed, "blocked", decision, actor, "payment.failed"); err != nil {
			return nil, err
		}
		return nil, ErrBlocked
	}

	entry, err := s.buildEntry(ctx, tx, in)
	if err != nil {
		return nil, err
	}

	// Post inside a savepoint: if funds are short we roll back only the ledger write and still
	// record the failure on the intent.
	sp, err := tx.Begin(ctx)
	if err != nil {
		return nil, err
	}
	_, postErr := s.Ledger.Post(ctx, sp, entry)
	var insufficient *ledger.InsufficientFundsError
	switch {
	case postErr == nil:
		// The payee's balance row is locked by the post, so this check holds under concurrency.
		if in.Kind == KindP2P {
			if err := s.checkBalanceCap(ctx, sp, in.PayeeID, in.Amount.Currency, 0, "recipient"); err != nil {
				if rbErr := sp.Rollback(ctx); rbErr != nil {
					return nil, rbErr
				}
				return s.failOnLimit(ctx, tx, in, err)
			}
		}
		if err := sp.Commit(ctx); err != nil {
			return nil, err
		}
	case errors.As(postErr, &insufficient):
		if err := sp.Rollback(ctx); err != nil {
			return nil, err
		}
		if err := s.finish(ctx, tx, in, StatusFailed, "insufficient_funds", decision, actor, "payment.failed"); err != nil {
			return nil, err
		}
		failed, err := s.Get(ctx, payerID, intentID)
		if err != nil {
			return nil, err
		}
		return failed, ErrInsufficientFunds
	default:
		return nil, postErr
	}

	if err := s.finish(ctx, tx, in, StatusSucceeded, "", decision, actor, "payment.succeeded"); err != nil {
		return nil, err
	}
	return s.Get(ctx, payerID, intentID)
}

// failOnLimit records a limit refusal on the intent (failed, nothing charged) and returns the
// failed intent with the limit error. Any other error is returned as is.
func (s *Service) failOnLimit(ctx context.Context, tx pgx.Tx, in *Intent, err error) (*Intent, error) {
	var le *LimitError
	code := "limit_exceeded"
	switch {
	case errors.As(err, &le):
	case errors.Is(err, ErrKYCRequired):
		code = "kyc_required"
	default:
		return nil, err
	}
	d := Decision{Action: ActionBlock, Model: "limits", Rules: []string{code}}
	if ferr := s.finish(ctx, tx, in, StatusFailed, code, d, "system", "payment.failed"); ferr != nil {
		return nil, ferr
	}
	failed, gerr := s.Get(ctx, in.PayerID, in.ID)
	if gerr != nil {
		return nil, gerr
	}
	return failed, err
}

// buildEntry turns an intent into balanced postings.
//
//	p2p:          Dr payer wallet          Cr payee wallet
//	qr_merchant:  Dr payer wallet (amount) Cr merchant settlement (amount − commission)
//	                                       Cr commission revenue  (commission)
func (s *Service) buildEntry(ctx context.Context, tx pgx.Tx, in *Intent) (ledger.Entry, error) {
	cur := in.Amount.Currency
	amt := in.Amount.AmountMinor
	payer, err := s.Ledger.EnsureAccount(ctx, tx, ledger.UserWallet, ledger.OwnerUser, in.PayerID, cur)
	if err != nil {
		return ledger.Entry{}, err
	}
	e := ledger.Entry{SourceType: "payments.intent", SourceID: in.ID}

	switch in.Kind {
	case KindP2P:
		payee, err := s.Ledger.EnsureAccount(ctx, tx, ledger.UserWallet, ledger.OwnerUser, in.PayeeID, cur)
		if err != nil {
			return e, err
		}
		e.Kind = "p2p_transfer"
		e.Postings = []ledger.Posting{
			{AccountID: payer, Side: ledger.Debit, AmountMinor: amt, Currency: cur},
			{AccountID: payee, Side: ledger.Credit, AmountMinor: amt, Currency: cur},
		}

	case KindQRMerchant:
		settlement, err := s.Ledger.EnsureAccount(ctx, tx, ledger.MerchantSettlement, ledger.OwnerMerchant, in.PayeeID, cur)
		if err != nil {
			return e, err
		}
		bps, err := s.merchantBps(ctx, tx, in.PayeeID)
		if err != nil {
			return e, err
		}
		commission := money.BasisPoints(amt, bps)
		e.Kind = "qr_payment"
		e.Postings = []ledger.Posting{
			{AccountID: payer, Side: ledger.Debit, AmountMinor: amt, Currency: cur},
			{AccountID: settlement, Side: ledger.Credit, AmountMinor: amt - commission, Currency: cur},
		}
		if commission > 0 {
			rev, err := s.Ledger.EnsureAccount(ctx, tx, ledger.CommissionRevenue, ledger.OwnerPlatform, uuid.Nil, cur)
			if err != nil {
				return e, err
			}
			e.Postings = append(e.Postings, ledger.Posting{AccountID: rev, Side: ledger.Credit, AmountMinor: commission, Currency: cur})
		}

	default:
		return e, ErrUnsupportedKind
	}

	if in.FeeMinor > 0 {
		feeAcct, err := s.Ledger.EnsureAccount(ctx, tx, ledger.FeeRevenue, ledger.OwnerPlatform, uuid.Nil, cur)
		if err != nil {
			return e, err
		}
		e.Postings = append(e.Postings,
			ledger.Posting{AccountID: payer, Side: ledger.Debit, AmountMinor: in.FeeMinor, Currency: cur},
			ledger.Posting{AccountID: feeAcct, Side: ledger.Credit, AmountMinor: in.FeeMinor, Currency: cur})
	}
	return e, nil
}

// merchantBps returns the merchant's current QR commission plan, else the default.
func (s *Service) merchantBps(ctx context.Context, tx pgx.Tx, merchantID uuid.UUID) (int64, error) {
	var bps int64
	err := tx.QueryRow(ctx, `
		SELECT rate_bps FROM merchant.commission_plans
		WHERE merchant_id = $1 AND channel = 'qr' AND valid_from <= now() AND (valid_to IS NULL OR valid_to > now())
		ORDER BY valid_from DESC LIMIT 1`, merchantID).Scan(&bps)
	if errors.Is(err, pgx.ErrNoRows) {
		return s.DefaultMerchantBps, nil
	}
	return bps, err
}

func (s *Service) finish(ctx context.Context, tx pgx.Tx, in *Intent, to Status, failure string, d Decision, actor, topic string) error {
	_, err := tx.Exec(ctx, `
		UPDATE payments.intents
		SET status = $2::payments.intent_status, failure_code = NULLIF($3, ''), risk_score = $4, confirmed_at = now(),
		    completed_at = CASE WHEN $2::payments.intent_status IN ('succeeded', 'failed') THEN now() END, updated_at = now()
		WHERE id = $1`, in.ID, string(to), failure, d.Score)
	if err != nil {
		return err
	}
	if err := addEvent(ctx, tx, in.ID, StatusRequiresConfirmation, to, failure, actor); err != nil {
		return err
	}
	if err := platform.WriteOutbox(ctx, tx, topic, in.ID, map[string]any{
		"intent_id": in.ID, "kind": in.Kind, "payer_user_id": in.PayerID,
		"payee_type": in.PayeeType, "payee_id": in.PayeeID,
		"amount": in.Amount, "status": to, "failure_code": failure,
		"points_to_earn": in.PointsToEarn(),
	}); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// ---------------------------------------------------------------------------
// Cancel, Get
// ---------------------------------------------------------------------------

func (s *Service) Cancel(ctx context.Context, payerID, intentID uuid.UUID) (*Intent, error) {
	err := pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		var status string
		err := tx.QueryRow(ctx, `SELECT status FROM payments.intents WHERE id = $1 AND payer_user_id = $2 FOR UPDATE`,
			intentID, payerID).Scan(&status)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if Status(status) == StatusCancelled {
			return nil
		}
		if Status(status) != StatusRequiresConfirmation {
			return ErrInvalidState
		}
		if _, err := tx.Exec(ctx, `UPDATE payments.intents SET status = 'cancelled', updated_at = now() WHERE id = $1`, intentID); err != nil {
			return err
		}
		return addEvent(ctx, tx, intentID, Status(status), StatusCancelled, "cancelled by payer", "user:"+payerID.String())
	})
	if err != nil {
		return nil, err
	}
	return s.Get(ctx, payerID, intentID)
}

// Get returns an intent the caller paid. Other people's intents look the same as missing ones.
func (s *Service) Get(ctx context.Context, payerID, intentID uuid.UUID) (*Intent, error) {
	in := &Intent{}
	var kind, cur, status string
	var payeeID uuid.NullUUID
	var payeeName, failure, note *string
	err := s.Pool.QueryRow(ctx, `
		SELECT i.id, i.kind, i.payer_user_id, i.payee_type, i.payee_id,
		       COALESCE(u.display_name, m.trade_name),
		       i.amount_minor, i.currency, i.fee_minor, i.rail, i.status, i.failure_code,
		       i.risk_score::float8, i.note, i.created_at, i.completed_at
		FROM payments.intents i
		LEFT JOIN identity.users u ON i.payee_type = 'user' AND u.id = i.payee_id
		LEFT JOIN merchant.merchants m ON i.payee_type = 'merchant' AND m.id = i.payee_id
		WHERE i.id = $1 AND i.payer_user_id = $2`, intentID, payerID).
		Scan(&in.ID, &kind, &in.PayerID, &in.PayeeType, &payeeID, &payeeName,
			&in.Amount.AmountMinor, &cur, &in.FeeMinor, &in.Rail, &status, &failure,
			&in.RiskScore, &note, &in.CreatedAt, &in.CompletedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	in.Kind, in.Amount.Currency, in.Status = Kind(kind), money.Currency(cur), Status(status)
	in.PayeeID = payeeID.UUID
	in.PayeeVerified = payeeID.Valid
	if payeeName != nil {
		in.PayeeName = *payeeName
	}
	if failure != nil {
		in.FailureCode = *failure
	}
	if note != nil {
		in.Note = *note
	}
	return in, nil
}

// ---------------------------------------------------------------------------
// Sandbox top-up
// ---------------------------------------------------------------------------

// SandboxTopUp credits a wallet as if money arrived from a partner rail. It records a real ledger entry
// against a partner clearing account, exactly as a confirmed partner top-up will. Only wired up in dev.
func (s *Service) SandboxTopUp(ctx context.Context, userID uuid.UUID, amt money.Money, rail string) (*Intent, error) {
	if amt.AmountMinor <= 0 || !amt.Currency.Valid() {
		return nil, &ValidationError{"Enter a positive amount in a supported currency."}
	}
	in := &Intent{ID: uuid.New(), Kind: KindTopUp, PayerID: userID, PayeeType: "user", PayeeID: userID, Amount: amt, Rail: rail}
	err := pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `
			INSERT INTO payments.intents (id, kind, payer_user_id, payee_type, payee_id, amount_minor, currency, rail, status, confirmed_at, completed_at)
			VALUES ($1, 'topup', $2, 'user', $2, $3, $4, $5, 'succeeded', now(), now())`,
			in.ID, userID, amt.AmountMinor, string(amt.Currency), rail); err != nil {
			return err
		}
		clearing, err := s.Ledger.EnsureAccount(ctx, tx, ledger.PartnerClearing, ledger.OwnerPartner, partnerID(rail), amt.Currency)
		if err != nil {
			return err
		}
		wallet, err := s.Ledger.EnsureAccount(ctx, tx, ledger.UserWallet, ledger.OwnerUser, userID, amt.Currency)
		if err != nil {
			return err
		}
		_, err = s.Ledger.Post(ctx, tx, ledger.Entry{Kind: "topup", SourceType: "payments.intent", SourceID: in.ID, Postings: []ledger.Posting{
			{AccountID: clearing, Side: ledger.Debit, AmountMinor: amt.AmountMinor, Currency: amt.Currency},
			{AccountID: wallet, Side: ledger.Credit, AmountMinor: amt.AmountMinor, Currency: amt.Currency},
		}})
		if err != nil {
			return err
		}
		return addEvent(ctx, tx, in.ID, "", StatusSucceeded, "sandbox top-up", "system")
	})
	if err != nil {
		return nil, err
	}
	return s.Get(ctx, userID, in.ID)
}

// partnerID gives each rail a stable owner id for its clearing account.
func partnerID(rail string) uuid.UUID {
	return uuid.NewSHA1(uuid.NameSpaceURL, []byte("https://gebeya.one/partners/"+rail))
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func (s *Service) requireActive(ctx context.Context, db platform.DB, userID uuid.UUID) error {
	var status string
	err := db.QueryRow(ctx, `SELECT status FROM identity.users WHERE id = $1`, userID).Scan(&status)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && status != "active") {
		return ErrPayerInactive
	}
	return err
}

func addEvent(ctx context.Context, tx pgx.Tx, intentID uuid.UUID, from, to Status, reason, actor string) error {
	_, err := tx.Exec(ctx, `
		INSERT INTO payments.intent_events (intent_id, from_status, to_status, reason, actor)
		VALUES ($1, NULLIF($2, '')::payments.intent_status, $3, NULLIF($4, ''), $5)`,
		intentID, string(from), string(to), reason, actor)
	return err
}

func recordDecision(ctx context.Context, tx pgx.Tx, intentID uuid.UUID, d Decision) error {
	rules := d.Rules
	if rules == nil {
		rules = []string{}
	}
	_, err := tx.Exec(ctx, `
		INSERT INTO risk.decisions (intent_id, score, model_version, rules_hit, action)
		VALUES ($1, $2, $3, $4, $5)`, intentID, d.Score, d.Model, rules, string(d.Action))
	if err != nil {
		return fmt.Errorf("record risk decision: %w", err)
	}
	return nil
}
