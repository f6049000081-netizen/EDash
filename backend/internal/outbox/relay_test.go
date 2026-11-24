package outbox_test

// Integration tests against PostgreSQL (TEST_DATABASE_URL). The Kafka round trip also needs
// TEST_KAFKA_BROKERS. Other packages' tests write outbox rows into the same database while these run,
// so every assertion looks only at this test's own aggregate ids.

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"superapp/backend/internal/outbox"
	"superapp/backend/internal/outbox/outboxtest"
	"superapp/backend/internal/platform"
)

var (
	poolOnce sync.Once
	pool     *pgxpool.Pool
	poolErr  error
)

func setup(t *testing.T) context.Context {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}
	poolOnce.Do(func() { pool, poolErr = pgxpool.New(context.Background(), dsn) })
	if poolErr != nil {
		t.Fatal(poolErr)
	}
	outboxtest.Exclusive(t, pool)
	return context.Background()
}

// write adds outbox rows the way services do: in a transaction, through platform.WriteOutbox.
func write(t *testing.T, ctx context.Context, agg uuid.UUID, topics ...string) {
	t.Helper()
	err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		for i, topic := range topics {
			if err := platform.WriteOutbox(ctx, tx, topic, agg, map[string]any{"seq": i}); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// recorder is a publisher that keeps this test's messages and can fail on one of them.
type recorder struct {
	mine   map[uuid.UUID]bool
	failAt string // fail when reaching this test's message with this topic
	got    []outbox.Message
}

func (r *recorder) Publish(_ context.Context, msgs []outbox.Message) (int, error) {
	for i, m := range msgs {
		if r.mine[m.AggregateID] {
			if m.Topic == r.failAt {
				return i, errors.New("broker unavailable")
			}
			r.got = append(r.got, m)
		}
	}
	return len(msgs), nil
}

func (r *recorder) topics() string {
	var s []string
	for _, m := range r.got {
		s = append(s, m.Topic)
	}
	return strings.Join(s, ",")
}

func drain(t *testing.T, ctx context.Context, relay *outbox.Relay) error {
	t.Helper()
	for i := 0; i < 1000; i++ {
		n, err := relay.RunOnce(ctx)
		if err != nil || n == 0 {
			return err
		}
	}
	t.Fatal("outbox never drained")
	return nil
}

func TestRelayKeepsOrderAndResumesAfterFailure(t *testing.T) {
	ctx := setup(t)
	agg := uuid.New()
	write(t, ctx, agg, "t.a", "t.b", "t.c")
	write(t, ctx, agg, "t.d")

	rec := &recorder{mine: map[uuid.UUID]bool{agg: true}, failAt: "t.c"}
	relay := &outbox.Relay{Pool: pool, Publisher: rec, Batch: 2}
	if err := drain(t, ctx, relay); err == nil {
		t.Fatal("expected the broker failure to surface")
	}
	if rec.topics() != "t.a,t.b" {
		t.Fatalf("before the failure: %s", rec.topics())
	}
	var unpublished int
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM platform.outbox WHERE aggregate_id = $1 AND published_at IS NULL`, agg).Scan(&unpublished)
	if unpublished != 2 {
		t.Fatalf("%d rows left unpublished, want t.c and t.d", unpublished)
	}

	rec.failAt = ""
	if err := drain(t, ctx, relay); err != nil {
		t.Fatal(err)
	}
	if rec.topics() != "t.a,t.b,t.c,t.d" {
		t.Errorf("order after resuming: %s", rec.topics())
	}
	var seq []int
	for _, m := range rec.got {
		var p struct{ Seq int }
		_ = json.Unmarshal(m.Payload, &p)
		seq = append(seq, p.Seq)
		if m.ID == 0 || m.CreatedAt.IsZero() {
			t.Errorf("message missing id or time: %+v", m)
		}
	}
	if len(seq) != 4 || seq[2] != 2 || seq[3] != 0 {
		t.Errorf("payloads %v", seq)
	}
}

func TestOnlyOneRelayRunsAtATime(t *testing.T) {
	ctx := setup(t)
	agg := uuid.New()
	write(t, ctx, agg, "t.lock")

	// Another relay is mid-batch: it holds the advisory lock in its transaction.
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, int64(0x67316f7574626f78)); err != nil {
		t.Fatal(err)
	}
	rec := &recorder{mine: map[uuid.UUID]bool{agg: true}}
	relay := &outbox.Relay{Pool: pool, Publisher: rec}
	if n, err := relay.RunOnce(ctx); n != 0 || err != nil || len(rec.got) != 0 {
		t.Fatalf("second relay ran: %d %v %v", n, err, rec.got)
	}
	_ = tx.Rollback(ctx)
	if err := drain(t, ctx, relay); err != nil || rec.topics() != "t.lock" {
		t.Fatalf("after the lock is released: %v %s", err, rec.topics())
	}
}

type failingHandler struct{ on uuid.UUID }

func (f failingHandler) Handle(_ context.Context, m outbox.Message) error {
	if m.AggregateID == f.on {
		return errors.New("db down")
	}
	return nil
}

func TestDirectStopsAtFirstHandlerError(t *testing.T) {
	bad := uuid.New()
	msgs := []outbox.Message{{AggregateID: uuid.New()}, {AggregateID: bad}, {AggregateID: uuid.New()}}
	n, err := outbox.Direct{Handlers: []outbox.Handler{failingHandler{on: bad}}}.Publish(context.Background(), msgs)
	if n != 1 || err == nil {
		t.Errorf("delivered %d, err %v; want 1 and an error", n, err)
	}
}

// TestKafkaRoundTrip runs against a real broker, e.g.
//
//	docker run -d --name g1kafka --network g1net apache/kafka-native:3.8.0
//	TEST_KAFKA_BROKERS=g1kafka:9092
func TestKafkaRoundTrip(t *testing.T) {
	brokers := os.Getenv("TEST_KAFKA_BROKERS")
	if brokers == "" {
		t.Skip("TEST_KAFKA_BROKERS not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	topic := "test." + strings.ReplaceAll(uuid.NewString(), "-", "")[:12]
	agg := uuid.New()
	producer, err := outbox.NewKafkaClient(strings.Split(brokers, ","))
	if err != nil {
		t.Fatal(err)
	}
	defer producer.Close()
	var msgs []outbox.Message
	for i := 0; i < 5; i++ {
		msgs = append(msgs, outbox.Message{ID: int64(1000 + i), Topic: topic, AggregateID: agg,
			Payload: json.RawMessage(`{"seq":` + string(rune('0'+i)) + `}`), CreatedAt: time.Now().Truncate(time.Millisecond)})
	}
	if n, err := (outbox.Kafka{Client: producer}).Publish(ctx, msgs); n != 5 || err != nil {
		t.Fatalf("publish: %d %v", n, err)
	}

	consumer, err := outbox.NewKafkaConsumer(strings.Split(brokers, ","), "test-"+uuid.NewString(), []string{topic})
	if err != nil {
		t.Fatal(err)
	}
	defer consumer.Close()
	h := &collect{want: 5, done: make(chan struct{})}
	cctx, stop := context.WithCancel(ctx)
	go func() { _ = outbox.Consume(cctx, consumer, h, nil) }()
	select {
	case <-h.done:
	case <-ctx.Done():
		t.Fatalf("got %d of 5 records", len(h.got))
	}
	stop()
	for i, m := range h.got {
		if m.ID != int64(1000+i) || m.AggregateID != agg || m.Topic != topic || string(m.Payload) != string(msgs[i].Payload) {
			t.Errorf("record %d: %+v", i, m)
		}
	}
}

type collect struct {
	mu   sync.Mutex
	got  []outbox.Message
	want int
	done chan struct{}
}

func (c *collect) Handle(_ context.Context, m outbox.Message) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.got = append(c.got, m)
	if len(c.got) == c.want {
		close(c.done)
	}
	return nil
}
