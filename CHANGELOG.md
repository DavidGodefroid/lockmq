# Changelog

All notable changes to lockmq. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/); versions follow
[Semantic Versioning](https://semver.org/) (no stability promise before 1.0).

## [Unreleased]

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
