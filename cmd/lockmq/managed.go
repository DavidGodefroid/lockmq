package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"path/filepath"
	"strings"

	kconfig "github.com/DavidGodefroid/lockkit/config"
	"github.com/DavidGodefroid/lockkit/verbs"

	"github.com/DavidGodefroid/lockmq/internal/config"
	"github.com/DavidGodefroid/lockmq/internal/mqconsole"
	"github.com/DavidGodefroid/lockmq/internal/product"
)

const (
	managedShowUsage  = "lockmq managed show"
	managedCheckUsage = "lockmq managed check [--profile P] [--project DIR]"
	// noManaged is what managed show, managed check and doctor print when
	// the machine has no managed file.
	noManaged = "no managed configuration"
)

// runManaged is `lockmq managed show|check`: the company's managed file
// (lockkit managed) as lockmq reads it. lox runs `managed check` before it
// starts the lock: its output and exit codes are a contract.
func runManaged(e env, args []string) int {
	if len(args) > 0 {
		switch args[0] {
		case "show":
			return runManagedShow(e, args[1:])
		case "check":
			return runManagedCheck(e, args[1:])
		}
	}
	// The subcommand is not quoted: it may be a token pasted in the wrong
	// place.
	return usageFail(e, "managed", managedShowUsage+"\n       "+managedCheckUsage, "unknown subcommand; want show or check")
}

// runManagedShow prints the managed file: its path, organisation,
// fingerprint and signature, then lockmq's bounds, defaults and profile
// names. No file: noManaged, exit 0. A file refused: the error, exit 3.
func runManagedShow(e env, args []string) int {
	if len(args) > 0 {
		return usageFail(e, "managed show", managedShowUsage, "unexpected arguments")
	}
	m, err := config.LoadManaged()
	if err != nil {
		fmt.Fprintln(e.stderr, "lockmq managed show:", err)
		return exitUsage
	}
	if m == nil {
		fmt.Fprintln(e.stdout, noManaged)
		return exitOK
	}
	f := m.File
	w := e.stdout
	fmt.Fprintln(w, f.Describe())
	fmt.Fprintln(w, "path: "+f.Path)
	org := f.Organisation
	if org == "" {
		org = "none"
	}
	fmt.Fprintln(w, "organisation: "+org)
	fmt.Fprintln(w, "fingerprint: sha256:"+f.Fingerprint)
	signed := "no"
	if f.Signed {
		signed = "yes"
	}
	fmt.Fprintln(w, "signed: "+signed)
	section(w, "bounds", m.Bounds.Describe())
	section(w, "defaults", m.DescribeDefaults())
	fmt.Fprintln(w, "profiles: "+orNone(f.Profiles(config.ManagedTool)))
	return exitOK
}

// section prints a titled list of lines, "none" when empty.
func section(w io.Writer, title string, lines []string) {
	if len(lines) == 0 {
		fmt.Fprintln(w, title+": none")
		return
	}
	fmt.Fprintln(w, title+":")
	for _, l := range lines {
		fmt.Fprintln(w, "  "+l)
	}
}

// runManagedCheck checks the profiles of the config files against the
// managed bounds, as the console does at start, without a connection or a
// credential: one "blocked by company policy: …" line per violation and
// exit 3 on any, else one ok line and exit 0.
func runManagedCheck(e env, args []string) int {
	const cmd = "managed check"
	fs := flag.NewFlagSet(cmd, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	name := fs.String(verbs.FlagProfile, "", "check one profile")
	project := fs.String(verbs.FlagProject, "", "project directory")
	if err := verbs.Parse(product.P, fs, args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			fmt.Fprintln(e.stdout, "usage: "+managedCheckUsage)
			return exitOK
		}
		return usageFail(e, cmd, managedCheckUsage, err.Error())
	}
	if fs.NArg() != 0 {
		return usageFail(e, cmd, managedCheckUsage, "unexpected arguments")
	}
	if *project != "" {
		abs, err := filepath.Abs(*project)
		if err != nil {
			return usageFail(e, cmd, managedCheckUsage, "--project: invalid directory")
		}
		e.cwd = abs
	}
	m, err := config.LoadManaged()
	if err != nil {
		fmt.Fprintln(e.stderr, "lockmq managed check:", err)
		return exitUsage
	}
	if m == nil {
		fmt.Fprintln(e.stdout, noManaged)
		return exitOK
	}
	ps, root, err := e.loadProfiles()
	if err != nil {
		fmt.Fprintln(e.stderr, "lockmq managed check:", err)
		return exitUsage
	}
	names := profileNames(ps)
	if *name != "" {
		if err := checkProfileArg(ps, *name); err != nil {
			return usageFail(e, cmd, managedCheckUsage, err.Error())
		}
		names = []string{*name}
	}
	userCfg := e.userConfig
	if userCfg == "" {
		userCfg, _ = kconfig.UserConfigPath(product.P)
	}
	blocked := false
	for _, n := range names {
		// The policy the console would load: the profile, the PII rules
		// and the managed ones.
		pol, err := mqconsole.New(mqconsole.Options{Profile: n, Root: root, UserConfig: userCfg}).LoadPolicy()
		if err != nil {
			fmt.Fprintln(e.stderr, "lockmq managed check:", err)
			return exitUsage
		}
		lines, err := m.BoundsOf().Violations(pol.(mqconsole.Policy).Policy)
		if err != nil {
			fmt.Fprintln(e.stderr, "lockmq managed check:", err)
			return exitUsage
		}
		for _, l := range lines {
			fmt.Fprintln(e.stdout, l)
			blocked = true
		}
	}
	if blocked {
		return exitUsage
	}
	fmt.Fprintf(e.stdout, "ok: %s within the company policy (%s)\n", plural(len(names), "profile"), strings.Join(names, ", "))
	return exitOK
}

func plural(n int, word string) string {
	if n == 1 {
		return "1 " + word
	}
	return fmt.Sprintf("%d %ss", n, word)
}
