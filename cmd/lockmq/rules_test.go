package main

import (
	"encoding/json"
	"strings"
	"testing"
)

// `profile rules --json` gives lox run each profile's effective rules:
// the defaults, a table, the full-auto preset; --profile picks one.
func TestProfileRulesJSON(t *testing.T) {
	root := project(t, oneProfile+
		"[profiles.local]\nurl = \"http://127.0.0.1:15672\"\nauto = \"full-auto\"\n"+
		"[profiles.prod]\nurl = \"https://mq.example.com\"\nproduction = true\n[profiles.prod.auto]\npurge = \"deny\"\ndelete = \"deny\"\n")
	code, out, errOut := runWith(t, root, nil, nil, "", "profile", "rules", "--json")
	if code != 0 {
		t.Fatalf("exit %d: %s %s", code, out, errOut)
	}
	var got rulesOut
	if err := json.Unmarshal([]byte(out), &got); err != nil || got.Lock != "lockmq" || len(got.Profiles) != 3 || len(got.Actions) != 8 {
		t.Fatalf("%s %v", out, err)
	}
	by := map[string]profileRules{}
	for _, p := range got.Profiles {
		by[p.Profile] = p
	}
	if d := by["dev"]; d.FullAuto || d.Rules["connect"] != "ask" || d.Rules["peek"] != "auto" || d.Rules["publish"] != "ask" {
		t.Errorf("dev: %+v", d)
	}
	if l := by["local"]; !l.FullAuto || l.Rules["connect"] != "auto" || l.Rules["delete"] != "auto" {
		t.Errorf("local: %+v", l)
	}
	if p := by["prod"]; !p.Production || p.Rules["purge"] != "deny" || p.Rules["connect"] != "ask" || p.IDs["delete"] != "auto.delete=deny" {
		t.Errorf("prod: %+v", p)
	}
	code, out, _ = runWith(t, root, nil, nil, "", "profile", "rules", "--profile", "prod")
	if code != 0 || strings.Count(out, "\n") != 1 || !strings.Contains(out, "prod (production): connect ask") || !strings.Contains(out, "purge deny") {
		t.Errorf("text: %d %q", code, out)
	}
	if code, _, _ = runWith(t, root, nil, nil, "", "profile", "rules", "--profile", "nope"); code == 0 {
		t.Error("unknown profile accepted")
	}
}
