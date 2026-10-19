package ops

import (
	"fmt"
	"strconv"
	"strings"
	"sync"

	"github.com/DavidGodefroid/lockkit/refusal"

	"github.com/DavidGodefroid/lockmq/internal/broker/mgmt"
	"github.com/DavidGodefroid/lockmq/internal/config"
)

// CodeDeliveryLimit refuses a quorum queue peek too close to the queue's
// delivery limit.
const CodeDeliveryLimit refusal.Code = "E_DELIVERY_LIMIT"

func init() {
	refusal.Register(CodeDeliveryLimit, refusal.RetryAskUser,
		"a peek could dead-letter or drop messages of this quorum queue",
		"do not peek this queue again; ask the user")
}

// DefaultQuorumDeliveryLimit is RabbitMQ 4.x's delivery limit of a quorum
// queue that sets none (3.x has no default).
const DefaultQuorumDeliveryLimit = 20

// QueueInfo is what the peek guard reads of a queue.
type QueueInfo struct {
	Name, VHost, Type string
	Arguments         map[string]any
	// PolicyDefinition is the queue's effective policy definition;
	// OperatorPolicyDefinition the definition of its operator policy.
	PolicyDefinition         map[string]any
	OperatorPolicyDefinition map[string]any
	MessagesReady            int
	MessagesUnacked          int
	Consumers                int
	// BrokerMajor is the broker's major version (3, 4).
	BrokerMajor int
}

// QueueInfoFrom reads a management API queue object; version is the
// broker's rabbitmq_version, operator the definition of the queue's
// operator policy (nil if none).
func QueueInfoFrom(q map[string]any, version string, operator map[string]any) QueueInfo {
	str := func(k string) string { s, _ := q[k].(string); return s }
	obj := func(k string) map[string]any { m, _ := q[k].(map[string]any); return m }
	major, _ := strconv.Atoi(strings.SplitN(version, ".", 2)[0])
	num := func(k string) int { n, _ := toInt(q[k]); return n }
	return QueueInfo{
		Name: str("name"), VHost: str("vhost"), Type: str("type"),
		Arguments: obj("arguments"), PolicyDefinition: obj("effective_policy_definition"),
		OperatorPolicyDefinition: operator,
		MessagesReady:            num("messages_ready"), MessagesUnacked: num("messages_unacknowledged"),
		Consumers: num("consumers"), BrokerMajor: major,
	}
}

func toInt(v any) (int, bool) {
	switch x := v.(type) {
	case float64:
		return int(x), true
	case int:
		return x, true
	case int64:
		return int(x), true
	case int32:
		return int(x), true
	}
	return 0, false
}

// EffectiveDeliveryLimit is the delivery limit of a quorum queue: the
// lowest of its x-delivery-limit argument, its policy's and its operator
// policy's delivery-limit; RabbitMQ 4.x's default 20 when none is set. A
// negative limit (4.x) disables it. ok is false for a queue without one.
func EffectiveDeliveryLimit(q QueueInfo) (limit int, ok bool) {
	if q.Type != "quorum" {
		return 0, false
	}
	set := false
	unlimited := false
	for _, v := range []any{q.Arguments["x-delivery-limit"], q.PolicyDefinition["delivery-limit"], q.OperatorPolicyDefinition["delivery-limit"]} {
		n, isNum := toInt(v)
		if !isNum {
			continue
		}
		if n < 0 {
			unlimited = true
			continue
		}
		if !set || n < limit {
			limit, set = n, true
		}
	}
	switch {
	case set:
		return limit, true
	case unlimited:
		return 0, false
	case q.BrokerMajor >= 4:
		return DefaultQuorumDeliveryLimit, true
	}
	return 0, false
}

// Seen keeps, per queue and for one console session, the highest
// x-delivery-count earlier peeks saw. Memory only.
type Seen struct {
	mu  sync.Mutex
	max map[string]int
}

func seenKey(vhost, queue string) string { return vhost + "\x00" + queue }

// Note records the highest delivery count of one peek.
func (s *Seen) Note(vhost, queue string, maxCount int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.max == nil {
		s.max = map[string]int{}
	}
	k := seenKey(vhost, queue)
	if cur, ok := s.max[k]; !ok || maxCount > cur {
		s.max[k] = maxCount
	}
}

// Max is the highest delivery count seen on the queue, -1 before its first
// peek.
func (s *Seen) Max(vhost, queue string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	if v, ok := s.max[seenKey(vhost, queue)]; ok {
		return v
	}
	return -1
}

// MaxDeliveryCount is the highest x-delivery-count among peeked messages.
func MaxDeliveryCount(msgs []mgmt.Message) int {
	maxCount := 0
	for _, m := range msgs {
		h, _ := m.Properties["headers"].(map[string]any)
		if n, ok := toInt(h["x-delivery-count"]); ok && n > maxCount {
			maxCount = n
		}
	}
	return maxCount
}

// CheckPeek decides a peek of q on a profile of tier tier: a refusal, or
// the warnings of the approval screen. seen is Seen.Max (-1: first peek).
// The refusals: a tier below peek, a stream (use read) or a type that is
// neither classic nor quorum, and on a quorum queue a delivery limit at
// or under headroom, or within headroom of the highest count seen.
func CheckPeek(tier config.Tier, q QueueInfo, seen, headroom int) ([]string, *refusal.Error) {
	if tier < config.TierPeek {
		return nil, refusal.New(CodeTier, `peek needs tier = "peek" on the profile; the profile's tier is read, and only the user can change it`)
	}
	switch q.Type {
	case "classic", "quorum":
	case "stream":
		return nil, refusal.Newf(CodeWrongQueueType, "%s is a stream: use read, which moves nothing", q.Name)
	default:
		return nil, refusal.Newf(CodeWrongQueueType, "peek reads classic and quorum queues; %s is a %q queue", q.Name, q.Type)
	}
	notReadOnly := "NOT READ-ONLY: messages are taken from the head and requeued; they come back redelivered=true"
	if q.Type == "quorum" {
		notReadOnly += " and their delivery count grows by 1"
	}
	warnings := []string{
		notReadOnly,
		fmt.Sprintf("queue: %s · ready %d · unacked %d (not visible) · consumers %d", q.Type, q.MessagesReady, q.MessagesUnacked, q.Consumers),
	}
	if q.Consumers > 0 {
		warnings = append(warnings, "active consumers: the order they see can change while the peek holds messages")
	}
	limit, hasLimit := EffectiveDeliveryLimit(q)
	if !hasLimit {
		return warnings, nil
	}
	if limit <= headroom {
		return nil, refusal.Newf(CodeDeliveryLimit, "%s has delivery-limit %d, at or under the profile's delivery_headroom %d: a peek could dead-letter or drop its messages", q.Name, limit, headroom)
	}
	if seen < 0 {
		warnings = append(warnings, fmt.Sprintf("delivery-limit %d · first peek of this queue: delivery counts unknown", limit))
		return warnings, nil
	}
	if limit-seen <= headroom {
		return nil, refusal.Newf(CodeDeliveryLimit, "%s: delivery-limit %d, highest delivery count seen %d, within the profile's delivery_headroom %d", q.Name, limit, seen, headroom)
	}
	warnings = append(warnings, fmt.Sprintf("delivery-limit %d · max seen so far %d/%d", limit, seen, limit))
	return warnings, nil
}
