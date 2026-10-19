package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/DavidGodefroid/lockkit/agentwire"
	"github.com/DavidGodefroid/lockkit/client"
	"github.com/DavidGodefroid/lockkit/ipc"

	"github.com/DavidGodefroid/lockmq/internal/agentassets"
	"github.com/DavidGodefroid/lockmq/internal/config"
	"github.com/DavidGodefroid/lockmq/internal/product"
)

// runDoctor is `lockmq doctor`: the config, each profile's tier, whether
// its console runs, and the agent wiring.
func runDoctor(e env, args []string) int {
	const usage = "lockmq doctor [--profile P]"
	fs := flag.NewFlagSet("doctor", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	only := fs.String("profile", "", "check one profile")
	pos, err := parseInterleaved(fs, args)
	if err != nil || len(pos) > 0 {
		return usageFail(e, "doctor", usage, "wrong arguments")
	}
	code := exitOK
	ps, root, err := e.loadProfiles()
	if err != nil {
		fmt.Fprintln(e.stdout, "✗ config:", err)
		return exitUsage
	}
	where := "user config only (no .lockmq/config.toml found from here)"
	if root != "" {
		where = "project " + root
	}
	fmt.Fprintln(e.stdout, "✓ config:", where)
	names := profileNames(ps)
	if *only != "" {
		if _, ok := ps[*only]; !ok {
			fmt.Fprintf(e.stdout, "✗ profile %s: not configured (configured: %s)\n", *only, orNone(names))
			return exitUsage
		}
		names = []string{*only}
	}
	if len(names) == 0 {
		fmt.Fprintln(e.stdout, "✗ profiles: none; run lockmq setup")
		code = exitUsage
	}
	for _, n := range names {
		fmt.Fprintln(e.stdout, profileLine(ps[n]))
		fmt.Fprintln(e.stdout, "    "+consoleLine(e, n))
	}
	if env, err := e.agents(); err == nil {
		found, pending := agentassets.Wiring.Unwired(env)
		switch {
		case len(found) == 0:
			fmt.Fprintln(e.stdout, "- agent wiring: no agent found on this machine")
		case len(pending) == 0:
			fmt.Fprintln(e.stdout, "✓ agent wiring:", strings.Join(found, ", "))
		default:
			fmt.Fprintf(e.stdout, "! agent wiring: %s not wired; run lockmq init %s\n", strings.Join(pending, ", "), strings.Join(pending, " "))
		}
	}
	return code
}

// agents is the account the agents are wired for.
func (e env) agents() (agentwire.Env, error) {
	if e.agentEnv != nil {
		return e.agentEnv()
	}
	return agentwire.SystemEnv()
}

// profileLine sums up a profile: its tier first.
func profileLine(p config.Profile) string {
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
	return fmt.Sprintf("✓ profile %s: %s · %s · %s · credentials %s", p.Name, tier, kind, strings.Join(protos, "+"), p.Credentials)
}

// consoleLine tells whether the profile's console runs.
func consoleLine(e env, profile string) string {
	c, err := e.dialer()(profile)
	if err != nil {
		var nc *client.NoConsoleError
		if errors.As(err, &nc) {
			return "console: not running; start it with: " + nc.Command()
		}
		return "console: " + err.Error()
	}
	defer c.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var st map[string]any
	if err := c.Call(ctx, ipc.MethodStatus, nil, &st); err != nil {
		return "console: " + err.Error()
	}
	return "console: running"
}

// runInit is `lockmq init [agent ...]`: the agent files of the project.
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
