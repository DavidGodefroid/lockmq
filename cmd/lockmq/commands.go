package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"sort"
	"strings"
	"syscall"

	"github.com/DavidGodefroid/lockkit/client"
	"github.com/DavidGodefroid/lockkit/ipc"
	"github.com/DavidGodefroid/lockkit/mcpkit"
	"github.com/DavidGodefroid/lockkit/render"

	"github.com/DavidGodefroid/lockmq/internal/config"
	"github.com/DavidGodefroid/lockmq/internal/mcptools"
	"github.com/DavidGodefroid/lockmq/internal/mqconsole"
)

// parseInterleaved parses flags anywhere among the positional arguments.
func parseInterleaved(fs *flag.FlagSet, args []string) ([]string, error) {
	var pos []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		rest := fs.Args()
		if len(rest) == 0 {
			return pos, nil
		}
		if used := len(args) - len(rest); used > 0 && args[used-1] == "--" {
			return append(pos, rest...), nil
		}
		pos = append(pos, rest[0])
		args = rest[1:]
	}
}

// usageError is a usage or configuration error (exit 3).
type usageError struct{ msg string }

func (e usageError) Error() string { return e.msg }

func (e env) loadProfiles() (map[string]config.Profile, string, error) {
	if e.userConfig != "" {
		return config.LoadFrom(e.cwd, e.userConfig)
	}
	return config.Load(e.cwd)
}

func profileNames(ps map[string]config.Profile) []string {
	names := make([]string, 0, len(ps))
	for n := range ps {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// resolveProfile checks name against the configured profiles, or picks the
// only one when name is empty.
func resolveProfile(e env, name string) (string, error) {
	ps, _, err := e.loadProfiles()
	if err != nil {
		return "", usageError{err.Error()}
	}
	names := profileNames(ps)
	if name != "" {
		if _, ok := ps[name]; !ok {
			return "", usageError{fmt.Sprintf("unknown profile %q; configured profiles: %s", name, orNone(names))}
		}
		return name, nil
	}
	switch len(names) {
	case 0:
		return "", usageError{"no profile is configured: run lockmq setup, or add one to .lockmq/config.toml"}
	case 1:
		return names[0], nil
	}
	return "", usageError{fmt.Sprintf("several profiles are configured (%s): pass --profile", strings.Join(names, ", "))}
}

func orNone(s []string) string {
	if len(s) == 0 {
		return "none"
	}
	return strings.Join(s, ", ")
}

func (e env) dialer() func(profile string) (mcptools.Conn, error) {
	if e.dial != nil {
		return e.dial
	}
	return mcptools.DialClient(e.cwd, version)
}

// cliOpts are the flags of the client commands.
type cliOpts struct {
	profile, vhost, filter, sort, from, consumer, queue, exchange, intent string
	page, count, max                                                      int
	json, dryRun, unmask                                                  bool
}

func (o *cliOpts) flags(fs *flag.FlagSet) {
	fs.StringVar(&o.profile, "profile", "", "profile")
	fs.StringVar(&o.vhost, "vhost", "", "vhost")
	fs.StringVar(&o.filter, "filter", "", "queue name glob")
	fs.StringVar(&o.sort, "sort", "", "sort key")
	fs.StringVar(&o.from, "from", "", "first, last, an offset or an RFC 3339 timestamp")
	fs.StringVar(&o.consumer, "consumer", "", "named consumer reference")
	fs.StringVar(&o.queue, "queue", "", "bindings of this queue")
	fs.StringVar(&o.exchange, "exchange", "", "bindings of this exchange")
	fs.StringVar(&o.intent, "intent", "", "one line telling the human why")
	fs.IntVar(&o.page, "page", 0, "page from 1")
	fs.IntVar(&o.count, "count", 0, "messages to peek")
	fs.IntVar(&o.max, "max", 0, "messages to read")
	fs.BoolVar(&o.json, "json", false, "JSON output")
	fs.BoolVar(&o.dryRun, "dry-run", false, "show the verdict and the approval screen only")
	fs.BoolVar(&o.unmask, "unmask", false, "ask the human for the messages in clear (refused unless allowed in the console)")
}

// clientUsage is the usage line of each client command; positional is
// how many positional arguments it takes.
var clientUsage = map[string]struct {
	usage      string
	positional int
}{
	"status":        {"lockmq status [--profile P] [--json]", 0},
	"overview":      {"lockmq overview [--profile P] [--json]", 0},
	"queues":        {"lockmq queues [--vhost V] [--filter GLOB] [--sort messages] [--page N] [--profile P] [--json]", 0},
	"queue":         {"lockmq queue NAME [--vhost V] [--profile P] [--json]", 1},
	"exchanges":     {"lockmq exchanges [--vhost V] [--page N] [--profile P] [--json]", 0},
	"bindings":      {"lockmq bindings [--vhost V] [--queue Q | --exchange E] [--profile P] [--json]", 0},
	"policies":      {"lockmq policies [--vhost V] [--profile P] [--json]", 0},
	"consumers":     {"lockmq consumers [--vhost V] [--profile P] [--json]", 0},
	"connections":   {"lockmq connections [--page N] [--profile P] [--json]", 0},
	"channels":      {"lockmq channels [--page N] [--profile P] [--json]", 0},
	"stream":        {"lockmq stream NAME [--vhost V] [--profile P] [--json]", 1},
	"stream-offset": {"lockmq stream-offset NAME --consumer REF [--vhost V] [--profile P] [--json]", 1},
	"read":          {"lockmq read STREAM --from first|last|N|TIMESTAMP [--max N] [--vhost V] [--intent TEXT] [--unmask] [--dry-run] [--profile P] [--json]", 1},
	"peek":          {"lockmq peek QUEUE [--count N] [--vhost V] [--intent TEXT] [--unmask] [--dry-run] [--profile P] [--json]", 1},
	"pii":           {`lockmq pii list | add "queue=VHOST/GLOB body=$.path" [--profile P] [--json]`, -1},
	"request":       {`lockmq request "CHANGE" [--profile P] [--json]`, 1},
	"logout":        {"lockmq logout [--profile P] [--json]", 0},
}

func signalContext() (context.Context, context.CancelFunc) {
	return signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
}

// runClient runs one client command against the profile's console.
func runClient(e env, cmd string, args []string) int {
	u := clientUsage[cmd]
	fs := flag.NewFlagSet(cmd, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	var o cliOpts
	o.flags(fs)
	pos, err := parseInterleaved(fs, args)
	if err != nil {
		return usageFail(e, cmd, u.usage, err.Error())
	}
	if u.positional >= 0 && len(pos) != u.positional {
		return usageFail(e, cmd, u.usage, "wrong number of arguments")
	}
	method, params, what, err := request(cmd, pos, o)
	if err != nil {
		return usageFail(e, cmd, u.usage, err.Error())
	}
	profile, err := resolveProfile(e, o.profile)
	if err != nil {
		return usageFail(e, cmd, u.usage, err.Error())
	}
	c, err := e.dialer()(profile)
	if err != nil {
		var nc *client.NoConsoleError
		if errors.As(err, &nc) {
			fmt.Fprintf(e.stderr, "lockmq %s: no console runs for profile %s: %s\n", cmd, profile, nc.Hint())
			return exitNoConsole
		}
		fmt.Fprintf(e.stderr, "lockmq %s: %v\n", cmd, err)
		return exitFail
	}
	defer c.Close()
	// No timeout: the human may take up to the console's approval timeout.
	ctx, stop := signalContext()
	defer stop()
	var out any
	if err := c.Call(ctx, method, params, &out); err != nil {
		return callFail(e, cmd, err)
	}
	return printOut(e, o, what, out)
}

// request turns a command into the console call.
func request(cmd string, pos []string, o cliOpts) (method string, params any, what string, err error) {
	switch cmd {
	case "status":
		return ipc.MethodStatus, nil, "", nil
	case "logout":
		return ipc.MethodLogout, nil, "", nil
	case "request":
		return ipc.MethodChangeRequest, ipc.ChangeParams{Change: pos[0]}, "", nil
	case "pii":
		switch {
		case len(pos) == 1 && pos[0] == "list":
			return ipc.MethodPIIList, nil, "", nil
		case len(pos) == 2 && pos[0] == "add":
			return ipc.MethodPIIAdd, ipc.PIIAddParams{Pattern: pos[1]}, "", nil
		}
		return "", nil, "", errors.New(`want "list" or "add RULE"`)
	}
	in := mcptools.OpIn{VHost: o.vhost, Filter: o.filter, Sort: o.sort, Page: o.page, From: o.from, Consumer: o.consumer,
		Queue: o.queue, Exchange: o.exchange, Intent: o.intent, Unmask: o.unmask}
	if len(pos) == 1 {
		in.Name = pos[0]
	}
	switch cmd {
	case "read":
		in.Count = o.max
		if o.count != 0 {
			return "", nil, "", errors.New("read takes --max, not --count")
		}
	case "peek":
		in.Count = o.count
		if o.max != 0 {
			return "", nil, "", errors.New("peek takes --count, not --max")
		}
	default:
		if o.count != 0 || o.max != 0 || o.dryRun || o.intent != "" || o.unmask {
			return "", nil, "", errors.New("--count, --max, --intent, --unmask and --dry-run apply to read and peek only")
		}
	}
	p, err := mcptools.Params(cmd, in)
	if err != nil {
		return "", nil, "", err
	}
	if o.dryRun {
		return mqconsole.MethodPlan, p, "", nil
	}
	return mqconsole.MethodRun, p, "results", nil
}

// callFail reports a console error: a refusal or denial (exit 1), a closed
// console (exit 2).
func callFail(e env, cmd string, err error) int {
	var re *ipc.RPCError
	if errors.As(err, &re) {
		if re.Code == ipc.CodeRefused && re.Data != nil {
			fmt.Fprintf(e.stderr, "lockmq %s: refused [%s]: %s\n", cmd, re.Data.Code, re.Message)
			if re.Data.Hint != "" {
				fmt.Fprintf(e.stderr, "  hint: %s (retry: %s)\n", re.Data.Hint, re.Data.Retry)
			}
			return exitFail
		}
		fmt.Fprintf(e.stderr, "lockmq %s: %s\n", cmd, re.Message)
		return exitFail
	}
	if errors.Is(err, client.ErrConsoleClosed) {
		fmt.Fprintf(e.stderr, "lockmq %s: the console closed the connection\n", cmd)
		return exitNoConsole
	}
	fmt.Fprintf(e.stderr, "lockmq %s: %v\n", cmd, err)
	return exitFail
}

// outputCaps bound what a client command prints.
var outputCaps = render.Caps{MaxCellChars: 4096, MaxOutputBytes: 1 << 20}

func printOut(e env, o cliOpts, what string, out any) int {
	if res, ok := out.(map[string]any); ok && what != "" {
		// op.run: the data, framed.
		out = res["data"]
	}
	if o.json {
		if err := render.Value(e.stdout, out, outputCaps); err != nil {
			fmt.Fprintln(e.stderr, "lockmq:", err)
			return exitFail
		}
		return exitOK
	}
	if what != "" {
		fmt.Fprintln(e.stdout, mcpkit.Untrusted(what, "the broker"))
	}
	b, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		fmt.Fprintln(e.stderr, "lockmq:", err)
		return exitFail
	}
	fmt.Fprintln(e.stdout, string(b))
	return exitOK
}

// runMCP is `lockmq mcp [--profile P]`: an MCP server on stdin/stdout.
func runMCP(e env, args []string) int {
	const usage = "lockmq mcp [--profile P]"
	fs := flag.NewFlagSet("mcp", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	profile := fs.String("profile", "", "pin the server to one profile")
	pos, err := parseInterleaved(fs, args)
	if err != nil || len(pos) > 0 {
		return usageFail(e, "mcp", usage, "wrong arguments")
	}
	if _, _, err := e.loadProfiles(); err != nil {
		return usageFail(e, "mcp", usage, err.Error())
	}
	if *profile != "" {
		if _, err := resolveProfile(e, *profile); err != nil {
			return usageFail(e, "mcp", usage, err.Error())
		}
	}
	s := mcptools.New(mcptools.Options{
		Pinned: *profile,
		Profiles: func() ([]string, error) {
			ps, _, err := e.loadProfiles()
			return profileNames(ps), err
		},
		Dial:    e.dialer(),
		Version: version,
	})
	ctx, stop := signalContext()
	defer stop()
	if err := mcpkit.Serve(ctx, s, e.stdin, e.stdout); err != nil {
		fmt.Fprintf(e.stderr, "lockmq mcp: %v\n", err)
		return exitFail
	}
	return exitOK
}
