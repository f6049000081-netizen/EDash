package ops

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"superapp/backend/internal/merchants"
	"superapp/backend/internal/money"
)

// ReconResolutions are what staff can record when closing a reconciliation exception. None of them moves
// money: if the ledger needs correcting, that is a separate change with a second person.
var ReconResolutions = map[string]string{
	"timing":            "Timing: on the partner's statement for another day",
	"partner_corrected": "The partner corrected their statement",
	"settled_since":     "Our payment has settled since",
	"customer_refunded": "The partner refunded the customer",
	"duplicate":         "Duplicate of another exception",
	"other":             "Other (explained in the note)",
}

// ReconStatement is one imported partner statement and how it matched.
type ReconStatement struct {
	ID           uuid.UUID
	Rail         string
	BusinessDate time.Time
	Lines        int
	Matched      int
	Exceptions   int
	Open         int
	ImportedAt   time.Time
}

// ReconException is a statement line that didn't match, or a payment of ours the statement doesn't have.
type ReconException struct {
	ID           uuid.UUID
	StatementID  uuid.UUID
	Rail         string
	BusinessDate time.Time
	Kind         string
	PartnerRef   string
	PartnerAt    *time.Time
	Partner      *money.Money // what the partner says (nil when the statement doesn't have it)
	IntentID     *uuid.UUID
	Ours         *money.Money // what we recorded (nil when we have no record)
	OurStatus    string       // the payment's status now, which may have changed since the import
	Customer     string       // name, or masked phone
	Status       string
	Resolution   string
	Note         string
	ResolvedBy   string
	ResolvedAt   *time.Time
	CreatedAt    time.Time
}

// ReconStatements lists the latest statements, newest business day first.
func (s *Service) ReconStatements(ctx context.Context, limit int) ([]ReconStatement, error) {
	rows, err := s.Pool.Query(ctx, `
		SELECT p.id, p.rail::text, p.business_date, p.line_count,
		       (SELECT count(*) FROM recon.statement_lines l WHERE l.statement_id = p.id AND l.match_status = 'matched'),
		       (SELECT count(*) FROM recon.exceptions e WHERE e.statement_id = p.id),
		       (SELECT count(*) FROM recon.exceptions e WHERE e.statement_id = p.id AND e.status = 'open'),
		       p.imported_at
		FROM recon.partner_statements p ORDER BY p.business_date DESC, p.rail LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (ReconStatement, error) {
		var x ReconStatement
		err := r.Scan(&x.ID, &x.Rail, &x.BusinessDate, &x.Lines, &x.Matched, &x.Exceptions, &x.Open, &x.ImportedAt)
		return x, err
	})
}

// ReconExceptions lists open exceptions, oldest business day first, or the latest resolved ones.
func (s *Service) ReconExceptions(ctx context.Context, open bool, limit int) ([]ReconException, error) {
	order := "p.business_date, e.created_at"
	if !open {
		order = "e.resolved_at DESC"
	}
	rows, err := s.Pool.Query(ctx, `
		SELECT e.id, e.statement_id, p.rail::text, p.business_date, e.kind,
		       COALESCE(l.partner_ref, i.partner_ref, ''), l.occurred_at, l.amount_minor, l.currency::text,
		       e.intent_id, i.amount_minor, i.currency::text, COALESCE(i.status::text, ''),
		       COALESCE(u.display_name, ''), COALESCE(u.phone_e164, ''),
		       e.status, COALESCE(e.resolution, ''), COALESCE(e.resolution_note, ''), COALESCE(st.full_name, ''),
		       e.resolved_at, e.created_at
		FROM recon.exceptions e
		JOIN recon.partner_statements p ON p.id = e.statement_id
		LEFT JOIN recon.statement_lines l ON l.id = e.line_id
		LEFT JOIN payments.intents i ON i.id = e.intent_id
		LEFT JOIN identity.users u ON u.id = i.payer_user_id
		LEFT JOIN admin.staff st ON st.id = e.resolved_by
		WHERE (e.status = 'open') = $1
		ORDER BY `+order+` LIMIT $2`, open, limit)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (ReconException, error) {
		var x ReconException
		var lineMinor, ourMinor *int64
		var lineCur, ourCur *string
		var name, phone string
		err := r.Scan(&x.ID, &x.StatementID, &x.Rail, &x.BusinessDate, &x.Kind, &x.PartnerRef, &x.PartnerAt, &lineMinor, &lineCur,
			&x.IntentID, &ourMinor, &ourCur, &x.OurStatus, &name, &phone,
			&x.Status, &x.Resolution, &x.Note, &x.ResolvedBy, &x.ResolvedAt, &x.CreatedAt)
		if lineMinor != nil && lineCur != nil {
			m := money.New(*lineMinor, money.Currency(*lineCur))
			x.Partner = &m
		}
		if ourMinor != nil && ourCur != nil {
			m := money.New(*ourMinor, money.Currency(*ourCur))
			x.Ours = &m
		}
		x.Customer = name
		if x.Customer == "" && phone != "" {
			x.Customer = MaskPhone(phone)
		}
		return x, err
	})
}

// resolveReconTx closes an open exception with a reason. It records what happened and moves no money.
func resolveReconTx(ctx context.Context, tx pgx.Tx, staff uuid.UUID, id uuid.UUID, reason, note string) error {
	if _, ok := ReconResolutions[reason]; !ok {
		return &merchants.ConflictError{Msg: "Choose what happened."}
	}
	var status string
	err := tx.QueryRow(ctx, `SELECT status FROM recon.exceptions WHERE id = $1 FOR UPDATE`, id).Scan(&status)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if status != "open" {
		return &merchants.ConflictError{Msg: "This exception is already resolved."}
	}
	_, err = tx.Exec(ctx, `
		UPDATE recon.exceptions SET status = 'resolved', resolution = $2, resolution_note = $3, resolved_by = $4, resolved_at = now()
		WHERE id = $1`, id, reason, note, staff)
	return err
}
