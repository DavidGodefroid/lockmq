// Package mgmt is lockmq's client of the RabbitMQ management HTTP API. It
// calls only the endpoints of Allowed with GET, plus the management "get"
// of a queue peek with ackmode ack_requeue_true. Its errors are generic:
// never a response body, a URL or a credential.
package mgmt

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// MaxResponseBytes caps one response body.
const MaxResponseBytes = 16 << 20

// MaxPageSize is the largest page the management API serves.
const MaxPageSize = 500

// MaxPeek is the most messages one peek may take (the hard ceiling of
// peek_max).
const MaxPeek = 20

// StatusError is a non-2xx answer. Its text holds the code only.
type StatusError struct{ Code int }

func (e *StatusError) Error() string { return "mgmt: HTTP " + strconv.Itoa(e.Code) }

var (
	errTimeout  = errors.New("mgmt: timeout")
	errConnect  = errors.New("mgmt: connection failed")
	errTooLarge = errors.New("mgmt: response too large")
	errDecode   = errors.New("mgmt: invalid JSON response")
	errRedirect = errors.New("mgmt: redirect refused")
)

// Client calls the management API of one broker as one user.
type Client struct {
	base    *url.URL
	user    string
	secret  []byte
	http    *http.Client
	timeout time.Duration
}

// New returns a client of the management API at base (scheme, host, port
// and an optional path prefix; no userinfo). secret is copied: the caller
// may wipe its own. dial, when set, opens every connection (an SSH
// tunnel); timeout bounds each request.
func New(base *url.URL, user string, secret []byte, tlsCfg *tls.Config, dial func(ctx context.Context, network, addr string) (net.Conn, error), timeout time.Duration) *Client {
	b := *base
	b.User = nil
	b.RawQuery, b.Fragment = "", ""
	b.Path = strings.TrimSuffix(b.Path, "/")
	b.RawPath = strings.TrimSuffix(b.RawPath, "/")
	if dial == nil {
		dial = (&net.Dialer{Timeout: timeout}).DialContext
	}
	tr := &http.Transport{
		// Never an environment proxy: the broker is reached directly or
		// through the profile's tunnel.
		Proxy:               nil,
		DialContext:         dial,
		TLSClientConfig:     tlsCfg,
		MaxIdleConns:        2,
		IdleConnTimeout:     90 * time.Second,
		TLSHandshakeTimeout: timeout,
		ForceAttemptHTTP2:   false,
	}
	return &Client{
		base:   &b,
		user:   user,
		secret: append([]byte(nil), secret...),
		http: &http.Client{
			Transport: tr,
			Timeout:   timeout,
			// The Authorization header never follows a redirect.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
		timeout: timeout,
	}
}

// Close wipes the secret and closes idle connections.
func (c *Client) Close() {
	for i := range c.secret {
		c.secret[i] = 0
	}
	c.http.CloseIdleConnections()
}

// Get reads the endpoint p, with the query q, into into. A kind outside
// Allowed is refused before any I/O.
func (c *Client) Get(ctx context.Context, p Path, q url.Values, into any) error {
	path, err := p.build()
	if err != nil {
		return err
	}
	return c.do(ctx, http.MethodGet, path, q, nil, into)
}

type page struct {
	FilteredCount *int            `json:"filtered_count"`
	TotalCount    int             `json:"total_count"`
	Items         json.RawMessage `json:"items"`
}

// GetPage reads page page (from 1) of size items of a list endpoint, with
// only columns when set, into into (a slice). It returns the number of
// items of the whole list.
func (c *Client) GetPage(ctx context.Context, p Path, pageNo, size int, columns []string, into any) (int, error) {
	return c.GetPageQ(ctx, p, pageNo, size, columns, nil, into)
}

// GetPageQ is GetPage with extra query parameters (name filter, sort).
func (c *Client) GetPageQ(ctx context.Context, p Path, pageNo, size int, columns []string, extra url.Values, into any) (int, error) {
	if pageNo < 1 || size < 1 || size > MaxPageSize {
		return 0, fmt.Errorf("mgmt: page %d of size %d out of range (size 1..%d)", pageNo, size, MaxPageSize)
	}
	q := url.Values{}
	for k, v := range extra {
		q[k] = v
	}
	q.Set("page", strconv.Itoa(pageNo))
	q.Set("page_size", strconv.Itoa(size))
	if len(columns) > 0 {
		q.Set("columns", strings.Join(columns, ","))
	}
	var pg page
	if err := c.Get(ctx, p, q, &pg); err != nil {
		return 0, err
	}
	if len(pg.Items) > 0 {
		if err := json.Unmarshal(pg.Items, into); err != nil {
			return 0, errDecode
		}
	}
	if pg.FilteredCount != nil {
		return *pg.FilteredCount, nil
	}
	return pg.TotalCount, nil
}

// Message is one message returned by a peek, as the management API sends
// it.
type Message struct {
	PayloadBytes    int            `json:"payload_bytes"`
	Redelivered     bool           `json:"redelivered"`
	Exchange        string         `json:"exchange"`
	RoutingKey      string         `json:"routing_key"`
	MessageCount    int            `json:"message_count"`
	Properties      map[string]any `json:"properties"`
	Payload         string         `json:"payload"`
	PayloadEncoding string         `json:"payload_encoding"`
}

// PeekRequeue takes up to count ready messages from the head of the queue
// and requeues them in the same call (ackmode ack_requeue_true, encoding
// auto, bodies cut at truncate bytes). It is NOT read-only: the messages
// come back redelivered and, on a quorum queue, their delivery count
// grows. The caller guards it (tier, approval, delivery limit).
func (c *Client) PeekRequeue(ctx context.Context, vhost, queue string, count, truncate int) ([]Message, error) {
	if count < 1 || count > MaxPeek {
		return nil, fmt.Errorf("mgmt: peek count %d out of range 1..%d", count, MaxPeek)
	}
	if truncate < 0 {
		return nil, fmt.Errorf("mgmt: negative truncate")
	}
	path, err := fill(peekPath, []string{vhost, queue})
	if err != nil {
		return nil, err
	}
	body, err := json.Marshal(map[string]any{
		"count": count, "ackmode": "ack_requeue_true", "encoding": "auto", "truncate": truncate,
	})
	if err != nil {
		return nil, err
	}
	var msgs []Message
	if err := c.do(ctx, http.MethodPost, path, nil, body, &msgs); err != nil {
		return nil, err
	}
	return msgs, nil
}

func (c *Client) do(ctx context.Context, method, path string, q url.Values, body []byte, into any) error {
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	u := c.base.Scheme + "://" + c.base.Host + c.base.EscapedPath() + path
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, u, rd)
	if err != nil {
		return errors.New("mgmt: invalid request")
	}
	req.Header.Set("Authorization", c.basicAuth())
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "lockmq")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return classify(ctx, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 && resp.StatusCode < 400 {
		return errRedirect
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
		return &StatusError{Code: resp.StatusCode}
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, MaxResponseBytes+1))
	if err != nil {
		return classify(ctx, err)
	}
	if len(data) > MaxResponseBytes {
		return errTooLarge
	}
	if err := json.Unmarshal(data, into); err != nil {
		return errDecode
	}
	return nil
}

// classify turns a transport error into a generic one: its text (a URL, an
// address) never reaches a client.
func classify(ctx context.Context, err error) error {
	var ne net.Error
	if errors.Is(err, context.DeadlineExceeded) || ctx.Err() == context.DeadlineExceeded || (errors.As(err, &ne) && ne.Timeout()) {
		return errTimeout
	}
	if errors.Is(err, context.Canceled) {
		return context.Canceled
	}
	return errConnect
}

// basicAuth builds the header value from the secret for one request.
func (c *Client) basicAuth() string {
	raw := make([]byte, 0, len(c.user)+1+len(c.secret))
	raw = append(raw, c.user...)
	raw = append(raw, ':')
	raw = append(raw, c.secret...)
	enc := make([]byte, base64.StdEncoding.EncodedLen(len(raw)))
	base64.StdEncoding.Encode(enc, raw)
	for i := range raw {
		raw[i] = 0
	}
	return "Basic " + string(enc)
}
