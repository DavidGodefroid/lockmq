// Package ops is lockmq's operation catalogue: an allowlist of what a
// client may ask, each operation with its tier, protocol and whether it
// needs the human's approval, and the field allowlists that filter what
// the broker answers before it reaches a client.
package ops

import (
	"context"
	"errors"
	"net/url"
	"regexp"
	"slices"
	"sort"
	"strings"

	"github.com/DavidGodefroid/lockkit/pii"
	"github.com/DavidGodefroid/lockkit/refusal"

	"github.com/DavidGodefroid/lockmq/internal/broker/mgmt"
	"github.com/DavidGodefroid/lockmq/internal/config"
)

// Refusal codes of the catalogue. Never rename one: they are part of the
// client protocol.
const (
	CodeVHostNotServed        refusal.Code = "E_VHOST_NOT_SERVED"
	CodeProtocolNotConfigured refusal.Code = "E_PROTOCOL_NOT_CONFIGURED"
	CodeBadArgs               refusal.Code = "E_BAD_ARGS"
	CodeWrongQueueType        refusal.Code = "E_WRONG_QUEUE_TYPE"
	CodeTier                  refusal.Code = "E_TIER"
	CodeUnsafeName            refusal.Code = "E_UNSAFE_NAME"
	CodeUnmaskOff             refusal.Code = "E_UNMASK_OFF"
)

func init() {
	refusal.Register(CodeVHostNotServed, refusal.RetryAskUser,
		"a vhost this profile does not serve", "use a vhost listed by lockmq status, or ask the user")
	refusal.Register(CodeProtocolNotConfigured, refusal.RetryAskUser,
		"the operation needs a protocol the profile does not configure", "ask the user to set the profile key named in the reason")
	refusal.Register(CodeBadArgs, refusal.RetryRewrite,
		"invalid arguments", "fix the arguments named in the reason")
	refusal.Register(CodeWrongQueueType, refusal.RetryRewrite,
		"the operation does not apply to this queue type", "use the operation named in the reason")
	refusal.Register(CodeTier, refusal.RetryAskUser,
		"the profile's tier does not allow this operation", "only the user can change the profile's tier")
	refusal.Register(CodeUnsafeName, refusal.RetryStop,
		`a vhost or name that is, or holds, a "." or ".." path segment`, "lockmq cannot address it; do not retry")
	refusal.Register(CodeUnmaskOff, refusal.RetryAskUser,
		"unmasked output is off in this console", "read masked, or ask the user to allow it in the console (:unmask)")
}

// Protocols an operation needs.
const (
	ProtoMgmt   = "management"
	ProtoStream = "stream" // stream protocol, or AMQP for reads
)

// Mgmt is what the catalogue uses of the management client.
type Mgmt interface {
	Get(ctx context.Context, p mgmt.Path, q url.Values, into any) error
	GetPageQ(ctx context.Context, p mgmt.Path, page, size int, columns []string, extra url.Values, into any) (int, error)
	PeekRequeue(ctx context.Context, vhost, queue string, count, truncate int) ([]mgmt.Message, error)
}

// StreamInfo is the stream protocol's metadata side (package stream).
type StreamInfo interface {
	Stats(ctx context.Context, vhost, stream string) (first, last int64, err error)
	QueryOffset(ctx context.Context, vhost, stream, consumerRef string) (int64, error)
}

// Deps are the broker clients and the profile an operation runs with.
// Stream is nil when the profile has no stream_url.
type Deps struct {
	Mgmt    Mgmt
	Stream  StreamInfo
	Profile config.Profile
}

// Args are an operation's arguments, as a client sends them.
type Args struct {
	VHost    string `json:"vhost,omitempty"`
	Target   string `json:"target,omitempty"`
	Filter   string `json:"filter,omitempty"`
	Sort     string `json:"sort,omitempty"`
	From     string `json:"from,omitempty"`
	Consumer string `json:"consumer,omitempty"`
	Page     int    `json:"page,omitempty"`
	Count    int    `json:"count,omitempty"`
}

// Op is one operation of the catalogue.
type Op struct {
	Name string
	// Tier is the lowest profile tier that allows it.
	Tier     config.Tier
	Protocol string
	// Prompt: the human approves every run (message content).
	Prompt bool
	Run    func(ctx context.Context, d Deps, a Args) (any, error)
}

// List is a page of a list operation.
type List struct {
	Items    any `json:"items"`
	Page     int `json:"page,omitempty"`
	PageSize int `json:"page_size,omitempty"`
	Total    int `json:"total,omitempty"`
}

// Catalogue returns the operations of design §5.2; nothing else exists.
func Catalogue() map[string]Op {
	ops := []Op{
		{Name: "overview", Protocol: ProtoMgmt, Run: overview},
		{Name: "queues", Protocol: ProtoMgmt, Run: queues},
		{Name: "queue", Protocol: ProtoMgmt, Run: queue},
		{Name: "exchanges", Protocol: ProtoMgmt, Run: exchanges},
		{Name: "bindings", Protocol: ProtoMgmt, Run: bindings},
		{Name: "policies", Protocol: ProtoMgmt, Run: policies},
		{Name: "consumers", Protocol: ProtoMgmt, Run: consumers},
		{Name: "connections", Protocol: ProtoMgmt, Run: connections},
		{Name: "channels", Protocol: ProtoMgmt, Run: channels},
		{Name: "stream", Protocol: ProtoStream, Run: streamInfo},
		{Name: "stream-offset", Protocol: ProtoStream, Run: streamOffset},
		{Name: "read", Protocol: ProtoStream, Prompt: true, Run: viaConsole},
		{Name: "peek", Tier: config.TierPeek, Protocol: ProtoMgmt, Prompt: true, Run: viaConsole},
	}
	out := make(map[string]Op, len(ops))
	for _, op := range ops {
		out[op.Name] = op
	}
	return out
}

// viaConsole: message reads run through the console's plan, approval and
// masking path (package mqconsole), never as a bare catalogue call.
func viaConsole(context.Context, Deps, Args) (any, error) {
	return nil, errors.New("ops: message reads run through the console's approval path")
}

// ResolveVHost returns the vhost an operation acts on: the argument when
// the profile serves it, else the profile's only vhost, else "/" for a
// profile that serves every vhost.
func ResolveVHost(p config.Profile, arg string) (string, error) {
	if arg == "" {
		switch len(p.VHosts) {
		case 0:
			return "/", nil
		case 1:
			return p.VHosts[0], nil
		default:
			return "", refusal.Newf(CodeBadArgs, "name a vhost: the profile serves %s", strings.Join(p.VHosts, ", "))
		}
	}
	if err := SafeName("vhost", arg); err != nil {
		return "", err
	}
	if len(p.VHosts) > 0 && !slices.Contains(p.VHosts, arg) {
		return "", refusal.Newf(CodeVHostNotServed, "vhost %q is not served by profile %s", arg, p.Name)
	}
	return arg, nil
}

func served(p config.Profile, vhost string) bool {
	return len(p.VHosts) == 0 || slices.Contains(p.VHosts, vhost)
}

// MaxNameLen bounds a queue, exchange or consumer name (RabbitMQ: 255 bytes).
const MaxNameLen = 255

func needName(what, s string) error {
	if s == "" {
		return refusal.Newf(CodeBadArgs, "%s is required", what)
	}
	if len(s) > MaxNameLen {
		return refusal.Newf(CodeBadArgs, "%s is longer than %d bytes", what, MaxNameLen)
	}
	return SafeName(what, s)
}

// SafeName refuses, before any I/O, a name that would be a "." or ".."
// segment of a management API path (mgmt.DotSegment): such a path could
// reach an endpoint outside the allowlist.
func SafeName(what, s string) error {
	if mgmt.DotSegment(s) {
		return refusal.Newf(CodeUnsafeName, `%s %q is or holds a "." or ".." path segment: refused`, what, clipName(s))
	}
	return nil
}

func clipName(s string) string {
	if len(s) > 64 {
		return s[:64] + "…"
	}
	return s
}

func pageOf(a Args) int {
	if a.Page < 1 {
		return 1
	}
	return a.Page
}

func overview(ctx context.Context, d Deps, _ Args) (any, error) {
	var ov, nodes, flags any
	if err := d.Mgmt.Get(ctx, mgmt.Path{Kind: "overview"}, nil, &ov); err != nil {
		return nil, err
	}
	if err := d.Mgmt.Get(ctx, mgmt.Path{Kind: "nodes"}, nil, &nodes); err != nil {
		return nil, err
	}
	if err := d.Mgmt.Get(ctx, mgmt.Path{Kind: "feature-flags"}, nil, &flags); err != nil {
		return nil, err
	}
	return map[string]any{
		"overview":      pick(ov, overviewFields),
		"nodes":         pick(nodes, nodeFields),
		"feature_flags": pick(flags, featureFlagFields),
	}, nil
}

// sortKeys are the queue list's sort keys; every one but name sorts
// largest first.
var sortKeys = []string{"name", "messages", "messages_ready", "messages_unacknowledged", "consumers"}

// globRegexp turns a name glob (* and ?) into an anchored regexp.
func globRegexp(g string) string {
	q := regexp.QuoteMeta(g)
	q = strings.ReplaceAll(q, `\*`, ".*")
	q = strings.ReplaceAll(q, `\?`, ".")
	return "^" + q + "$"
}

func queues(ctx context.Context, d Deps, a Args) (any, error) {
	vh, err := ResolveVHost(d.Profile, a.VHost)
	if err != nil {
		return nil, err
	}
	extra := url.Values{}
	if a.Filter != "" {
		if len(a.Filter) > MaxNameLen {
			return nil, refusal.Newf(CodeBadArgs, "filter is longer than %d bytes", MaxNameLen)
		}
		extra.Set("name", globRegexp(a.Filter))
		extra.Set("use_regex", "true")
	}
	if a.Sort != "" {
		if !slices.Contains(sortKeys, a.Sort) {
			return nil, refusal.Newf(CodeBadArgs, "sort: want one of %s", strings.Join(sortKeys, ", "))
		}
		extra.Set("sort", a.Sort)
		if a.Sort != "name" {
			extra.Set("sort_reverse", "true")
		}
	}
	return listPage(ctx, d, mgmt.Path{Kind: "queues", Segs: []string{vh}}, a, queueListFields, extra, nil)
}

// listPage reads one page of a list endpoint, keeps the allowlisted fields
// and, when keep is set, only the items it accepts.
func listPage(ctx context.Context, d Deps, p mgmt.Path, a Args, f fields, extra url.Values, keep func(map[string]any) bool) (any, error) {
	size := d.Profile.Limits.ListPageSize
	if size <= 0 || size > mgmt.MaxPageSize {
		size = mgmt.MaxPageSize
	}
	var items []any
	total, err := d.Mgmt.GetPageQ(ctx, p, pageOf(a), size, f.columns(), extra, &items)
	if err != nil {
		return nil, err
	}
	if keep != nil {
		kept := items[:0]
		for _, it := range items {
			if m, ok := it.(map[string]any); ok && keep(m) {
				kept = append(kept, it)
			}
		}
		items = kept
	}
	out := pick(items, f)
	if out == nil {
		out = []any{}
	}
	return List{Items: out, Page: pageOf(a), PageSize: size, Total: total}, nil
}

func queue(ctx context.Context, d Deps, a Args) (any, error) {
	vh, err := ResolveVHost(d.Profile, a.VHost)
	if err != nil {
		return nil, err
	}
	if err := needName("queue name (target)", a.Target); err != nil {
		return nil, err
	}
	segs := []string{vh, a.Target}
	var q, binds any
	if err := d.Mgmt.Get(ctx, mgmt.Path{Kind: "queue", Segs: segs}, nil, &q); err != nil {
		return nil, err
	}
	if err := d.Mgmt.Get(ctx, mgmt.Path{Kind: "queue-bindings", Segs: segs}, nil, &binds); err != nil {
		return nil, err
	}
	// The queue object lists its own consumers (consumer_details): no
	// vhost-wide /api/consumers, which is not paginated and can exceed
	// the response cap on a large vhost.
	var cons any
	if m, ok := q.(map[string]any); ok {
		cons = m["consumer_details"]
	}
	out := map[string]any{"queue": pick(q, queueFields), "bindings": orEmpty(pick(binds, bindingFields))}
	out["consumers"] = orEmpty(pick(cons, consumerFields))
	return out, nil
}

func orEmpty(v any) any {
	if v == nil {
		return []any{}
	}
	return v
}

func exchanges(ctx context.Context, d Deps, a Args) (any, error) {
	vh, err := ResolveVHost(d.Profile, a.VHost)
	if err != nil {
		return nil, err
	}
	return listPage(ctx, d, mgmt.Path{Kind: "exchanges", Segs: []string{vh}}, a, exchangeFields, nil, nil)
}

// bindings lists the bindings of a vhost, of one queue (target
// "queue:NAME") or of one exchange, both directions (target
// "exchange:NAME").
func bindings(ctx context.Context, d Deps, a Args) (any, error) {
	vh, err := ResolveVHost(d.Profile, a.VHost)
	if err != nil {
		return nil, err
	}
	get := func(p mgmt.Path) (any, error) {
		var v any
		if err := d.Mgmt.Get(ctx, p, nil, &v); err != nil {
			return nil, err
		}
		return orEmpty(pick(v, bindingFields)), nil
	}
	switch kind, name, _ := strings.Cut(a.Target, ":"); {
	case a.Target == "":
		return get(mgmt.Path{Kind: "bindings", Segs: []string{vh}})
	case kind == "queue":
		if err := needName("queue name", name); err != nil {
			return nil, err
		}
		return get(mgmt.Path{Kind: "queue-bindings", Segs: []string{vh, name}})
	case kind == "exchange":
		if err := needName("exchange name", name); err != nil {
			return nil, err
		}
		src, err := get(mgmt.Path{Kind: "bindings-src", Segs: []string{vh, name}})
		if err != nil {
			return nil, err
		}
		dst, err := get(mgmt.Path{Kind: "bindings-dst", Segs: []string{vh, name}})
		if err != nil {
			return nil, err
		}
		return map[string]any{"source": src, "destination": dst}, nil
	default:
		return nil, refusal.New(CodeBadArgs, `target: want "queue:NAME" or "exchange:NAME"`)
	}
}

// policies lists the policies, operator policies and runtime parameters
// (shovels, federation upstreams) of a vhost; every URI of a parameter
// loses its userinfo.
func policies(ctx context.Context, d Deps, a Args) (any, error) {
	vh, err := ResolveVHost(d.Profile, a.VHost)
	if err != nil {
		return nil, err
	}
	var pol, opol, params any
	if err := d.Mgmt.Get(ctx, mgmt.Path{Kind: "policies", Segs: []string{vh}}, nil, &pol); err != nil {
		return nil, err
	}
	if err := d.Mgmt.Get(ctx, mgmt.Path{Kind: "operator-policies", Segs: []string{vh}}, nil, &opol); err != nil {
		return nil, err
	}
	if err := d.Mgmt.Get(ctx, mgmt.Path{Kind: "parameters"}, nil, &params); err != nil {
		// The management API shows runtime parameters to policymakers
		// only: a monitoring account gets 401.
		var se *mgmt.StatusError
		if !errors.As(err, &se) || (se.Code != 401 && se.Code != 403) {
			return nil, err
		}
		return map[string]any{
			"policies":          orEmpty(pick(pol, policyFields)),
			"operator_policies": orEmpty(pick(opol, policyFields)),
			"parameters":        "not visible to this account (the management API shows runtime parameters to the policymaker tag only)",
		}, nil
	}
	var mine []any
	if list, ok := params.([]any); ok {
		for _, p := range list {
			if m, ok := p.(map[string]any); ok && m["vhost"] == vh {
				mine = append(mine, p)
			}
		}
	}
	return map[string]any{
		"policies":          orEmpty(pick(pol, policyFields)),
		"operator_policies": orEmpty(pick(opol, policyFields)),
		"parameters":        orEmpty(stripAll(pick(mine, parameterFields))),
	}, nil
}

func consumers(ctx context.Context, d Deps, a Args) (any, error) {
	vh, err := ResolveVHost(d.Profile, a.VHost)
	if err != nil {
		return nil, err
	}
	var v any
	if err := d.Mgmt.Get(ctx, mgmt.Path{Kind: "consumers", Segs: []string{vh}}, nil, &v); err != nil {
		return nil, err
	}
	return orEmpty(pick(v, consumerFields)), nil
}

// connections and channels are broker-wide: only those of served vhosts
// are kept.
func connections(ctx context.Context, d Deps, a Args) (any, error) {
	return detected(d, "user", "client_properties")(listPage(ctx, d, mgmt.Path{Kind: "connections"}, a, connectionFields, nil, vhostFilter(d.Profile)))
}

func channels(ctx context.Context, d Deps, a Args) (any, error) {
	return detected(d, "user")(listPage(ctx, d, mgmt.Path{Kind: "channels"}, a, channelFields, nil, vhostFilter(d.Profile)))
}

// detected runs the profile's detectors over the free text the client
// sets (a connection name such as "worker-jane.doe@example.com", a user
// name): the string values of keys, or of the maps under them, of every
// item of a list page.
func detected(d Deps, keys ...string) func(any, error) (any, error) {
	return func(v any, err error) (any, error) {
		l, ok := v.(List)
		if err != nil || !ok {
			return v, err
		}
		ds, _ := pii.Detectors(d.Profile.Detectors)
		items, _ := l.Items.([]any)
		for _, it := range items {
			m, ok := it.(map[string]any)
			if !ok {
				continue
			}
			for _, k := range keys {
				switch x := m[k].(type) {
				case string:
					m[k] = pii.Detect(x, ds)
				case map[string]any:
					for sk, sv := range x {
						if s, ok := sv.(string); ok {
							x[sk] = pii.Detect(s, ds)
						}
					}
				}
			}
		}
		return l, nil
	}
}

func vhostFilter(p config.Profile) func(map[string]any) bool {
	return func(m map[string]any) bool {
		vh, _ := m["vhost"].(string)
		return served(p, vh)
	}
}

// QueueType reads the type of a queue ("classic", "quorum", "stream").
func QueueType(ctx context.Context, m Mgmt, vhost, name string) (map[string]any, string, error) {
	var q map[string]any
	if err := m.Get(ctx, mgmt.Path{Kind: "queue", Segs: []string{vhost, name}}, nil, &q); err != nil {
		return nil, "", err
	}
	t, _ := q["type"].(string)
	return q, t, nil
}

func needStream(d Deps) error {
	if d.Stream == nil {
		return refusal.New(CodeProtocolNotConfigured, "this operation needs the stream protocol: set stream_url on the profile")
	}
	return nil
}

func streamInfo(ctx context.Context, d Deps, a Args) (any, error) {
	vh, err := ResolveVHost(d.Profile, a.VHost)
	if err != nil {
		return nil, err
	}
	if err := needName("stream name (target)", a.Target); err != nil {
		return nil, err
	}
	q, typ, err := QueueType(ctx, d.Mgmt, vh, a.Target)
	if err != nil {
		return nil, err
	}
	if typ != "stream" {
		return nil, refusal.Newf(CodeWrongQueueType, "%s is a %s queue, not a stream: use queue (or peek on a profile with tier peek)", a.Target, typ)
	}
	out := map[string]any{"queue": pick(q, queueFields)}
	if d.Stream == nil {
		out["stats"] = "stream_url not set: first/last offsets need the stream protocol"
		return out, nil
	}
	first, last, err := d.Stream.Stats(ctx, vh, a.Target)
	if err != nil {
		return nil, err
	}
	out["stats"] = map[string]any{"first_offset": first, "last_offset": last, "precision": chunkPrecision}
	return out, nil
}

// chunkPrecision qualifies the stream protocol's offsets: the broker
// reports chunk ids, not message offsets.
const chunkPrecision = "chunk: last_offset is the first offset of the stream's last committed chunk; the stream can hold up to one chunk more, and lag can be that much higher"

func streamOffset(ctx context.Context, d Deps, a Args) (any, error) {
	vh, err := ResolveVHost(d.Profile, a.VHost)
	if err != nil {
		return nil, err
	}
	if err := needName("stream name (target)", a.Target); err != nil {
		return nil, err
	}
	if err := needName("consumer reference (consumer)", a.Consumer); err != nil {
		return nil, err
	}
	if err := needStream(d); err != nil {
		return nil, err
	}
	stored, err := d.Stream.QueryOffset(ctx, vh, a.Target, a.Consumer)
	if err != nil {
		return nil, err
	}
	_, last, err := d.Stream.Stats(ctx, vh, a.Target)
	if err != nil {
		return nil, err
	}
	return map[string]any{"consumer": a.Consumer, "stored_offset": stored, "last_offset": last, "lag": max(last-stored, 0), "precision": chunkPrecision}, nil
}

func sortStrings(s []string) { sort.Strings(s) }
