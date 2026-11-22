// Package kyb keeps the documents a business sends to be verified (know your business): its TIN
// certificate, trade licence and the owner's ID. Owners upload them from the app; operations staff open
// and decide them in the console. Files are checked by their content, encrypted, and kept in object storage.
package kyb

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"superapp/backend/internal/platform"
)

const (
	MaxSize = 10 << 20 // 10 MB: a phone photo or a scanned PDF
	MinSize = 1 << 10
	// MaxOpen is how many documents a merchant can have that aren't replaced.
	MaxOpen = 20
)

var (
	ErrNotFound  = errors.New("kyb: not found")
	ErrForbidden = errors.New("kyb: only the owner can do this")
)

type ValidationError struct{ Msg string }

func (e *ValidationError) Error() string { return e.Msg }

// Kinds of document, with the names staff see.
var Kinds = map[string]string{
	"tin_certificate": "TIN certificate",
	"trade_licence":   "Trade licence",
	"owner_id":        "Owner's ID (Fayda, kebele ID or passport)",
	"other":           "Other document",
}

type Document struct {
	ID          uuid.UUID
	MerchantID  uuid.UUID
	Kind        string
	ContentType string
	Size        int
	SHA256      string
	Status      string
	UploadedAt  time.Time
	ReviewedAt  *time.Time
	ReviewNote  string
	objectKey   string
	keyID       string
}

type Service struct {
	Pool   *pgxpool.Pool
	Store  Store
	Sealer *Sealer
	Now    func() time.Time
}

func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

// Sniff names a file's type from its first bytes. Only JPEG, PNG and PDF are taken; what the client
// claims doesn't count.
func Sniff(b []byte) string {
	switch {
	case bytes.HasPrefix(b, []byte{0xFF, 0xD8, 0xFF}):
		return "image/jpeg"
	case bytes.HasPrefix(b, []byte("\x89PNG\r\n\x1a\n")):
		return "image/png"
	case bytes.HasPrefix(b, []byte("%PDF-")):
		return "application/pdf"
	}
	return ""
}

const docCols = `id, merchant_id, kind, content_type, size_bytes, sha256, status, uploaded_at, reviewed_at,
	COALESCE(review_note, ''), object_key, key_id`

func scanDoc(row pgx.Row) (*Document, error) {
	d := &Document{}
	err := row.Scan(&d.ID, &d.MerchantID, &d.Kind, &d.ContentType, &d.Size, &d.SHA256, &d.Status, &d.UploadedAt,
		&d.ReviewedAt, &d.ReviewNote, &d.objectKey, &d.keyID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	return d, err
}

func collect(rows pgx.Rows, err error) ([]Document, error) {
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Document
	for rows.Next() {
		d, err := scanDoc(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *d)
	}
	return out, rows.Err()
}

// role is the user's role at the merchant; strangers get ErrNotFound.
func role(ctx context.Context, db platform.DB, user, merchant uuid.UUID) (role, status string, err error) {
	err = db.QueryRow(ctx, `
		SELECT mm.role, m.status FROM merchant.members mm JOIN merchant.merchants m ON m.id = mm.merchant_id
		WHERE mm.merchant_id = $1 AND mm.user_id = $2`, merchant, user).Scan(&role, &status)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", "", ErrNotFound
	}
	return role, status, err
}

// Upload stores a document for the owner's merchant. Sending the same file again returns the document
// already stored. A new document of a kind replaces the earlier ones of that kind (except "other").
func (s *Service) Upload(ctx context.Context, user, merchant uuid.UUID, kind string, data []byte) (*Document, error) {
	r, status, err := role(ctx, s.Pool, user, merchant)
	if err != nil {
		return nil, err
	}
	if r != "owner" {
		return nil, ErrForbidden
	}
	if status == "closed" {
		return nil, &ValidationError{"This application was closed. Apply again to send documents."}
	}
	if Kinds[kind] == "" {
		return nil, &ValidationError{"Choose what the document is."}
	}
	if len(data) > MaxSize {
		return nil, &ValidationError{"The file is over 10 MB. Take the photo again at a lower resolution."}
	}
	ct := Sniff(data)
	if ct == "" || len(data) < MinSize {
		return nil, &ValidationError{"Send a photo (JPEG or PNG) or a PDF."}
	}
	sum := sha256.Sum256(data)
	hash := hex.EncodeToString(sum[:])
	if d, err := scanDoc(s.Pool.QueryRow(ctx, `SELECT `+docCols+` FROM merchant.documents
		WHERE merchant_id = $1 AND sha256 = $2 AND status <> 'replaced'`, merchant, hash)); err == nil {
		return d, nil
	} else if !errors.Is(err, ErrNotFound) {
		return nil, err
	}

	id := uuid.New()
	key := "kyb/" + merchant.String() + "/" + id.String()
	keyID, blob := s.Sealer.Seal(key, data)
	if err := s.Store.Put(ctx, key, blob); err != nil {
		return nil, err
	}
	var doc *Document
	err = pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		// One upload at a time per merchant, so the count and the replacing can't race.
		if _, err := tx.Exec(ctx, `SELECT 1 FROM merchant.merchants WHERE id = $1 FOR UPDATE`, merchant); err != nil {
			return err
		}
		var open int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM merchant.documents WHERE merchant_id = $1 AND status <> 'replaced'`,
			merchant).Scan(&open); err != nil {
			return err
		}
		if kind != "other" {
			if _, err := tx.Exec(ctx, `UPDATE merchant.documents SET status = 'replaced'
				WHERE merchant_id = $1 AND kind = $2 AND status IN ('submitted', 'rejected')`, merchant, kind); err != nil {
				return err
			}
		}
		if open >= MaxOpen {
			return &ValidationError{"This shop already has the most documents allowed. Contact support."}
		}
		d, err := scanDoc(tx.QueryRow(ctx, `
			INSERT INTO merchant.documents (id, merchant_id, kind, content_type, size_bytes, sha256, object_key, key_id,
				uploaded_by, uploaded_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10) RETURNING `+docCols,
			id, merchant, kind, ct, len(data), hash, key, keyID, user, s.now()))
		doc = d
		return err
	})
	if err != nil {
		_ = s.Store.Delete(context.WithoutCancel(ctx), key)
		var pg *pgconn.PgError
		if errors.As(err, &pg) && pg.Code == "23505" { // the same file, sent twice at once
			return scanDoc(s.Pool.QueryRow(ctx, `SELECT `+docCols+` FROM merchant.documents
				WHERE merchant_id = $1 AND sha256 = $2 AND status <> 'replaced'`, merchant, hash))
		}
		return nil, err
	}
	return doc, nil
}

// List shows the owner and managers the documents that count (not the replaced ones).
func (s *Service) List(ctx context.Context, user, merchant uuid.UUID) ([]Document, error) {
	r, _, err := role(ctx, s.Pool, user, merchant)
	if err != nil {
		return nil, err
	}
	if r != "owner" && r != "manager" {
		return nil, ErrForbidden
	}
	return collect(s.Pool.Query(ctx, `SELECT `+docCols+` FROM merchant.documents
		WHERE merchant_id = $1 AND status <> 'replaced' ORDER BY uploaded_at DESC`, merchant))
}

// ForMerchant is the staff view: every document, replaced ones included.
func (s *Service) ForMerchant(ctx context.Context, merchant uuid.UUID) ([]Document, error) {
	return collect(s.Pool.Query(ctx, `SELECT `+docCols+` FROM merchant.documents
		WHERE merchant_id = $1 ORDER BY (status = 'replaced'), uploaded_at DESC`, merchant))
}

// Open reads and decrypts a document for staff, and checks it is the file that was uploaded.
func (s *Service) Open(ctx context.Context, id uuid.UUID) (*Document, []byte, error) {
	d, err := scanDoc(s.Pool.QueryRow(ctx, `SELECT `+docCols+` FROM merchant.documents WHERE id = $1`, id))
	if err != nil {
		return nil, nil, err
	}
	blob, err := s.Store.Get(ctx, d.objectKey)
	if err != nil {
		return nil, nil, err
	}
	data, err := s.Sealer.Open(d.keyID, d.objectKey, blob)
	if err != nil {
		return nil, nil, err
	}
	if sum := sha256.Sum256(data); hex.EncodeToString(sum[:]) != d.SHA256 {
		return nil, nil, errors.New("kyb: stored file doesn't match its checksum")
	}
	return d, data, nil
}

// ReviewTx accepts or rejects a submitted document. A rejection needs a reason, which the owner sees.
func (s *Service) ReviewTx(ctx context.Context, tx pgx.Tx, id, staff uuid.UUID, accept bool, note string) (*Document, error) {
	d, err := scanDoc(tx.QueryRow(ctx, `SELECT `+docCols+` FROM merchant.documents WHERE id = $1 FOR UPDATE`, id))
	if err != nil {
		return nil, err
	}
	if d.Status != "submitted" {
		return nil, &ValidationError{"This document was already decided or replaced."}
	}
	if !accept && len([]rune(note)) < 10 {
		return nil, &ValidationError{"Say why it can't be accepted, so the owner can send a better one."}
	}
	status := map[bool]string{true: "accepted", false: "rejected"}[accept]
	d, err = scanDoc(tx.QueryRow(ctx, `UPDATE merchant.documents SET status = $2, reviewed_by = $3, reviewed_at = $4,
		review_note = NULLIF($5, '') WHERE id = $1 RETURNING `+docCols, id, status, staff, s.now(), note))
	if err != nil || accept {
		return d, err
	}
	return d, platform.WriteOutbox(ctx, tx, "merchant.document_rejected", d.MerchantID,
		map[string]any{"merchant_id": d.MerchantID, "document": d.Kind, "reason": note})
}

// MissingTx lists the documents an application still needs accepted before it can be approved: the TIN
// certificate and the owner's ID, and the trade licence when the application gave a licence number.
func MissingTx(ctx context.Context, db platform.DB, merchant uuid.UUID) ([]string, error) {
	rows, err := db.Query(ctx, `
		SELECT k FROM merchant.merchants m,
			unnest(ARRAY['tin_certificate', 'owner_id'] || CASE WHEN m.business_licence IS NOT NULL THEN ARRAY['trade_licence'] ELSE '{}' END) k
		WHERE m.id = $1 AND NOT EXISTS (
			SELECT 1 FROM merchant.documents d WHERE d.merchant_id = m.id AND d.kind = k AND d.status = 'accepted')
		ORDER BY array_position(ARRAY['tin_certificate', 'trade_licence', 'owner_id'], k)`, merchant)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[string])
}

// RequiredTx lists the kinds an application needs: the TIN certificate and the owner's ID, and the trade
// licence when the application gave a licence number.
func RequiredTx(ctx context.Context, db platform.DB, merchant uuid.UUID) ([]string, error) {
	var licence bool
	if err := db.QueryRow(ctx, `SELECT business_licence IS NOT NULL FROM merchant.merchants WHERE id = $1`, merchant).Scan(&licence); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	if licence {
		return []string{"tin_certificate", "trade_licence", "owner_id"}, nil
	}
	return []string{"tin_certificate", "owner_id"}, nil
}
