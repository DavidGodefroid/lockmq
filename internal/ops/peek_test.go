package ops

import (
	"strings"
	"testing"

	"github.com/DavidGodefroid/lockkit/refusal"

	"github.com/DavidGodefroid/lockmq/internal/broker/mgmt"
	"github.com/DavidGodefroid/lockmq/internal/config"
)

func quorum(args, policy, operator map[string]any, major int) QueueInfo {
	return QueueInfo{Name: "payments", VHost: "/", Type: "quorum", Arguments: args, PolicyDefinition: policy,
		OperatorPolicyDefinition: operator, BrokerMajor: major, MessagesReady: 12, MessagesUnacked: 2}
}

func TestEffectiveDeliveryLimit(t *testing.T) {
	cases := []struct {
		name  string
		q     QueueInfo
		limit int
		ok    bool
	}{
		{"classic", QueueInfo{Type: "classic", BrokerMajor: 4}, 0, false},
		{"argument", quorum(map[string]any{"x-delivery-limit": 20.0}, nil, nil, 4), 20, true},
		{"policy 10 + argument 20", quorum(map[string]any{"x-delivery-limit": 20.0}, map[string]any{"delivery-limit": 10.0}, nil, 4), 10, true},
		{"operator policy 5", quorum(map[string]any{"x-delivery-limit": 20.0}, map[string]any{"delivery-limit": 10.0}, map[string]any{"delivery-limit": 5.0}, 4), 5, true},
		{"4.x default", quorum(nil, nil, nil, 4), 20, true},
		{"3.13 no default", quorum(nil, nil, nil, 3), 0, false},
		{"4.x unlimited (-1)", quorum(map[string]any{"x-delivery-limit": -1.0}, nil, nil, 4), 0, false},
		{"int argument", quorum(map[string]any{"x-delivery-limit": int64(7)}, nil, nil, 3), 7, true},
	}
	for _, c := range cases {
		limit, ok := EffectiveDeliveryLimit(c.q)
		if limit != c.limit || ok != c.ok {
			t.Errorf("%s: %d %v, want %d %v", c.name, limit, ok, c.limit, c.ok)
		}
	}
}

func refusedWith(t *testing.T, r *refusal.Error, code refusal.Code, words ...string) {
	t.Helper()
	if r == nil {
		t.Fatalf("not refused, want %s", code)
	}
	if r.Code != code {
		t.Errorf("code = %s, want %s (%s)", r.Code, code, r.Reason)
	}
	for _, w := range words {
		if !strings.Contains(r.Reason, w) {
			t.Errorf("reason %q does not name %q", r.Reason, w)
		}
	}
}

func TestCheckPeek(t *testing.T) {
	classic := QueueInfo{Name: "orders.failed", VHost: "/", Type: "classic", BrokerMajor: 4, MessagesUnacked: 3}
	w, r := CheckPeek(config.TierPeek, classic, -1, 3)
	if r != nil {
		t.Fatalf("classic refused: %v", r)
	}
	joined := strings.Join(w, "\n")
	for _, want := range []string{"NOT READ-ONLY", "redelivered", "unacked 3"} {
		if !strings.Contains(joined, want) {
			t.Errorf("warnings miss %q: %v", want, w)
		}
	}

	q20 := quorum(map[string]any{"x-delivery-limit": 20.0}, nil, nil, 4)
	if _, r := CheckPeek(config.TierPeek, q20, 16, 3); r != nil {
		t.Errorf("limit 20 seen 16 headroom 3 refused: %v", r)
	}
	_, r = CheckPeek(config.TierPeek, q20, 17, 3)
	refusedWith(t, r, CodeDeliveryLimit, "20", "17")

	w, r = CheckPeek(config.TierPeek, q20, -1, 3)
	if r != nil || !strings.Contains(strings.Join(w, "\n"), "first peek of this queue: delivery counts unknown") {
		t.Errorf("first peek: %v %v", w, r)
	}

	_, r = CheckPeek(config.TierPeek, quorum(map[string]any{"x-delivery-limit": 3.0}, nil, nil, 4), -1, 3)
	refusedWith(t, r, CodeDeliveryLimit, "3")

	_, r = CheckPeek(config.TierPeek, QueueInfo{Name: "order-events", Type: "stream"}, -1, 3)
	refusedWith(t, r, CodeWrongQueueType, "read")

	_, r = CheckPeek(config.TierRead, classic, -1, 3)
	refusedWith(t, r, CodeTier, "tier")

	_, r = CheckPeek(config.TierPeek, QueueInfo{Name: "x", Type: "mqtt_qos0"}, -1, 3)
	refusedWith(t, r, CodeWrongQueueType)
}

func TestSeen(t *testing.T) {
	var s Seen
	if s.Max("/", "payments") != -1 {
		t.Error("unknown queue not -1")
	}
	s.Note("/", "payments", 2)
	s.Note("/", "payments", 1)
	s.Note("orders", "payments", 9)
	if s.Max("/", "payments") != 2 || s.Max("orders", "payments") != 9 {
		t.Errorf("max = %d / %d", s.Max("/", "payments"), s.Max("orders", "payments"))
	}
}

func TestMaxDeliveryCount(t *testing.T) {
	msgs := []mgmt.Message{
		{Properties: map[string]any{"headers": map[string]any{"x-delivery-count": 2.0}}},
		{Properties: map[string]any{"headers": map[string]any{"x-delivery-count": 5.0}}},
		{Properties: map[string]any{}},
	}
	if n := MaxDeliveryCount(msgs); n != 5 {
		t.Errorf("max = %d", n)
	}
	if n := MaxDeliveryCount(nil); n != 0 {
		t.Errorf("empty = %d", n)
	}
}

func TestQueueInfoFrom(t *testing.T) {
	raw := map[string]any{"name": "payments", "vhost": "/", "type": "quorum", "messages_ready": 4.0,
		"messages_unacknowledged": 1.0, "consumers": 2.0, "arguments": map[string]any{"x-delivery-limit": 5.0},
		"effective_policy_definition": map[string]any{}}
	q := QueueInfoFrom(raw, "4.1.8", nil)
	if q.Name != "payments" || q.Type != "quorum" || q.BrokerMajor != 4 || q.MessagesReady != 4 || q.MessagesUnacked != 1 || q.Consumers != 2 {
		t.Errorf("q = %+v", q)
	}
	if l, ok := EffectiveDeliveryLimit(q); !ok || l != 5 {
		t.Errorf("limit = %d %v", l, ok)
	}
}
