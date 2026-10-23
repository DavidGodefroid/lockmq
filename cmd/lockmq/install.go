package main

import (
	"bufio"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/DavidGodefroid/lockmq/internal/product"
	"github.com/DavidGodefroid/lockmq/internal/separate"
)

// runInstall is `lockmq install`: the one-time setup that separates the
// console from the agent. It prints the root commands and, on Linux, runs
// them with sudo once the human confirms (or --yes); on macOS it prints
// them only.
func runInstall(e env, args []string) int {
	const usage = "lockmq install [--client USER] [--user lockmq] [--group lockmq-clients] [--print] [--trust-binary] [--yes]"
	fs := flag.NewFlagSet("install", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	var o separate.Options
	fs.StringVar(&o.Client, "client", "", "the account the agent runs as (default: you)")
	fs.StringVar(&o.Service, "user", product.P.Account, "the console account to create")
	fs.StringVar(&o.Group, "group", product.P.ClientsGroup, "the group allowed to reach the console")
	printOnly := fs.Bool("print", false, "print the commands, do not run them")
	trust := fs.Bool("trust-binary", false, "copy this binary even when another account than root can change it")
	yes := fs.Bool("yes", false, "run the commands without asking")
	if err := fs.Parse(args); err != nil || fs.NArg() > 0 {
		return usageFail(e, "install", usage, "wrong arguments")
	}
	separate.Fill(product.P, &o)
	if err := separate.Check(o); err != nil {
		return usageFail(e, "install", usage, err.Error())
	}
	bin := e.executable
	if bin == "" {
		var err error
		if bin, err = os.Executable(); err == nil {
			bin, err = filepath.EvalSymlinks(bin)
		}
		if err != nil {
			fmt.Fprintln(e.stderr, "lockmq install:", err)
			return exitFail
		}
	}
	pin := ""
	if *trust {
		var err error
		if pin, err = separate.FileSum(bin); err != nil {
			fmt.Fprintln(e.stderr, "lockmq install:", err)
			return exitFail
		}
		fmt.Fprintf(e.stdout, "--trust-binary: copying %s (sha256 %s, checked again before it is installed)\n\n", bin, pin)
	} else if msg, err := separate.SourceRefusal(product.P, bin); err != nil {
		fmt.Fprintln(e.stderr, "lockmq install:", err)
		return exitFail
	} else if msg != "" {
		fmt.Fprint(e.stderr, msg)
		return exitFail
	}
	goos := e.goos
	if goos == "" {
		goos = runtime.GOOS
	}
	script := separate.Script(product.P, goos, bin, o, pin)
	fmt.Fprintf(e.stdout, `lockmq install separates the console from the agent:
  - the console runs as %[1]q, in its own login session (use Wayland), and alone
    holds the broker credentials (its keychain, Vault or its prompt);
  - the agent's account %[2]q joins group %[3]q, which may reach the console's
    socket; the kernel checks every connection;
  - lockmq is copied to /usr/local/bin, owned by root, so that the agent cannot
    replace the binary the console runs.

These commands run as root:

    %[4]s
`, o.Service, o.Client, o.Group, strings.ReplaceAll(strings.TrimRight(script, "\n"), "\n", "\n    "))
	if *printOnly || goos != "linux" {
		if goos != "linux" {
			fmt.Fprintln(e.stdout, "\nRun them in a terminal as an administrator.")
		}
		return exitOK
	}
	if !*yes {
		if !e.tty {
			fmt.Fprintln(e.stderr, "lockmq install: confirm in a terminal, or run with --print or --yes")
			return exitUsage
		}
		fmt.Fprint(e.stdout, "Run them now with sudo? [y/N] ")
		ans, _ := bufio.NewReader(e.stdin).ReadString('\n')
		if !strings.EqualFold(strings.TrimSpace(ans), "y") {
			fmt.Fprintln(e.stdout, "nothing done")
			return exitOK
		}
	}
	if err := e.asRoot(script); err != nil {
		fmt.Fprintln(e.stderr, "lockmq install:", err)
		return exitFail
	}
	fmt.Fprintf(e.stdout, `
Done. Next:
  1. sudo passwd %[1]s            (a password for the console account)
  2. log out and in again as %[2]s (the new group applies at login)
  3. open a separate session as %[1]s (switch user, Wayland), then run there:
       lockmq console --profile <name> --project <the agent's project dir>
  4. lockmq doctor                (from both accounts)
`, o.Service, o.Client)
	return exitOK
}

// asRoot runs a shell script as root: e.root when set (tests), else sudo
// sh -c on the human's terminal, or sh -c when this already is root (a
// container run with --user 0, which has no sudo). The script holds no
// secret.
func (e env) asRoot(script string) error {
	if e.root != nil {
		return e.root(script)
	}
	cmd := exec.Command("sudo", "sh", "-c", script)
	if e.geteuid() == 0 {
		cmd = exec.Command("sh", "-c", script)
	}
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, e.stdout, e.stderr
	return cmd.Run()
}
