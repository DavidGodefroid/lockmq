# Changelog

All notable changes to lockmq. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/); versions follow
[Semantic Versioning](https://semver.org/) (no stability promise before 1.0).

## [Unreleased]

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
