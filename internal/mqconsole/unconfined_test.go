package mqconsole

import (
	"testing"

	"github.com/DavidGodefroid/lockmq/internal/config"
)

// The profile's unconfined = true reaches console.Profile, which serves
// unconfined agents only then (lockkit#133).
func TestProfileUnconfined(t *testing.T) {
	var p Policy
	p.Policy.Profile = config.Profile{Name: "dev", URL: "http://localhost:15672"}
	if p.Profile().Unconfined {
		t.Fatal("unconfined by default")
	}
	p.Policy.Profile.Unconfined = true
	if !p.Profile().Unconfined {
		t.Fatal("unconfined = true does not reach console.Profile")
	}
}
