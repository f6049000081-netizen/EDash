// Package outbox publishes rows of platform.outbox, written in the same transaction as the state change
// they describe, to the event bus.
//
// Delivery is at least once: a row is marked published only after the publisher accepted it, so a crash
// between the two re-sends it. Consumers must treat Message.ID as an idempotency key.
//
// Order is preserved. Only one relay works at a time (a transaction-scoped advisory lock), it takes rows
// in id order, and it stops a batch at the first failure, so a later event for the same payment is never
// published before an earlier one.
package outbox

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Message is one outbox row.
type Message struct {
	ID          int64 // platform.outbox.id: unique, increasing; the consumer's idempotency key
	Topic       string
	AggregateID uuid.UUID // the partition key: events about one payment or user stay in order
	Payload     json.RawMessage
	CreatedAt   time.Time
}

// Publisher delivers messages in the order given. It returns how many of them, from the start, were
// delivered; the relay marks exactly those as published and retries the rest later.
type Publisher interface {
	Publish(ctx context.Context, msgs []Message) (int, error)
}

// relayLock is the advisory lock key that keeps a single relay active.
const relayLock = 0x67316f7574626f78 // "g1outbox"

type Relay struct {
	Pool      *pgxpool.Pool
	Publisher Publisher
	Batch     int           // rows per transaction (default 100)
	Interval  time.Duration // pause when the outbox is empty or publishing failed (default 1s)
	Log       *slog.Logger
}

// RunOnce publishes up to one batch. It returns how many rows were marked published. When another relay
// holds the lock it returns 0 and does nothing.
func (r *Relay) RunOnce(ctx context.Context) (int, error) {
	batch := r.batch()
	tx, err := r.Pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx)

	var locked bool
	if err := tx.QueryRow(ctx, `SELECT pg_try_advisory_xact_lock($1)`, int64(relayLock)).Scan(&locked); err != nil {
		return 0, err
	}
	if !locked {
		return 0, nil
	}
	rows, err := tx.Query(ctx, `
		SELECT id, topic, aggregate_id, payload, created_at FROM platform.outbox
		WHERE published_at IS NULL ORDER BY id LIMIT $1`, batch)
	if err != nil {
		return 0, err
	}
	msgs, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Message, error) {
		var m Message
		err := row.Scan(&m.ID, &m.Topic, &m.AggregateID, &m.Payload, &m.CreatedAt)
		return m, err
	})
	if err != nil || len(msgs) == 0 {
		return 0, err
	}

	n, pubErr := r.Publisher.Publish(ctx, msgs)
	if n < 0 || n > len(msgs) {
		return 0, errors.New("outbox: publisher reported an impossible count")
	}
	if n > 0 {
		ids := make([]int64, n)
		for i := range ids {
			ids[i] = msgs[i].ID
		}
		if _, err := tx.Exec(ctx, `UPDATE platform.outbox SET published_at = now() WHERE id = ANY($1)`, ids); err != nil {
			return 0, err
		}
		if err := tx.Commit(ctx); err != nil {
			return 0, err // the rows will be sent again: consumers dedupe on Message.ID
		}
	}
	return n, pubErr
}

// Run publishes until ctx ends. It keeps going straight away while batches come back full, and waits
// Interval when the outbox is empty or a publish failed.
func (r *Relay) Run(ctx context.Context) {
	wait := r.Interval
	if wait <= 0 {
		wait = time.Second
	}
	for {
		n, err := r.RunOnce(ctx)
		if err != nil && ctx.Err() == nil && r.Log != nil {
			r.Log.Warn("outbox relay", "err", err, "published", n)
		}
		full := err == nil && n >= r.batch()
		if !full {
			select {
			case <-ctx.Done():
				return
			case <-time.After(wait):
			}
		}
		if ctx.Err() != nil {
			return
		}
	}
}

func (r *Relay) batch() int {
	if r.Batch <= 0 {
		return 100
	}
	return r.Batch
}

// Handler consumes one message. It must be idempotent on Message.ID.
type Handler interface {
	Handle(ctx context.Context, m Message) error
}

// Direct delivers messages to in-process handlers, in order, without a broker. The dev server uses it when
// no Kafka brokers are configured; each handler then sees every topic.
type Direct struct {
	Handlers []Handler
}

func (d Direct) Publish(ctx context.Context, msgs []Message) (int, error) {
	for i, m := range msgs {
		for _, h := range d.Handlers {
			if err := h.Handle(ctx, m); err != nil {
				return i, err
			}
		}
	}
	return len(msgs), nil
}
