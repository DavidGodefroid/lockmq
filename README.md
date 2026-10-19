<p align="center">
  <img alt="lockmq" src="site/assets/logo.svg" width="96">
</p>

<h1 align="center">lockmq</h1>

<p align="center"><i>Your AI reads the queue. You hold the key.</i></p>

<p align="center">
  <a href="https://github.com/DavidGodefroid/lockmq/actions/workflows/ci.yml"><img alt="CI" src="https://github.com/DavidGodefroid/lockmq/actions/workflows/ci.yml/badge.svg"></a>
  <img alt="Go 1.26+" src="https://img.shields.io/badge/go-1.26%2B-00ADD8?logo=go&logoColor=white">
  <img alt="Linux | macOS" src="https://img.shields.io/badge/platform-linux%20%7C%20macOS-475569">
  <img alt="CGO_ENABLED=0" src="https://img.shields.io/badge/cgo-off-8B5CF6">
  <a href="LICENSE"><img alt="Apache 2.0" src="https://img.shields.io/badge/license-Apache%202.0-14B8A6"></a>
</p>

<p align="center">
  <b>Let Claude Code, Codex, Cursor or Gemini CLI inspect your RabbitMQ brokers<br>
  without ever handing them a password.</b>
</p>

<p align="center">
  <a href="#quick-start">Quick start</a> ·
  <a href="#how-it-works">How it works</a> ·
  <a href="#safety-model">Safety model</a> ·
  <a href="#configuration">Configuration</a> ·
  <a href="docs/usage.md">Usage</a> ·
  <a href="docs/security-model.md">Security model</a>
</p>

---

lockmq puts a **human-held console** between your AI agent and RabbitMQ. The
agent asks; the console, running in a terminal you keep in view under its own
account, holds the broker credentials and the only broker connections, serves
an allowlist of read-only operations, masks personal data in messages and
waits for **your** approval before any message content leaves it. The agent
never sees a credential and never opens a connection.

> [!NOTE]
> **Pre-release** (`v0.1.x`). No stability promise before v1.

## Why lockmq?

"Why does `orders.failed` fill up?" is a good question for an agent, and
answering it means looking at queues, bindings, policies, consumers and, in
the end, a few messages. Handing the agent `rabbitmqctl`, the management API
or an admin password gives it every write too (purge, delete, publish,
shovels), and every message body in clear.

| | Agent with broker credentials | lockmq |
|---|:---:|:---:|
| Agent never sees the broker password | ❌ | ✅ |
| Agent has no broker connection | ❌ | ✅ |
| Only an allowlist of read operations exists | ❌ | ✅ |
| Every message read approved by a human | ❌ | ✅ |
| Message bodies, headers and properties masked | ❌ | ✅ |
| Stream reads never store an offset | ❌ | ✅ |
| Queue peek off by default, never auto-approved, refused near the delivery limit | ❌ | ✅ |
| Policy loosening needs a human confirmation | ❌ | ✅ |
| Append-only audit log (never message content) | ❌ | ✅ |

## Highlights

- **One key holder.** Credentials are typed at console start, or read from the
  OS keychain or HashiCorp Vault (the RabbitMQ secrets engine works as is).
  Never in argv, env, files, logs, the socket or error messages; URI
  passwords are stripped from every answer.
- **Catalogue first, no prompt.** Overview (alarms, versions), queues (paged,
  filtered, sorted), one queue (arguments, DLX, TTL, delivery-limit, effective
  policy, bindings, consumers), exchanges, bindings, policies, consumers,
  connections, channels, stream offsets and consumer lag. Each answer keeps
  only the fields lockmq renders.
- **Messages, approved.** `read` reads a stream from `first`, `last`, an
  offset or a timestamp, through the stream protocol or AMQP 0-9-1, with an
  unnamed consumer: nothing stored, nothing moved. You approve each read.
- **Peek, if you allow it.** Reading a classic or quorum queue requeues the
  messages (`redelivered=true`, the delivery count grows): it exists only on a
  profile you set to tier `peek`, every peek is approved by you, never
  auto-approved, and refused when the quorum queue is near its delivery limit.
- **Masking for messages.** JSON bodies masked per leaf (`redact-unknown` on
  production: every string unless an allow rule names it), headers and
  properties by rule and name heuristics, value detectors (email, phone, IBAN,
  card, national ids), `<redacted:rN.K>` references, binary and compressed
  bodies reduced to their size and hash.
- **The AI tightens, the human loosens.** The agent may add mask rules or
  tighten a profile; any loosening (a tier, a limit, a vhost, a URL) waits for
  your confirmation in the console.
- **Refusals the agent can act on.** Stable codes with a hint and a retry
  rule: rewrite, ask the user, or stop.

## Quick start

```sh
# 1. Install (see Install below for packages, checksums and signatures)
curl -fsSL https://raw.githubusercontent.com/DavidGodefroid/lockmq/main/scripts/install.sh | sh

# 2. Separate the console from the agent (once per machine; prints the root
#    commands and runs them with sudo after you confirm)
lockmq install

# 3. In your project: a broker profile, and your agents
lockmq setup
lockmq init

# 4. In a session of the lockmq account (switch user, Wayland), start the
#    console and keep it open
lockmq console --profile dev --project /path/to/project
```

Then ask your agent: "which queues have no consumer?", "why does
orders.failed fill up?". Catalogue calls answer at once; message reads wait
for your approval in the console. `lockmq doctor` checks the setup.

## How it works

```
 agent account                         lockmq account (its own session)
┌──────────────────────┐   socket    ┌────────────────────────────────────┐
│ Claude Code / Codex  │ ──────────▶ │ lockmq console                     │
│  lockmq mcp / CLI    │ ◀────────── │  policy · allowlist · masking      │
│  (no secret, no      │  JSON-RPC,  │  approval screens · audit log      │
│   broker connection) │  peer uid   │  credentials (prompt/keychain/Vault)│
└──────────────────────┘  checked    └───────────────┬────────────────────┘
                                                     │ management API (GET)
                                                     │ stream protocol / AMQP
                                                     ▼
                                                 RabbitMQ
```

1. The agent calls an MCP tool (`lockmq_queues`, `lockmq_read`, ...) or the
   CLI. Both talk to the console of the profile over a Unix socket the kernel
   restricts to the clients group.
2. The console checks the operation exists, the vhost is served, the tier
   allows it and the arguments are in range, before any broker call.
3. Catalogue operations run at once (audited). `read` and `peek` show an
   approval screen first:

```
╭─ PROD · rabbitmq.example.com:15671 / orders · user lockmq-ro · tier peek ─────
│ requested by uid 1000 (alice) · pid 48211 (claude)
│ agent: see why order events fail in the dead-letter queue
│
│   PEEK orders.failed  count 3
│
│ PII: masked (2 mask rules, 1 allow rules; body redact-unknown; detectors: card, email, iban, phone)
│ ▲ NOT READ-ONLY: messages are taken from the head and requeued; they come back redelivered=true and their delivery count grows by 1
│ ▲ queue: quorum · ready 1204 · unacked 0 (not visible) · consumers 0
│ ▲ delivery-limit 20 · max seen so far 2/20
╰─ Type the profile name "prod" to approve:
```

4. On approval the console reads, masks, caps the output and answers, framed
   as untrusted data. The audit log records who asked what and the decision,
   never message content.

## Protocols

| Protocol | Used for | Profile key |
|---|---|---|
| Management HTTP API | every catalogue operation; `peek` (`get` with `ack_requeue_true`) | `url` (required) |
| Stream protocol | stream reads, first/last offsets, a named consumer's stored offset | `stream_url` |
| AMQP 0-9-1 | stream reads (`x-stream-offset` consumer) when the stream port is not reachable, or through ssh | `amqp_url` |

One broker user for all three. lockmq never calls `/api/definitions`,
`/api/users*`, other users' `/api/permissions*`, any non-GET endpoint but the
`get` of `peek`, `basic.get`, `basic.publish`, a declare, a delete, a purge or
`store_offset`.

## Safety model

The console is the enforcement point; skills, rules and tool descriptions
only guide the agent. The full model, its threats and its limits are in
[docs/security-model.md](docs/security-model.md). In short:

- **Separate account.** `lockmq install` creates the console account, the
  clients group and `/etc/lockmq/system.toml`; the console refuses to start
  without them.
- **Allowlist.** What is not in the catalogue does not exist.
- **Read-only by default.** Tier `read` reads metadata and streams, and moves
  nothing. Tier `peek` is not strictly read-only and is labelled as such
  everywhere.
- **Broker user.** Use a user tagged `monitoring` with `configure "^$"` and
  `write "^$"`. RabbitMQ lets the `read` permission **purge** a queue: grant
  `read` on the streams only, and on queues only for profiles with tier
  `peek`. At start the console refuses an `administrator` or `policymaker`
  user on production.

### Peek (tier `peek`)

The management `get` takes up to `count` ready messages from the head and
requeues them in the same call. Consequences, all on the approval screen:
redelivered messages, a delivery count that grows on quorum queues (over the
limit, the message is dead-lettered or dropped), a possible change of order
for active consumers, and only ready messages at the head are visible. The
console reads the effective delivery limit (argument, policy, operator
policy; the lowest; RabbitMQ 4.x's default 20) and refuses a peek within
`delivery_headroom` of it, from the highest count it saw this session.

## Configuration

`.lockmq/config.toml` in the project (or `~/.config/lockmq/config.toml`). No
file holds a secret.

```toml
[profiles.prod]
url          = "https://rabbitmq.example.com:15671"   # management API
amqp_url     = "amqps://rabbitmq.example.com:5671"    # optional
stream_url   = "rabbitmq-stream+tls://rabbitmq.example.com:5551"  # optional
vhosts       = ["orders"]                              # empty: every vhost the account sees
user         = ""                                      # empty: asked at console start
credentials  = "vault"                                 # ask | keychain | vault
vault_path   = "rabbitmq/creds/lockmq-ro"
tls          = "verify-full"
tier         = "read"                                  # read | peek
production   = true
detectors    = ["email", "phone", "iban", "card"]
body_mode    = "redact-unknown"                        # redact-unknown | detect

[profiles.prod.limits]
request_timeout   = "10s"
read_max          = 20      # messages per stream read (ceiling 200)
peek_max          = 5       # messages per peek (ceiling 20)
delivery_headroom = 3
max_body_bytes    = 16384
max_output_bytes  = 65536
list_page_size    = 100
idle_timeout      = "20m"
max_session       = "4h"
```

An `[profiles.NAME.ssh]` table reaches a broker behind a bastion (the console
opens the tunnel; streams are then read over AMQP).

### PII rules

`.lockmq/pii.toml`:

```toml
[[mask]]
queue = ["orders/order-events"]     # vhost/name globs; "//name" for the vhost /
body  = "$.customer.email"          # JSON path: $.a.b, $.a[*].b, $.a[3]
mode  = "email"                     # redact (default) | partial | email

[[mask]]
queue  = ["*/*"]
header = "x-customer-*"

[[allow]]
queue = ["orders/order-events", "orders/order-events.dlq"]
body  = "$.items[*].sku"
```

Mask rules of the queue read, of the origin queue in a message's first
`x-death` and of its exchange apply; allow rules only from the queue read, so
a forged `x-death` can only add masking. The agent may add a mask rule
(`lockmq pii add`); an allow rule is a loosening the human confirms.

## Limitations

- No writes of any kind in v0.1 (purge, move, requeue, publish, shovel).
- JSON and UTF-8 text bodies only; other bodies are shown as size and hash.
- Masking is best effort: a value in a free-text field that no detector,
  rule or name heuristic catches comes back in clear.
- A `monitoring` user cannot read runtime parameters (shovels, federation)
  nor its own permissions: `policies` says so; permissions are not audited.
- Through an ssh tunnel the stream protocol is unavailable (its client dials
  the broker's advertised address): streams are read over AMQP, and stream
  offsets and consumer lag are not available.
- The first peek of a queue cannot know the delivery counts (the count is
  only seen after a `get`); the screen says so.
- No console in a container yet (locksql's Docker mode): a follow-up.

## Documentation

- [docs/usage.md](docs/usage.md): every command, the console, the MCP tools.
- [docs/security-model.md](docs/security-model.md): threats, boundaries, limits.
- [docs/docker.md](docs/docker.md): brokers in containers; the console in a container (follow-up).
- [CHANGELOG.md](CHANGELOG.md)

## Install

```sh
curl -fsSL https://raw.githubusercontent.com/DavidGodefroid/lockmq/main/scripts/install.sh | sh
```

Better: download it, read it, then run it. It checks the archive against
`checksums.txt` and, when `cosign` is on `PATH`, the keyless signature of
`checksums.txt`. Releases also ship `.deb`, `.rpm` and `.apk` packages and an
SPDX SBOM per archive. Linux and macOS, amd64 and arm64.

## Build

```sh
CGO_ENABLED=0 go build ./cmd/lockmq
go test ./...
go test -tags integration ./test/integration/   # RabbitMQ 3.13 and 4.1 in Docker
```

## Licence

Apache License 2.0. See [LICENSE](LICENSE) and [NOTICE](NOTICE).
