package main

import (
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/DavidGodefroid/lockkit/console"
	"github.com/DavidGodefroid/lockkit/sysconf"
)

// parseAllowedUIDs reads `console --allowed-uids`: comma-separated decimal
// uids, as the image's ALLOWED_UIDS build argument takes them. Root (0) and
// leading zeros are refused here; the console's own uid is refused by the
// isolation checks (the separation would be void).
func parseAllowedUIDs(s string) ([]int, error) {
	if s == "" {
		return nil, nil
	}
	var uids []int
	for _, f := range strings.Split(s, ",") {
		if f == "" || strings.Trim(f, "0123456789") != "" {
			return nil, fmt.Errorf("--allowed-uids: comma-separated uids, got %q", s)
		}
		if f[0] == '0' {
			return nil, fmt.Errorf("--allowed-uids: %s is root or has a leading zero", f)
		}
		n, err := strconv.Atoi(f)
		if err != nil || n >= 1<<32-1 {
			return nil, fmt.Errorf("--allowed-uids: %s is not a uid", f)
		}
		if !slices.Contains(uids, n) {
			uids = append(uids, n)
		}
	}
	return uids, nil
}

// allowUIDs returns env whose separated setup also lets uids connect, as
// allowed_uids in the image's system.toml does: every check that reads
// the setup sees them (the peers served, and the accounts whose session or
// terminal the console refuses). Without a separated setup it refuses.
func allowUIDs(env console.IsolationEnv, uids []int) console.IsolationEnv {
	if len(uids) == 0 {
		return env
	}
	load := env.LoadSys
	env.LoadSys = func() (*sysconf.Config, error) {
		sys, err := load()
		if err != nil {
			return nil, err
		}
		if sys == nil {
			return nil, errors.New("--allowed-uids needs the separated setup (/etc/lockmq/system.toml, as in the image)")
		}
		c := *sys
		c.AllowedUIDs = slices.Clone(sys.AllowedUIDs)
		for _, u := range uids {
			if !slices.Contains(c.AllowedUIDs, u) {
				c.AllowedUIDs = append(c.AllowedUIDs, u)
			}
		}
		return &c, nil
	}
	return env
}

// joinUIDs prints uids as the flag takes them.
func joinUIDs(uids []int) string {
	s := make([]string, len(uids))
	for i, u := range uids {
		s[i] = strconv.Itoa(u)
	}
	return strings.Join(s, ",")
}
