// Command lockmq lets an AI agent inspect a RabbitMQ broker through a
// console the human holds: read-only by default, message content masked,
// every content read approved.
package main

import (
	"fmt"
	"io"
	"os"
	"strings"

	"golang.org/x/term"

	"github.com/DavidGodefroid/lockkit/agentwire"
	"github.com/DavidGodefroid/lockkit/sysconf"

	"github.com/DavidGodefroid/lockmq/internal/mcptools"
)

// version is set at build time (-X main.version=...).
var version = "dev"

// Exit codes: 0 ok, 1 refused/denied/failed, 2 no console, 3 usage or
// configuration error.
const (
	exitOK        = 0
	exitFail      = 1
	exitNoConsole = 2
	exitUsage     = 3
)

// env is what a command reads and writes; tests replace it.
type env struct {
	stdin          io.Reader
	stdout, stderr io.Writer
	cwd            string
	// tty reports whether stdin is a terminal.
	tty bool
	// dial connects to the console of a profile; nil is the real socket.
	dial func(profile string) (mcptools.Conn, error)
	// userConfig is the user config file; "" is the real one.
	userConfig string
	// agentEnv is the account the agents are wired for; nil is the real
	// one.
	agentEnv func() (agentwire.Env, error)
	// root runs install's script as root; nil is sudo sh -c.
	root func(script string) error
	// sys reads the separated setup; nil is lockkit's sysconf.Load.
	sys func() (*sysconf.Config, error)
	// executable and goos replace the running binary and system (tests).
	executable, goos string
}

const usageText = `usage: lockmq <command> [arguments]

Human commands
  install    [--client USER] [--print] [--trust-binary] [--yes]
                                        separate the console from the agent: console
                                        account, clients group, /etc/lockmq (sudo)
  setup     [--project DIR]            add a broker profile to the project (asks)
  console    [--profile P] [--project DIR] [--auto] [--allow-unmask] [--show-results]
                                        run the approval console
  doctor     [--profile P]              check the config, each profile's tier, the consoles
                                        and the agent wiring
  init       [claude|codex|cursor|gemini ...]
                                        wire agents to lockmq in the project

Client commands (--profile P, --json)
  status
  overview
  queues        [--vhost V] [--filter GLOB] [--sort messages] [--page N]
  queue         NAME [--vhost V]
  exchanges     [--vhost V] [--page N]
  bindings      [--vhost V] [--queue Q | --exchange E]
  policies      [--vhost V]
  consumers     [--vhost V]
  connections   [--page N]
  channels      [--page N]
  stream        NAME [--vhost V]
  stream-offset NAME --consumer REF [--vhost V]
  read          STREAM --from first|last|N|TIMESTAMP [--max N] [--vhost V] [--intent TEXT] [--dry-run]
  peek          QUEUE [--count N] [--vhost V] [--intent TEXT] [--dry-run]   (NOT read-only; tier peek)
  pii           list | add "queue=VHOST/GLOB body=$.path"
  request       "tier=peek"             queue a change for the human (never applied)
  logout
  mcp           [--profile P]           MCP server on stdin/stdout

Other
  version, help

Exit codes  0 ok · 1 refused/denied/failed · 2 no console · 3 usage/config error
`

func main() {
	cwd, err := os.Getwd()
	if err != nil {
		fmt.Fprintln(os.Stderr, "lockmq:", err)
		os.Exit(exitFail)
	}
	os.Exit(run(env{stdin: os.Stdin, stdout: os.Stdout, stderr: os.Stderr, cwd: cwd,
		tty: term.IsTerminal(int(os.Stdin.Fd()))}, os.Args[1:]))
}

// clientCommands are the commands that talk to a console.
var clientCommands = []string{"status", "overview", "queues", "queue", "exchanges", "bindings", "policies", "consumers",
	"connections", "channels", "stream", "stream-offset", "read", "peek", "pii", "request", "logout"}

func run(e env, args []string) int {
	if len(args) == 0 {
		fmt.Fprint(e.stderr, usageText)
		return exitUsage
	}
	switch cmd := args[0]; cmd {
	case "version", "--version":
		fmt.Fprintln(e.stdout, "lockmq "+version)
		return exitOK
	case "help", "-h", "--help":
		fmt.Fprint(e.stdout, usageText)
		return exitOK
	case "console":
		return runConsole(e, args[1:])
	case "mcp":
		return runMCP(e, args[1:])
	case "setup":
		return runSetup(e, args[1:])
	case "doctor":
		return runDoctor(e, args[1:])
	case "init":
		return runInit(e, args[1:])
	case "install":
		return runInstall(e, args[1:])
	default:
		for _, c := range clientCommands {
			if c == cmd {
				return runClient(e, cmd, args[1:])
			}
		}
		fmt.Fprintf(e.stderr, "lockmq: unknown command %q\n\n%s", cmd, usageText)
		return exitUsage
	}
}

// usageFail prints a usage error.
func usageFail(e env, cmd, usage, msg string) int {
	fmt.Fprintf(e.stderr, "lockmq %s: %s\nusage: %s\n", cmd, msg, strings.TrimSpace(usage))
	return exitUsage
}
