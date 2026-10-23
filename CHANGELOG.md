# Changelog

All notable changes to lockmq. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/); versions follow
[Semantic Versioning](https://semver.org/) (no stability promise before 1.0).

## [Unreleased]

### Docker image

Docker is now the default and only documented way to run lockmq, at parity
with locksql.

- A multi-stage `Dockerfile` (base images pinned by digest): a static
  build, then the console image with the separated setup of `lockmq
  install` baked in: `lockmq` (uid 10011) runs the console, `agent` (uid
  10012, in `lockmq-clients`, gid 10010) runs the MCP server and the CLI,
  root-owns the binary and `/etc/lockmq/system.toml`, no `sudo`, and
  `/run/lockmq` (2710) and `/home/lockmq` as volumes. `--build-arg
  ALLOWED_UIDS=` names the uid of an agent in a container of its own; a
  `vault` target adds the Vault CLI checked against pinned sums.
- `lockmq setup` knows when it runs in a container: the management URL
  defaults to `host.docker.internal`, `localhost`, `127.0.0.1` and `::1`
  are refused, and the keychain is not offered.
- `lockmq doctor` checks the project's `.lockmq` (owned by the console
  account, by uid, mode 0755, its files readable) and `doctor --fix
  [--yes]` repairs it as root, without `sudo` when it already is root
  (`docker run --user 0 … lockmq doctor --fix --yes`). The repair enters the
  directory and changes `.`, so a symbolic link swapped in by the agent's
  account redirects nothing; a file is made readable only once the
  directory belongs to the console account and no other account may write
  it; a `.lockmq` that is a link is reported only.
- docs/docker.md is the setup guide (build, project, console, Vault, agent
  wiring, agent in its own container, broker in a container, what Docker
  does and does not separate). The README, docs/usage.md and the site
  describe only the Docker setup; the other deployment modes stay in the
  code, no longer documented.
- CI builds both targets, checks `ALLOWED_UIDS`, and runs an end-to-end
  test of the guide against RabbitMQ (`test/docker/e2e.py`).

### v0.1.1

Fixes from the v0.1.0 spec audit.

- Security: a body without content type that looks like JSON but does not
  parse is opaque under `redact-unknown` (it was masked as text, detectors
  only), and a body the broker cut short on peek is always opaque: names and
  addresses of a large dead-lettered message no longer come back in clear.
- Security: a vhost or name that is, or holds, a `.` or `..` path segment is
  refused before any request (`E_UNSAFE_NAME`) and in the config, so a
  management path cannot be normalised outside the endpoint allowlist.
- `limits.delivery_headroom = 0` is refused (a peek could dead-letter a
  message); the peek guard never goes below 1.
- Ctrl-O / `:results` prints results in clear in the console, and
  `--unmask` (MCP `unmask`) returns them unmasked when the human allowed it
  with `:unmask`: approved every time, audited `"unmasked":true`; refused
  otherwise (`E_UNMASK_OFF`). Both were documented but did nothing.
- `status` shows the profile's tier.
- Stream offsets and lag say they are precise to one chunk (`precision`).
- Connection and channel user names and client properties go through the
  detectors.
- `queue` reads its own consumers instead of the whole vhost's.

### v0.1.0

First release.

- Human-held console on lockkit v0.1.0: separate account (`lockmq install`),
  approval screens, typed profile name on production, policy review (the AI
  tightens, the human loosens), audit log without message content.
- Catalogue over the RabbitMQ management API, an allowlist of read-only
  operations: overview, queues, queue, exchanges, bindings, policies,
  consumers, connections, channels; stream metadata and a named consumer's
  lag over the stream protocol.
- `read`: stream messages from first, last, an offset or a timestamp, over
  the stream protocol or AMQP 0-9-1, with an unnamed consumer; approved by
  the human.
- `peek` (tier `peek` only, off by default): the first messages of a classic
  or quorum queue, requeued; never auto-approved; refused near a quorum
  queue's delivery limit.
- Message masking: JSON paths, header and property rules, `redact-unknown`
  and `detect` body modes, value detectors, redacted references, opaque
  binary and compressed bodies; rules in `.lockmq/pii.toml`.
- CLI and MCP server (17 tools), `setup`, `doctor`, `init` for Claude Code,
  Codex, Cursor and Gemini CLI.
- Integration suite on RabbitMQ 3.13 and 4.1.
