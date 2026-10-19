package stream

import (
	"context"
	"net"
	"strings"
	"testing"
	"time"
)

func TestFromParsing(t *testing.T) {
	for in, want := range map[string]From{
		"first":                {Kind: "first"},
		"last":                 {Kind: "last"},
		"1200":                 {Kind: "offset", Offset: 1200},
		"0":                    {Kind: "offset"},
		"2026-10-19T10:40:00Z": {Kind: "timestamp", Time: time.Date(2026, 10, 19, 10, 40, 0, 0, time.UTC)},
	} {
		got, err := ParseFrom(in)
		if err != nil || got.Kind != want.Kind || got.Offset != want.Offset || !got.Time.Equal(want.Time) {
			t.Errorf("ParseFrom(%q) = %+v, %v", in, got, err)
		}
	}
	for _, bad := range []string{"", "next", "-1", "12a", "2026-10-19", "yesterday", "1e3"} {
		if _, err := ParseFrom(bad); err == nil {
			t.Errorf("ParseFrom(%q) accepted", bad)
		}
	}
}

func TestAMQPConfigHasNoUserinfo(t *testing.T) {
	r := NewAMQP("broker.example.com", 0, nil, "lockmq-ro", []byte("s3cr3t-pw"), nil).(*amqpReader)
	if u := r.url(); u != "amqp://broker.example.com:5672/" {
		t.Errorf("url = %q", u)
	}
	for _, s := range []string{r.url()} {
		if strings.Contains(s, "lockmq-ro") || strings.Contains(s, "s3cr3t") || strings.Contains(s, "@") {
			t.Errorf("url holds credentials: %q", s)
		}
	}
	cfg := r.config(context.Background(), "orders")
	if len(cfg.SASL) != 1 || cfg.SASL[0].Mechanism() != "PLAIN" || cfg.Vhost != "orders" {
		t.Errorf("config = %+v", cfg)
	}
	if cfg.Properties["connection_name"] != "lockmq" {
		t.Errorf("connection_name = %v", cfg.Properties["connection_name"])
	}

	// A refused connection: the error names neither the user, the password
	// nor the host.
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	l.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	for _, rd := range []Reader{
		NewAMQP("127.0.0.1", port, nil, "lockmq-ro", []byte("s3cr3t-pw"), nil),
		NewStreamProto("127.0.0.1", port, nil, "lockmq-ro", []byte("s3cr3t-pw"), nil),
	} {
		_, err := rd.Read(ctx, "orders", "order-events", From{Kind: "first"}, 5)
		if err == nil {
			t.Fatal("no error")
		}
		for _, leak := range []string{"lockmq-ro", "s3cr3t", "127.0.0.1", "@"} {
			if strings.Contains(err.Error(), leak) {
				t.Errorf("error %q holds %q", err, leak)
			}
		}
	}
}

func TestStreamProtoRefusesTunnel(t *testing.T) {
	dial := func(context.Context, string, string) (net.Conn, error) { return nil, nil }
	r := NewStreamProto("h", 0, nil, "u", []byte("p"), dial)
	if _, err := r.Read(context.Background(), "/", "s", From{Kind: "first"}, 1); err != ErrNoTunnel {
		t.Errorf("err = %v", err)
	}
}

func TestReadArgs(t *testing.T) {
	r := NewAMQP("127.0.0.1", 1, nil, "u", []byte("p"), nil)
	for _, max := range []int{0, -1, 201} {
		if _, err := r.Read(context.Background(), "/", "s", From{Kind: "first"}, max); err == nil {
			t.Errorf("max %d accepted", max)
		}
	}
	if _, _, err := r.Stats(context.Background(), "/", "s"); err != ErrNeedsStreamProtocol {
		t.Errorf("Stats over AMQP: %v", err)
	}
}
