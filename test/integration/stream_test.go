//go:build integration

package integration

import (
	"context"
	"fmt"
	"reflect"
	"testing"
	"time"

	rstream "github.com/rabbitmq/rabbitmq-stream-go-client/pkg/stream"

	"github.com/DavidGodefroid/lockmq/internal/broker/stream"
)

const requestTimeout = 10 * time.Second

// storeNamedOffset stores an offset for a named consumer, as a real
// application would (admin credentials: lockmq never does this).
func storeNamedOffset(b *broker) error {
	env, err := rstream.NewEnvironment(rstream.NewEnvironmentOptions().
		SetHost("localhost").SetPort(b.Stream).SetUser(adminUser).SetPassword(adminPass))
	if err != nil {
		return fmt.Errorf("stream env: %w", err)
	}
	defer env.Close()
	return env.StoreOffset(namedRef, streamName, namedOffset)
}

func readers(b *broker) map[string]stream.Reader {
	return map[string]stream.Reader{
		"stream-protocol": stream.NewStreamProto("localhost", b.Stream, nil, roUser, []byte(roPass), nil),
		"amqp":            stream.NewAMQP("127.0.0.1", b.AMQP, nil, roUser, []byte(roPass), nil),
	}
}

func TestReadDoesNotMoveNamedConsumer(t *testing.T) {
	each(t, func(t *testing.T, b *broker) {
		sp := stream.NewStreamProto("localhost", b.Stream, nil, roUser, []byte(roPass), nil)
		ctx, cancel := context.WithTimeout(context.Background(), requestTimeout)
		defer cancel()
		before, err := sp.QueryOffset(ctx, "/", streamName, namedRef)
		if err != nil || before != namedOffset {
			t.Fatalf("offset before = %d, %v", before, err)
		}
		for name, r := range readers(b) {
			msgs, err := r.Read(ctx, "/", streamName, stream.From{Kind: "first"}, 20)
			if err != nil || len(msgs) == 0 {
				t.Fatalf("%s: %d messages, %v", name, len(msgs), err)
			}
		}
		after, err := sp.QueryOffset(ctx, "/", streamName, namedRef)
		if err != nil || after != before {
			t.Errorf("offset after = %d, %v (before %d)", after, err, before)
		}
		// Nothing removed: every message is still there.
		all, err := sp.Read(ctx, "/", streamName, stream.From{Kind: "first"}, 200)
		if err != nil || len(all) != streamCount {
			t.Errorf("stream holds %d messages, %v (want %d)", len(all), err, streamCount)
		}
	})
}

func TestReadEmptyStreamReturnsFast(t *testing.T) {
	each(t, func(t *testing.T, b *broker) {
		for name, r := range readers(b) {
			for _, c := range []struct {
				stream string
				from   stream.From
			}{
				{emptyStream, stream.From{Kind: "first"}},
				{emptyStream, stream.From{Kind: "last"}},
				{streamName, stream.From{Kind: "offset", Offset: 100000}},
			} {
				ctx, cancel := context.WithTimeout(context.Background(), requestTimeout)
				start := time.Now()
				msgs, err := r.Read(ctx, "/", c.stream, c.from, 20)
				cancel()
				if err != nil || len(msgs) != 0 {
					t.Errorf("%s %s %s: %d messages, %v", name, c.stream, c.from, len(msgs), err)
				}
				if d := time.Since(start); d >= requestTimeout {
					t.Errorf("%s %s %s: took %s", name, c.stream, c.from, d)
				}
			}
		}
	})
}

func TestReadMaxRespected(t *testing.T) {
	each(t, func(t *testing.T, b *broker) {
		for name, r := range readers(b) {
			ctx, cancel := context.WithTimeout(context.Background(), requestTimeout)
			msgs, err := r.Read(ctx, "/", streamName, stream.From{Kind: "first"}, 7)
			cancel()
			if err != nil || len(msgs) != 7 {
				t.Errorf("%s: %d messages, %v", name, len(msgs), err)
			}
		}
	})
}

func TestBothPathsSameMessages(t *testing.T) {
	each(t, func(t *testing.T, b *broker) {
		got := map[string][]string{}
		for name, r := range readers(b) {
			ctx, cancel := context.WithTimeout(context.Background(), requestTimeout)
			msgs, err := r.Read(ctx, "/", streamName, stream.From{Kind: "offset", Offset: 10}, 5)
			cancel()
			if err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			for _, m := range msgs {
				got[name] = append(got[name], fmt.Sprintf("%v|%s|%s", m.Headers["x-stream-offset"], m.Properties["message_id"], m.Body))
			}
		}
		if len(got["amqp"]) != 5 || !reflect.DeepEqual(got["amqp"], got["stream-protocol"]) {
			t.Errorf("paths differ:\n amqp   %v\n stream %v", got["amqp"], got["stream-protocol"])
		}
	})
}
