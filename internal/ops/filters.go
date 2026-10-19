package ops

import "strings"

// fields is a field allowlist: a key maps to nil (keep the whole value) or
// to the allowlist of its own fields.
type fields map[string]fields

// spec builds an allowlist from dotted paths ("message_stats.ack_details.rate").
func spec(paths ...string) fields {
	root := fields{}
	for _, p := range paths {
		cur := root
		parts := strings.Split(p, ".")
		for i, part := range parts {
			if i == len(parts)-1 {
				if _, ok := cur[part]; !ok {
					cur[part] = nil
				}
				break
			}
			next := cur[part]
			if next == nil {
				next = fields{}
				cur[part] = next
			}
			cur = next
		}
	}
	return root
}

// columns lists the dotted paths of f, for the management API's columns.
func (f fields) columns() []string {
	var out []string
	var walk func(prefix string, f fields)
	walk = func(prefix string, f fields) {
		for k, sub := range f {
			if sub == nil {
				out = append(out, prefix+k)
				continue
			}
			walk(prefix+k+".", sub)
		}
	}
	walk("", f)
	sortStrings(out)
	return out
}

// pick keeps only the allowlisted fields of v: a map is filtered, a list
// filtered element by element; anything else is dropped when an allowlist
// expects an object.
func pick(v any, f fields) any {
	switch x := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(f))
		for k, sub := range f {
			e, ok := x[k]
			if !ok {
				continue
			}
			if sub == nil {
				out[k] = e
				continue
			}
			if p := pick(e, sub); p != nil {
				out[k] = p
			}
		}
		return out
	case []any:
		out := make([]any, 0, len(x))
		for _, e := range x {
			if p := pick(e, f); p != nil {
				out = append(out, p)
			}
		}
		return out
	default:
		return nil
	}
}

var rates = []string{
	"message_stats.publish_details.rate",
	"message_stats.deliver_get_details.rate",
	"message_stats.ack_details.rate",
	"message_stats.redeliver_details.rate",
}

var (
	overviewFields = spec(append([]string{"rabbitmq_version", "erlang_version", "product_name", "product_version",
		"management_version", "cluster_name", "object_totals", "queue_totals"}, rates[:3]...)...)
	nodeFields = spec("name", "type", "running", "mem_alarm", "disk_free_alarm", "mem_used", "mem_limit",
		"disk_free", "disk_free_limit", "fd_used", "fd_total", "sockets_used", "sockets_total",
		"proc_used", "proc_total", "uptime", "partitions", "being_drained")
	featureFlagFields = spec("name", "state", "stability")

	queueListFields = spec(append([]string{"name", "vhost", "type", "state", "messages", "messages_ready",
		"messages_unacknowledged", "consumers", "consumer_utilisation", "policy", "operator_policy",
		"idle_since", "arguments"}, rates...)...)
	queueFields = spec(append([]string{"name", "vhost", "type", "state", "durable", "auto_delete", "exclusive",
		"node", "leader", "members", "messages", "messages_ready", "messages_unacknowledged", "consumers",
		"consumer_utilisation", "arguments", "effective_policy_definition", "policy", "operator_policy",
		"idle_since"}, rates...)...)
	exchangeFields = spec("name", "vhost", "type", "durable", "auto_delete", "internal", "arguments", "policy",
		"message_stats.publish_in_details.rate", "message_stats.publish_out_details.rate")
	bindingFields   = spec("source", "destination", "destination_type", "routing_key", "arguments", "vhost")
	policyFields    = spec("name", "vhost", "pattern", "apply-to", "definition", "priority")
	parameterFields = spec("component", "name", "vhost", "value")
	consumerFields  = spec("queue.name", "queue.vhost", "consumer_tag", "ack_required", "prefetch_count",
		"active", "activity_status", "exclusive", "arguments")
	connectionFields = spec("vhost", "user", "state", "protocol", "channels", "connected_at", "ssl",
		"auth_mechanism", "client_properties.product", "client_properties.version",
		"client_properties.platform", "client_properties.connection_name")
	channelFields = spec("vhost", "user", "number", "state", "prefetch_count", "global_prefetch_count",
		"consumer_count", "messages_unacknowledged", "messages_unconfirmed", "messages_uncommitted",
		"confirm", "transactional", "idle_since")
)
