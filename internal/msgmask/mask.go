package msgmask

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/DavidGodefroid/lockkit/pii"
)

// Body modes (the profile's body_mode).
const (
	ModeRedactUnknown = "redact-unknown"
	ModeDetect        = "detect"
)

// maxDepth is the deepest JSON body parsed; a deeper one is opaque.
const maxDepth = 64

// Message is one message as read from the broker.
type Message struct {
	Properties map[string]any
	// Headers are the AMQP headers or the stream's application properties.
	Headers map[string]any
	Body    []byte
	// Cut marks a body the source shortened (peek's truncate): it is
	// opaque, whatever its type, never parsed or read as text.
	Cut             bool
	ContentType     string
	ContentEncoding string
	// Exchange is the exchange the message was published to, "" if unknown.
	Exchange string
	// Origin is the queue named by the message's first x-death entry; ""
	// takes it from Headers.
	Origin string
}

// Masked is a message fit for a client: every personal value masked.
type Masked struct {
	Properties map[string]any `json:"properties,omitempty"`
	Headers    map[string]any `json:"headers,omitempty"`
	// Body is the parsed JSON, a string, or "<bytes:N sha256:H type:T>".
	Body      any      `json:"body"`
	Truncated bool     `json:"truncated,omitempty"`
	Proposals []string `json:"proposals,omitempty"`
}

// Masker masks the messages of one console session.
type Masker struct {
	rules     Rules
	detectors []pii.Detector
	bodyMode  string
	refs      *pii.RefStore
	maxBody   int
}

// NewMasker returns a masker. bodyMode is redact-unknown or detect (any
// other value is redact-unknown: fail closed); refs keeps the clear values
// behind the references; maxBody caps a body after masking.
func NewMasker(r Rules, detectors []pii.Detector, bodyMode string, refs *pii.RefStore, maxBody int) *Masker {
	if bodyMode != ModeDetect {
		bodyMode = ModeRedactUnknown
	}
	if refs == nil {
		refs = &pii.RefStore{}
	}
	if maxBody <= 0 {
		maxBody = 16384
	}
	return &Masker{rules: r, detectors: detectors, bodyMode: bodyMode, refs: refs, maxBody: maxBody}
}

// job is the masking of one message: the rules that apply to it and the
// proposals found.
type job struct {
	m         *Masker
	result    int
	vhost     string
	queue     string
	masks     []Rule // from the queue, the origin and the exchange
	allows    []Rule // from the queue only
	proposals map[string]bool
}

// Mask masks msg, read from queue (or stream) on vhost, for result number
// result of the reference store (RefStore.Begin).
func (m *Masker) Mask(result int, vhost, queue string, msg Message) Masked {
	if msg.Origin == "" {
		msg.Origin = firstDeathQueue(msg.Headers)
	}
	j := &job{m: m, result: result, vhost: vhost, queue: queue, proposals: map[string]bool{}}
	for _, r := range m.rules.Mask {
		if matches(r.Queues, vhost, queue) || (msg.Origin != "" && matches(r.Queues, vhost, msg.Origin)) ||
			(msg.Exchange != "" && matches(r.Exchanges, vhost, msg.Exchange)) {
			j.masks = append(j.masks, r)
		}
	}
	// Allow rules come from the queue read only: the origin and the
	// exchange a message names can add masking, never remove it.
	for _, r := range m.rules.Allow {
		if matches(r.Queues, vhost, queue) {
			j.allows = append(j.allows, r)
		}
	}
	out := Masked{
		Properties: j.properties(msg.Properties),
		Headers:    j.headers(msg.Headers),
	}
	out.Body, out.Truncated = j.body(msg)
	for p := range j.proposals {
		out.Proposals = append(out.Proposals, p)
	}
	sort.Strings(out.Proposals)
	return out
}

// Clear is msg unmasked, for an approved unmask or the console's clear
// view: headers and properties as read, the body parsed JSON or text, and
// opaque when binary, encoded, too deep or cut by the source (a cut body
// still shows as text, marked truncated). The body is capped at maxBody.
func Clear(msg Message, maxBody int) Masked {
	if maxBody <= 0 {
		maxBody = 16384
	}
	out := Masked{}
	if len(msg.Properties) > 0 {
		out.Properties = map[string]any{}
		for k, v := range msg.Properties {
			if k != "headers" {
				out.Properties[k] = normalize(v)
			}
		}
	}
	if len(msg.Headers) > 0 {
		out.Headers, _ = normalize(msg.Headers).(map[string]any)
	}
	kind, label := classify(msg)
	var v any
	switch {
	case kind == "opaque":
		return withBody(out, opaque(msg.Body, label), false)
	case kind == "json" && !msg.Cut && len(msg.Body) <= 4*maxBody && !tooDeep(msg.Body):
		if parsed, ok := decodeJSON(msg.Body); ok {
			v = parsed
			break
		}
		fallthrough
	default:
		if !utf8.Valid(msg.Body) || !printable(msg.Body) {
			return withBody(out, opaque(msg.Body, label), false)
		}
		v = string(msg.Body)
	}
	if s, ok := v.(string); ok {
		if len(s) > maxBody {
			return withBody(out, pii.CutAt(s, maxBody), true)
		}
		return withBody(out, s, msg.Cut)
	}
	b, err := json.Marshal(v)
	if err != nil {
		return withBody(out, opaque(msg.Body, label), false)
	}
	if len(b) > maxBody {
		return withBody(out, pii.CutAt(string(b), maxBody), true)
	}
	return withBody(out, v, false)
}

func withBody(m Masked, body any, truncated bool) Masked {
	m.Body, m.Truncated = body, truncated
	return m
}

// redact replaces a value with its reference.
func (j *job) redact(v any) any {
	ref := j.m.refs.PutDistinct(j.result, pii.CellText(v))
	if ref == "" {
		return pii.Redacted
	}
	return pii.RedactedRef(ref)
}

// maskMode masks v in a rule's mode: redact gives a reference.
func (j *job) maskMode(v any, mode string) any {
	if v == nil {
		return nil
	}
	if b, ok := v.(bool); ok {
		return b
	}
	if mode == pii.ModeRedact {
		return j.redact(v)
	}
	return pii.MaskValue(pii.CellText(v), mode)
}

// detect runs the detectors over a leaf (text or number).
func (j *job) detect(v any) any {
	switch x := v.(type) {
	case json.Number:
		if out := pii.Detect(x.String(), j.m.detectors); out != x.String() {
			return out
		}
		return x
	case float64:
		s := strconv.FormatFloat(x, 'f', -1, 64)
		if out := pii.Detect(s, j.m.detectors); out != s {
			return out
		}
		return x
	}
	return pii.Detect(v, j.m.detectors)
}

// mapLeaves applies f to every leaf of v, keys through the detectors.
func (j *job) mapLeaves(v any, f func(any) any) any {
	switch x := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(x))
		for _, k := range sortedKeys(x) {
			out[j.detectText(k)] = j.mapLeaves(x[k], f)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = j.mapLeaves(e, f)
		}
		return out
	case nil, bool:
		return x
	default:
		return f(x)
	}
}

func (j *job) detectText(s string) string {
	out, _ := pii.Detect(s, j.m.detectors).(string)
	return out
}

// --- body -----------------------------------------------------------------

func (j *job) body(msg Message) (any, bool) {
	kind, label := classify(msg)
	if msg.Cut {
		return opaque(msg.Body, label+"+truncated"), false
	}
	var v any
	switch kind {
	case "json":
		if len(msg.Body) > 4*j.m.maxBody || tooDeep(msg.Body) {
			return opaque(msg.Body, label), false
		}
		parsed, ok := decodeJSON(msg.Body)
		if !ok {
			// An undeclared body that looks like JSON but does not parse
			// is text only in detect mode; redact-unknown cannot tell its
			// values apart, so it is opaque (fail closed).
			if msg.ContentType == "" && j.m.bodyMode == ModeDetect && utf8.Valid(msg.Body) && printable(msg.Body) {
				v = j.detectText(string(msg.Body))
				break
			}
			return opaque(msg.Body, label), false
		}
		v = j.walk(parsed, nil)
	case "text":
		v = j.detectText(string(msg.Body))
	default:
		return opaque(msg.Body, label), false
	}
	if s, ok := v.(string); ok {
		if len(s) > j.m.maxBody {
			return pii.CutAt(s, j.m.maxBody), true
		}
		return s, false
	}
	b, err := json.Marshal(v)
	if err != nil {
		return opaque(msg.Body, label), false
	}
	if len(b) > j.m.maxBody {
		return pii.CutAt(string(b), j.m.maxBody), true
	}
	return v, false
}

// walk masks a parsed JSON value at the concrete path p.
func (j *job) walk(v any, p []step) any {
	switch x := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(x))
		for _, k := range sortedKeys(x) {
			cp := append(p[:len(p):len(p)], step{kind: stepKey, key: k})
			out[j.detectText(k)] = j.walk(x[k], cp)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = j.walk(e, append(p[:len(p):len(p)], step{kind: stepIndex, index: i}))
		}
		return out
	case nil, bool:
		return x
	default:
		return j.leaf(x, p)
	}
}

// leaf masks a string or number of the body at path p: a mask rule wins,
// then an allow rule, then the body mode.
func (j *job) leaf(v any, p []step) any {
	for _, r := range j.masks {
		if r.path != nil && r.path.covers(p) {
			return j.maskMode(v, r.Mode)
		}
	}
	allowed := false
	for _, r := range j.allows {
		if r.path != nil && r.path.covers(p) {
			allowed = true
			break
		}
	}
	if !allowed {
		if at := personalAt(p); at > 0 {
			j.propose(p[:at])
			if _, isText := v.(string); isText || j.m.bodyMode == ModeDetect {
				return j.redact(v)
			}
		}
		if _, isText := v.(string); isText && j.m.bodyMode == ModeRedactUnknown {
			return j.redact(v)
		}
	}
	return j.detect(v)
}

// personalAt returns the length of the shortest prefix of p whose last key
// looks like personal data, alone or with its parent key
// ("customer"+"name"), or 0.
func personalAt(p []step) int {
	parent := ""
	for i, s := range p {
		if s.kind != stepKey {
			continue
		}
		if pii.LooksPersonal(s.key) || (parent != "" && pii.LooksPersonal(parent+"_"+s.key)) {
			return i + 1
		}
		parent = s.key
	}
	return 0
}

// propose records a mask rule for the body path p. A key that a rule could
// not name, or that a detector would mask, is never proposed: a proposal
// reaches the client.
func (j *job) propose(p []step) {
	q := j.vhost + "/" + j.queue
	if j.vhost == "/" {
		q = "//" + j.queue
	}
	if strings.ContainsAny(q, " \t\r\n,=") || j.detectText(q) != q || len(j.proposals) >= 32 {
		return
	}
	for _, s := range p {
		if s.kind == stepKey && (!validKey(s.key) || j.detectText(s.key) != s.key) {
			return
		}
	}
	j.proposals["queue="+q+" body="+general(p).String()] = true
}

// classify decides how a body is read: "json", "text" or "opaque", and
// the type label of an opaque body.
func classify(msg Message) (string, string) {
	ct, _, err := mime.ParseMediaType(msg.ContentType)
	if err != nil {
		ct = ""
	}
	ct = strings.ToLower(ct)
	label := safeLabel(ct)
	if enc := strings.ToLower(strings.TrimSpace(msg.ContentEncoding)); enc != "" && enc != "identity" {
		return "opaque", label + "+" + safeLabel(enc)
	}
	switch {
	case msg.ContentType != "" && ct == "":
		return "opaque", "invalid"
	case ct == "application/json" || strings.HasSuffix(ct, "+json"):
		return "json", label
	case strings.HasPrefix(ct, "text/"):
		if utf8.Valid(msg.Body) {
			return "text", label
		}
		return "opaque", label
	case ct == "":
		if !utf8.Valid(msg.Body) || !printable(msg.Body) {
			return "opaque", "unknown"
		}
		if t := bytes.TrimSpace(msg.Body); len(t) > 0 && (t[0] == '{' || t[0] == '[') {
			return "json", "unknown"
		}
		return "text", "unknown"
	default:
		return "opaque", label
	}
}

// safeLabel keeps a content type label free of anything but a media
// type's characters: it is publisher-controlled text.
func safeLabel(s string) string {
	var b strings.Builder
	for _, r := range s {
		if b.Len() >= 64 {
			break
		}
		if r < 128 && (unicode.IsLetter(r) || unicode.IsDigit(r) || strings.ContainsRune("./+-", r)) {
			b.WriteRune(r)
		}
	}
	if b.Len() == 0 {
		return "unknown"
	}
	return b.String()
}

func printable(b []byte) bool {
	for _, r := range string(b) {
		if unicode.IsControl(r) && r != '\n' && r != '\r' && r != '\t' {
			return false
		}
	}
	return true
}

func opaque(body []byte, label string) string {
	sum := sha256.Sum256(body)
	return fmt.Sprintf("<bytes:%d sha256:%s type:%s>", len(body), hex.EncodeToString(sum[:8]), label)
}

// tooDeep reports a JSON text nested deeper than maxDepth.
func tooDeep(b []byte) bool {
	depth, inStr, esc := 0, false, false
	for _, c := range b {
		switch {
		case inStr:
			switch {
			case esc:
				esc = false
			case c == '\\':
				esc = true
			case c == '"':
				inStr = false
			}
		case c == '"':
			inStr = true
		case c == '{' || c == '[':
			depth++
			if depth > maxDepth {
				return true
			}
		case c == '}' || c == ']':
			depth--
		}
	}
	return false
}

func decodeJSON(b []byte) (any, bool) {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, false
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, false
	}
	return v, true
}

// --- headers and properties -------------------------------------------------

// structuralHeaders are set by the broker; their strings still pass the
// detectors.
var structuralPrefixes = []string{"x-first-death-", "x-last-death-"}
var structuralHeaders = map[string]bool{"x-delivery-count": true, "x-stream-offset": true}

func (j *job) headers(h map[string]any) map[string]any {
	if len(h) == 0 {
		return nil
	}
	out := make(map[string]any, len(h))
	for _, name := range sortedKeys(h) {
		v := normalize(h[name])
		key := j.detectText(name)
		if name == "x-death" {
			out[key] = j.xDeath(v)
			continue
		}
		out[key] = j.named(name, v, func(r Rule) bool { return r.Header != "" && glob(r.Header, name) },
			structuralHeaders[name] || hasPrefix(name, structuralPrefixes), true)
	}
	return out
}

// named masks the value of a header or property: a mask rule wins, then
// an allow rule or a structural field (detectors), then the field name
// heuristic, then the body mode for headers (redactUnknown).
func (j *job) named(name string, v any, rule func(Rule) bool, structural, redactUnknown bool) any {
	for _, r := range j.masks {
		if rule(r) {
			mode := r.Mode
			return j.mapLeaves(v, func(x any) any { return j.maskMode(x, mode) })
		}
	}
	for _, r := range j.allows {
		if rule(r) {
			return j.mapLeaves(v, j.detect)
		}
	}
	if structural {
		return j.mapLeaves(v, j.detect)
	}
	if pii.LooksPersonal(name) {
		return j.mapLeaves(v, j.redact)
	}
	if redactUnknown && j.m.bodyMode == ModeRedactUnknown {
		return j.mapLeaves(v, func(x any) any {
			if _, ok := x.(string); ok {
				return j.redact(x)
			}
			return j.detect(x)
		})
	}
	return j.mapLeaves(v, j.detect)
}

// xDeath keeps the structure of the x-death header: queue, reason, count,
// exchange, time and original expiration clear (names, not data), the
// routing keys through the detectors; anything else is dropped.
func (j *job) xDeath(v any) any {
	list, ok := v.([]any)
	if !ok {
		return j.mapLeaves(v, j.redact)
	}
	out := make([]any, 0, len(list))
	for _, e := range list {
		m, ok := e.(map[string]any)
		if !ok {
			continue
		}
		d := map[string]any{}
		for _, k := range []string{"queue", "reason", "exchange", "time", "original-expiration"} {
			if s, ok := m[k].(string); ok {
				d[k] = j.detectText(s)
			}
		}
		switch c := m["count"].(type) {
		case int64, json.Number, float64, int:
			d["count"] = c
		}
		if rks, ok := m["routing-keys"].([]any); ok {
			keys := make([]any, 0, len(rks))
			for _, rk := range rks {
				if s, ok := rk.(string); ok {
					keys = append(keys, j.detectText(s))
				}
			}
			d["routing-keys"] = keys
		}
		out = append(out, d)
	}
	return out
}

func (j *job) properties(p map[string]any) map[string]any {
	if len(p) == 0 {
		return nil
	}
	out := make(map[string]any, len(p))
	for _, name := range sortedKeys(p) {
		if name == "headers" {
			continue // the caller moves them to Message.Headers
		}
		v := normalize(p[name])
		out[j.detectText(name)] = j.named(name, v, func(r Rule) bool { return r.Property == name }, false, false)
	}
	return out
}

func firstDeathQueue(h map[string]any) string {
	list, ok := normalize(h["x-death"]).([]any)
	if !ok || len(list) == 0 {
		return ""
	}
	if m, ok := list[0].(map[string]any); ok {
		q, _ := m["queue"].(string)
		return q
	}
	return ""
}

func hasPrefix(s string, prefixes []string) bool {
	for _, p := range prefixes {
		if strings.HasPrefix(s, p) {
			return true
		}
	}
	return false
}

func sortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// normalize turns a header or property value of either client library
// (amqp091 tables, stream application properties) into JSON-like values:
// maps with string keys, lists, strings, numbers, booleans and nil.
func normalize(v any) any {
	switch x := v.(type) {
	case nil, string, bool, json.Number, int64, float64:
		return x
	case int:
		return int64(x)
	case int8:
		return int64(x)
	case int16:
		return int64(x)
	case int32:
		return int64(x)
	case uint8:
		return int64(x)
	case uint16:
		return int64(x)
	case uint32:
		return int64(x)
	case uint64:
		return strconv.FormatUint(x, 10)
	case float32:
		return float64(x)
	case []byte:
		if utf8.Valid(x) {
			return string(x)
		}
		return opaque(x, "bytes")
	case time.Time:
		return x.UTC().Format(time.RFC3339Nano)
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, e := range x {
			out[k] = normalize(e)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = normalize(e)
		}
		return out
	}
	rv := reflect.ValueOf(v)
	switch rv.Kind() {
	case reflect.Map:
		if rv.Type().Key().Kind() == reflect.String {
			out := make(map[string]any, rv.Len())
			it := rv.MapRange()
			for it.Next() {
				out[it.Key().String()] = normalize(it.Value().Interface())
			}
			return out
		}
	case reflect.Slice, reflect.Array:
		out := make([]any, rv.Len())
		for i := range out {
			out[i] = normalize(rv.Index(i).Interface())
		}
		return out
	case reflect.String:
		return rv.String()
	}
	return fmt.Sprint(v)
}
