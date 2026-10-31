package msgmask

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/DavidGodefroid/lockkit/pii"
)

func detectors(t testing.TB) []pii.Detector {
	t.Helper()
	ds, err := pii.Detectors([]string{"email", "phone", "iban", "card"})
	if err != nil {
		t.Fatal(err)
	}
	return ds
}

func mustRules(t testing.TB, toml string) Rules {
	t.Helper()
	path := filepath.Join(t.TempDir(), "pii.toml")
	if err := os.WriteFile(path, []byte(toml), 0o600); err != nil {
		t.Fatal(err)
	}
	r, err := LoadRules(path)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func jsonMsg(body string) Message {
	return Message{Body: []byte(body), ContentType: "application/json"}
}

func out(t testing.TB, m Masked) string {
	t.Helper()
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(m); err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(b.String())
}

const order = `{"id": 42, "paid": true, "note": null, "status": "shipped",
 "customer": {"name": "Alice Example", "email": "alice@example.com", "city": "Liège"},
 "items": [{"sku": "SKU-1", "qty": 2}, {"sku": "SKU-2", "qty": 1}]}`

func TestRedactUnknown(t *testing.T) {
	r := mustRules(t, `
[[allow]]
queue = ["orders/order-events"]
body = "$.items[*].sku"
[[allow]]
queue = ["orders/order-events"]
body = "$.status"
`)
	refs := &pii.RefStore{}
	m := NewMasker(r, detectors(t), "redact-unknown", refs, 16384)
	res := m.Mask(refs.Begin(), "orders", "order-events", jsonMsg(order))
	s := out(t, res)
	for _, gone := range []string{"Alice", "alice@example.com", "Liège"} {
		if strings.Contains(s, gone) {
			t.Errorf("%q in clear: %s", gone, s)
		}
	}
	for _, kept := range []string{`"id":42`, `"paid":true`, `"note":null`, `"status":"shipped"`, `"SKU-1"`, `"SKU-2"`, `"qty":2`, `"customer":{`, `"email":"<redacted:r1.`} {
		if !strings.Contains(s, kept) {
			t.Errorf("%q missing: %s", kept, s)
		}
	}
	// An allow rule of another queue does not apply.
	res = m.Mask(refs.Begin(), "orders", "other", jsonMsg(order))
	if s := out(t, res); strings.Contains(s, "SKU-1") || strings.Contains(s, "shipped") {
		t.Errorf("allow rule of another queue applied: %s", s)
	}
}

func TestDetect(t *testing.T) {
	r := mustRules(t, `
[[mask]]
queue = ["*/*"]
body = "$.customer.city"
mode = "partial"
`)
	refs := &pii.RefStore{}
	m := NewMasker(r, detectors(t), "detect", refs, 16384)
	res := m.Mask(refs.Begin(), "orders", "order-events", jsonMsg(`{"status":"shipped","customer":{"name":"Alice Example","city":"Liège"},"comment":"call +32 470 12 34 56 or bob@example.com"}`))
	s := out(t, res)
	for _, gone := range []string{"Alice", "Liège", "470 12 34 56", "bob@example.com"} {
		if strings.Contains(s, gone) {
			t.Errorf("%q in clear: %s", gone, s)
		}
	}
	if !strings.Contains(s, `"status":"shipped"`) || !strings.Contains(s, `"city":"L***(5)"`) {
		t.Errorf("unexpected: %s", s)
	}
}

func TestSameValueSameRef(t *testing.T) {
	refs := &pii.RefStore{}
	m := NewMasker(Rules{}, detectors(t), "redact-unknown", refs, 16384)
	n := refs.Begin()
	a := out(t, m.Mask(n, "orders", "q", jsonMsg(`{"email":"alice@example.com"}`)))
	b := out(t, m.Mask(n, "orders", "q", jsonMsg(`{"contact":{"email":"alice@example.com"},"other":"x"}`)))
	if !strings.Contains(a, `<redacted:r1.1>`) || !strings.Contains(b, `<redacted:r1.1>`) || !strings.Contains(b, `<redacted:r1.2>`) {
		t.Errorf("refs:\n%s\n%s", a, b)
	}
	if v, ok := refs.Get("r1.1"); !ok || v != "alice@example.com" {
		t.Errorf("ref r1.1 = %q", v)
	}
}

func TestOriginAddsMasksOnly(t *testing.T) {
	r := mustRules(t, `
[[allow]]
queue = ["orders/orders"]
body = "$.customer"
[[mask]]
queue = ["orders/orders"]
body = "$.status"
`)
	refs := &pii.RefStore{}
	m := NewMasker(r, detectors(t), "redact-unknown", refs, 16384)
	msg := jsonMsg(`{"customer":"Alice Example","status":"failed"}`)
	msg.Origin = "orders" // forged or real x-death: names a queue with an allow rule
	s := out(t, m.Mask(refs.Begin(), "orders", "orders.failed", msg))
	if strings.Contains(s, "Alice") {
		t.Errorf("origin's allow rule unmasked: %s", s)
	}
	// The origin's mask rule applies, in detect mode too.
	m = NewMasker(r, detectors(t), "detect", refs, 16384)
	s = out(t, m.Mask(refs.Begin(), "orders", "orders.failed", msg))
	if strings.Contains(s, "failed") {
		t.Errorf("origin's mask rule not applied: %s", s)
	}
}

func TestXDeathKept(t *testing.T) {
	refs := &pii.RefStore{}
	m := NewMasker(Rules{}, detectors(t), "redact-unknown", refs, 16384)
	msg := jsonMsg(`{}`)
	msg.Headers = map[string]any{
		"x-death": []any{map[string]any{
			"queue": "orders", "reason": "rejected", "count": int64(3), "exchange": "order-events",
			"routing-keys": []any{"order.alice@example.com", "order.created"},
			"time":         "2026-10-19T10:40:00Z", "original-expiration": "60000",
		}},
		"x-customer": "Alice Example",
	}
	res := m.Mask(refs.Begin(), "orders", "orders.failed", msg)
	s := out(t, res)
	for _, kept := range []string{`"queue":"orders"`, `"reason":"rejected"`, `"count":3`, `"exchange":"order-events"`, `"order.created"`} {
		if !strings.Contains(s, kept) {
			t.Errorf("%q missing: %s", kept, s)
		}
	}
	for _, gone := range []string{"alice@example.com", "Alice Example"} {
		if strings.Contains(s, gone) {
			t.Errorf("%q in clear: %s", gone, s)
		}
	}
}

func TestOpaqueBodies(t *testing.T) {
	refs := &pii.RefStore{}
	m := NewMasker(Rules{}, detectors(t), "detect", refs, 16384)
	deep := strings.Repeat(`{"a":`, 65) + `"alice@example.com"` + strings.Repeat(`}`, 65)
	big := `{"a":"` + strings.Repeat("x", 10<<20) + `"}`
	for name, msg := range map[string]Message{
		"binary":       {Body: []byte{0xff, 0xfe, 0x00, 0x01, 'a'}, ContentType: ""},
		"gzip":         {Body: []byte(`{"email":"alice@example.com"}`), ContentType: "application/json", ContentEncoding: "gzip"},
		"invalid json": {Body: []byte(`{"email":"alice@example.com"`), ContentType: "application/json"},
		"depth 65":     {Body: []byte(deep), ContentType: "application/json"},
		"10 MB":        {Body: []byte(big), ContentType: "application/json"},
		"octet":        {Body: []byte("alice@example.com"), ContentType: "application/octet-stream"},
	} {
		res := m.Mask(refs.Begin(), "orders", "q", msg)
		body, ok := res.Body.(string)
		if !ok || !strings.HasPrefix(body, "<bytes:") || !strings.Contains(body, "sha256:") || !strings.Contains(body, "type:") {
			t.Errorf("%s: body = %.80v", name, res.Body)
		}
		if s := out(t, res); strings.Contains(s, "alice") {
			t.Errorf("%s: clear text: %.200s", name, s)
		}
	}
}

// TestUndeclaredBrokenJSONFailsClosed: a body without content type that
// looks like JSON but does not parse (a DLQ message cut by peek, or just
// malformed) is opaque under redact-unknown: names and streets are no
// detector's business, so text masking would leave them in clear.
func TestUndeclaredBrokenJSONFailsClosed(t *testing.T) {
	refs := &pii.RefStore{}
	record := `{"name":"Jane Doe","street":"12 Elm Road","note":"` + strings.Repeat("x", 300) + `"},`
	full := "[" + strings.Repeat(record, 300) // ~100 KiB, unterminated
	for name, msg := range map[string]Message{
		"malformed": {Body: []byte(`{"name":"Jane Doe","street":"12 Elm Road"`)},
		"large cut": {Body: []byte(full[:4*16384])},
	} {
		res := NewMasker(Rules{}, detectors(t), "redact-unknown", refs, 16384).Mask(refs.Begin(), "orders", "orders.dlq", msg)
		body, ok := res.Body.(string)
		if !ok || !strings.HasPrefix(body, "<bytes:") || !strings.Contains(body, "type:unknown") {
			t.Errorf("%s: body = %.80v", name, res.Body)
		}
		if s := out(t, res); strings.Contains(s, "Jane") || strings.Contains(s, "Elm") {
			t.Errorf("%s: clear text: %.200s", name, s)
		}
	}
	// detect mode keeps the text fallback: detectors only, as for text.
	res := NewMasker(Rules{}, detectors(t), "detect", refs, 16384).Mask(refs.Begin(), "orders", "q",
		Message{Body: []byte(`{"mail":"alice@example.com"`)})
	if s, ok := res.Body.(string); !ok || strings.Contains(s, "alice@example.com") || !strings.Contains(s, `{"mail":`) {
		t.Errorf("detect fallback: body = %v", res.Body)
	}
}

// TestCutBodyOpaque: a body the source shortened is opaque whatever its
// declared type and the body mode.
func TestCutBodyOpaque(t *testing.T) {
	refs := &pii.RefStore{}
	for _, mode := range []string{"redact-unknown", "detect"} {
		for _, ct := range []string{"", "text/plain", "application/json"} {
			msg := Message{Body: []byte(`{"name":"Jane Doe"}`), ContentType: ct, Cut: true}
			res := NewMasker(Rules{}, detectors(t), mode, refs, 16384).Mask(refs.Begin(), "orders", "q", msg)
			body, ok := res.Body.(string)
			if !ok || !strings.HasPrefix(body, "<bytes:") || !strings.Contains(body, "+truncated") {
				t.Errorf("%s %q: body = %.80v", mode, ct, res.Body)
			}
		}
	}
}

func TestClear(t *testing.T) {
	msg := jsonMsg(`{"email":"alice@example.com"}`)
	msg.Headers = map[string]any{"x-customer-ref": "C-1"}
	res := Clear(msg, 16384)
	if b, ok := res.Body.(map[string]any); !ok || b["email"] != "alice@example.com" || res.Headers["x-customer-ref"] != "C-1" {
		t.Errorf("clear = %+v", res)
	}
	if res := Clear(Message{Body: []byte(`{"name":"Jane`), Cut: true}, 16384); res.Body != `{"name":"Jane` || !res.Truncated {
		t.Errorf("cut = %+v", res)
	}
	if s, _ := Clear(Message{Body: []byte{0xff, 0x00}}, 16384).Body.(string); !strings.HasPrefix(s, "<bytes:") {
		t.Errorf("binary = %q", s)
	}
}

func TestTextBody(t *testing.T) {
	refs := &pii.RefStore{}
	m := NewMasker(Rules{}, detectors(t), "redact-unknown", refs, 16384)
	res := m.Mask(refs.Begin(), "orders", "q", Message{Body: []byte("refund for bob@example.com done"), ContentType: "text/plain"})
	s, _ := res.Body.(string)
	if strings.Contains(s, "bob@example.com") || !strings.Contains(s, "refund for") {
		t.Errorf("body = %q", s)
	}
}

func TestTruncated(t *testing.T) {
	refs := &pii.RefStore{}
	m := NewMasker(Rules{}, detectors(t), "detect", refs, 64)
	res := m.Mask(refs.Begin(), "orders", "q", jsonMsg(`{"items":[`+strings.Repeat(`1,`, 100)+`1]}`))
	if !res.Truncated {
		t.Error("not truncated")
	}
	if s, ok := res.Body.(string); !ok || len(s) > 64+len(pii.CutMark) {
		t.Errorf("body = %v", res.Body)
	}
}

func TestHeaderAndProperty(t *testing.T) {
	r := mustRules(t, `
[[mask]]
queue = ["*/*"]
header = "x-customer-*"
[[mask]]
queue = ["*/*"]
property = "user_id"
[[allow]]
queue = ["orders/q"]
header = "x-tenant"
`)
	refs := &pii.RefStore{}
	m := NewMasker(r, detectors(t), "detect", refs, 16384)
	msg := jsonMsg(`{}`)
	msg.Headers = map[string]any{"x-customer-ref": "C-123", "x-tenant": "acme", "x-trace": "abc", "x-contact": "bob@example.com"}
	msg.Properties = map[string]any{"user_id": "svc-orders", "message_id": "m-1", "delivery_mode": 2, "correlation_id": "alice@example.com"}
	s := out(t, m.Mask(refs.Begin(), "orders", "q", msg))
	for _, gone := range []string{"C-123", "svc-orders", "bob@example.com", "alice@example.com"} {
		if strings.Contains(s, gone) {
			t.Errorf("%q in clear: %s", gone, s)
		}
	}
	for _, kept := range []string{`"x-tenant":"acme"`, `"x-trace":"abc"`, `"message_id":"m-1"`, `"delivery_mode":2`} {
		if !strings.Contains(s, kept) {
			t.Errorf("%q missing: %s", kept, s)
		}
	}
	// redact-unknown: an unruled header string is redacted.
	m = NewMasker(r, detectors(t), "redact-unknown", refs, 16384)
	s = out(t, m.Mask(refs.Begin(), "orders", "q", msg))
	if strings.Contains(s, `"abc"`) || !strings.Contains(s, `"x-tenant":"acme"`) {
		t.Errorf("redact-unknown headers: %s", s)
	}
}

func TestProposals(t *testing.T) {
	refs := &pii.RefStore{}
	m := NewMasker(Rules{}, detectors(t), "detect", refs, 16384)
	res := m.Mask(refs.Begin(), "orders", "order-events", jsonMsg(`{"customer_name":"Alice","lines":[{"phone_number":"x"}],"status":"ok"}`))
	want := []string{"queue=orders/order-events body=$.customer_name", "queue=orders/order-events body=$.lines[*].phone_number"}
	for _, w := range want {
		found := false
		for _, p := range res.Proposals {
			found = found || p == w
		}
		if !found {
			t.Errorf("proposal %q missing: %v", w, res.Proposals)
		}
	}
	for _, p := range res.Proposals {
		if strings.Contains(p, "status") {
			t.Errorf("status proposed: %v", res.Proposals)
		}
		if _, err := ParseRule(p); err != nil {
			t.Errorf("proposal %q does not parse: %v", p, err)
		}
	}
}

func TestRuleSyntax(t *testing.T) {
	for _, bad := range []string{
		`[[mask]]
queue = ["orders/q"]
body = "$..a"`,
		`[[mask]]
queue = ["orders/q"]
body = "$.a[?(@.x)]"`,
		`[[mask]]
queue = ["orders/q"]
body = "$"`,
		`[[mask]]
queue = ["orders"]
body = "$.a"`,
		`[[mask]]
queue = ["orders/q"]`,
		`[[mask]]
queue = ["orders/q"]
body = "$.a"
header = "x"`,
		`[[mask]]
queue = ["orders/q"]
body = "$.a"
mode = "shout"`,
		`[[allow]]
queue = ["orders/q"]
body = "$.a"
mode = "partial"`,
		`[[mask]]
body = "$.a"`,
		`[[mask]]
queue = ["orders/q"]
property = "headers"`,
		`[[mask]]
queue = ["orders/q"]
body = "$.a"
colour = "red"`,
	} {
		path := filepath.Join(t.TempDir(), "pii.toml")
		os.WriteFile(path, []byte(bad), 0o600)
		if _, err := LoadRules(path); err == nil {
			t.Errorf("accepted:\n%s", bad)
		}
	}
	r := mustRules(t, `
[[mask]]
queue = ["orders/q", "//root-q"]
exchange = ["orders/ex"]
body = "$.a[3].b"
mode = "email"
`)
	text := r.Mask[0].String()
	if text != "queue=//root-q,orders/q exchange=orders/ex body=$.a[3].b" {
		t.Errorf("canonical = %q", text)
	}
	back, err := ParseRule(text)
	if err != nil || back.String() != text {
		t.Errorf("round trip: %q %v", back.String(), err)
	}
	if mask, _, modes := r.Texts(); len(mask) != 1 || modes[text] != "email" {
		t.Errorf("texts: %v %v", mask, modes)
	}
	if _, err := LoadRules(filepath.Join(t.TempDir(), "missing.toml")); err != nil {
		t.Errorf("missing file: %v", err)
	}
}

// The policy's texts rebuild the same rules, and an appended mask rule
// reads back from the file with the rules already there.
func TestFromTextsAndAppendMask(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".lockmq", "pii.toml")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`# kept as is
[[mask]]
queue = ["orders/q"]
body = "$.customer.email"
mode = "email"

[[allow]]
queue = ["orders/q"]
body = "$.items[*].sku"
`), 0o600); err != nil {
		t.Fatal(err)
	}
	r, err := LoadRules(path)
	if err != nil {
		t.Fatal(err)
	}
	mask, allow, modes := r.Texts()
	back, err := FromTexts(mask, allow, modes)
	if err != nil {
		t.Fatal(err)
	}
	m2, a2, mo2 := back.Texts()
	if strings.Join(m2, "|") != strings.Join(mask, "|") || strings.Join(a2, "|") != strings.Join(allow, "|") || mo2[mask[0]] != "email" {
		t.Errorf("round trip: %v %v %v", m2, a2, mo2)
	}
	if _, err := FromTexts([]string{"queue=orders/q body=$..x"}, nil, nil); err == nil {
		t.Error("invalid text accepted")
	}

	add, err := ParseRule("queue=*/* header=x-customer-*")
	if err != nil {
		t.Fatal(err)
	}
	if err := AppendMask(path, add); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(path)
	if !strings.HasPrefix(string(raw), "# kept as is") {
		t.Errorf("file rewritten:\n%s", raw)
	}
	r, err = LoadRules(path)
	if err != nil {
		t.Fatalf("%v\n%s", err, raw)
	}
	mask, allow, _ = r.Texts()
	if len(mask) != 2 || len(allow) != 1 || mask[0] != "queue=*/* header=x-customer-*" {
		t.Errorf("after append: %v %v", mask, allow)
	}
	fresh := filepath.Join(t.TempDir(), "new", "pii.toml")
	if err := AppendMask(fresh, add); err != nil {
		t.Fatal(err)
	}
	if r, err := LoadRules(fresh); err != nil || len(r.Mask) != 1 {
		t.Errorf("new file: %v %v", r, err)
	}
}

func TestVHostSlashGlob(t *testing.T) {
	r := mustRules(t, `
[[mask]]
queue = ["//orders"]
body = "$.a"
`)
	m := NewMasker(r, detectors(t), "detect", &pii.RefStore{}, 1024)
	refs := m.refs
	s := out(t, m.Mask(refs.Begin(), "/", "orders", jsonMsg(`{"a":"x1"}`)))
	if strings.Contains(s, "x1") {
		t.Errorf("vhost / rule not applied: %s", s)
	}
	s = out(t, m.Mask(refs.Begin(), "other", "orders", jsonMsg(`{"a":"x1"}`)))
	if !strings.Contains(s, "x1") {
		t.Errorf("vhost / rule applied to another vhost: %s", s)
	}
}

func FuzzMaskNoPanic(f *testing.F) {
	f.Add([]byte(order), "application/json", "")
	f.Add([]byte(`[1,"a",{"b":[null,true]}]`), "", "")
	f.Add([]byte("plain text"), "text/plain", "")
	f.Add([]byte{0, 1, 2, 0xff}, "application/octet-stream", "gzip")
	f.Add([]byte(strings.Repeat("[", 100)), "application/json", "")
	const planted = "planted.canary@example.com"
	f.Fuzz(func(t *testing.T, body []byte, ctype, cenc string) {
		refs := &pii.RefStore{}
		ds := detectors(t)
		for _, mode := range []string{"redact-unknown", "detect"} {
			m := NewMasker(Rules{}, ds, mode, refs, 4096)
			// The canary goes in as a JSON string leaf, a key, a header and
			// raw text around the fuzzed body.
			for _, b := range [][]byte{
				body,
				append(append([]byte(`{"x":"`+planted+`","`+planted+`":`), body...), '}'),
				append(append([]byte(planted+" "), body...), []byte(" "+planted)...),
			} {
				msg := Message{Body: b, ContentType: ctype, ContentEncoding: cenc,
					Headers:    map[string]any{"h": planted, planted: "v"},
					Properties: map[string]any{"correlation_id": planted}}
				s := out(t, m.Mask(refs.Begin(), "v", "q", msg))
				if strings.Contains(s, planted) && !bytes.Contains(body, []byte(planted)) {
					t.Fatalf("canary in clear (mode %s): %.300s", mode, s)
				}
			}
		}
	})
}

// A canary secret in any rule field never appears in the error, which
// still names the field.
func TestRuleErrorsDoNotEchoValues(t *testing.T) {
	const canary = "glsa_CanarySecretToken123"
	for _, v := range []string{canary, "https://user:glsa_Canary@h"} {
		files := []struct{ name, body, key string }{
			{"queue glob", "[[mask]]\nqueue = [\"" + v + "\"]\nbody = \"$.a\"\n", "queue entry 1: glob"},
			{"queue glob space", "[[mask]]\nqueue = [\"o/" + v + " x\"]\nbody = \"$.a\"\n", "glob"},
			{"queue glob control", "[[mask]]\nqueue = [\"o/" + v + "\\u001b\"]\nbody = \"$.a\"\n", "glob"},
			{"exchange glob", "[[mask]]\nexchange = [\"o/q\", \"" + v + "\"]\nbody = \"$.a\"\n", "exchange entry 2: glob"},
			{"header", "[[mask]]\nqueue = [\"o/q\"]\nheader = \"" + v + " x\"\n", "header"},
			{"body", "[[mask]]\nqueue = [\"o/q\"]\nbody = \"" + v + "\"\n", "body"},
			{"property", "[[mask]]\nqueue = [\"o/q\"]\nproperty = \"" + v + "\"\n", "property"},
			{"mode", "[[mask]]\nqueue = [\"o/q\"]\nbody = \"$.a\"\nmode = \"" + v + "\"\n", "mode"},
		}
		for _, c := range files {
			path := filepath.Join(t.TempDir(), "pii.toml")
			if err := os.WriteFile(path, []byte(c.body), 0o600); err != nil {
				t.Fatal(err)
			}
			_, err := LoadRules(path)
			checkNoEcho(t, c.name, err, c.key)
		}
		texts := []struct{ name, text, key string }{
			{"text glob", "queue=" + v + " body=$.a", "glob"},
			{"text field", v + "=x queue=o/q body=$.a", "field"},
			{"text field no value", "queue=o/q " + v, "rule"},
			{"text header", "queue=o/q header=" + v + "=", "header"},
			{"text property", "queue=o/q property=" + v, "property"},
		}
		for _, c := range texts {
			_, err := ParseRule(c.text)
			checkNoEcho(t, c.name, err, c.key)
		}
	}
}

func checkNoEcho(t *testing.T, name string, err error, key string) {
	t.Helper()
	if err == nil {
		t.Errorf("%s: accepted", name)
		return
	}
	msg := err.Error()
	if strings.Contains(msg, "glsa_Canary") || strings.Contains(msg, "user:") {
		t.Errorf("%s: error echoes the value: %q", name, msg)
	}
	if !strings.Contains(msg, key) {
		t.Errorf("%s: error %q does not name %q", name, msg, key)
	}
}
