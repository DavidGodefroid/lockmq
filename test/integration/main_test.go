//go:build integration

package integration

import (
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
)

// brokers are the running containers, one per image.
var brokers []*broker

// Fixture names: neutral, fabricated.
const (
	streamName  = "order-events"
	emptyStream = "empty-events"
	streamCount = 30
	namedRef    = "billing"
	namedOffset = 5
)

func images() []string {
	if v := os.Getenv("LOCKMQ_IT_IMAGES"); v != "" {
		return strings.Split(v, ",")
	}
	return []string{"rabbitmq:3.13-management", "rabbitmq:4.1-management"}
}

func TestMain(m *testing.M) {
	var (
		mu   sync.Mutex
		wg   sync.WaitGroup
		errs []string
	)
	for _, img := range images() {
		wg.Add(1)
		go func(img string) {
			defer wg.Done()
			b, err := startBroker(img)
			if err == nil {
				err = seed(b)
				if err != nil {
					b.Remove()
				}
			}
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				errs = append(errs, err.Error())
				return
			}
			brokers = append(brokers, b)
		}(img)
	}
	wg.Wait()
	code := 1
	if len(errs) == 0 {
		code = m.Run()
	} else {
		fmt.Fprintln(os.Stderr, "integration: brokers did not start:\n"+strings.Join(errs, "\n"))
	}
	for _, b := range brokers {
		b.Remove()
	}
	os.Exit(code)
}

// seed creates the read-only user and the fixtures on vhost "/".
func seed(b *broker) error {
	steps := []struct {
		method, path string
		body         any
	}{
		{"PUT", "/api/users/" + roUser, map[string]any{"password": roPass, "tags": "monitoring"}},
		{"PUT", "/api/permissions/%2F/" + roUser, map[string]any{"configure": "^$", "write": "^$", "read": ".*"}},
		{"PUT", "/api/queues/%2F/" + streamName, map[string]any{"durable": true, "arguments": map[string]any{"x-queue-type": "stream"}}},
		{"PUT", "/api/queues/%2F/" + emptyStream, map[string]any{"durable": true, "arguments": map[string]any{"x-queue-type": "stream"}}},
	}
	for _, s := range steps {
		if err := b.mustAdmin(s.method, s.path, s.body); err != nil {
			return err
		}
	}
	for i := range streamCount {
		body := fmt.Sprintf(`{"order_id": %d, "status": "created", "customer": {"email": "customer%d@example.com"}}`, i, i)
		props := map[string]any{"content_type": "application/json", "message_id": fmt.Sprintf("m-%d", i),
			"headers": map[string]any{"x-tenant": "acme"}}
		if err := b.publish("/", streamName, body, props); err != nil {
			return err
		}
	}
	return storeNamedOffset(b)
}

// each runs f once per broker.
func each(t *testing.T, f func(t *testing.T, b *broker)) {
	t.Helper()
	for _, b := range brokers {
		t.Run(b.Version, func(t *testing.T) { f(t, b) })
	}
}
