package main

import (
	"errors"
	"flag"
	"fmt"
	"path/filepath"

	"github.com/DavidGodefroid/lockkit/verbs"

	"github.com/DavidGodefroid/lockmq/internal/config"
	"github.com/DavidGodefroid/lockmq/internal/product"
)

const profileRulesUsage = `lockmq profile rules [--json] [--project DIR] [--profile NAME]`

// profileRules is one profile's effective auto rules, as `lox run` reads
// them (`lockmq profile rules --json`).
type profileRules struct {
	Profile    string                      `json:"profile"`
	Production bool                        `json:"production"`
	FullAuto   bool                        `json:"full_auto"`
	Rules      map[string]config.AutoValue `json:"rules"`
	// IDs name each action's rule as the lock audits it
	// ("auto.peek=ask"): `lox run` may name its own rule after it.
	IDs map[string]string `json:"ids"`
}

// rulesOut is the --json output of `profile rules`.
type rulesOut struct {
	Lock     string         `json:"lock"`
	Actions  []string       `json:"actions"`
	Profiles []profileRules `json:"profiles"`
}

// runProfileRules is `lockmq profile rules`: the effective auto rules of
// the profiles of the config files (the defaults where a profile sets
// none). It prints what the files say: the lock enforces the rules of the
// policy its console approved (a loosening of a production profile needs
// the typed profile name there first).
func runProfileRules(e env, args []string) int {
	const cmd = "profile rules"
	asJSON := verbs.WantsJSON(args)
	fail := func(err error) int { return verbFail(e, cmd, profileRulesUsage, asJSON, err) }
	fs := flag.NewFlagSet(cmd, flag.ContinueOnError)
	fs.Bool(verbs.FlagJSON, false, "one line of JSON on stdout")
	project := fs.String(verbs.FlagProject, "", "project directory")
	name := fs.String(verbs.FlagProfile, "", "profile name")
	if err := verbs.Parse(product.P, fs, args); err != nil {
		return fail(err)
	}
	if fs.NArg() != 0 {
		return fail(errors.New("unexpected arguments"))
	}
	if *project != "" {
		abs, err := filepath.Abs(*project)
		if err != nil {
			return fail(errors.New("--project: invalid directory"))
		}
		e.cwd = abs
	}
	ps, _, err := e.loadProfiles()
	if err != nil {
		return fail(configError{err})
	}
	names := profileNames(ps)
	if *name != "" {
		if _, ok := ps[*name]; !ok {
			return fail(errors.New("no such profile in the config files"))
		}
		names = []string{*name}
	}
	out := rulesOut{Lock: product.P.Name, Actions: config.AutoActions, Profiles: []profileRules{}}
	for _, n := range names {
		p := ps[n]
		pr := profileRules{Profile: n, Production: p.Production, FullAuto: p.Auto.IsFullAuto(), Rules: p.Auto.Effective(), IDs: map[string]string{}}
		for _, act := range config.AutoActions {
			pr.IDs[act] = p.Auto.RuleID(act)
		}
		out.Profiles = append(out.Profiles, pr)
	}
	if asJSON {
		if err := verbs.WriteJSON(e.stdout, out); err != nil {
			return exitFail
		}
		return 0
	}
	if len(names) == 0 {
		fmt.Fprintln(e.stdout, "no profile in the config files")
	}
	for _, n := range names {
		p := ps[n]
		tag := ""
		if p.Production {
			tag = " (production)"
		}
		fmt.Fprintf(e.stdout, "%s%s: %s\n", n, tag, p.Auto)
	}
	return 0
}
