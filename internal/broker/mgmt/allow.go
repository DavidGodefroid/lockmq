package mgmt

// Allowed is the allowlist of management API endpoints lockmq reads, all
// with GET: what is not listed is never called. {v} is the vhost and {n} a
// queue or exchange name. /api/definitions, /api/users* and
// /api/permissions* (password hashes, other users' grants) are left out
// on purpose.
var Allowed = map[string]string{
	"overview":          "/api/overview",
	"nodes":             "/api/nodes",
	"feature-flags":     "/api/feature-flags",
	"queues":            "/api/queues/{v}",
	"queue":             "/api/queues/{v}/{n}",
	"queue-bindings":    "/api/queues/{v}/{n}/bindings",
	"exchanges":         "/api/exchanges/{v}",
	"bindings":          "/api/bindings/{v}",
	"bindings-src":      "/api/exchanges/{v}/{n}/bindings/source",
	"bindings-dst":      "/api/exchanges/{v}/{n}/bindings/destination",
	"policies":          "/api/policies/{v}",
	"operator-policies": "/api/operator-policies/{v}",
	"consumers":         "/api/consumers/{v}",
	"connections":       "/api/connections",
	"channels":          "/api/channels",
	"whoami":            "/api/whoami",
	"parameters":        "/api/parameters",
	"global-parameters": "/api/global-parameters",
}

// peekPath is the one endpoint called with POST: the management "get" of
// a queue peek, always with ackmode ack_requeue_true.
const peekPath = "/api/queues/{v}/{n}/get"
