package config

import (
	"encoding/json"
	"reflect"
	"testing"
	"time"

	kconfig "github.com/DavidGodefroid/lockkit/config"
)

type changeT = kconfig.Change

func basePolicy(t *testing.T) Policy {
	t.Helper()
	p, err := parseOne(t, `
[profiles.main]
url = "https://rabbitmq.example.com:15671"
amqp_url = "amqps://rabbitmq.example.com:5671"
stream_url = "rabbitmq-stream+tls://rabbitmq.example.com:5551"
vhosts = ["orders"]
production = true
`)
	if err != nil {
		t.Fatal(err)
	}
	return NewPolicy(p, []string{"queue=*/* header=x-customer-ref"}, []string{"queue=orders/order-events body=$.items[*].sku"}, nil)
}

func loosens(changes []changeT) bool {
	for _, c := range changes {
		if c.Loosens {
			return true
		}
	}
	return false
}

func TestDiffLoosens(t *testing.T) {
	cases := []struct {
		name string
		edit func(p *Policy)
		// symmetric: the reverse change loosens too (another broker).
		symmetric bool
	}{
		{"read to peek", func(p *Policy) { p.Profile.Tier = TierPeek }, false},
		{"lower headroom", func(p *Policy) { p.Profile.Limits.DeliveryHeadroom = 1 }, false},
		{"higher peek_max", func(p *Policy) { p.Profile.Limits.PeekMax = 10 }, false},
		{"higher read_max", func(p *Policy) { p.Profile.Limits.ReadMax = 100 }, false},
		{"higher max_body_bytes", func(p *Policy) { p.Profile.Limits.MaxBodyBytes = 65536 }, false},
		{"higher max_output_bytes", func(p *Policy) { p.Profile.Limits.MaxOutputBytes = 1 << 20 }, false},
		{"higher list_page_size", func(p *Policy) { p.Profile.Limits.ListPageSize = 500 }, false},
		{"longer request_timeout", func(p *Policy) { p.Profile.Limits.RequestTimeout = time.Minute }, false},
		{"redact-unknown to detect", func(p *Policy) { p.Profile.BodyMode = BodyDetect }, false},
		{"new vhost", func(p *Policy) { p.Profile.VHosts = []string{"orders", "payments"} }, false},
		{"every vhost", func(p *Policy) { p.Profile.VHosts = []string{} }, false},
		{"changed url", func(p *Policy) { p.Profile.URL = "https://other.example.com:15671" }, true},
		{"changed amqp_url", func(p *Policy) { p.Profile.AMQPURL = "amqps://other.example.com:5671" }, true},
		{"changed stream_url", func(p *Policy) { p.Profile.StreamURL = "rabbitmq-stream+tls://other.example.com:5551" }, true},
		{"removed detector", func(p *Policy) { p.Profile.Detectors = []string{"email"} }, false},
		{"removed mask", func(p *Policy) { p.Mask = []string{} }, false},
		{"added allow", func(p *Policy) { p.Allow = append(p.Allow, "queue=*/* body=$.id") }, false},
		{"mode away from redact", func(p *Policy) { p.Modes = map[string]string{"queue=*/* header=x-customer-ref": "partial"} }, false},
		{"production off", func(p *Policy) { p.Profile.Production = false }, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			approved := basePolicy(t)
			cur := basePolicy(t)
			c.edit(&cur)
			cur = Canonical(cur)
			if ch := cur.Diff(approved); !loosens(ch) {
				t.Errorf("%s: not a loosening: %+v", c.name, ch)
			}
			// The reverse change tightens.
			if ch := approved.Diff(cur); !c.symmetric && loosens(ch) {
				t.Errorf("reverse of %s loosens: %+v", c.name, ch)
			}
		})
	}
}

func TestDiffRemovedProtocolTightens(t *testing.T) {
	approved, cur := basePolicy(t), basePolicy(t)
	cur.Profile.StreamURL = ""
	cur.Profile.AMQPURL = ""
	if ch := cur.Diff(approved); loosens(ch) || len(ch) != 2 {
		t.Errorf("removing protocols: %+v", ch)
	}
	if ch := approved.Diff(cur); !loosens(ch) {
		t.Errorf("adding protocols does not loosen: %+v", ch)
	}
}

func TestDiffSame(t *testing.T) {
	a, b := basePolicy(t), basePolicy(t)
	if ch := b.Diff(a); len(ch) != 0 {
		t.Errorf("same policy differs: %+v", ch)
	}
}

func TestPolicyJSONRoundTrip(t *testing.T) {
	p := basePolicy(t)
	p.Profile.Tier = TierPeek
	b, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	var back Policy
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(Canonical(back), p) {
		t.Errorf("round trip:\n got %+v\nwant %+v", back, p)
	}
	if len(p.Describe()) == 0 {
		t.Error("empty Describe")
	}
}
