package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/DavidGodefroid/lockkit/agentwire"
	"github.com/DavidGodefroid/lockkit/client"
	"github.com/DavidGodefroid/lockkit/ipc"
	"github.com/DavidGodefroid/lockkit/sysconf"
	"github.com/DavidGodefroid/lockkit/verbs"

	"github.com/DavidGodefroid/lockmq/internal/agentassets"
	"github.com/DavidGodefroid/lockmq/internal/config"
	"github.com/DavidGodefroid/lockmq/internal/product"
)

// doctorRun collects the checks of one doctor: printed as they come for a
// human, or as one verbs.Doctor with --json.
type doctorRun struct {
	e      env
	asJSON bool
	// out takes the human messages besides the checks (the repair script,
	// the question): stdout, or stderr with --json.
	out    io.Writer
	checks []verbs.Check
	fixed  []string
}

// add records a check; line is what a human reads.
func (d *doctorRun) add(c verbs.Check, line string) {
	d.checks = append(d.checks, c)
	if !d.asJSON {
		fmt.Fprintln(d.e.stdout, line)
	}
}

// finish prints the JSON report and returns the exit status: 0 exactly
// when no check failed, else code (or exitFail when code is 0).
func (d *doctorRun) finish(code int) int {
	rep := verbs.NewDoctor(product.P, d.checks, d.fixed)
	if d.asJSON {
		_ = verbs.WriteJSON(d.e.stdout, rep)
	}
	switch {
	case rep.OK:
		return exitOK
	case code == exitOK:
		return exitFail
	}
	return code
}

// mark is the human mark of a status.
func mark(status string) string {
	switch status {
	case verbs.StatusOK:
		return "✓"
	case verbs.StatusWarn:
		return "!"
	}
	return "✗"
}

// runDoctor is `lockmq doctor`: the config, the separated setup, the
// project's .lockmq, each profile's tier, whether its console runs, and
// the agent wiring (lockkit GUIDE §14).
func runDoctor(e env, args []string) int {
	const usage = "lockmq doctor [--profile P] [--fix [--yes]] [--json]"
	asJSON := verbs.WantsJSON(args)
	fail := func(err error) int { return verbFail(e, "doctor", usage, asJSON, err) }
	fs := flag.NewFlagSet("doctor", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	only := fs.String("profile", "", "check one profile")
	fix := fs.Bool(verbs.FlagFix, false, "repair the project's .lockmq as root (sudo, or directly when run as root)")
	yes := fs.Bool(verbs.FlagYes, false, "with --fix: repair without asking")
	fs.Bool(verbs.FlagJSON, false, "one line of JSON on stdout")
	pos, err := parseInterleaved(fs, args)
	switch {
	case err != nil || len(pos) > 0:
		return fail(errors.New("wrong arguments"))
	case *yes && !*fix:
		return fail(errors.New("--yes is for --fix"))
	case *fix && !*yes && !e.tty:
		return fail(errors.New("--fix asks before each repair: confirm in a terminal, or run with --fix --yes"))
	}
	d := &doctorRun{e: e, asJSON: asJSON, out: e.stdout}
	if asJSON {
		d.out = e.stderr
	}
	ps, root, err := e.loadProfiles()
	if err != nil {
		d.add(verbs.Check{Check: "config", Status: verbs.StatusFail, Detail: err.Error()}, "✗ config: "+err.Error())
		return d.finish(exitUsage)
	}
	names := profileNames(ps)
	if *only != "" {
		if err := checkProfileArg(ps, *only); err != nil {
			return fail(err)
		}
		names = []string{*only}
	}
	code := exitOK
	where := "user config only (no .lockmq/config.toml found from here)"
	if root != "" {
		where = "project " + root
	}
	d.add(verbs.Check{Check: "config", Status: verbs.StatusOK, Detail: where}, "✓ config: "+where)
	const sysFix = "run lockmq through the launcher (docs/docker.md)"
	switch sys, err := e.sysconf(); {
	case err != nil:
		d.add(verbs.Check{Check: "separated setup", Status: verbs.StatusFail, Detail: err.Error(), Fix: sysFix}, "✗ separated setup: "+err.Error())
	case sys == nil:
		const detail = "none (/etc/lockmq/system.toml): the console refuses to start; lockmq runs in Docker: run it through the launcher (docs/docker.md)"
		d.add(verbs.Check{Check: "separated setup", Status: verbs.StatusFail, Detail: detail, Fix: sysFix}, "✗ separated setup: "+detail)
	default:
		detail := fmt.Sprintf("console account %s, clients group %s", sys.ServiceUser, sys.ClientGroup)
		d.add(verbs.Check{Check: "separated setup", Status: verbs.StatusOK, Detail: detail}, "✓ separated setup: "+detail)
		if root != "" {
			d.project(root, sys.ServiceUser, *fix, *yes)
		}
	}
	if len(names) == 0 {
		d.add(verbs.Check{Check: "profiles", Status: verbs.StatusFail, Detail: "none", Fix: "lockmq profile add"},
			"✗ profiles: none; run lockmq profile add")
		code = exitUsage
	}
	for _, n := range names {
		sum := profileSummary(ps[n])
		d.add(verbs.Check{Check: "profile " + n, Status: verbs.StatusOK, Detail: sum}, "✓ profile "+n+": "+sum)
		status, detail := consoleState(e, n)
		d.add(verbs.Check{Check: "console " + n, Status: status, Detail: detail}, "    console: "+detail)
	}
	if env, err := e.agents(); err == nil {
		found, pending := agentassets.Wiring.Unwired(env)
		switch {
		case len(found) == 0:
			const detail = "no agent found on this machine"
			d.add(verbs.Check{Check: "agent wiring", Status: verbs.StatusWarn, Detail: detail}, "- agent wiring: "+detail)
		case len(pending) == 0:
			detail := strings.Join(found, ", ")
			d.add(verbs.Check{Check: "agent wiring", Status: verbs.StatusOK, Detail: detail}, "✓ agent wiring: "+detail)
		default:
			detail := strings.Join(pending, ", ") + " not wired"
			fix := "lockmq init " + strings.Join(pending, " ")
			d.add(verbs.Check{Check: "agent wiring", Status: verbs.StatusWarn, Detail: detail, Fix: fix},
				"! agent wiring: "+detail+"; run "+fix)
		}
	}
	return d.finish(code)
}

// project checks the project's .lockmq for the console account and, with
// fix, repairs it as root, then checks again.
func (d *doctorRun) project(root, service string, fix, yes bool) {
	e := d.e
	svc, ok := e.service(service)
	if !ok {
		detail := fmt.Sprintf("account %s not found on this machine; not checked", service)
		d.add(verbs.Check{Check: "project", Status: verbs.StatusWarn, Detail: detail},
			fmt.Sprintf("- project %s: %s", product.P.ProjectDir(), detail))
		return
	}
	first := len(d.checks)
	var repairs, repaired []string
	for _, c := range projectChecks(root, svc) {
		d.addProject(c)
		if c.repair != "" {
			repairs = append(repairs, c.repair)
			repaired = append(repaired, c.line)
		}
	}
	if len(repairs) == 0 {
		return
	}
	if !fix {
		fmt.Fprintln(d.out, "  repair: lockmq doctor --fix (as root in the image: see docs/docker.md)")
		return
	}
	script := "set -eu\n" + strings.Join(repairs, "\n") + "\n"
	fmt.Fprintf(d.out, "These repairs run as root:\n    %s\n", strings.ReplaceAll(strings.TrimRight(script, "\n"), "\n", "\n    "))
	if !yes {
		q := "Run them now with sudo? [y/N] "
		if e.geteuid() == 0 {
			q = "Run them now? [y/N] "
		}
		fmt.Fprint(d.out, q)
		ans, _ := bufio.NewReader(e.stdin).ReadString('\n')
		if !strings.EqualFold(strings.TrimSpace(ans), "y") {
			fmt.Fprintln(d.out, "root repairs not made")
			return
		}
	}
	if err := e.asRoot(script); err != nil {
		d.add(verbs.Check{Check: "project repair", Status: verbs.StatusFail, Detail: err.Error()}, "✗ project repair: "+err.Error())
		return
	}
	d.fixed = append(d.fixed, repaired...)
	// The checks after the repairs replace those before them.
	d.checks = d.checks[:first]
	for _, c := range projectChecks(root, svc) {
		d.addProject(c)
	}
}

// addProject records one check of the project's .lockmq.
func (d *doctorRun) addProject(c projectCheck) {
	status, fix := verbs.StatusOK, ""
	if !c.ok {
		status = verbs.StatusFail
		if c.repair != "" {
			fix = "lockmq doctor --fix"
		}
	}
	d.add(verbs.Check{Check: "project", Status: status, Detail: c.line, Fix: fix}, mark(status)+" "+c.line)
}

// service is the console account; false when there is none.
func (e env) service(name string) (svcAccount, bool) {
	if e.svc != nil {
		return e.svc(name)
	}
	return lookupService(name)
}

// geteuid is the effective uid.
func (e env) geteuid() int {
	if e.euid != nil {
		return e.euid()
	}
	return os.Geteuid()
}

// agents is the account the agents are wired for.
func (e env) agents() (agentwire.Env, error) {
	if e.agentEnv != nil {
		return e.agentEnv()
	}
	return agentwire.SystemEnv()
}

// sysconf is the machine's separated setup (nil: none).
func (e env) sysconf() (*sysconf.Config, error) {
	if e.sys != nil {
		return e.sys()
	}
	return sysconf.Load(product.P)
}

// profileSummary sums up a profile: its tier first.
func profileSummary(p config.Profile) string {
	tier := "tier " + p.Tier.String()
	if p.Tier == config.TierPeek {
		tier += " (NOT READ-ONLY: queue peeks requeue messages)"
	}
	kind := "non-production"
	if p.Production {
		kind = "PRODUCTION"
	}
	protos := []string{"management"}
	if p.StreamURL != "" {
		protos = append(protos, "stream")
	}
	if p.AMQPURL != "" {
		protos = append(protos, "amqp")
	}
	return fmt.Sprintf("%s · %s · %s · credentials %s", tier, kind, strings.Join(protos, "+"), p.Credentials)
}

// consoleState tells whether the profile's console runs: a console that
// does not run is a warning (it runs under lox up, after the setup).
func consoleState(e env, profile string) (string, string) {
	c, err := e.dialer()(profile)
	if err != nil {
		var nc *client.NoConsoleError
		if errors.As(err, &nc) {
			return verbs.StatusWarn, fmt.Sprintf("not running; the console runs under `%[1]s`: run `%[1]s`, then `lox attach <instance>` for its questions", nc.Command())
		}
		return verbs.StatusWarn, err.Error()
	}
	defer c.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var st map[string]any
	if err := c.Call(ctx, ipc.MethodStatus, nil, &st); err != nil {
		return verbs.StatusWarn, err.Error()
	}
	return verbs.StatusOK, "running"
}

// runInit is `lockmq init [agent ...]`: the agent files of the project. It
// never asks and is idempotent; with no agent named it wires the agents
// found on this machine.
func runInit(e env, args []string) int {
	agents := args
	if len(agents) == 0 {
		env, err := e.agents()
		if err != nil {
			fmt.Fprintln(e.stderr, "lockmq init:", err)
			return exitFail
		}
		agents = agentwire.Detect(env)
		if len(agents) == 0 {
			fmt.Fprintf(e.stderr, "lockmq init: no agent found; name one of: %s\n", strings.Join(agentwire.Agents(), ", "))
			return exitUsage
		}
	}
	known := agentwire.Agents()
	for _, a := range agents {
		// A name that is not an agent's (a flag, a token pasted in the
		// wrong place) is not quoted, nor passed to agentwire, whose
		// error would quote it.
		if !slices.Contains(known, a) {
			fmt.Fprintf(e.stderr, "lockmq init: unknown agent; name one of: %s\n", strings.Join(known, ", "))
			return exitUsage
		}
	}
	root := agentwire.ProjectRoot(product.P, e.cwd)
	for _, a := range agents {
		res, err := agentassets.Wiring.Init(root, a)
		if err != nil {
			fmt.Fprintf(e.stderr, "lockmq init %s: %v\n", a, err)
			return exitFail
		}
		for _, act := range res.Actions {
			line := fmt.Sprintf("%s: %s %s", a, act.Status, act.Path)
			if act.Detail != "" {
				line += " (" + act.Detail + ")"
			}
			fmt.Fprintln(e.stdout, line)
		}
		if res.Notes != "" {
			fmt.Fprintln(e.stdout, res.Notes)
		}
	}
	return exitOK
}
