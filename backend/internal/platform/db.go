// Package platform holds cross-cutting pieces: database access, errors, idempotency, outbox, auth.
package platform

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// DB is satisfied by *pgxpool.Pool, *pgx.Conn and pgx.Tx, so read helpers work inside or outside a transaction.
type DB interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// IsUniqueViolation reports a Postgres unique_violation (SQLSTATE 23505).
func IsUniqueViolation(err error) bool {
	var pg *pgconn.PgError
	return errors.As(err, &pg) && pg.Code == "23505"
}

// WriteOutbox records a domain event in the same transaction as the state change that caused it.
// A relay process publishes unpublished rows to the event bus.
func WriteOutbox(ctx context.Context, tx pgx.Tx, topic string, aggregateID uuid.UUID, payload any) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx,
		`INSERT INTO platform.outbox (topic, aggregate_id, payload) VALUES ($1, $2, $3)`,
		topic, aggregateID, body)
	return err
}
