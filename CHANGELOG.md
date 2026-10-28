# Changelog

All notable changes to lockmq. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/); versions follow
[Semantic Versioning](https://semver.org/) (no stability promise before 1.0).

## [Unreleased]

### One `lockmq` command, backed by Docker

Every lockmq command now runs in Docker, through one host command: the
`lockmq` launcher.

- `scripts/lockmq`, a POSIX shell launcher, runs each command in
  `ghcr.io/davidgodefroid/lockmq` with the account and volumes it needs:
  `console` as `lockmq` in a container of its own per project
  (`lockmq-console-<n>`), confined (no capability, `no-new-privileges`,
  read-only root with a small `/tmp`, 256 processes, 1 GB), with no log
  driver, `host.docker.internal` mapped, `VAULT_ADDR`/`VAULT_NAMESPACE`
  passed and `VAULT_CACERT`'s file mounted read-only, volumes `lockmq-run`
  and `lockmq-home`, `--project` added;
  `init` as you with your home; `setup` as you; `doctor --fix` as root;
  everything else, `mcp` included, as `agent` with `lockmq-run`. It mounts
  the project (the nearest directory up holding `.lockmq/`) at its own
  path (a path with `:` is refused) and never mounts the Docker socket.
  `LOCKMQ_IMAGE`, `LOCKMQ_VAULT=1`, `LOCKMQ_DOCKER_ARGS` (never globbed),
  `LOCKMQ_READONLY_PROJECT=1` and `LOCKMQ_CONSOLE_NAME` adjust it.
- `scripts/install.sh` installs the launcher, not the binary, after
  checking the cosign signature of `checksums.txt` against the release
  workflow run for that very tag (a signature of another release does not
  verify; only a `vX.Y.Z` tag is accepted), the archive's checksum and the images' signatures;
  cosign is now required (`LOCKMQ_SKIP_COSIGN=1` is the explicit way past
  it, and replaces `LOCKMQ_REQUIRE_COSIGN`). The launcher runs its
  release's image by the verified digest.
- `lockmq version` prints the launcher's and the image's versions;
  `lockmq update [vX.Y.Z]` replaces the launcher the same way, then pulls
  its image.
- The release workflow builds the image (and its `-vault` variant) for
  amd64 and arm64, publishes it to ghcr.io and signs it with cosign. The
  archives ship the launcher; the `.deb`, `.rpm` and `.apk` packages are
  gone.
- The binary refuses `install` and `update` ("lockmq runs in Docker").
  `lockmq init` writes `lockmq mcp`, which now works as is.

**Breaking, migration:** `install.sh` installs the launcher in place of the
binary, and releases no longer ship packages. Replace the `docker run …
lockmq …` commands and `sh -c 'exec docker run … lockmq mcp'` agent entries
of the former guide with `lockmq …` (the old ones keep working). A host
install (`sudo lockmq install`) leaves the `lockmq` account and group,
`/etc/lockmq/` and the socket directory behind: see
[docs/docker.md](docs/docker.md#migrating).

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
  `vault` target adds the Vault CLI checked against pinned sums. The build
  refuses root (0) and the console's uid (10011) in `ALLOWED_UIDS`.
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
  it, and only a regular file of its checked, non-root owner with one link;
  a `pii.toml` the console account does not own is replaced by a copy it
  owns (a new inode: a descriptor held open on the old file writes nowhere). A `.lockmq` that is a link,
  and a directory or file owned by root, are reported only.
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
