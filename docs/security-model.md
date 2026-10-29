# lockmq security model

lockmq lets an AI agent inspect a RabbitMQ broker without holding its
credentials or connecting to it. This document says what it protects, how,
and where it stops.

## Principles

1. **Two processes, one key holder.** The console holds the credentials and
   the only broker connections; the clients (CLI, MCP server) hold neither.
2. **The console is the enforcement point.** Skills, rules and tool
   descriptions only guide the agent.
3. **The AI tightens, the human loosens.** Any change that widens what the
   agent can do takes effect only after the human confirms it in the console.
4. **Fail closed.** An unknown operation, field, body format or rule is
   refused or treated as the most restrictive case.
5. **Defence in depth.** The broker user's own permissions back the
   allowlist where RabbitMQ allows it.
6. **Results are data, not instructions.** Everything read from the broker is
   framed as untrusted.
7. **Reading a message is not always free.** An operation that can change
   what a consumer receives (`peek`) needs its own tier, set by the human, and
   a human approval every time, with its side effects on the screen.

## Boundaries

| Boundary | Enforced by |
|---|---|
| Agent ↔ console | separate OS account (the image's `lockmq` account; the agent runs as `agent`); a Unix socket in a directory only the clients group reaches; the kernel's peer credentials checked on every connection; the console refuses to start without the separated setup |
| Console ↔ human | approval screens in the console's own terminal; on production the typed profile name; 5-minute timeout = denied. In hub mode the screen goes masked in a ticket signed by the instance key (kept in the console account's home); a read or peek runs only on a decision verified under the pinned hub key, for this instance and the ticket, under the same policy and request hash, once (nonce), for the agent token that asked |
| Console ↔ broker | an allowlist of operations and endpoints; one broker user; TLS `verify-full` by default; optional ssh tunnel opened by the console |
| Policy | config files read every 2 s; loosenings held until confirmed; the approved policy stored in the console account's state |

## What lockmq sends to the broker

Only the operations of the catalogue exist:

- management API: `GET` on overview, nodes, feature flags, queues, one
  queue and its bindings, exchanges, bindings, policies, operator policies,
  consumers, connections, channels, whoami, runtime parameters; plus, under
  tier `peek` only, `POST /api/queues/{vhost}/{name}/get` with
  `ackmode = "ack_requeue_true"`;
- stream protocol: an unnamed consumer (no stored offset, no
  single-active-consumer group), metadata and `QueryOffset`;
- AMQP 0-9-1: `basic.qos`, `basic.consume` with `x-stream-offset` on a
  stream, manual acks (on a stream an ack only returns credit),
  `basic.cancel`.

Never: `/api/definitions`, `/api/users*` (password hashes), other users'
`/api/permissions*`, any other non-GET endpoint, `basic.get`,
`basic.publish`, a declare, a delete, a purge, `store_offset`,
`ack_requeue_false` or `reject_requeue_false`. Every management URL path
segment is escaped (a queue `a/b` on vhost `/` is `/api/queues/%2F/a%2Fb`),
and a vhost or name that is, or holds between slashes, a `.` or `..` segment
is refused before any request (`E_UNSAFE_NAME`; in the config, at load): an
HTTP server or proxy that removes dot segments would otherwise route
`/api/queues/../definitions` outside the allowlist.

## The broker user

Recommended: tag `monitoring` (metadata of every vhost), permissions
`configure "^$"`, `write "^$"`, and `read` on what lockmq may read.

- With `configure` and `write` empty, the broker refuses publish, declare,
  delete and policy changes from that user (tested on RabbitMQ 3.13 and 4.1).
- **The `read` permission also allows a purge.** RabbitMQ authorises
  `queue.purge` (and the management API's `DELETE .../contents`) with `read`
  alone. lockmq never purges, but the broker user cannot back that up: grant
  `read` on the streams only (`^(order-events|payment-events)$`), and on
  queues only for profiles with tier `peek`.
- A `monitoring` user cannot read runtime parameters nor its own
  permissions (HTTP 401): `policies` says the parameters are not visible, and
  the console's privilege audit relies on the user's tags only.
- At start (and at each reconnection) the console reads `whoami`: a user
  tagged `administrator` or `policymaker` is refused on production and needs
  a typed `continue` elsewhere.

## Credentials

Asked at console start (echo off), read from the console account's OS
keychain, or read from Vault at every connection (the console never handles
the Vault token; a lease is renewed by reconnecting before it ends). They
never appear in argv, the environment, a file, the socket, the audit log or
an error: errors are generic (`mgmt: HTTP 401`), URIs are built without
userinfo (AMQP and stream logins go through SASL), and every error text that
leaves the console is scrubbed of known secrets, URI userinfo and
`Authorization` headers. URIs inside runtime parameters (shovels,
federation) lose their userinfo before they reach a client.

## Message content

- **Approval.** Every `read` and `peek` shows a screen: the queue or stream,
  the starting point and count, what moves (nothing, for a stream read), the
  masking in force, the agent's intent. Auto mode (non-production only)
  approves `read`; never `peek`.
- **Masking.** JSON bodies are parsed and masked per leaf: `redact-unknown`
  (the production default) redacts every string no allow rule names;
  `detect` uses mask rules, field-name heuristics and value detectors.
  Headers and properties follow rules and heuristics; `x-death` keeps its
  structure with routing keys through the detectors. Bodies that are binary,
  compressed, invalid JSON, deeper than 64 levels or larger than four times
  `max_body_bytes` become `<bytes:N sha256:H type:T>`; so does a body without
  content type that looks like JSON but does not parse, under
  `redact-unknown` (under `detect` it is text, through the detectors), and
  any body the broker cut short (a peek asks for at most four times
  `max_body_bytes`): `type:…+truncated`. Properties (`message_id`,
  `correlation_id`, …) follow rules, the name heuristic and the detectors,
  not `redact-unknown`. Redacted values become
  `<redacted:rN.K>` references (same value, same reference within a result);
  the values stay in the console's memory.
- **Dead-letter queues.** Mask rules of the queue read, of the origin queue
  in a message's first `x-death` and of its exchange apply; allow rules only
  from the queue read: a forged header can only add masking.
- **Clear output.** With results on (Ctrl-O, `--show-results`) the console
  prints each read and peek result in clear in its own terminal; the agent
  still gets it masked. At desktop security level 1 (the agent's account can
  read the console window) results stay masked: `--show-results` is refused
  and Ctrl-O keeps them masked. Unmasked output to a client (`--unmask`) is refused
  unless the human allowed it in the console (`:unmask`,
  `--allow-unmask`); each such request shows `PII: UNMASKED`, is approved
  every time, never by auto mode, and is audited `"unmasked":true`.
- **Free text in the catalogue.** Connection and channel user names and
  client properties (a connection name) go through the profile's detectors.
- **Size.** At most `read_max` (≤ 200) or `peek_max` (≤ 20) messages, each
  body cut at `max_body_bytes`, the answer cut at `max_output_bytes`
  (messages dropped from the end and counted).

## Peek

`peek` is the one operation that changes delivery state. It is off unless
the profile's tier is `peek` (setting it is a loosening the human confirms;
`lockmq setup` asks with the warning, default no). When on:

- the screen says NOT READ-ONLY, shows the queue's type, ready and unacked
  counts, consumers and delivery limit;
- every peek is approved by the human, never by auto mode;
- on a quorum queue the console computes the effective delivery limit (the
  lowest of the `x-delivery-limit` argument, the policy and the operator
  policy; RabbitMQ 4.x's default 20 when none is set, none on 3.x) and keeps
  the highest `x-delivery-count` it saw per queue this session. It refuses a
  peek when `limit - highest seen <= delivery_headroom` or when the limit
  itself is at or under the headroom. Each requeue raises the count by one,
  and the message leaves the queue on the requeue that takes it over the
  limit (verified on 3.13 and 4.1): the next get shows `seen + 1` and its
  requeue makes it `seen + 2`, so `delivery_headroom` is at least 1 (0 is
  refused at load) and the last peek allowed is at `seen = limit - 2`;
- the first peek of a queue is blind on the count (it is only known after a
  `get`); the screen says so.

## Threats

| Threat | Mitigation |
|---|---|
| Agent changes broker state | operation allowlist; GET only plus the `get` of peek; no publish, declare, purge, store_offset or destructive ack mode; broker user with empty configure/write |
| Peek dead-letters or drops a message | off by default; tier per profile set by the human; delivery limit read first, refusal near it; side effects on the screen; never auto-approved |
| Agent turns peek on | `read` → `peek` is a loosening: no effect until confirmed in the console; no IPC method loosens |
| A read moves a real consumer | unnamed stream consumer; never `store_offset` |
| Password hashes or URI passwords leak | those endpoints never called; userinfo stripped from every URI |
| PII in bodies of unknown shape | `redact-unknown` on production; detectors; proposals of mask rules |
| Prompt injection in payloads | results framed as untrusted data |
| Endpoint allowlist escaped through `.`/`..` path segments | such names refused before any request, and in the config |
| Broker overload | paginated lists with selected columns, one request at a time, `request_timeout`, `read_max`; `queue` reads its own consumers, not the vhost's |
| Agent reads the console's terminal or types into it | separate account and session; same-user mode refused |

## Limits

- Masking is best effort: a personal value in a free-text field that no
  rule, heuristic or detector catches comes back in clear. Use
  `redact-unknown` and allow rules on production.
- Purge is allowed by RabbitMQ to any user with `read` on the queue: the
  allowlist is the only barrier (see "The broker user").
- The approval screen can only show what the broker reports before the read:
  unacked messages are invisible to a peek, and a peek cannot know the next
  message's delivery count before taking it.
- Through an ssh tunnel streams are read over AMQP; offsets and lag need the
  stream protocol and are unavailable. The stream protocol reports chunk ids:
  the last offset and a lag are precise to one chunk.
- `consumers` and vhost-wide `bindings` are single unpaginated management
  calls (the API does not page them): on a very large vhost they can exceed
  the response cap; use `queue NAME` or `bindings --queue/--exchange`.
- The audit log is append-only by convention (`chattr +a` on Linux makes it
  so for the console account too); there is no hash chain.
