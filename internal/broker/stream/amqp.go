package stream

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
	"strings"
	"sync"
	"time"

	amqp091 "github.com/rabbitmq/amqp091-go"

	"github.com/DavidGodefroid/lockmq/internal/msgmask"
)

type amqpReader struct {
	host   string
	port   int
	tls    *tls.Config
	user   string
	secret []byte
	dial   DialFunc
}

// NewAMQP returns a reader over AMQP 0-9-1: a consumer with
// x-stream-offset, prefetch max, manual acks (on a stream an ack only
// returns credit) and a cancel after max. Stats and QueryOffset return
// ErrNeedsStreamProtocol.
func NewAMQP(host string, port int, tlsCfg *tls.Config, user string, secret []byte, dial DialFunc) Reader {
	if port == 0 {
		port = 5672
		if tlsCfg != nil {
			port = 5671
		}
	}
	return &amqpReader{host: host, port: port, tls: tlsCfg, user: user, secret: append([]byte(nil), secret...), dial: dial}
}

func (a *amqpReader) Stats(context.Context, string, string) (int64, int64, error) {
	return 0, 0, ErrNeedsStreamProtocol
}

func (a *amqpReader) QueryOffset(context.Context, string, string, string) (int64, error) {
	return 0, ErrNeedsStreamProtocol
}

// url is the broker URL, never with userinfo: the credentials go through
// SASL PLAIN.
func (a *amqpReader) url() string {
	scheme := "amqp://"
	if a.tls != nil {
		scheme = "amqps://"
	}
	return scheme + hostPort(a.host, a.port) + "/"
}

func (a *amqpReader) config(ctx context.Context, vhost string) amqp091.Config {
	props := amqp091.NewConnectionProperties()
	props.SetClientConnectionName("lockmq")
	props["product"] = "lockmq"
	dial := a.dial
	if dial == nil {
		dial = (&net.Dialer{}).DialContext
	}
	return amqp091.Config{
		SASL:            []amqp091.Authentication{&amqp091.PlainAuth{Username: a.user, Password: string(a.secret)}},
		Vhost:           vhost,
		TLSClientConfig: a.tls,
		Heartbeat:       10 * time.Second,
		Properties:      props,
		Dial: func(network, addr string) (net.Conn, error) {
			return dial(ctx, network, addr)
		},
	}
}

func (a *amqpReader) Read(ctx context.Context, vhost, stream string, from From, max int) ([]msgmask.Message, error) {
	if err := checkArgs(stream, max); err != nil {
		return nil, err
	}
	type res struct {
		conn *amqp091.Connection
		err  error
	}
	ch := make(chan res, 1)
	go func() {
		c, err := amqp091.DialConfig(a.url(), a.config(ctx, vhost))
		ch <- res{c, err}
	}()
	var conn *amqp091.Connection
	select {
	case r := <-ch:
		if r.err != nil {
			return nil, classifyAMQP(r.err)
		}
		conn = r.conn
	case <-ctx.Done():
		go func() {
			if r := <-ch; r.conn != nil {
				r.conn.Close()
			}
		}()
		return nil, ErrTimeout
	}
	defer conn.Close()
	chn, err := conn.Channel()
	if err != nil {
		return nil, classifyAMQP(err)
	}
	defer chn.Close()
	if err := chn.Qos(max, 0, false); err != nil {
		return nil, classifyAMQP(err)
	}
	const tag = "lockmq-read"
	deliveries, err := chn.ConsumeWithContext(ctx, stream, tag, false, false, false, false,
		amqp091.Table{"x-stream-offset": amqpOffset(from)})
	if err != nil {
		return nil, classifyAMQP(err)
	}
	var (
		out []msgmask.Message
		mu  sync.Mutex
	)
	idle := time.NewTimer(idleEnd)
	defer idle.Stop()
loop:
	for len(out) < max {
		select {
		case d, ok := <-deliveries:
			if !ok {
				break loop
			}
			mu.Lock()
			out = append(out, fromDelivery(d))
			mu.Unlock()
			// On a stream an ack only returns credit: nothing is removed.
			d.Ack(false)
			if !idle.Stop() {
				select {
				case <-idle.C:
				default:
				}
			}
			idle.Reset(idleEnd)
		case <-idle.C:
			break loop
		case <-ctx.Done():
			break loop
		}
	}
	chn.Cancel(tag, false)
	return out, nil
}

func amqpOffset(f From) any {
	switch f.Kind {
	case "last":
		return "last"
	case "offset":
		return f.Offset
	case "timestamp":
		return f.Time
	}
	return "first"
}

func classifyAMQP(err error) error {
	var ae *amqp091.Error
	if errors.As(err, &ae) {
		switch ae.Code {
		case amqp091.AccessRefused:
			return ErrAccess
		case amqp091.NotFound:
			return ErrNoSuchStream
		}
	}
	low := strings.ToLower(err.Error())
	switch {
	case strings.Contains(low, "username or password not allowed"), strings.Contains(low, "access_refused"), strings.Contains(low, "access refused"):
		return ErrAccess
	case errors.Is(err, context.DeadlineExceeded), strings.Contains(low, "timeout"):
		return ErrTimeout
	}
	return ErrFailed
}

// fromDelivery maps an AMQP delivery to a message to mask.
func fromDelivery(d amqp091.Delivery) msgmask.Message {
	msg := msgmask.Message{
		Properties:      map[string]any{},
		Headers:         map[string]any{},
		Body:            d.Body,
		ContentType:     d.ContentType,
		ContentEncoding: d.ContentEncoding,
		Exchange:        d.Exchange,
	}
	for k, v := range d.Headers {
		msg.Headers[k] = v
	}
	for k, v := range map[string]string{
		"content_type": d.ContentType, "content_encoding": d.ContentEncoding, "correlation_id": d.CorrelationId,
		"reply_to": d.ReplyTo, "expiration": d.Expiration, "message_id": d.MessageId, "type": d.Type,
		"user_id": d.UserId, "app_id": d.AppId,
	} {
		if v != "" {
			msg.Properties[k] = v
		}
	}
	if d.DeliveryMode != 0 {
		msg.Properties["delivery_mode"] = int64(d.DeliveryMode)
	}
	if d.Priority != 0 {
		msg.Properties["priority"] = int64(d.Priority)
	}
	if !d.Timestamp.IsZero() {
		msg.Properties["timestamp"] = d.Timestamp.UTC().Format(time.RFC3339)
	}
	return msg
}
