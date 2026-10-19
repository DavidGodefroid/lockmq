//go:build integration

package integration

import (
	"testing"
)

// The recommended broker user (tag monitoring, configure and write "^$")
// cannot change the broker: defence in depth under lockmq's allowlist.
func TestReadOnlyUserCannotWrite(t *testing.T) {
	each(t, func(t *testing.T, b *broker) {
		ro := func(method, path string, body any) int {
			code, err := b.call(roUser, roPass, method, path, body, nil)
			if err != nil {
				t.Fatalf("%s %s: %v", method, path, err)
			}
			return code
		}
		before, err := b.queueDepth(classicName)
		if err != nil {
			t.Fatal(err)
		}
		if code := ro("POST", "/api/exchanges/%2F/amq.default/publish", map[string]any{
			"properties": map[string]any{}, "routing_key": classicName, "payload": "x", "payload_encoding": "string"}); code < 400 {
			t.Errorf("publish: HTTP %d", code)
		}
		if code := ro("PUT", "/api/queues/%2F/it-new-queue", map[string]any{"durable": false}); code < 400 {
			t.Errorf("declare: HTTP %d", code)
		}
		// Not checked: purge. RabbitMQ allows queue.purge (and DELETE
		// .../contents) with the read permission alone, so a broker user that
		// can peek a queue can also purge it; only lockmq's endpoint
		// allowlist stands in the way (docs/security-model.md).
		if code := ro("DELETE", "/api/queues/%2F/"+classicName, nil); code < 400 {
			t.Errorf("delete: HTTP %d", code)
		}
		if code := ro("PUT", "/api/policies/%2F/it-policy", map[string]any{"pattern": ".*", "definition": map[string]any{"max-length": 1}}); code < 400 {
			t.Errorf("policy: HTTP %d", code)
		}
		if after, err := b.waitDepth(classicName, before); err != nil || after != before {
			t.Errorf("depth %d → %d (%v)", before, after, err)
		}
	})
}
