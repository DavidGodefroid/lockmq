package stream

import (
	"context"
	"crypto/tls"
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/rabbitmq/rabbitmq-stream-go-client/pkg/amqp"
	"github.com/rabbitmq/rabbitmq-stream-go-client/pkg/logs"
	rstream "github.com/rabbitmq/rabbitmq-stream-go-client/pkg/stream"

	"github.com/DavidGodefroid/lockmq/internal/msgmask"
)

func init() {
	// The client library logs to stdout, and its lines can name hosts and
	// connection URIs: the console's terminal never sees them.
	logs.SetLogger(silent{})
}

type silent struct{}

func (silent) Info(string, ...any)  {}
func (silent) Error(string, ...any) {}
func (silent) Debug(string, ...any) {}
func (silent) Warn(string, ...any)  {}

type streamProto struct {
	host   string
	port   int
	tls    *tls.Config
	user   string
	secret []byte
	tunnel bool
}

// NewStreamProto returns a reader over the stream protocol. The client
// library dials TCP itself: with dial set (an SSH tunnel) every call
// returns ErrNoTunnel.
func NewStreamProto(host string, port int, tlsCfg *tls.Config, user string, secret []byte, dial DialFunc) Reader {
	if port == 0 {
		port = 5552
		if tlsCfg != nil {
			port = 5551
		}
	}
	return &streamProto{host: host, port: port, tls: tlsCfg, user: user, secret: append([]byte(nil), secret...), tunnel: dial != nil}
}

// env opens an environment on vhost. The library takes the password as a
// string; it lives as long as the call.
func (s *streamProto) env(ctx context.Context, vhost string) (*rstream.Environment, error) {
	if s.tunnel {
		return nil, ErrNoTunnel
	}
	opts := rstream.NewEnvironmentOptions().
		SetHost(s.host).SetPort(s.port).SetVHost(vhost).
		SetUser(s.user).SetPassword(string(s.secret)).
		SetMaxConsumersPerClient(1)
	if d, ok := ctx.Deadline(); ok {
		opts.SetRPCTimeout(max(time.Until(d), 100*time.Millisecond))
	}
	if s.tls != nil {
		opts.IsTLS(true).SetTLSConfig(s.tls)
	}
	type res struct {
		env *rstream.Environment
		err error
	}
	ch := make(chan res, 1)
	go func() {
		env, err := rstream.NewEnvironment(opts)
		ch <- res{env, err}
	}()
	select {
	case r := <-ch:
		if r.err != nil {
			return nil, classify(r.err)
		}
		return r.env, nil
	case <-ctx.Done():
		go func() {
			if r := <-ch; r.env != nil {
				r.env.Close()
			}
		}()
		return nil, ErrTimeout
	}
}

func classify(err error) error {
	switch {
	case errors.Is(err, rstream.StreamDoesNotExist):
		return ErrNoSuchStream
	case errors.Is(err, rstream.AuthenticationFailure), strings.Contains(strings.ToLower(err.Error()), "access refused"),
		strings.Contains(strings.ToLower(err.Error()), "access_refused"):
		return ErrAccess
	case errors.Is(err, context.DeadlineExceeded), strings.Contains(strings.ToLower(err.Error()), "timeout"):
		return ErrTimeout
	}
	return ErrFailed
}

func (s *streamProto) Stats(ctx context.Context, vhost, stream string) (int64, int64, error) {
	env, err := s.env(ctx, vhost)
	if err != nil {
		return 0, 0, err
	}
	defer env.Close()
	st, err := env.StreamStats(stream)
	if err != nil {
		return 0, 0, classify(err)
	}
	first, err := st.FirstOffset()
	if err != nil {
		return -1, -1, nil // empty stream
	}
	last, err := st.CommittedChunkId()
	if err != nil {
		return first, -1, nil
	}
	return first, last, nil
}

func (s *streamProto) QueryOffset(ctx context.Context, vhost, stream, ref string) (int64, error) {
	if ref == "" {
		return 0, errors.New("stream: a consumer reference is required")
	}
	env, err := s.env(ctx, vhost)
	if err != nil {
		return 0, err
	}
	defer env.Close()
	off, err := env.QueryOffset(ref, stream)
	if err != nil {
		return 0, classify(err)
	}
	return off, nil
}

func (s *streamProto) Read(ctx context.Context, vhost, stream string, from From, max int) ([]msgmask.Message, error) {
	if err := checkArgs(stream, max); err != nil {
		return nil, err
	}
	env, err := s.env(ctx, vhost)
	if err != nil {
		return nil, err
	}
	defer env.Close()

	st, err := env.StreamStats(stream)
	if err != nil {
		return nil, classify(err)
	}
	if _, err := st.FirstOffset(); err != nil {
		return nil, nil // empty stream: nothing to wait for
	}

	var (
		mu    sync.Mutex
		out   []msgmask.Message
		done  = make(chan struct{})
		got   = make(chan struct{}, 1)
		ended bool
	)
	handler := func(cc rstream.ConsumerContext, m *amqp.Message) {
		mu.Lock()
		defer mu.Unlock()
		if ended {
			return
		}
		out = append(out, fromAMQP10(m, cc.Consumer.GetOffset()))
		if len(out) >= max {
			ended = true
			close(done)
		}
		select {
		case got <- struct{}{}:
		default:
		}
	}
	// No consumer name: the broker stores no offset for it and it joins no
	// single-active-consumer group. Auto-commit is off by default.
	opts := rstream.NewConsumerOptions().
		SetOffset(offsetSpec(from)).
		SetInitialCredits(1).
		SetClientProvidedName("lockmq")
	consumer, err := env.NewConsumer(stream, handler, opts)
	if err != nil {
		return nil, classify(err)
	}
	waitIdle(ctx, done, got)
	mu.Lock()
	ended = true
	res := out
	if len(res) > max {
		res = res[:max]
	}
	mu.Unlock()
	consumer.Close()
	return res, nil
}

// waitIdle returns at done, at ctx's end, or once no message came for
// idleEnd.
func waitIdle(ctx context.Context, done <-chan struct{}, got <-chan struct{}) {
	idle := time.NewTimer(idleEnd)
	defer idle.Stop()
	for {
		select {
		case <-done:
			return
		case <-ctx.Done():
			return
		case <-idle.C:
			return
		case <-got:
			if !idle.Stop() {
				select {
				case <-idle.C:
				default:
				}
			}
			idle.Reset(idleEnd)
		}
	}
}

func offsetSpec(f From) rstream.OffsetSpecification {
	switch f.Kind {
	case "last":
		return rstream.OffsetSpecification{}.Last()
	case "offset":
		return rstream.OffsetSpecification{}.Offset(f.Offset)
	case "timestamp":
		return rstream.OffsetSpecification{}.Timestamp(f.Time.UnixMilli())
	}
	return rstream.OffsetSpecification{}.First()
}

// fromAMQP10 maps a stream message (AMQP 1.0) to a message to mask.
func fromAMQP10(m *amqp.Message, offset int64) msgmask.Message {
	msg := msgmask.Message{Properties: map[string]any{}, Headers: map[string]any{}}
	for k, v := range m.ApplicationProperties {
		msg.Headers[k] = v
	}
	msg.Headers["x-stream-offset"] = offset
	if p := m.Properties; p != nil {
		set := func(k string, v any) {
			switch x := v.(type) {
			case nil:
				return
			case string:
				if x == "" {
					return
				}
			case []byte:
				if len(x) == 0 {
					return
				}
			}
			msg.Properties[k] = v
		}
		set("message_id", p.MessageID)
		set("user_id", p.UserID)
		set("to", p.To)
		set("subject", p.Subject)
		set("reply_to", p.ReplyTo)
		set("correlation_id", p.CorrelationID)
		set("content_type", p.ContentType)
		set("content_encoding", p.ContentEncoding)
		set("group_id", p.GroupID)
		if !p.CreationTime.IsZero() {
			msg.Properties["timestamp"] = p.CreationTime.UTC().Format(time.RFC3339Nano)
		}
		msg.ContentType = p.ContentType
		msg.ContentEncoding = p.ContentEncoding
	}
	if ex, ok := m.Annotations["x-exchange"].(string); ok {
		msg.Exchange = ex
	}
	switch {
	case len(m.Data) > 0:
		for _, d := range m.Data {
			msg.Body = append(msg.Body, d...)
		}
	case m.Value != nil:
		switch v := m.Value.(type) {
		case string:
			msg.Body = []byte(v)
		case []byte:
			msg.Body = v
		}
	}
	return msg
}
