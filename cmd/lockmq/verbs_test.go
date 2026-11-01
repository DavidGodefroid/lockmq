package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/DavidGodefroid/lockkit/verbs"

	"github.com/DavidGodefroid/lockmq/internal/config"
	"github.com/DavidGodefroid/lockmq/internal/mcptools"
)

// oneJSON decodes stdout, which must be exactly one line of JSON, into v,
// refusing unknown keys.
func oneJSON(t *testing.T, stdout string, v any) {
	t.Helper()
	if strings.Count(stdout, "\n") != 1 || !strings.HasSuffix(stdout, "\n") {
		t.Fatalf("stdout is not one line: %q", stdout)
	}
	dec := json.NewDecoder(strings.NewReader(stdout))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		t.Fatalf("stdout %q: %v", stdout, err)
	}
}

func readConfig(t *testing.T, root string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(root, ".lockmq", "config.toml"))
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	return string(data)
}

// lox init's call: the profile is written from the flags alone, with no
// question, and reported as one verbs.Profile.
func TestProfileAddNonInteractiveJSON(t *testing.T) {
	root := project(t, oneProfile)
	code, out, stderr := runWith(t, root, nil, nil, "", "profile", "add", "--non-interactive", "--json", "--profile", "uat",
		"--production", "--credentials", "vault", "--vault-path", "rabbitmq/creds/ro", "--url", "https://rabbitmq.example.com:15671",
		"--user", "ro", "--vhost", "/", "--vhost", "orders", "--stream-url", "rabbitmq-stream+tls://rabbitmq.example.com:5551")
	if code != exitOK {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	var p verbs.Profile
	oneJSON(t, out, &p)
	want := verbs.Profile{Lock: "lockmq", Profile: "uat", Config: filepath.Join(root, ".lockmq", "config.toml"), Production: true, Credentials: "vault"}
	if p != want {
		t.Errorf("JSON %+v, want %+v", p, want)
	}
	if strings.Contains(stderr, "?") {
		t.Errorf("asked a question: %s", stderr)
	}
	ps, _, err := config.LoadFrom(root, "")
	if err != nil {
		t.Fatal(err)
	}
	got := ps["uat"]
	if got.URL != "https://rabbitmq.example.com:15671" || got.User != "ro" || !slices.Equal(got.VHosts, []string{"/", "orders"}) ||
		got.Credentials != "vault" || got.VaultPath != "rabbitmq/creds/ro" || !got.Production || got.Tier != config.TierRead || got.StreamURL == "" {
		t.Errorf("profile %+v", got)
	}
	if _, ok := ps["dev"]; !ok {
		t.Error("existing profile lost")
	}
}

// The defaults of the non-interactive form: profile dev, credentials ask,
// tier read; --tier peek is the only way to peek without a question.
func TestProfileAddNonInteractiveDefaults(t *testing.T) {
	root := project(t, "")
	code, out, stderr := runWith(t, root, nil, nil, "", "profile", "add", "--non-interactive", "--json", "--url", "http://127.0.0.1:15672")
	if code != exitOK {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	var p verbs.Profile
	oneJSON(t, out, &p)
	if p.Profile != "dev" || p.Credentials != "ask" || p.Production {
		t.Errorf("JSON %+v", p)
	}
	root = project(t, "")
	if code, _, stderr := runWith(t, root, nil, nil, "", "profile", "add", "--non-interactive", "--url", "http://127.0.0.1:15672", "--tier", "peek"); code != exitOK {
		t.Fatalf("--tier peek: exit %d: %s", code, stderr)
	}
	if ps, _, err := config.LoadFrom(root, ""); err != nil || ps["dev"].Tier != config.TierPeek {
		t.Errorf("--tier peek: %+v %v", ps["dev"], err)
	}
}

// Every refusal writes nothing and, with --json, prints one verbs.Error
// with its code; a value that may be a secret is never echoed.
func TestProfileAddRefusals(t *testing.T) {
	const canary = "glsa_CanarySecret"
	base := []string{"profile", "add", "--non-interactive", "--json"}
	url := []string{"--url", "http://127.0.0.1:15672"}
	for _, c := range []struct {
		name string
		args []string
		code string
	}{
		{"missing url", nil, verbs.CodeUsage},
		{"password flag", append(slices.Clone(url), "--password", canary), verbs.CodeSecret},
		{"vault token flag", append(slices.Clone(url), "--vault-token="+canary), verbs.CodeSecret},
		{"password in url", []string{"--url", "http://ro:" + canary + "@127.0.0.1:15672"}, verbs.CodeSecret},
		{"keychain", append(slices.Clone(url), "--credentials", "keychain"), verbs.CodeSecret},
		{"unknown source", append(slices.Clone(url), "--credentials", canary), verbs.CodeSecret},
		{"vault without path", append(slices.Clone(url), "--credentials", "vault"), verbs.CodeUsage},
		{"path without vault", append(slices.Clone(url), "--vault-path", "rabbitmq/creds/ro"), verbs.CodeUsage},
		{"positional", append(slices.Clone(url), canary), verbs.CodeUsage},
		{"positional password", append(slices.Clone(url), "ro:"+canary+"@host"), verbs.CodeSecret},
		{"unknown flag", append(slices.Clone(url), "--color", canary), verbs.CodeUsage},
		{"bad tier", append(slices.Clone(url), "--tier", canary), verbs.CodeUsage},
		{"bad scheme", []string{"--profile", "qa", "--url", "ftp://" + canary + ":15672"}, verbs.CodeUsage},
		{"bad name", append(slices.Clone(url), "--profile", "https://"+canary), verbs.CodeUsage},
		{"exists", append(slices.Clone(url), "--profile", "dev"), verbs.CodeExists},
	} {
		root := project(t, oneProfile)
		before := readConfig(t, root)
		code, out, stderr := runWith(t, root, nil, nil, "", append(slices.Clone(base), c.args...)...)
		if code == exitOK {
			t.Errorf("%s: exit 0", c.name)
			continue
		}
		var e verbs.Error
		oneJSON(t, out, &e)
		if e.Code != c.code || e.Lock != "lockmq" || e.Error == "" {
			t.Errorf("%s: JSON %+v, want code %s", c.name, e, c.code)
		}
		if strings.Contains(out+stderr, canary) {
			t.Errorf("%s: echoed the value: stdout %q stderr %q", c.name, out, stderr)
		}
		if readConfig(t, root) != before {
			t.Errorf("%s: config changed", c.name)
		}
	}
}

// Without --non-interactive and without a terminal, profile add refuses
// rather than wait for an answer.
func TestProfileAddNoTerminal(t *testing.T) {
	root := project(t, "")
	code, out, stderr := runWith(t, root, nil, nil, addAnswers(""), "profile", "add", "--json")
	var e verbs.Error
	oneJSON(t, out, &e)
	if code != exitUsage || e.Code != verbs.CodeUsage || !strings.Contains(stderr, "--non-interactive") || readConfig(t, root) != "" {
		t.Errorf("exit %d %+v %s", code, e, stderr)
	}
}

// The interactive form asks only for what the flags did not give, never
// for a secret; with --json its questions go to stderr.
func TestProfileAddInteractiveAsksTheRest(t *testing.T) {
	root := project(t, "")
	// Asked: AMQP, stream, vhosts, user, credentials, production, peek.
	answers := strings.Join([]string{"", "", "orders", "ro", "", "", ""}, "\n") + "\n"
	code, out, stderr := runTTY(t, root, answers, "profile", "add", "--json", "--profile", "qa", "--url", "http://127.0.0.1:15672")
	if code != exitOK {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	var p verbs.Profile
	oneJSON(t, out, &p)
	if p.Profile != "qa" || p.Credentials != "ask" {
		t.Errorf("JSON %+v", p)
	}
	if strings.Contains(stderr, "Profile name") || strings.Contains(stderr, "Management API URL") || !strings.Contains(stderr, "Vhosts") ||
		strings.Contains(strings.ToLower(stderr), "password:") {
		t.Errorf("questions:\n%s", stderr)
	}
	if ps, _, err := config.LoadFrom(root, ""); err != nil || !slices.Equal(ps["qa"].VHosts, []string{"orders"}) || ps["qa"].User != "ro" {
		t.Errorf("profile %+v %v", ps["qa"], err)
	}
	// A flag is checked as in the non-interactive form.
	if code, _, _ := runTTY(t, project(t, ""), answers, "profile", "add", "--credentials", "keychain"); code != exitUsage {
		t.Errorf("--credentials keychain: exit %d", code)
	}
	// An existing --profile is refused before any question.
	code, out, stderr = runTTY(t, project(t, oneProfile), answers, "profile", "add", "--profile", "dev")
	if code != exitUsage || out != "" || !strings.Contains(stderr, "already") {
		t.Errorf("exists: exit %d stdout %q stderr %q", code, out, stderr)
	}
}

func TestProfileUnknownSubcommand(t *testing.T) {
	for _, args := range [][]string{{"profile"}, {"profile", "glsa_CanarySecret"}} {
		code, _, stderr := runWith(t, project(t, ""), nil, nil, "", args...)
		if code != exitUsage || strings.Contains(stderr, "glsa_Canary") || !strings.Contains(stderr, "add") {
			t.Errorf("%v: exit %d %s", args, code, stderr)
		}
	}
	code, out, _ := runWith(t, project(t, ""), nil, nil, "", "profile", "--json")
	var e verbs.Error
	oneJSON(t, out, &e)
	if code != exitUsage || e.Code != verbs.CodeUsage {
		t.Errorf("exit %d %+v", code, e)
	}
}

// doctor --json prints one verbs.Doctor; its exit status is 0 exactly
// when no check fails.
func TestDoctorJSON(t *testing.T) {
	code, out, _ := runWith(t, project(t, oneProfile), nil, os.ErrNotExist, "", "doctor", "--json")
	var d verbs.Doctor
	oneJSON(t, out, &d)
	if code != exitOK || !d.OK || d.Lock != "lockmq" || d.Fixed == nil || len(d.Checks) == 0 {
		t.Fatalf("exit %d %+v", code, d)
	}
	names := map[string]string{}
	for _, c := range d.Checks {
		names[c.Check] = c.Status
	}
	if names["config"] != verbs.StatusOK || names["profile dev"] != verbs.StatusOK || names["console dev"] != verbs.StatusWarn {
		t.Errorf("checks %+v", d.Checks)
	}
	// No profile: a failing check, still a verbs.Doctor.
	code, out, _ = runWith(t, project(t, ""), nil, os.ErrNotExist, "", "doctor", "--json")
	d = verbs.Doctor{}
	oneJSON(t, out, &d)
	if code == exitOK || d.OK {
		t.Errorf("no profile: exit %d %+v", code, d)
	}
}

// The flag rules of doctor: --yes needs --fix, and --fix without a
// terminal needs --yes; both answer a verbs.Error with --json.
func TestDoctorFlagRules(t *testing.T) {
	for _, args := range [][]string{{"--yes"}, {"--fix"}, {"--wat"}, {"extra"}} {
		code, out, _ := runWith(t, project(t, oneProfile), nil, os.ErrNotExist, "", append([]string{"doctor", "--json"}, args...)...)
		var e verbs.Error
		oneJSON(t, out, &e)
		if code != exitUsage || e.Code != verbs.CodeUsage {
			t.Errorf("%v: exit %d %+v", args, code, e)
		}
	}
}

// lox init's doctor --fix --yes --json repairs, reports the repairs in
// fixed and the checks after them.
func TestDoctorFixYesJSON(t *testing.T) {
	root := project(t, oneProfile)
	// .lockmq is 0700: the console account (here, this uid) must have it
	// 0755.
	svc := svcAccount{name: "lockmq", uid: os.Getuid()}
	var out, errb bytes.Buffer
	var scripts []string
	e := containerEnv(t, root, "", &out, &errb)
	e.svc = func(name string) (svcAccount, bool) { return svc, name == "lockmq" }
	e.euid = func() int { return 0 }
	// The repair: the test makes the state pass as the script would.
	e.root = func(s string) error {
		scripts = append(scripts, s)
		return os.Chmod(filepath.Join(root, ".lockmq"), 0o755)
	}
	e.dial = func(string) (mcptools.Conn, error) { return nil, os.ErrNotExist }
	if err := os.Chmod(filepath.Join(root, ".lockmq", "config.toml"), 0o644); err != nil {
		t.Fatal(err)
	}
	code := run(e, []string{"doctor", "--fix", "--yes", "--json"})
	var d verbs.Doctor
	oneJSON(t, out.String(), &d)
	if len(scripts) != 1 || code != exitOK || !d.OK || len(d.Fixed) == 0 {
		t.Fatalf("exit %d scripts %d %+v\nstderr %s", code, len(scripts), d, errb.String())
	}
	for _, c := range d.Checks {
		if c.Status == verbs.StatusFail {
			t.Errorf("a failing check after the repair: %+v", c)
		}
	}
	if !strings.Contains(errb.String(), "These repairs run as root") {
		t.Errorf("the script is not on stderr: %s", errb.String())
	}
}
