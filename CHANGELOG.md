# Changelog

All notable changes to lockmq. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/); versions follow
[Semantic Versioning](https://semver.org/) (no stability promise before 1.0).

## [Unreleased]

### Added

- `lockmq console --allowed-uids UID,...`: these uids may connect besides the members of
  `lockmq-clients`, as `allowed_uids` of the image's `system.toml` (the `ALLOWED_UIDS` build
  argument) does, for that console only, with the release image unchanged: an agent in a
  container of its own whose uid has no account in the console's image (lea's agents run as
  the host user's uid). Set only by whoever starts the console (the human: the launcher passes it on,
  `lock up` of lockhub v0.2.4 a lock's reviewed `allowed_uids`); root, leading zeros, 65534
  (nobody) and the console's own uid are refused; every isolation check sees the uids (a session or terminal of one is the
  agent's), and the console prints them at start.

### Changed

- The `ALLOWED_UIDS` build argument also refuses 65534 (nobody, the kernel's overflow uid).
- `scripts/hygiene.sh` no longer matches lockkit's `CheckIsolation`.

## [0.4.0] - 2026-10-30

Hub mode on lockkit v0.6.0: expired tickets are approved again, reveal from a local
facade, and a console restarted without the hub withdraws its hub plans.

### Added

- Hub mode renewals (lockkit decision H1): the run of an expired ticket answers `pending`
  with a new ticket that renews it, instead of a `timeout`; the hub's own renew requests are
  served too. The plan records its plan-hash inputs by name (`params`, `queue`). The renewal
  is recomputed under the approved policy without the broker (it may run on the hub's watch
  goroutine): the params are compared, the queue is `unchecked` (never claimed unchanged), and
  the new ticket's run reads the queue and refuses one that moved. Renewals are audited
  (`renew`) with the stored request.
- Hub mode reveal: `lockmq console --facade-keys-file FILE` (written by `lock up`) pins the
  local facades that may reveal a ticket's clear screen (Ctrl-O), audited (`reveal`).

### Changed

- A console started without the hub flags after hub mode withdraws the plans left pending
  and refuses a run naming a `ticket` with `withdrawn`, before anything runs.
- Builds on lockkit v0.6.0 (`ticket.UncheckedInput`, `console.Options.OnHub`: the recompute is
  registered at hub start, before any request; was v0.3.1).
- A closed broker session is no longer the backend's: nothing reads on it.
- `lockmq console`'s usage and `lockmq help` list `--hub-plain-http` (the flag `lock up`
  passes for its `http://lockhub:8443`); `docs/usage.md` says when it is needed.

## [0.3.0] - 2026-10-30

Hub mode: reads and peeks can be decided on lockhub instead of the console's own
prompt.

### Added

- Hub mode (`lockmq console --hub URL --hub-key-file FILE --instance NAME`): a read or peek
  never runs without a decided plan. `op.run` stores it as a plan (lockkit plans: the
  client's uid, the agent's lea token, the policy), pushes a signed ticket with a masked
  screen and answers `pending`; `op.run` with `ticket` (`--ticket`, the MCP tools' `ticket`)
  runs it once decided, prepared again and hashing the same. Decisions are audited with the
  ticket, approver, facade and hub signature; the dry run says `hub`. `lockmq hub-key
  --instance NAME` prints the instance key. Requests carry the agent's lea name and the hash
  of its token (the raw token stays in the agent's process, lockkit#71).
- Hub mode plan hash: the operation, vhost, target and arguments, plus the queue's
  properties read on the operation's own session (type, arguments, policy, operator policy,
  broker major; not its message counts). A queue changed between the approval and the run
  is refused `E_STALE` (retry: rewrite) before anything is read.
- Builds on lockkit v0.3.1 (was v0.2.1). An `http://` hub other than a
  loopback address needs `--hub-plain-http` and must be a service name without a dot or a
  private address; redirects from the hub are refused (lockkit#72).

## [0.2.0]

Docker-only: every command runs in the image through the `lockmq` launcher,
the host install code is gone, and lockmq builds on lockkit v0.2.1. Upgrade
from v0.1.0 with `scripts/install.sh` (it installs the launcher); see
[docs/docker.md](docs/docker.md#migrating). This release also holds the fixes
from the v0.1.0 spec audit (below), never tagged on their own as v0.1.1.

### lockkit v0.2.1

lockmq now builds on lockkit v0.2.1 (was v0.1.0), the version locksql and
lockhttp use. What changes for lockmq:

- **Clear results are refused at desktop security level 1** (the agent's
  account can read the console window): `lockmq console --show-results` stops
  with a configuration error there, and Ctrl-O / `:results` keep the results
  masked with `results stay masked at level 1: the agent's account can read
  this window`. Levels 0 and 2 are unchanged; `--auto` and `--allow-unmask`
  are unchanged.
- **The PII detectors mask more**: Unicode e-mail domains; non-breaking,
  narrow and thin spaces and full-width digits inside phone, IBAN, card and
  national-id values; integral floats in JSON bodies. A value masked before is
  still masked.
### Removed

- The host install (`cmd/lockmq/install.go`) and `internal/separate`: the binary
  refused `install` since the Docker-only launcher. `lockmq install` and `lockmq
  update` in the image still print the hint to the launcher. `doctor --fix`
  keeps its project-access repair (`access.go`), which runs in the image.

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

### Fixes from the v0.1.0 spec audit

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

## [0.1.0] - 2026-10-19

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
