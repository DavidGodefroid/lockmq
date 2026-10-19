package main

import (
	"bufio"
	"bytes"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/BurntSushi/toml"

	"github.com/DavidGodefroid/lockkit/agentwire"
	kconfig "github.com/DavidGodefroid/lockkit/config"
	"github.com/DavidGodefroid/lockkit/sanitize"

	"github.com/DavidGodefroid/lockmq/internal/config"
	"github.com/DavidGodefroid/lockmq/internal/product"
)

// peekQuestion is asked for every profile setup creates (design §5.4).
const peekQuestion = "Allow reading messages of classic and quorum queues? Not strictly read-only: messages are requeued and marked redelivered, and may count toward a quorum queue's delivery limit. [y/N] "

// setupProfile is a profile as setup writes it.
type setupProfile struct {
	URL         string   `toml:"url"`
	AMQPURL     string   `toml:"amqp_url,omitempty"`
	StreamURL   string   `toml:"stream_url,omitempty"`
	VHosts      []string `toml:"vhosts,omitempty"`
	User        string   `toml:"user,omitempty"`
	Credentials string   `toml:"credentials,omitempty"`
	VaultPath   string   `toml:"vault_path,omitempty"`
	Tier        string   `toml:"tier"`
	Production  bool     `toml:"production,omitempty"`
}

// runSetup is `lockmq setup`: it asks for a broker profile and adds it to
// the project's .lockmq/config.toml.
func runSetup(e env, args []string) int {
	const usage = "lockmq setup [--project DIR]"
	fs := flag.NewFlagSet("setup", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	project := fs.String("project", "", "project directory")
	pos, err := parseInterleaved(fs, args)
	if err != nil || len(pos) > 0 {
		return usageFail(e, "setup", usage, "wrong arguments")
	}
	root := agentwire.ProjectRoot(product.P, e.cwd)
	if *project != "" {
		if root, err = filepath.Abs(*project); err != nil {
			return usageFail(e, "setup", usage, err.Error())
		}
	}
	path := kconfig.ProjectConfigPath(product.P, root)
	existing := map[string]config.Profile{}
	if data, err := os.ReadFile(path); err == nil {
		if existing, err = config.ParseProfiles(data, path); err != nil {
			fmt.Fprintln(e.stderr, "lockmq setup:", err)
			return exitUsage
		}
	}
	in := bufio.NewReader(e.stdin)
	ask := func(prompt, def string) (string, error) {
		fmt.Fprint(e.stdout, prompt)
		line, err := in.ReadString('\n')
		if err != nil && (line == "" || !errors.Is(err, io.EOF)) {
			return "", errors.New("no answer")
		}
		if line = strings.TrimSpace(line); line == "" {
			return def, nil
		}
		return line, nil
	}
	name, sp, err := askProfile(ask)
	if err != nil {
		fmt.Fprintln(e.stderr, "lockmq setup:", err)
		return exitFail
	}
	if _, dup := existing[name]; dup {
		fmt.Fprintf(e.stderr, "lockmq setup: profile %q is already in %s\n", name, path)
		return exitUsage
	}
	var buf bytes.Buffer
	if err := toml.NewEncoder(&buf).Encode(map[string]map[string]setupProfile{"profiles": {name: sp}}); err != nil {
		fmt.Fprintln(e.stderr, "lockmq setup:", err)
		return exitFail
	}
	if _, err := config.ParseProfiles(buf.Bytes(), "the new profile"); err != nil {
		fmt.Fprintln(e.stderr, "lockmq setup:", err)
		return exitUsage
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		fmt.Fprintln(e.stderr, "lockmq setup:", err)
		return exitFail
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o644)
	if err != nil {
		fmt.Fprintln(e.stderr, "lockmq setup:", err)
		return exitFail
	}
	_, werr := f.Write(append([]byte("\n"), buf.Bytes()...))
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	if werr != nil {
		fmt.Fprintln(e.stderr, "lockmq setup:", werr)
		return exitFail
	}
	fmt.Fprintf(e.stdout, "\nProfile %s written to %s (tier %s; no secret in the file).\n", name, path, sp.Tier)
	fmt.Fprintf(e.stdout, "Start its console in a terminal of the lockmq account: lockmq console --profile %s --project %s\n", name, root)
	fmt.Fprintln(e.stdout, "Wire your agents with: lockmq init")
	return exitOK
}

// askProfile asks the profile's questions.
func askProfile(ask func(prompt, def string) (string, error)) (string, setupProfile, error) {
	var sp setupProfile
	var err error
	q := func(dst *string, prompt, def string) {
		if err == nil {
			*dst, err = ask(prompt, def)
		}
	}
	var name, vhosts, prod, peek string
	q(&name, "Profile name [dev]: ", "dev")
	q(&sp.URL, "Management API URL [http://127.0.0.1:15672]: ", "http://127.0.0.1:15672")
	q(&sp.AMQPURL, "AMQP URL for stream reads (empty: none), such as amqps://rabbitmq.example.com:5671: ", "")
	q(&sp.StreamURL, "Stream protocol URL (empty: none), such as rabbitmq-stream+tls://rabbitmq.example.com:5551: ", "")
	q(&vhosts, "Vhosts, comma-separated (empty: every vhost the account sees): ", "")
	q(&sp.User, "Broker user (empty: asked when the console starts): ", "")
	q(&sp.Credentials, "Credentials: ask, keychain or vault [ask]: ", "ask")
	if sp.Credentials == kconfig.CredentialsVault {
		q(&sp.VaultPath, "Vault path of the login, such as rabbitmq/creds/lockmq-ro: ", "")
	}
	q(&prod, "Production? [y/N] ", "n")
	q(&peek, peekQuestion, "n")
	if err != nil {
		return "", sp, err
	}
	if sanitize.Terminal(name, false) != name {
		return "", sp, errors.New("invalid profile name")
	}
	for _, v := range strings.Split(vhosts, ",") {
		if v = strings.TrimSpace(v); v != "" {
			sp.VHosts = append(sp.VHosts, v)
		}
	}
	sp.Production = yes(prod)
	sp.Tier = config.TierRead.String()
	if yes(peek) {
		sp.Tier = config.TierPeek.String()
	}
	return name, sp, nil
}

func yes(s string) bool {
	s = strings.ToLower(strings.TrimSpace(s))
	return s == "y" || s == "yes"
}
