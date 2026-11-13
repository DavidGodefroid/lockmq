package msgmask

import (
	"encoding/json"
	"strings"
	"testing"
)

func mustRule(t *testing.T, text string, mask bool) Rule {
	t.Helper()
	r, err := parseText(text)
	if err != nil {
		t.Fatal(err)
	}
	rule, err := build(r, mask)
	if err != nil {
		t.Fatal(err)
	}
	return rule
}

func TestOverlaps(t *testing.T) {
	m := mustRule(t, "queue=orders/* body=$.customer.email", true)
	for _, c := range []struct {
		allow string
		want  bool
	}{
		{"queue=orders/* body=$.customer.email", true},
		{"queue=orders/order-* body=$.customer", true},
		{"queue=*/* body=$.customer", true},
		{"queue=orders/* body=$.customer.email.domain", true},
		{"queue=orders/* body=$.customer.name", false},
		{"queue=billing/* body=$.customer.email", false},
		{"queue=orders/* header=x-customer-email", false},
		{"queue=ord*/q body=$.customer.email", true},
	} {
		if got := Overlaps(mustRule(t, c.allow, false), m); got != c.want {
			t.Errorf("%s: %t, want %t", c.allow, got, c.want)
		}
	}
	h := mustRule(t, "queue=*/* header=x-customer-*", true)
	if !Overlaps(mustRule(t, "queue=orders/q header=x-customer-email", false), h) {
		t.Error("a header inside the masked glob")
	}
	if Overlaps(mustRule(t, "queue=orders/q header=x-trace", false), h) {
		t.Error("another header")
	}
	ex := mustRule(t, "exchange=orders/events property=user_id", true)
	if !Overlaps(mustRule(t, "queue=billing/q property=user_id", false), ex) {
		t.Error("a mask on an exchange covers any queue")
	}
}

// A redact rule wins over a weaker rule on the same value, whatever their
// order.
func TestStrictestMaskWins(t *testing.T) {
	partial := mustRule(t, "queue=orders/* body=$.customer", true)
	partial.Mode = "partial"
	redact := mustRule(t, "queue=*/* body=$.customer.email", true)
	m := NewMasker(Rules{Mask: []Rule{partial, redact}}, nil, ModeDetect, nil, 0)
	out := m.Mask(m.refs.Begin(), "orders", "q", Message{Body: []byte(`{"customer":{"email":"alice@example.com"}}`)})
	b, _ := json.Marshal(out.Body)
	if strings.Contains(string(b), "alice") || strings.Contains(string(b), "example") {
		t.Fatalf("partial won over redact: %s", b)
	}
}
