package config

import (
	"strings"
	"testing"

	kconfig "github.com/DavidGodefroid/lockkit/config"
)

// unconfined = true is read into the profile, is part of the fingerprint,
// and switching it on is a loosening (lockkit#133).
func TestUnconfinedKey(t *testing.T) {
	p, err := parseOne(t, `
[profiles.main]
url = "http://localhost:15672"
unconfined = true
`)
	if err != nil {
		t.Fatal(err)
	}
	if !p.Unconfined {
		t.Fatal("unconfined = true not read into the profile")
	}

	off, err := parseOne(t, `
[profiles.main]
url = "http://localhost:15672"
`)
	if err != nil {
		t.Fatal(err)
	}
	if off.Unconfined {
		t.Fatal("unconfined is on by default")
	}

	a, b := NewPolicy(off, nil, nil, nil), NewPolicy(p, nil, nil, nil)
	if kconfig.Fingerprint(a) == kconfig.Fingerprint(b) {
		t.Error("unconfined is not in the fingerprint")
	}
	var on *changeT
	for _, c := range b.Diff(a) {
		if c.Field == "unconfined" {
			on = &c
		}
	}
	if on == nil || !on.Loosens {
		t.Errorf("switching unconfined on: %+v, want a loosening", b.Diff(a))
	}
	if loosens(a.Diff(b)) {
		t.Errorf("switching unconfined off loosens: %+v", a.Diff(b))
	}
	if got := strings.Join(b.Describe(), "\n"); !strings.Contains(got, "unconfined: true") {
		t.Errorf("Describe does not name unconfined:\n%s", got)
	}
}

// A production profile never accepts an unconfined agent.
func TestUnconfinedProductionRefused(t *testing.T) {
	_, err := parseOne(t, `
[profiles.main]
url = "https://rabbitmq.example.com:15671"
production = true
unconfined = true
`)
	if err == nil || !strings.Contains(err.Error(), "unconfined is never allowed on a production profile") {
		t.Fatalf("production with unconfined: %v", err)
	}
}
