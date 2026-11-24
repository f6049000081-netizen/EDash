// Package outboxtest helps tests that run an outbox relay. Every package's tests share one database and
// Go runs packages in parallel, so two tests draining the whole outbox at once would take each other's
// events. Tests that run a relay hold Exclusive while they do.
package outboxtest

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Exclusive holds a database lock that every relay-running test takes, until the test ends. The lock is
// on its own connection, not one borrowed from [pool], so a test that closes its pool with defer (which
// runs before cleanups) doesn't wait on it.
func Exclusive(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	ctx := context.Background()
	conn, err := pgx.ConnectConfig(ctx, pool.Config().ConnConfig.Copy())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock(hashtextextended('test:outbox-relay', 0))`); err != nil {
		_ = conn.Close(ctx)
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close(context.Background()) }) // closing the session releases the lock
}
