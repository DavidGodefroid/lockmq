package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	kconfig "github.com/DavidGodefroid/lockkit/config"

	"github.com/DavidGodefroid/lockmq/internal/config"
	"github.com/DavidGodefroid/lockmq/internal/product"
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
	if d := by["dev"]; d.FullAuto || d.Rules["connect"] != "ask" || d.Rules["peek"] != "ask" || d.Rules["publish"] != "ask" {
		t.Errorf("dev: %+v", d)
	}
	if l := by["local"]; !l.FullAuto || l.Rules["connect"] != "auto" || l.Rules["delete"] != "auto" {
		t.Errorf("local: %+v", l)
	}
	if p := by["prod"]; p.Source != "files" || p.PendingReview || p.Rules["purge"] != "deny" || p.Rules["connect"] != "ask" || p.IDs["delete"] != "auto.delete=deny" {
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

// `profile rules` reports the rules of the policy the console approved,
// not a file edit the human has not approved: an approved prod profile
// with connect = "ask", whose config file the agent loosened to "auto",
// still reports ask, flagged pending review with the file's rules beside.
func TestProfileRulesApprovedPolicy(t *testing.T) {
	approvedTOML := "[profiles.prod]\nurl = \"https://mq.example.com\"\nproduction = true\n"
	root := project(t, approvedTOML)
	ps, _, err := config.LoadFrom(root, "")
	if err != nil {
		t.Fatal(err)
	}
	pol := config.NewPolicy(ps["prod"], nil, nil, nil)
	if err := kconfig.SaveApproved(product.P, testStateDir(t, root), kconfig.ApprovedKey(root, "prod"), pol); err != nil {
		t.Fatal(err)
	}
	rules := func() profileRules {
		t.Helper()
		code, out, errOut := runWith(t, root, nil, nil, "", "profile", "rules", "--json")
		var got rulesOut
		if code != 0 || json.Unmarshal([]byte(out), &got) != nil || len(got.Profiles) != 1 {
			t.Fatalf("exit %d: %s %s", code, out, errOut)
		}
		return got.Profiles[0]
	}
	if p := rules(); p.Source != "approved" || p.PendingReview || p.Rules["connect"] != "ask" {
		t.Fatalf("approved, unchanged: %+v", p)
	}
	// The agent edits the file: connect = "auto".
	project2 := approvedTOML + "[profiles.prod.auto]\nconnect = \"auto\"\n"
	writeConfig(t, root, project2)
	p := rules()
	if p.Source != "approved" || !p.PendingReview || p.Rules["connect"] != "ask" || p.IDs["connect"] != "auto.connect=ask" || p.FileRules["connect"] != "auto" {
		t.Fatalf("a file-only loosening reported as the rule: %+v", p)
	}
	code, out, _ := runWith(t, root, nil, nil, "", "profile", "rules")
	if code != 0 || !strings.Contains(out, "prod (production): connect ask") || !strings.Contains(out, "pending :review") {
		t.Errorf("text: %d %q", code, out)
	}
}

// writeConfig rewrites the project config of root.
func writeConfig(t *testing.T, root, profiles string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(root, ".lockmq", "config.toml"), []byte(profiles), 0o600); err != nil {
		t.Fatal(err)
	}
}
