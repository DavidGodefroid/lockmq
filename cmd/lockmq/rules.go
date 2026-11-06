package main

import (
	"errors"
	"flag"
	"fmt"
	"path/filepath"

	kconfig "github.com/DavidGodefroid/lockkit/config"
	"github.com/DavidGodefroid/lockkit/kit"
	"github.com/DavidGodefroid/lockkit/verbs"

	"github.com/DavidGodefroid/lockmq/internal/config"
	"github.com/DavidGodefroid/lockmq/internal/product"
)

const profileRulesUsage = `lockmq profile rules [--json] [--project DIR] [--profile NAME]`

// profileRules is one profile's auto rules, as `lox run` reads them
// (`lockmq profile rules --json`): those of the policy the console
// approved when there is one (Source "approved"), else the config files'
// (Source "files", nothing approved yet or not readable here).
type profileRules struct {
	Profile    string                      `json:"profile"`
	Production bool                        `json:"production"`
	FullAuto   bool                        `json:"full_auto"`
	Rules      map[string]config.AutoValue `json:"rules"`
	// IDs name each action's rule as the lock audits it
	// ("auto.peek=ask"): `lox run` may name its own rule after it.
	IDs map[string]string `json:"ids"`
	// Source is where Rules come from: "approved" or "files".
	Source string `json:"source"`
	// PendingReview marks config files whose rules differ from the
	// approved policy's: the lock enforces the approved ones until the
	// human applies the change at :review. FileRules are the files'.
	PendingReview bool                        `json:"pending_review"`
	FileRules     map[string]config.AutoValue `json:"file_rules,omitempty"`
	text          string
}

// rulesOut is the --json output of `profile rules`.
type rulesOut struct {
	Lock     string         `json:"lock"`
	Actions  []string       `json:"actions"`
	Profiles []profileRules `json:"profiles"`
}

// runProfileRules is `lockmq profile rules`: the effective auto rules of
// the profiles of the config files (the defaults where a profile sets
// none), taken from the policy the console approved when this account can
// read it, never from a file edit the human has not approved: the config
// files are in the project, which the agent can write. A file whose rules
// differ from the approved ones is flagged pending review. The lock does
// not trust this output either: a ticket its rules leave to the human is
// signed never_auto, and an auto rule's approval of it is refused.
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
	root, _ := kconfig.FindProjectRoot(product.P, e.cwd)
	out := rulesOut{Lock: product.P.Name, Actions: config.AutoActions, Profiles: []profileRules{}}
	shown := map[string]profileRules{}
	for _, n := range names {
		p := ps[n]
		pr := rulesOf(n, p, e.approvedProfile(root, n))
		shown[n] = pr
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
		pr := shown[n]
		tag := ""
		if pr.Production {
			tag = " (production)"
		}
		note := " [approved]"
		if pr.Source != sourceApproved {
			note = " [config files: not approved yet]"
		}
		if pr.PendingReview {
			note = " [approved; the config files differ: pending :review]"
		}
		fmt.Fprintf(e.stdout, "%s%s: %s%s\n", n, tag, pr.text, note)
	}
	return 0
}

// Where the rules of `profile rules` come from.
const (
	sourceApproved = "approved"
	sourceFiles    = "files"
)

// rulesOf is profile n's rules: the approved profile's when ap is not
// nil, the files' p otherwise, flagged when they differ.
func rulesOf(n string, p config.Profile, ap *config.Profile) profileRules {
	src, eff := p, sourceFiles
	if ap != nil {
		src, eff = *ap, sourceApproved
	}
	pr := profileRules{Profile: n, Production: src.Production || p.Production, FullAuto: src.Auto.IsFullAuto(),
		Rules: src.Auto.Effective(), IDs: map[string]string{}, Source: eff, text: src.Auto.String()}
	for _, act := range config.AutoActions {
		pr.IDs[act] = src.Auto.RuleID(act)
	}
	if ap != nil {
		files := p.Auto.Effective()
		for _, act := range config.AutoActions {
			if files[act] != pr.Rules[act] {
				pr.PendingReview, pr.FileRules = true, files
				break
			}
		}
	}
	return pr
}

// approvedProfile is the profile of the policy the console approved for
// profile name of the project at root, nil when there is none or it
// cannot be read by this account (a console run by a separated service
// account keeps it in that account's state).
func (e env) approvedProfile(root, name string) *config.Profile {
	state := e.stateDir
	if state == "" {
		var err error
		if state, err = kit.StateBase(); err != nil {
			return nil
		}
	}
	var ap config.Policy
	ok, err := kconfig.LoadApproved(product.P, state, kconfig.ApprovedKey(root, name), &ap)
	if err != nil || !ok || ap.Profile.Name != name {
		return nil
	}
	return &ap.Profile
}
