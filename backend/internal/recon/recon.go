// Package recon matches a partner's daily statement against our own records.
//
// Every line on the statement is one payment the partner says happened. Each one is matched by the
// partner's reference to an intent on that rail:
//
//	matched           same reference, same amount and currency, and our intent succeeded
//	amount_mismatch   same reference, different amount or currency
//	unmatched         same reference, but our intent isn't succeeded (held, failed or still processing)
//	missing_internal  the partner has a payment we have no record of (for example a lost notification)
//
// The other direction is reported too: intents we marked succeeded on that business day that the
// statement doesn't mention (MissingAtPartner). Nothing here moves money; operations staff resolve
// exceptions through the admin console, with maker-checker approval for any ledger correction.
package recon

import (
	"context"
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"superapp/backend/internal/money"
	"superapp/backend/internal/platform"
)

// EAT is Ethiopia's time zone (UTC+3, no daylight saving). Business days run midnight to midnight EAT.
var EAT = time.FixedZone("EAT", 3*60*60)

type Line struct {
	PartnerRef string
	Amount     money.Money
	OccurredAt time.Time
}

type Report struct {
	StatementID      uuid.UUID
	Rail             string
	BusinessDate     string
	Lines            int
	Matched          int
	AmountMismatch   int
	Unmatched        int
	MissingInternal  int
	MissingAtPartner []uuid.UUID // our succeeded intents absent from the statement
	StatementTotal   money.Money
	MatchedTotal     money.Money
}

// Clean reports whether every line matched and nothing is missing on either side.
func (r *Report) Clean() bool {
	return r.Matched == r.Lines && len(r.MissingAtPartner) == 0
}

var ErrAlreadyImported = errors.New("statement already imported for this rail and day, or this exact file was imported before")

// Import stores a statement and its lines and matches them, all in one transaction.
// businessDate is a calendar day in EAT; fileSHA256 is the hex digest of the original file.
func Import(ctx context.Context, pool *pgxpool.Pool, rail string, businessDate time.Time, fileSHA256 string, lines []Line) (*Report, error) {
	day := time.Date(businessDate.Year(), businessDate.Month(), businessDate.Day(), 0, 0, 0, 0, EAT)
	rep := &Report{Rail: rail, BusinessDate: day.Format("2006-01-02"), Lines: len(lines)}
	if len(lines) > 0 {
		rep.StatementTotal = money.New(0, lines[0].Amount.Currency)
		rep.MatchedTotal = money.New(0, lines[0].Amount.Currency)
	}
	seen := map[string]bool{}
	for _, l := range lines {
		if l.PartnerRef == "" || !l.Amount.Currency.Valid() || l.Amount.AmountMinor <= 0 {
			return nil, fmt.Errorf("invalid statement line %+v", l)
		}
		if seen[l.PartnerRef] {
			return nil, fmt.Errorf("partner reference %s appears twice in the statement", l.PartnerRef)
		}
		seen[l.PartnerRef] = true
	}

	err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		id, err := uuid.NewV7()
		if err != nil {
			return err
		}
		rep.StatementID = id
		if _, err := tx.Exec(ctx, `
			INSERT INTO recon.partner_statements (id, rail, business_date, file_sha256, line_count)
			VALUES ($1, $2, $3, $4, $5)`, id, rail, rep.BusinessDate, fileSHA256, len(lines)); err != nil {
			if platform.IsUniqueViolation(err) {
				return ErrAlreadyImported
			}
			return err
		}

		for _, l := range lines {
			if total, err := rep.StatementTotal.Add(l.Amount); err == nil {
				rep.StatementTotal = total
			}
			var intentID uuid.NullUUID
			var amount int64
			var cur, status string
			err := tx.QueryRow(ctx, `
				SELECT id, amount_minor, currency, status FROM payments.intents
				WHERE rail = $1 AND partner_ref = $2`, rail, l.PartnerRef).Scan(&intentID, &amount, &cur, &status)
			match := "missing_internal"
			switch {
			case errors.Is(err, pgx.ErrNoRows):
				rep.MissingInternal++
			case err != nil:
				return err
			case amount != l.Amount.AmountMinor || money.Currency(cur) != l.Amount.Currency:
				match = "amount_mismatch"
				rep.AmountMismatch++
			case status != "succeeded":
				match = "unmatched"
				rep.Unmatched++
			default:
				match = "matched"
				rep.Matched++
				if total, err := rep.MatchedTotal.Add(l.Amount); err == nil {
					rep.MatchedTotal = total
				}
			}
			if _, err := tx.Exec(ctx, `
				INSERT INTO recon.statement_lines (statement_id, partner_ref, amount_minor, currency, occurred_at, matched_intent_id, match_status)
				VALUES ($1, $2, $3, $4, $5, $6, $7)`,
				id, l.PartnerRef, l.Amount.AmountMinor, string(l.Amount.Currency), l.OccurredAt, intentID, match); err != nil {
				return err
			}
		}

		rows, err := tx.Query(ctx, `
			SELECT i.id FROM payments.intents i
			WHERE i.rail = $1 AND i.status = 'succeeded' AND i.completed_at >= $2 AND i.completed_at < $3
			  AND NOT EXISTS (SELECT 1 FROM recon.statement_lines s WHERE s.statement_id = $4 AND s.partner_ref = i.partner_ref)
			ORDER BY i.completed_at`, rail, day, day.AddDate(0, 0, 1), id)
		if err != nil {
			return err
		}
		rep.MissingAtPartner, err = pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
		if err != nil {
			return err
		}
		return platform.WriteOutbox(ctx, tx, "recon.statement_imported", id, map[string]any{
			"statement_id": id, "rail": rail, "business_date": rep.BusinessDate, "lines": rep.Lines,
			"matched": rep.Matched, "amount_mismatch": rep.AmountMismatch, "unmatched": rep.Unmatched,
			"missing_internal": rep.MissingInternal, "missing_at_partner": len(rep.MissingAtPartner),
		})
	})
	if err != nil {
		return nil, err
	}
	return rep, nil
}

// ParseCSV reads our normalised statement format:
//
//	partner_ref,amount,currency,occurred_at
//	TB123,100.00,ETB,2026-11-20T09:15:00+03:00
//
// Each partner's own statement layout is converted to this by a small mapper once its real file
// format is known; telebirr's comes with the merchant agreement.
func ParseCSV(r io.Reader) ([]Line, error) {
	cr := csv.NewReader(r)
	cr.FieldsPerRecord = 4
	header, err := cr.Read()
	if err != nil {
		return nil, fmt.Errorf("read header: %w", err)
	}
	want := []string{"partner_ref", "amount", "currency", "occurred_at"}
	for i, h := range header {
		if strings.TrimSpace(strings.TrimPrefix(h, "\uFEFF")) != want[i] {
			return nil, fmt.Errorf("header must be %s", strings.Join(want, ","))
		}
	}
	var lines []Line
	for n := 2; ; n++ {
		rec, err := cr.Read()
		if errors.Is(err, io.EOF) {
			return lines, nil
		}
		if err != nil {
			return nil, fmt.Errorf("line %d: %w", n, err)
		}
		cur := money.Currency(strings.TrimSpace(rec[2]))
		if !cur.Valid() {
			return nil, fmt.Errorf("line %d: unknown currency %q", n, rec[2])
		}
		amt, err := money.ParseDecimal(strings.TrimSpace(rec[1]), cur)
		if err != nil {
			return nil, fmt.Errorf("line %d: amount %q: %w", n, rec[1], err)
		}
		at, err := time.Parse(time.RFC3339, strings.TrimSpace(rec[3]))
		if err != nil {
			return nil, fmt.Errorf("line %d: occurred_at %q: %w", n, rec[3], err)
		}
		lines = append(lines, Line{PartnerRef: strings.TrimSpace(rec[0]), Amount: amt, OccurredAt: at})
	}
}
