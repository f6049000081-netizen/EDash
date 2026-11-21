package outbox

import (
	"context"
	"log/slog"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/twmb/franz-go/pkg/kgo"
)

// Each record carries the outbox id in this header, so consumers can drop redeliveries.
const idHeader = "outbox-id"

// Kafka publishes to Kafka. The record key is the aggregate id, so events about one payment or user land
// on one partition and stay in order. The client should use the defaults for acks (all in-sync replicas)
// and idempotent production.
type Kafka struct {
	Client *kgo.Client
}

// NewKafkaClient connects a producer. Topics are created on first use when the cluster allows it.
func NewKafkaClient(brokers []string, opts ...kgo.Opt) (*kgo.Client, error) {
	return kgo.NewClient(append([]kgo.Opt{kgo.SeedBrokers(brokers...), kgo.AllowAutoTopicCreation()}, opts...)...)
}

func (k Kafka) Publish(ctx context.Context, msgs []Message) (int, error) {
	recs := make([]*kgo.Record, len(msgs))
	for i, m := range msgs {
		recs[i] = &kgo.Record{
			Topic:     m.Topic,
			Key:       []byte(m.AggregateID.String()),
			Value:     m.Payload,
			Timestamp: m.CreatedAt,
			Headers:   []kgo.RecordHeader{{Key: idHeader, Value: []byte(strconv.FormatInt(m.ID, 10))}},
		}
	}
	results := k.Client.ProduceSync(ctx, recs...)
	failed := map[*kgo.Record]error{}
	for _, r := range results {
		if r.Err != nil {
			failed[r.Record] = r.Err
		}
	}
	// Results arrive in completion order; report the delivered prefix in outbox order.
	for i, rec := range recs {
		if err, bad := failed[rec]; bad {
			return i, err
		}
	}
	return len(msgs), nil
}

// NewKafkaConsumer joins a consumer group for the given topics. Offsets are committed only after the
// handler succeeds, and a new group starts from the oldest retained event.
func NewKafkaConsumer(brokers []string, group string, topics []string, opts ...kgo.Opt) (*kgo.Client, error) {
	return kgo.NewClient(append([]kgo.Opt{
		kgo.SeedBrokers(brokers...),
		kgo.ConsumerGroup(group),
		kgo.ConsumeTopics(topics...),
		kgo.DisableAutoCommit(),
		kgo.ConsumeResetOffset(kgo.NewOffset().AtStart()),
		kgo.AllowAutoTopicCreation(),
	}, opts...)...)
}

// Consume hands each record to h, retrying a failing record with backoff until it succeeds or ctx ends,
// so one bad moment in the database doesn't skip a notification. It returns when ctx ends.
func Consume(ctx context.Context, client *kgo.Client, h Handler, log *slog.Logger) error {
	for {
		fetches := client.PollFetches(ctx)
		if ctx.Err() != nil || fetches.IsClientClosed() {
			return ctx.Err()
		}
		fetches.EachError(func(topic string, part int32, err error) {
			if log != nil {
				log.Warn("kafka fetch", "topic", topic, "partition", part, "err", err)
			}
		})
		var handled []*kgo.Record
		iter := fetches.RecordIter()
		for !iter.Done() {
			rec := iter.Next()
			m := FromRecord(rec)
			for wait := time.Second; ; wait = min(wait*2, 30*time.Second) {
				err := h.Handle(ctx, m)
				if err == nil {
					break
				}
				if log != nil {
					log.Warn("event handler failed; retrying", "topic", m.Topic, "outbox_id", m.ID, "err", err)
				}
				select {
				case <-ctx.Done():
					_ = client.CommitRecords(context.Background(), handled...)
					return ctx.Err()
				case <-time.After(wait):
				}
			}
			handled = append(handled, rec)
		}
		if len(handled) > 0 {
			if err := client.CommitRecords(ctx, handled...); err != nil && log != nil {
				log.Warn("kafka commit", "err", err) // the records come again; the handler is idempotent
			}
		}
	}
}

// FromRecord turns a Kafka record written by [Kafka.Publish] back into a Message.
func FromRecord(rec *kgo.Record) Message {
	m := Message{Topic: rec.Topic, Payload: rec.Value, CreatedAt: rec.Timestamp}
	m.AggregateID, _ = uuid.ParseBytes(rec.Key)
	for _, h := range rec.Headers {
		if h.Key == idHeader {
			m.ID, _ = strconv.ParseInt(string(h.Value), 10, 64)
		}
	}
	return m
}
