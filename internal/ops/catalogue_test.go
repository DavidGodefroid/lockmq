package ops

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/DavidGodefroid/lockkit/refusal"

	"github.com/DavidGodefroid/lockmq/internal/broker/mgmt"
	"github.com/DavidGodefroid/lockmq/internal/config"
)

// fakeMgmt answers from testdata/<kind>.json and records the calls.
type fakeMgmt struct {
	calls []string
	query []url.Values
	// deny answers these kinds with HTTP 401.
	deny map[string]bool
}

func (f *fakeMgmt) load(kind string, into any) error {
	data, err := os.ReadFile(filepath.Join("testdata", kind+".json"))
	if errors.Is(err, os.ErrNotExist) {
		data = []byte(`null`)
	} else if err != nil {
		return err
	}
	return json.Unmarshal(data, into)
}

func (f *fakeMgmt) Get(_ context.Context, p mgmt.Path, q url.Values, into any) error {
	f.calls = append(f.calls, p.Kind+":"+strings.Join(p.Segs, "|"))
	f.query = append(f.query, q)
	if f.deny[p.Kind] {
		return &mgmt.StatusError{Code: 401}
	}
	return f.load(p.Kind, into)
}

// A monitoring account cannot list runtime parameters: policies still
// answers, saying so; any other error still fails.
func TestPoliciesWithoutParameters(t *testing.T) {
	d := Deps{Mgmt: &fakeMgmt{deny: map[string]bool{"parameters": true}}, Profile: profile("orders")}
	res, err := Catalogue()["policies"].Run(context.Background(), d, Args{VHost: "orders"})
	if err != nil {
		t.Fatal(err)
	}
	m := res.(map[string]any)
	if s, _ := m["parameters"].(string); !strings.Contains(s, "policymaker") || m["policies"] == nil {
		t.Errorf("policies: %v", m)
	}
	d = Deps{Mgmt: &fakeMgmt{deny: map[string]bool{"policies": true}}, Profile: profile("orders")}
	if _, err := Catalogue()["policies"].Run(context.Background(), d, Args{VHost: "orders"}); err == nil {
		t.Error("401 on policies accepted")
	}
}

func (f *fakeMgmt) GetPageQ(_ context.Context, p mgmt.Path, page, size int, columns []string, extra url.Values, into any) (int, error) {
	f.calls = append(f.calls, p.Kind+":"+strings.Join(p.Segs, "|"))
	q := url.Values{"columns": {strings.Join(columns, ",")}}
	for k, v := range extra {
		q[k] = v
	}
	f.query = append(f.query, q)
	var pg struct {
		Total int             `json:"filtered_count"`
		Items json.RawMessage `json:"items"`
	}
	if err := f.load(p.Kind, &pg); err != nil {
		return 0, err
	}
	if len(pg.Items) == 0 {
		return 0, nil
	}
	return pg.Total, json.Unmarshal(pg.Items, into)
}

func (f *fakeMgmt) PeekRequeue(context.Context, string, string, int, int) ([]mgmt.Message, error) {
	return nil, errors.New("not in this test")
}

func profile(vhosts ...string) config.Profile {
	p := config.Profile{Name: "main", VHosts: vhosts}
	p.Limits = config.DefaultLimits()
	return p
}

func TestCatalogueMatchesSpec(t *testing.T) {
	want := map[string]config.Tier{
		"overview": config.TierRead, "queues": config.TierRead, "queue": config.TierRead,
		"exchanges": config.TierRead, "bindings": config.TierRead, "policies": config.TierRead,
		"consumers": config.TierRead, "connections": config.TierRead, "channels": config.TierRead,
		"stream": config.TierRead, "stream-offset": config.TierRead, "read": config.TierRead,
		"peek": config.TierPeek,
	}
	cat := Catalogue()
	var got []string
	for name := range cat {
		got = append(got, name)
	}
	sort.Strings(got)
	var names []string
	for name := range want {
		names = append(names, name)
	}
	sort.Strings(names)
	if !reflect.DeepEqual(got, names) {
		t.Fatalf("catalogue = %v, want %v", got, names)
	}
	for name, tier := range want {
		op := cat[name]
		if op.Name != name || op.Tier != tier {
			t.Errorf("%s: %+v", name, op)
		}
		if prompt := name == "read" || name == "peek"; op.Prompt != prompt {
			t.Errorf("%s: Prompt = %v", name, op.Prompt)
		}
		if op.Protocol == "" {
			t.Errorf("%s: no protocol", name)
		}
	}
}

func TestFieldsFiltered(t *testing.T) {
	f := &fakeMgmt{}
	d := Deps{Mgmt: f, Profile: profile("orders")}
	res, err := Catalogue()["queue"].Run(context.Background(), d, Args{VHost: "orders", Target: "orders.failed"})
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(res)
	out := string(b)
	for _, gone := range []string{"backing_queue_status", "secret_internal", "garbage_collection", "exclusive_consumer_tag", "amq.ctag-should-go", "head_message_timestamp", "192.0.2.10", "drop me too"} {
		if strings.Contains(out, gone) {
			t.Errorf("%q kept: %s", gone, out)
		}
	}
	for _, kept := range []string{`"messages_ready":1204`, `"x-delivery-limit":20`, `"delivery-limit":10`, `"policy":"dlq-limits"`, `"rate":0.4`, `"type":"quorum"`, `"consumer_tag":"retry-worker-1"`} {
		if !strings.Contains(out, kept) {
			t.Errorf("%q dropped: %s", kept, out)
		}
	}
	// The queue's own consumer_details, never the vhost-wide consumer list.
	for _, c := range f.calls {
		if strings.HasPrefix(c, "consumers:") {
			t.Errorf("vhost-wide consumers fetched: %v", f.calls)
		}
	}
}

func TestConnectionsFiltered(t *testing.T) {
	f := &fakeMgmt{}
	d := Deps{Mgmt: f, Profile: profile("orders", "payments")}
	res, err := Catalogue()["connections"].Run(context.Background(), d, Args{})
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(res)
	out := string(b)
	for _, gone := range []string{"192.0.2.", "information", "capabilities", "custom_token", "secret-vhost"} {
		if strings.Contains(out, gone) {
			t.Errorf("%q kept: %s", gone, out)
		}
	}
	for _, kept := range []string{"amqp091-go", "golang", "order-service-7f9", `"version":"1.10.0"`, "payment-service"} {
		if !strings.Contains(out, kept) {
			t.Errorf("%q dropped: %s", kept, out)
		}
	}
}

// Free text a client sets on its connection goes through the profile's
// detectors.
func TestConnectionTextDetected(t *testing.T) {
	p := profile("orders", "payments")
	p.Detectors = []string{"email"}
	res, err := Catalogue()["connections"].Run(context.Background(), Deps{Mgmt: &fakeMgmt{}, Profile: p}, Args{})
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(res)
	if out := string(b); strings.Contains(out, "jane.doe@example.com") || !strings.Contains(out, "order-service-7f9") {
		t.Errorf("out: %s", out)
	}
}

// Names that are or hold a dot segment are refused before any call, on a
// profile that serves every vhost too.
func TestDotSegmentsRefused(t *testing.T) {
	f := &fakeMgmt{}
	d := Deps{Mgmt: f, Profile: profile()}
	for _, c := range []struct {
		op string
		a  Args
	}{
		{"queues", Args{VHost: ".."}},
		{"queue", Args{VHost: "..", Target: "definitions"}},
		{"queue", Args{Target: ".."}},
		{"exchanges", Args{VHost: "."}},
		{"bindings", Args{Target: "exchange:.."}},
		{"policies", Args{VHost: "a/../b"}},
		{"stream", Args{Target: "../users"}},
	} {
		_, err := Catalogue()[c.op].Run(context.Background(), d, c.a)
		var re *refusal.Error
		if !errors.As(err, &re) || re.Code != CodeUnsafeName {
			t.Errorf("%s %+v: error = %v", c.op, c.a, err)
		}
	}
	if len(f.calls) != 0 {
		t.Errorf("calls made: %v", f.calls)
	}
}

func TestStripUserinfo(t *testing.T) {
	for in, want := range map[string]string{
		"amqps://u:p@h:5671/v":                        "amqps://h:5671/v",
		"amqp://u@h":                                  "amqp://h",
		"amqps://h:5671":                              "amqps://h:5671",
		"see amqp://a:b@x and amqps://c@y/z":          "see amqp://x and amqps://y/z",
		"https://user:pass@example.com/path?a=b@c":    "https://example.com/path?a=b@c",
		"amqp://p%40ss:w%3Ard@h":                      "amqp://h",
		"mailto:someone@example.com is not a URI one": "mailto:someone@example.com is not a URI one",
	} {
		if got := StripUserinfo(in); got != want {
			t.Errorf("StripUserinfo(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestParametersStripped(t *testing.T) {
	f := &fakeMgmt{}
	d := Deps{Mgmt: f, Profile: profile("orders")}
	res, err := Catalogue()["policies"].Run(context.Background(), d, Args{VHost: "orders"})
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(res)
	out := string(b)
	for _, gone := range []string{"shovel-user", "shovel-pass", "only-user", "u:p@", "x:y@", "secret-vhost"} {
		if strings.Contains(out, gone) {
			t.Errorf("%q kept: %s", gone, out)
		}
	}
	for _, kept := range []string{"broker-a.example.com:5671/orders", "amqp://broker-b.example.com", "amqps://h:5671/v", "move-orders"} {
		if !strings.Contains(out, kept) {
			t.Errorf("%q dropped: %s", kept, out)
		}
	}
}

func TestVHostOutsideProfileRefused(t *testing.T) {
	f := &fakeMgmt{}
	d := Deps{Mgmt: f, Profile: profile("orders")}
	for _, name := range []string{"queues", "queue", "exchanges", "bindings", "policies", "consumers", "stream", "stream-offset"} {
		_, err := Catalogue()[name].Run(context.Background(), d, Args{VHost: "payments", Target: "x", Consumer: "c"})
		var re *refusal.Error
		if !errors.As(err, &re) || re.Code != CodeVHostNotServed {
			t.Errorf("%s: error = %v", name, err)
		}
	}
	if len(f.calls) != 0 {
		t.Errorf("calls made: %v", f.calls)
	}
}

func TestDefaultVHost(t *testing.T) {
	for _, c := range []struct {
		vhosts []string
		arg    string
		want   string
		ok     bool
	}{
		{nil, "", "/", true},
		{nil, "any", "any", true},
		{[]string{"orders"}, "", "orders", true},
		{[]string{"/", "orders"}, "", "", false},
		{[]string{"/", "orders"}, "orders", "orders", true},
	} {
		got, err := ResolveVHost(profile(c.vhosts...), c.arg)
		if (err == nil) != c.ok || got != c.want {
			t.Errorf("ResolveVHost(%v, %q) = %q, %v", c.vhosts, c.arg, got, err)
		}
	}
}

func TestQueuesQuery(t *testing.T) {
	f := &fakeMgmt{}
	d := Deps{Mgmt: f, Profile: profile("orders")}
	if _, err := Catalogue()["queues"].Run(context.Background(), d, Args{VHost: "orders", Filter: "order.*", Sort: "messages", Page: 2}); err != nil {
		t.Fatal(err)
	}
	q := f.query[0]
	if q.Get("name") != `^order\..*$` || q.Get("use_regex") != "true" || q.Get("sort") != "messages" || q.Get("sort_reverse") != "true" {
		t.Errorf("query = %v", q)
	}
	if !strings.Contains(q.Get("columns"), "messages_ready") {
		t.Errorf("columns = %q", q.Get("columns"))
	}
	if _, err := Catalogue()["queues"].Run(context.Background(), d, Args{VHost: "orders", Sort: "arguments"}); err == nil {
		t.Error("unknown sort key accepted")
	}
}

func TestStreamNeedsProtocol(t *testing.T) {
	d := Deps{Mgmt: &fakeMgmt{}, Profile: profile("orders")}
	_, err := Catalogue()["stream-offset"].Run(context.Background(), d, Args{VHost: "orders", Target: "order-events", Consumer: "billing"})
	var re *refusal.Error
	if !errors.As(err, &re) || re.Code != CodeProtocolNotConfigured || !strings.Contains(re.Reason, "stream_url") {
		t.Errorf("error = %v", err)
	}
}
