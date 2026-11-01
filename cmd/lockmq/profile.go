package main

import (
	"bufio"
	"bytes"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/BurntSushi/toml"

	"github.com/DavidGodefroid/lockkit/agentwire"
	kconfig "github.com/DavidGodefroid/lockkit/config"
	"github.com/DavidGodefroid/lockkit/verbs"

	"github.com/DavidGodefroid/lockmq/internal/config"
	"github.com/DavidGodefroid/lockmq/internal/product"
)

// peekQuestion is asked for every profile `profile add` creates
// interactively, unless --tier gives the answer (design §5.4).
const peekQuestion = "Allow reading messages of classic and quorum queues? Not strictly read-only: messages are requeued and marked redelivered, and may count toward a quorum queue's delivery limit. [y/N] "

// defaultProfile is the profile name when --profile is absent.
const defaultProfile = "dev"

const profileAddUsage = `lockmq profile add [--non-interactive] [--json] [--project DIR] [--profile NAME] [--production]
                          [--credentials ask|vault [--vault-path P]] [--url URL] [--user USER] [--vhost V]...
                          [--amqp-url URL] [--stream-url URL] [--tier read|peek]`

// newProfile is a profile as `profile add` writes it.
type newProfile struct {
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

// stringList is a repeatable flag; String renders every value, as
// verbs.RefuseSecrets needs.
type stringList []string

func (l *stringList) String() string     { return strings.Join(*l, ",") }
func (l *stringList) Set(v string) error { *l = append(*l, v); return nil }

// errExists: the profile is already in the project's config. The name is
// not quoted (it may be a secret typed at the wrong prompt).
var errExists = errors.New("a profile of that name is already in the project's config; nothing written")

// failedError is a failure that is not the caller's: a file not written.
type failedError struct{ error }

// configError is a project config that does not parse.
type configError struct{ error }

// verbCode is the verbs.Error code and the exit status of err.
func verbCode(err error) (string, int) {
	var fe failedError
	var ce configError
	switch {
	case errors.Is(err, errExists):
		return verbs.CodeExists, exitUsage
	case errors.Is(err, verbs.ErrSecret):
		return verbs.CodeSecret, exitUsage
	case errors.As(err, &fe):
		return verbs.CodeFailed, exitFail
	case errors.As(err, &ce):
		return verbs.CodeFailed, exitUsage
	}
	return verbs.CodeUsage, exitUsage
}

// verbFail reports the failure of a launcher verb on stderr (with the
// usage for a usage error) and, with --json, as one verbs.Error on stdout.
func verbFail(e env, cmd, usage string, asJSON bool, err error) int {
	code, exit := verbCode(err)
	fmt.Fprintf(e.stderr, "lockmq %s: %v\n", cmd, err)
	if code == verbs.CodeUsage && usage != "" {
		fmt.Fprintf(e.stderr, "usage: %s\n", usage)
	}
	if asJSON {
		_ = verbs.WriteJSON(e.stdout, verbs.Error{Lock: product.P.Name, Code: code, Error: err.Error()})
	}
	return exit
}

// runProfile is `lockmq profile <subcommand>`; add is the only one.
func runProfile(e env, args []string) int {
	if len(args) == 0 || args[0] != "add" {
		// The subcommand is not quoted: it may be a token pasted in the
		// wrong place.
		return verbFail(e, "profile", profileAddUsage, verbs.WantsJSON(args), errors.New("unknown subcommand; the only one is add"))
	}
	return runProfileAdd(e, args[1:])
}

// runProfileAdd is `lockmq profile add` (lockkit GUIDE §14): it adds one
// broker profile to the project's .lockmq/config.toml, from its flags and,
// without --non-interactive, from the answers to what they did not give.
// No form takes, asks for or writes a secret.
func runProfileAdd(e env, args []string) int {
	const cmd = "profile add"
	asJSON := verbs.WantsJSON(args)
	fail := func(err error) int { return verbFail(e, cmd, profileAddUsage, asJSON, err) }
	fs := flag.NewFlagSet(cmd, flag.ContinueOnError)
	nonInteractive := fs.Bool(verbs.FlagNonInteractive, false, "never ask")
	fs.Bool(verbs.FlagJSON, false, "one line of JSON on stdout")
	project := fs.String(verbs.FlagProject, "", "project directory")
	name := fs.String(verbs.FlagProfile, "", "profile name")
	production := fs.Bool(verbs.FlagProduction, false, "a production profile")
	credentials := fs.String(verbs.FlagCredentials, "", "ask or vault")
	vaultPath := fs.String(verbs.FlagVaultPath, "", "Vault path of the login")
	mgmt := fs.String("url", "", "management API URL")
	user := fs.String("user", "", "broker user")
	var vhosts stringList
	fs.Var(&vhosts, "vhost", "vhost (repeatable)")
	amqp := fs.String("amqp-url", "", "AMQP URL for stream reads")
	stream := fs.String("stream-url", "", "stream protocol URL")
	tier := fs.String("tier", "", "read or peek")
	if err := verbs.Parse(product.P, fs, args); err != nil {
		return fail(err)
	}
	set := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { set[f.Name] = true })
	if !*nonInteractive && !e.tty {
		return fail(errors.New("profile add asks in a terminal: run it in one, or give --non-interactive and the flags"))
	}

	root := agentwire.ProjectRoot(product.P, e.cwd)
	if *project != "" {
		abs, err := filepath.Abs(*project)
		if err != nil {
			return fail(errors.New("--project: invalid directory"))
		}
		root = abs
	}
	path := kconfig.ProjectConfigPath(product.P, root)
	existing := map[string]config.Profile{}
	if data, err := os.ReadFile(path); err == nil {
		if existing, err = config.ParseProfiles(data, path); err != nil {
			return fail(configError{err})
		}
	}
	// checkName is lockkit's check, which does not quote the name, and
	// the refusal of a name already configured.
	checkName := func(n string, prod bool) error {
		if err := kconfig.CheckProfileName(n, prod); err != nil {
			return err
		}
		if _, dup := existing[n]; dup {
			return errExists
		}
		return nil
	}

	np := newProfile{URL: *mgmt, AMQPURL: *amqp, StreamURL: *stream, VHosts: vhosts, User: *user, Production: *production}
	if *nonInteractive || set[verbs.FlagCredentials] || set[verbs.FlagVaultPath] {
		cred, err := verbs.Credentials(*credentials, *vaultPath)
		if err != nil {
			return fail(err)
		}
		np.Credentials = cred
		if cred == kconfig.CredentialsVault {
			np.VaultPath = *vaultPath
		}
	}
	if set["tier"] {
		t, err := config.ParseTier(*tier)
		if err != nil {
			return fail(errors.New("--tier: want read or peek"))
		}
		np.Tier = t.String()
	}
	pname := *name
	if pname != "" {
		if err := checkName(pname, np.Production); err != nil {
			return fail(fmt.Errorf("--%s: %w", verbs.FlagProfile, err))
		}
	}

	container := e.inContainer()
	out := e.stdout
	if asJSON {
		out = e.stderr
	}
	if *nonInteractive {
		if pname == "" {
			pname = defaultProfile
		}
		if np.URL == "" {
			return fail(errors.New("--url is required with --non-interactive"))
		}
		if np.Tier == "" {
			np.Tier = config.TierRead.String()
		}
	} else {
		in := bufio.NewReader(e.stdin)
		ask := func(prompt, def string) (string, error) {
			fmt.Fprint(out, prompt)
			line, err := in.ReadString('\n')
			if err != nil && (line == "" || !errors.Is(err, io.EOF)) {
				return "", errors.New("no answer")
			}
			if line = strings.TrimSpace(line); line == "" {
				return def, nil
			}
			return line, nil
		}
		if container {
			fmt.Fprintln(out, "In a container, localhost is the container itself: a broker on your machine is "+containerHostGateway+",\na broker in another container is its name on a network shared with the console's container.")
		}
		var err error
		if pname, err = askProfile(ask, container, set, pname, &np, checkName); err != nil {
			return fail(err)
		}
	}
	if err := checkContainer(container, np); err != nil {
		return fail(err)
	}
	if err := checkName(pname, np.Production); err != nil {
		return fail(err)
	}
	var buf bytes.Buffer
	if err := toml.NewEncoder(&buf).Encode(map[string]map[string]newProfile{"profiles": {pname: np}}); err != nil {
		return fail(failedError{err})
	}
	if _, err := config.ParseProfiles(buf.Bytes(), "the new profile"); err != nil {
		return fail(err)
	}
	if err := appendProfile(path, buf.Bytes()); err != nil {
		return fail(failedError{err})
	}
	cred := np.Credentials
	if cred == "" {
		cred = kconfig.CredentialsAsk
	}
	fmt.Fprintf(out, "Profile %s written to %s (tier %s; no secret in the file).\n", pname, path, np.Tier)
	if container {
		fmt.Fprintln(out, "Give .lockmq to the console account (lockmq doctor --fix), then start the console with its hub (lox add lockmq NAME, lox up): see docs/docker.md.")
	} else {
		fmt.Fprintf(out, "Start its console with its hub: lox add lockmq NAME (profile %s, project %s), then lox up\n", pname, root)
		fmt.Fprintln(out, "Wire your agents with: lockmq init <agent>")
	}
	if asJSON {
		_ = verbs.WriteJSON(e.stdout, verbs.Profile{Lock: product.P.Name, Profile: pname, Config: path,
			Production: np.Production, Credentials: cred})
	}
	return exitOK
}

// appendProfile appends the encoded profile to the config file at path.
func appendProfile(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o644)
	if err != nil {
		return err
	}
	_, werr := f.Write(append([]byte("\n"), data...))
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	return werr
}

// containerHostGateway is the host a container reaches its own machine by
// (Docker Desktop, or --add-host=host.docker.internal:host-gateway).
const containerHostGateway = "host.docker.internal"

// inContainer reports a process in a Docker or Podman container.
func (e env) inContainer() bool {
	if e.container != nil {
		return e.container()
	}
	for _, f := range []string{"/.dockerenv", "/run/.containerenv"} {
		if _, err := os.Stat(f); err == nil {
			return true
		}
	}
	return false
}

// checkContainer refuses, in a container, the keychain, which a container
// does not have, and loopback hosts, which are the container itself.
func checkContainer(container bool, np newProfile) error {
	if !container {
		return nil
	}
	if np.Credentials == kconfig.CredentialsKeychain {
		return errors.New("a container has no keychain: use ask or vault")
	}
	for _, u := range []string{np.URL, np.AMQPURL, np.StreamURL} {
		if h := urlHost(u); h != "" && kconfig.IsLoopback(h) {
			return fmt.Errorf("%s is the console's own container: name the broker by %s or by its container name", h, containerHostGateway)
		}
	}
	return nil
}

// askProfile asks for what the flags (set) did not give, into np, and
// returns the profile name (name when --profile gave it). checkName checks
// the name as soon as it is typed. In a container the defaults name the
// machine by containerHostGateway, and the keychain, which a container
// does not have, is not offered. It never asks for the secret.
func askProfile(ask func(prompt, def string) (string, error), container bool, set map[string]bool, name string, np *newProfile, checkName func(string, bool) error) (string, error) {
	var err error
	q := func(flagName string, dst *string, prompt, def string) {
		if err == nil && !set[flagName] {
			*dst, err = ask(prompt, def)
		}
	}
	var vhosts, prod, peek string
	if name == "" {
		q(verbs.FlagProfile, &name, "Profile name ["+defaultProfile+"]: ", defaultProfile)
		if err == nil {
			err = checkName(name, np.Production)
		}
	}
	mgmt, creds := "http://127.0.0.1:15672", "ask, keychain or vault"
	if container {
		mgmt, creds = "http://"+containerHostGateway+":15672", "ask or vault"
	}
	q("url", &np.URL, "Management API URL ["+mgmt+"]: ", mgmt)
	q("amqp-url", &np.AMQPURL, "AMQP URL for stream reads (empty: none), such as amqps://rabbitmq.example.com:5671: ", "")
	q("stream-url", &np.StreamURL, "Stream protocol URL (empty: none), such as rabbitmq-stream+tls://rabbitmq.example.com:5551: ", "")
	q("vhost", &vhosts, "Vhosts, comma-separated (empty: every vhost the account sees): ", "")
	q("user", &np.User, "Broker user (empty: asked when the console starts): ", "")
	if np.Credentials == "" {
		q(verbs.FlagCredentials, &np.Credentials, "Credentials: "+creds+" [ask]: ", kconfig.CredentialsAsk)
		if np.Credentials == kconfig.CredentialsVault {
			q(verbs.FlagVaultPath, &np.VaultPath, "Vault path of the login, such as rabbitmq/creds/lockmq-ro: ", "")
		}
	}
	q(verbs.FlagProduction, &prod, "Production? [y/N] ", "n")
	q("tier", &peek, peekQuestion, "n")
	if err != nil {
		return "", err
	}
	if !set["vhost"] {
		for _, v := range strings.Split(vhosts, ",") {
			if v = strings.TrimSpace(v); v != "" {
				np.VHosts = append(np.VHosts, v)
			}
		}
	}
	if !set[verbs.FlagProduction] {
		np.Production = yes(prod)
	}
	if !set["tier"] {
		np.Tier = config.TierRead.String()
		if yes(peek) {
			np.Tier = config.TierPeek.String()
		}
	}
	return name, nil
}

// urlHost is the host of a URL, or "" when it does not parse.
func urlHost(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return u.Hostname()
}

func yes(s string) bool {
	s = strings.ToLower(strings.TrimSpace(s))
	return s == "y" || s == "yes"
}
