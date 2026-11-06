package config

import (
	"errors"
	"fmt"
	"sort"
	"strings"

	kconfig "github.com/DavidGodefroid/lockkit/config"
)

// AutoValue is how `lox run`'s auto mode treats one action of a profile:
// approved without a question (auto), asked of the human (ask), or
// refused even when asked (deny). Manual mode asks everything a ticket
// carries whatever the rule; deny refuses in both modes.
type AutoValue string

// Auto values, from the tightest to the loosest.
const (
	AutoDeny AutoValue = "deny"
	AutoAsk  AutoValue = "ask"
	AutoAuto AutoValue = "auto"
)

// FullAuto is the preset that sets every action to auto
// (`auto = "full-auto"`).
const FullAuto = "full-auto"

// rank orders the values: a higher rank is looser.
func (v AutoValue) rank() int {
	switch v {
	case AutoDeny:
		return 0
	case AutoAsk:
		return 1
	case AutoAuto:
		return 2
	}
	return -1
}

// ParseAutoValue parses auto, ask or deny. Its error never quotes s.
func ParseAutoValue(s string) (AutoValue, error) {
	if v := AutoValue(s); v.rank() >= 0 {
		return v, nil
	}
	return "", errors.New("want auto, ask or deny")
}

// Actions of the auto rules. lockmq is read-only: get, publish, purge and
// delete name no operation of this release; their rules are validated and
// kept, so a profile is locked down before any such operation exists.
const (
	// ActConnect: open the broker connection (a connect ticket; `lox run`
	// asks "ask" once per session).
	ActConnect = "connect"
	// ActList: the catalogue's metadata reads (overview, queues,
	// exchanges, bindings, counts, consumers…), no message content.
	ActList = "list"
	// ActRead: read a stream's messages (no offset stored, nothing
	// removed).
	ActRead = "read"
	// ActPeek: read a classic or quorum queue's messages, requeued
	// (redelivered, the delivery count may grow).
	ActPeek = "peek"
	// ActGet: consume (ack) a message.
	ActGet = "get"
	// ActPublish: publish a message.
	ActPublish = "publish"
	// ActPurge: purge a queue.
	ActPurge = "purge"
	// ActDelete: delete a queue, exchange or binding.
	ActDelete = "delete"
)

// AutoActions lists every action, in display order.
var AutoActions = []string{ActConnect, ActList, ActRead, ActPeek, ActGet, ActPublish, ActPurge, ActDelete}

// autoDefaults are the rules of an action a profile does not set: one
// connection question per `lox run` session, the reads auto, everything
// that changes the broker asked.
var autoDefaults = map[string]AutoValue{
	ActConnect: AutoAsk,
	ActList:    AutoAuto,
	ActRead:    AutoAuto,
	ActPeek:    AutoAuto,
	ActGet:     AutoAsk,
	ActPublish: AutoAsk,
	ActPurge:   AutoAsk,
	ActDelete:  AutoAsk,
}

// AutoDefault is the rule of action when the profile sets none.
func AutoDefault(action string) AutoValue { return autoDefaults[action] }

// AutoRules are a profile's auto rules. An empty field is the action's
// default (AutoDefault): Canonical empties the fields equal to it, so a
// profile that sets no rule keeps the fingerprint it had before the rules
// existed.
type AutoRules struct {
	Connect AutoValue `json:"connect,omitempty"`
	List    AutoValue `json:"list,omitempty"`
	Read    AutoValue `json:"read,omitempty"`
	Peek    AutoValue `json:"peek,omitempty"`
	Get     AutoValue `json:"get,omitempty"`
	Publish AutoValue `json:"publish,omitempty"`
	Purge   AutoValue `json:"purge,omitempty"`
	Delete  AutoValue `json:"delete,omitempty"`
}

func (a *AutoRules) field(action string) *AutoValue {
	switch action {
	case ActConnect:
		return &a.Connect
	case ActList:
		return &a.List
	case ActRead:
		return &a.Read
	case ActPeek:
		return &a.Peek
	case ActGet:
		return &a.Get
	case ActPublish:
		return &a.Publish
	case ActPurge:
		return &a.Purge
	case ActDelete:
		return &a.Delete
	}
	return nil
}

// Rule is the effective rule of action: the profile's, or the default.
// An unknown action is denied (fail closed).
func (a AutoRules) Rule(action string) AutoValue {
	f := a.field(action)
	if f == nil {
		return AutoDeny
	}
	if *f == "" {
		return autoDefaults[action]
	}
	return *f
}

// Effective is every action with its effective rule.
func (a AutoRules) Effective() map[string]AutoValue {
	out := make(map[string]AutoValue, len(AutoActions))
	for _, act := range AutoActions {
		out[act] = a.Rule(act)
	}
	return out
}

// canonical empties the fields equal to their default.
func (a AutoRules) canonical() AutoRules {
	for _, act := range AutoActions {
		f := a.field(act)
		if *f == autoDefaults[act] {
			*f = ""
		}
	}
	return a
}

// IsFullAuto reports whether every action is auto.
func (a AutoRules) IsFullAuto() bool {
	for _, act := range AutoActions {
		if a.Rule(act) != AutoAuto {
			return false
		}
	}
	return true
}

// String is the effective rules on one line ("connect ask · list auto …").
func (a AutoRules) String() string {
	parts := make([]string, 0, len(AutoActions))
	for _, act := range AutoActions {
		parts = append(parts, act+" "+string(a.Rule(act)))
	}
	s := strings.Join(parts, " · ")
	if a.IsFullAuto() {
		s = FullAuto + " (" + s + ")"
	}
	return s
}

// RuleID names the rule that applies to action, for the audit and the
// ticket screen: "auto.<action>=<value>".
func (a AutoRules) RuleID(action string) string {
	return "auto." + action + "=" + string(a.Rule(action))
}

// diffAuto lists the changes of the effective rules from ap to cp; a
// looser value loosens (deny < ask < auto).
func diffAuto(ap, cp AutoRules) []kconfig.Change {
	var out []kconfig.Change
	for _, act := range AutoActions {
		o, n := ap.Rule(act), cp.Rule(act)
		if o != n {
			out = append(out, kconfig.Change{Field: "auto." + act, Old: string(o), New: string(n), Loosens: n.rank() > o.rank()})
		}
	}
	return out
}

// rawAuto is the auto key as the file holds it: UnmarshalTOML keeps the
// raw value (a string or a table) for parseAuto, which checks every key.
type rawAuto struct{ v any }

// UnmarshalTOML implements toml.Unmarshaler.
func (r *rawAuto) UnmarshalTOML(v any) error { r.v = v; return nil }

// parseAuto reads a profile's auto key: absent (nil), the full-auto
// preset, or a table of action = value. Unknown actions and values are
// errors that name the action, never the value.
func parseAuto(raw any) (AutoRules, error) {
	var a AutoRules
	switch r := raw.(type) {
	case nil:
		return a, nil
	case string:
		if r != FullAuto {
			return a, fmt.Errorf("auto: want %q or a table of actions (%s)", FullAuto, strings.Join(AutoActions, ", "))
		}
		for _, act := range AutoActions {
			*a.field(act) = AutoAuto
		}
		return a, nil
	case map[string]any:
		keys := make([]string, 0, len(r))
		for k := range r {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			f := a.field(k)
			if f == nil {
				return a, fmt.Errorf("auto: unknown action (want %s)", strings.Join(AutoActions, ", "))
			}
			s, ok := r[k].(string)
			if !ok {
				return a, fmt.Errorf("auto.%s: want auto, ask or deny", k)
			}
			v, err := ParseAutoValue(s)
			if err != nil {
				return a, fmt.Errorf("auto.%s: %v", k, err)
			}
			*f = v
		}
		return a, nil
	}
	return a, fmt.Errorf("auto: want %q or a table of actions (%s)", FullAuto, strings.Join(AutoActions, ", "))
}
