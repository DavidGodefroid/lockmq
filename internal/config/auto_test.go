package config

import (
	"encoding/json"
	"strings"
	"testing"
)

// A profile without auto rules has the documented defaults and keeps the
// policy JSON (so the fingerprint) it had before the rules existed.
func TestAutoDefaults(t *testing.T) {
	p, err := parseOne(t, minimal)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]AutoValue{
		ActConnect: AutoAsk, ActList: AutoAuto, ActRead: AutoAuto, ActPeek: AutoAsk,
		ActGet: AutoAsk, ActPublish: AutoAsk, ActPurge: AutoAsk, ActDelete: AutoAsk,
	}
	for act, v := range want {
		if got := p.Auto.Rule(act); got != v {
			t.Errorf("%s = %s, want %s", act, got, v)
		}
	}
	if len(p.Auto.Effective()) != len(AutoActions) {
		t.Errorf("effective: %v", p.Auto.Effective())
	}
	raw, err := json.Marshal(NewPolicy(p, nil, nil, nil))
	if err != nil || strings.Contains(string(raw), `"auto"`) {
		t.Errorf("default rules in the policy JSON: %s %v", raw, err)
	}
	if p.Auto.Rule("unknown") != AutoDeny {
		t.Error("an unknown action is not denied")
	}
}

// Every action takes every value; a value equal to the default is stored
// empty; the full-auto preset sets every action to auto.
func TestAutoActionValues(t *testing.T) {
	for _, act := range AutoActions {
		for _, v := range []AutoValue{AutoAuto, AutoAsk, AutoDeny} {
			p, err := parseOne(t, minimal+"[profiles.main.auto]\n"+act+` = "`+string(v)+`"`+"\n")
			if err != nil {
				t.Fatalf("%s=%s: %v", act, v, err)
			}
			if got := p.Auto.Rule(act); got != v {
				t.Errorf("%s=%s: rule %s", act, v, got)
			}
			if f := p.Auto.field(act); (v == AutoDefault(act)) != (*f == "") {
				t.Errorf("%s=%s: stored %q", act, v, *f)
			}
			for _, other := range AutoActions {
				if other != act && p.Auto.Rule(other) != AutoDefault(other) {
					t.Errorf("%s=%s changed %s", act, v, other)
				}
			}
			if id := p.Auto.RuleID(act); id != "auto."+act+"="+string(v) {
				t.Errorf("rule id %s", id)
			}
		}
	}
	p, err := parseOne(t, minimal[:len(minimal)-1]+"\nauto = \"full-auto\"\n")
	if err != nil {
		t.Fatal(err)
	}
	if !p.Auto.IsFullAuto() || !strings.HasPrefix(p.Auto.String(), "full-auto") {
		t.Errorf("full-auto: %s", p.Auto)
	}
	for _, act := range AutoActions {
		if p.Auto.Rule(act) != AutoAuto {
			t.Errorf("full-auto %s = %s", act, p.Auto.Rule(act))
		}
	}
}

// Unknown actions, values and shapes are refused, without quoting the
// value.
func TestAutoRefusals(t *testing.T) {
	for name, body := range map[string]string{
		"unknown action": "[profiles.main.auto]\nconsume = \"auto\"\n",
		"unknown value":  "[profiles.main.auto]\npublish = \"s3cr3t-maybe\"\n",
		"not a string":   "[profiles.main.auto]\npublish = true\n",
		"bad preset":     "",
		"number":         "",
	} {
		full := minimal + body
		switch name {
		case "bad preset":
			full = minimal[:len(minimal)-1] + "\nauto = \"s3cr3t-maybe\"\n"
		case "number":
			full = minimal[:len(minimal)-1] + "\nauto = 3\n"
		}
		_, err := parseOne(t, full)
		if err == nil {
			t.Errorf("%s: accepted", name)
			continue
		}
		if strings.Contains(err.Error(), "s3cr3t") {
			t.Errorf("%s: the error quotes the value: %v", name, err)
		}
	}
}

// A looser rule loosens the policy (deny < ask < auto), a tighter one
// tightens; the console asks the typed profile name for a loosening of
// a production profile.
func TestAutoDiff(t *testing.T) {
	base := basePolicy(t)
	set := func(mod func(*AutoRules)) Policy {
		p := base
		mod(&p.Profile.Auto)
		return Canonical(p)
	}
	cases := []struct {
		name    string
		mod     func(*AutoRules)
		loosens bool
	}{
		{"connect ask->auto", func(a *AutoRules) { a.Connect = AutoAuto }, true},
		{"publish ask->deny", func(a *AutoRules) { a.Publish = AutoDeny }, false},
		{"peek auto->ask", func(a *AutoRules) { a.Peek = AutoAsk }, false},
		{"delete ask->auto", func(a *AutoRules) { a.Delete = AutoAuto }, true},
		{"same as default", func(a *AutoRules) { a.List = AutoAuto }, false},
	}
	for _, c := range cases {
		ch := set(c.mod).Diff(base)
		if loosens(ch) != c.loosens {
			t.Errorf("%s: %+v", c.name, ch)
		}
		if c.name == "same as default" && len(ch) != 0 {
			t.Errorf("%s: %+v", c.name, ch)
		}
	}
	denied := set(func(a *AutoRules) { a.Purge = AutoDeny })
	if ch := base.Diff(denied); !loosens(ch) || len(ch) != 1 || ch[0].Field != "auto.purge" || ch[0].Old != "deny" || ch[0].New != "ask" {
		t.Errorf("deny->ask: %+v", ch)
	}
	full := set(func(a *AutoRules) {
		for _, act := range AutoActions {
			*a.field(act) = AutoAuto
		}
	})
	if ch := full.Diff(base); !loosens(ch) {
		t.Errorf("full-auto: %+v", ch)
	}
	if !strings.Contains(strings.Join(full.Describe(), "\n"), "auto rules (lox run): full-auto") {
		t.Errorf("describe: %v", full.Describe())
	}
}
