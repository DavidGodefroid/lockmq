package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	kconfig "github.com/DavidGodefroid/lockkit/config"
	"github.com/DavidGodefroid/lockkit/console"

	"github.com/DavidGodefroid/lockmq/internal/mqconsole"
	"github.com/DavidGodefroid/lockmq/internal/product"
)

// runConsole is `lockmq console`: the console of one profile, attached to
// its hub. It is launched by `lox up` only: the hub flags are required,
// and requests are decided on lockhub.
func runConsole(e env, args []string) int {
	const usage = "lockmq console --hub URL [--hub-plain-http] --hub-key-file FILE --instance NAME [--profile P] [--project DIR] [--allow-unmask] [--show-results] [--allowed-uids UID,...] (launched by lox up)"
	fs := flag.NewFlagSet("console", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	profile := fs.String("profile", "", "profile to serve")
	project := fs.String("project", "", "project directory (default: the current directory)")
	allowUnmask := fs.Bool("allow-unmask", false, "let clients ask for unmasked output (each approved here)")
	showResults := fs.Bool("show-results", false, "print results in clear in this console")
	var allowed onceFlag
	fs.Var(&allowed, "allowed-uids", "uids that may connect besides lockmq-clients' members (an agent in a container of its own)")
	var hub console.HubOptions
	hub.Register(fs)
	pos, err := parseInterleaved(fs, args)
	if err != nil || len(pos) > 0 {
		return usageFail(e, "console", usage, "wrong arguments")
	}
	uids, err := parseAllowedUIDs(allowed.v)
	if err != nil {
		return usageFail(e, "console", usage, err.Error())
	}
	if hub.URL == "" || hub.KeyFile == "" || hub.Instance == "" {
		// Hub-attached only: refused before the terminal, the profile or
		// any file is looked at.
		return usageFail(e, "console", usage, console.ErrNoHub.Error())
	}
	if !e.tty {
		fmt.Fprintln(e.stderr, "lockmq console: must run in a terminal: it asks the human for credentials and the console's own decisions")
		return exitUsage
	}
	pe := e
	if *project != "" {
		abs, err := filepath.Abs(*project)
		if err != nil {
			return usageFail(e, "console", usage, err.Error())
		}
		pe.cwd = abs
	}
	name, err := resolveProfile(pe, *profile)
	if err != nil {
		return usageFail(e, "console", usage, err.Error())
	}
	root, _ := kconfig.FindProjectRoot(product.P, pe.cwd)
	userCfg := e.userConfig
	if userCfg == "" {
		userCfg, _ = kconfig.UserConfigPath(product.P)
	}
	term := console.NewTerminal(os.Stdin, e.stdout)
	if err := term.Interactive(); err != nil {
		fmt.Fprintln(e.stderr, "lockmq console: warning:", err, "(the console keys and the status line are off)")
	}
	defer term.Restore()
	ctx, stop := signalContext()
	defer stop()
	p := product.P
	p.Version = strings.TrimPrefix(version, "v")
	be := mqconsole.New(mqconsole.Options{Profile: name, Root: root, UserConfig: userCfg, IO: term})
	var iso *console.IsolationEnv
	if len(uids) > 0 {
		// Set by whoever starts the console (the human, the launcher, lock
		// up), never by a client: the agent's uid, which names no account
		// in the console's container.
		env := allowUIDs(console.RealIsolationEnv(p, os.Stdin), uids)
		iso = &env
		term.Println("allowed uids besides lockmq-clients (--allowed-uids): " + joinUIDs(uids))
	}
	err = console.Run(ctx, p, be, console.Options{
		Profile: name, ProjectRoot: root, IO: term, TTY: os.Stdin, Isolation: iso,
		AllowUnmask: *allowUnmask, ShowResults: *showResults,
		Sanitizer: mqconsole.Sanitizer(), Hub: hub, OnHub: be.OnHub,
	})
	if err != nil {
		fmt.Fprintln(e.stderr, "lockmq console:", err)
		if errors.Is(err, console.ErrConfig) {
			return exitUsage
		}
		return exitFail
	}
	return exitOK
}
