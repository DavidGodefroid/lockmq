// Package stream reads the messages of a RabbitMQ stream without moving
// anything: over the stream protocol with an unnamed consumer (no offset
// stored, no single-active-consumer group joined), or over AMQP 0-9-1
// with an x-stream-offset consumer. Neither path ever stores an offset.
package stream

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/DavidGodefroid/lockmq/internal/msgmask"
)

// From is where a read starts.
type From struct {
	// Kind is first, last, offset or timestamp.
	Kind   string
	Offset int64
	Time   time.Time
}

// ParseFrom parses "first", "last", an offset ("1200") or an RFC 3339
// timestamp ("2026-10-19T10:40:00Z").
func ParseFrom(s string) (From, error) {
	switch s {
	case "first", "last":
		return From{Kind: s}, nil
	}
	if n, err := strconv.ParseInt(s, 10, 64); err == nil {
		if n < 0 {
			return From{}, errors.New("from: an offset is 0 or more")
		}
		return From{Kind: "offset", Offset: n}, nil
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return From{Kind: "timestamp", Time: t}, nil
	}
	return From{}, errors.New(`from: want first, last, an offset or an RFC 3339 timestamp such as "2026-10-19T10:40:00Z"`)
}

func (f From) String() string {
	switch f.Kind {
	case "offset":
		return "offset " + strconv.FormatInt(f.Offset, 10)
	case "timestamp":
		return "timestamp " + f.Time.UTC().Format(time.RFC3339)
	}
	return f.Kind
}

// Reader reads a stream.
type Reader interface {
	// Read returns at most max messages from from. It ends at max, when no
	// message came for a short while (the end of the stream), or at ctx's
	// end, whichever comes first: an empty stream or an offset past its end
	// returns no message, never blocks. Each message carries its offset in
	// the header x-stream-offset.
	Read(ctx context.Context, vhost, stream string, from From, max int) ([]msgmask.Message, error)
	// Stats returns the first offset and the first offset of the last
	// committed chunk; -1, -1 for an empty stream. Stream protocol only.
	Stats(ctx context.Context, vhost, stream string) (first, last int64, err error)
	// QueryOffset reads the offset stored by a named consumer. Stream
	// protocol only.
	QueryOffset(ctx context.Context, vhost, stream, consumerRef string) (int64, error)
}

// DialFunc opens a connection (an SSH tunnel's Dial).
type DialFunc func(ctx context.Context, network, addr string) (net.Conn, error)

// Errors. Their text never holds a host, a user or a secret.
var (
	ErrNeedsStreamProtocol = errors.New("stream: this needs the stream protocol: set stream_url on the profile")
	ErrNoTunnel            = errors.New("stream: the stream protocol cannot go through an ssh tunnel; set amqp_url to read streams over AMQP")
	ErrNoSuchStream        = errors.New("stream: no such stream")
	ErrAccess              = errors.New("stream: access refused")
	ErrTimeout             = errors.New("stream: timeout")
	ErrFailed              = errors.New("stream: request failed")
	ErrMax                 = errors.New("stream: max must be 1 or more")
)

// MaxRead is the hard ceiling of one read (read_max's ceiling).
const MaxRead = 200

// idleEnd is how long a read waits for the next message before it takes
// the stream's end as reached.
const idleEnd = 700 * time.Millisecond

func checkArgs(stream string, max int) error {
	if stream == "" {
		return errors.New("stream: a stream name is required")
	}
	if max < 1 || max > MaxRead {
		return fmt.Errorf("stream: max %d out of range 1..%d", max, MaxRead)
	}
	return nil
}

func hostPort(host string, port int) string {
	return net.JoinHostPort(strings.Trim(host, "[]"), strconv.Itoa(port))
}
