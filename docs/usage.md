# lockmq usage

Every command, the console and the MCP tools. For the why, see the
[README](../README.md); for the boundaries, the
[security model](security-model.md).

Every command below runs in the Docker image, through the `lockmq`
launcher ([docs/docker.md](docker.md)): it runs each one with the account
and the volumes it needs. `lockmq version` prints the launcher's version and
the image's; `lockmq update [vX.Y.Z]` replaces the launcher with a release's
(cosign signature and checksum checked) and pulls its image.

## Profiles

```sh
lockmq setup [--project DIR]
```

Asks for one profile and appends it to the project's `.lockmq/config.toml`:
the management API URL, the optional AMQP and stream URLs, the vhosts, the
user, the credentials mode (`ask` or `vault`; a container has no keychain),
production or not, and:

```
Allow reading messages of classic and quorum queues? Not strictly read-only:
messages are requeued and marked redelivered, and may count toward a quorum
queue's delivery limit. [y/N]
```

The default is no: `tier = "read"`. The profile is validated before it is
written (a URL with a password is refused). In a container the management
URL defaults to `http://host.docker.internal:15672`, and `localhost`,
`127.0.0.1` and `::1`, the container itself, are refused.

Every key is described in the README's Configuration section. Changing the
file later: tightenings (a lower limit, `peek` → `read`, a removed vhost, a
new mask rule) apply at once; loosenings wait for you in the console
(`:review`).

## Agents

```sh
lockmq init [claude|codex|cursor|gemini ...]
```

Writes the project's agent files (default: the agents found on the
machine): the MCP server entry (`lockmq mcp`), the Claude Code skill, the
`AGENTS.md` / `GEMINI.md` section, the Cursor rule, and an example
`.lockmq/config.toml` when there is none. Claude Code may call the catalogue
tools and `lockmq status` without a prompt; `lockmq_read`, `lockmq_peek` and
`lockmq_pii_add` always ask.

## The console

```sh
lockmq console [--profile P] [--project DIR] [--auto] [--allow-unmask] [--show-results]
              [--hub URL [--hub-plain-http] --hub-key-file FILE --instance NAME]
```

The launcher runs it in the console container, as `lockmq` ([docs/docker.md](docker.md#run-the-console)).
In order: the isolation checks,
the policy review (first start: describe and approve; later: tightenings
apply, loosenings are asked), the production banner and the typed profile
name, the user and the password (or Vault), the connection
(management API, then the stream readers), the privilege audit of the broker
user (`administrator` or `policymaker`: refused on production, typed
`continue` elsewhere), then the socket.

Keys and commands while it runs (lockkit's console):

| Key / command | Does |
|---|---|
| `:review` | show the pending loosening and the change requests of clients, apply or refuse |
| `:status` | the policy in force, the session |
| Shift+Tab, `:mode` | auto mode on/off (never on production; never for `peek`) |
| Ctrl-O, `:results` | print each read and peek result in clear in the console (the agent still gets it masked) |
| `:unmask` | let clients ask for unmasked output (`--unmask`), each such request approved here, never auto-approved |
| `:quit`, Ctrl-C | end the session |

**Hub mode.** With `--hub URL --hub-key-file FILE --instance NAME` (the three go together, never with
`--auto`; `lock up` writes them), reads and peeks are decided on lockhub, never on this
terminal; the policy, `:review` and the catalogue stay here. `op.run` (`lockmq read`,
`lockmq peek`, the MCP tools) never runs a read or peek without a decided plan: it stores
the request as a plan bound to the client's uid, the agent's lea token and the policy, pushes
a signed ticket with the screen masked, and answers `pending` with the ticket and the
instance. The same call with `ticket` (`--ticket`) runs it once the hub approved it: the
decision is verified (pinned hub key, this instance, the ticket, still usable, nonce once),
the request is prepared again under the policy in force and must hash the same: the plan hash
covers the operation, vhost, target and arguments, and the queue's properties read on the
session the operation uses (type, arguments, policy and operator policy, broker major; not its
message counts), so a queue changed after the approval is refused `E_STALE` before anything is
read; the hub is told it was consumed. A denial is `denied`; an expired ticket is asked again (below). Decisions are
audited (`approval: hub`, `ticket`, `approver`, `facade`, `hub_sig`). The dry run's verdict
is `hub`. `lockmq hub-key --instance NAME` prints the instance key for `lock up` to pin
(made once in the console account's home, `0600`).
`lock up` also passes `--hub-plain-http` for its `http://lockhub:8443` on the hub's own network:
without it an `http://` hub URL other than loopback is refused, and with it the URL must still
be a service name without a dot or a private address.

**Supported lockhub versions.** Which [lockhub](https://github.com/DavidGodefroid/lockhub) releases each
lockmq release works with under `lock up`:

| lockmq | lockkit | lockhub |
|---|---|---|
| v0.4.0 | v0.6.0 | v0.2.0 or later (tested through v0.2.2): hub mode, `devices: true` (paired phones), reveals (`reveal: true`, Ctrl-O) and renewals |
| v0.3.0 | v0.3.1 | v0.1.1 or later: hub mode only; leave `devices` and `reveal` off |
| v0.2.0 and older | v0.2.1 | none (no hub mode) |

lockhub v0.1.0 is not supported: its generated command lacks `--hub-plain-http`, which these
releases need. A lock older than a feature `lockfamily.yaml` asks of it refuses the flag and does
not start: leave `devices`/`reveal` off for it. lockhub's
[CHANGELOG](https://github.com/DavidGodefroid/lockhub/blob/main/CHANGELOG.md) ("Supported locks")
is the reference; this table follows it.

The session ends after `idle_timeout` without a request, after
`max_session`, on `lockmq logout` or when a loosening changes where the
broker is.

**Hub mode: renewal, reveal, rollback** (lockkit v0.5.0+). The run of an expired ticket (an
approval this console cannot use any more) does not fail: the ticket is asked again on the
hub, or the human asks it again there, and the call answers `pending` with a new ticket,
which renews the old one; run the new ticket once decided. The renewal is re-checked under the
approved policy (a changed policy is not renewed: `timeout`, plan again) without touching the
broker: the request's params are compared, the queue is shown `unchecked` (names only, never
values), so a renewal is never a one-click "unchanged" one. The run of the new ticket reads the
queue again and refuses one that moved `E_STALE`, as any hub run. Renewals are audited
(`event: renew`, with the stored request). `lock up` also passes `--facade-keys-file FILE`,
the local facades whose Ctrl-O may reveal a ticket's clear screen, sealed to them (audited
`event: reveal`). A console started again without the hub flags withdraws the plans its hub
mode left pending and refuses a run that names a `ticket` with `withdrawn` (plan again; the
human approves on this terminal): `ticket` is a hub-mode param only.

## Client commands

Every client command takes `--profile P` (optional when one profile is
configured) and `--json`. Exit codes: 0 ok, 1 refused, denied or failed, 2 no
console (the start command is printed), 3 usage or configuration error.

| Command | Does |
|---|---|
| `lockmq status` | the console of each profile: running, production, tier (of a running console, from the approved policy), broker version |
| `lockmq overview` | broker version, nodes and their alarms, feature flags, totals |
| `lockmq queues [--vhost V] [--filter GLOB] [--sort K] [--page N]` | one page of queues; sort by `name`, `messages`, `messages_ready`, `messages_unacknowledged`, `consumers` |
| `lockmq queue NAME [--vhost V]` | one queue with its arguments, policy, bindings and its own consumers |
| `lockmq exchanges [--vhost V] [--page N]` | one page of exchanges |
| `lockmq bindings [--vhost V] [--queue Q \| --exchange E]` | bindings of a vhost, a queue, or an exchange (both directions) |
| `lockmq policies [--vhost V]` | policies, operator policies, runtime parameters (URI passwords removed) |
| `lockmq consumers [--vhost V]` | consumers with prefetch and ack mode (one unpaginated call: on a very large vhost use `queue NAME`) |
| `lockmq connections [--page N]`, `lockmq channels [--page N]` | on the served vhosts only; user and connection names through the detectors |
| `lockmq stream NAME [--vhost V]` | a stream's queue data and first/last offsets |
| `lockmq stream-offset NAME --consumer REF [--vhost V]` | a named consumer's stored offset and its lag |
| `lockmq read STREAM --from first\|last\|N\|TIMESTAMP [--max N] [--intent TEXT] [--unmask] [--dry-run]` | stream messages, masked; approved in the console |
| `lockmq peek QUEUE [--count N] [--intent TEXT] [--unmask] [--dry-run]` | NOT read-only; tier `peek` only; approved every time |
| `lockmq pii list`, `lockmq pii add "RULE"` | the mask and allow rules; add a mask rule |
| `lockmq request "CHANGE"` | queue a change for the human (`tier=peek`, `limits.read_max=50`) |
| `lockmq logout` | end the console's session |

`--dry-run` asks the console for its verdict and the approval screen without
asking you anything and without reading a message: `run`, `auto`, `prompt`,
`prompt-every-time` or `refused` with its code.

A mask rule's text: `queue=VHOST/GLOB` (or `exchange=`), then one of
`body=$.json.path`, `header=NAME-GLOB` or `property=NAME`, e.g.
`queue=orders/* body=$.customer.email`. The vhost `/` is written `//name`.

The broker reports stream positions per chunk: `last_offset` is the first
offset of the stream's last committed chunk, so the stream can hold up to one
chunk more and a lag can be that much higher (`precision` in the output).

`--unmask` asks for the messages in clear. It is refused (`E_UNMASK_OFF`)
unless you allowed it in the console (`:unmask` or `--allow-unmask`); allowed,
the request shows `PII: UNMASKED` on its screen, is approved every time, even
in auto mode, and its audit record says `"unmasked":true`.

`--intent` is one line telling you why; it is shown on the approval screen
and kept in the audit log.

## MCP tools

`lockmq mcp [--profile P]` serves the same operations to the agent on stdio:
`lockmq_status`, `lockmq_overview`, `lockmq_queues`, `lockmq_queue`,
`lockmq_exchanges`, `lockmq_bindings`, `lockmq_policies`,
`lockmq_consumers`, `lockmq_connections`, `lockmq_channels`,
`lockmq_stream`, `lockmq_stream_offset`, `lockmq_read`, `lockmq_peek`,
`lockmq_pii_list`, `lockmq_pii_add`, `lockmq_request_change`. Results start
with an untrusted-data line; refusals come back as tool errors with a
structured `refusal` (`code`, `retry`, `hint`). While you decide, the server
sends progress notifications so the client does not time out.

## Refusal codes

| Code | Retry | Means |
|---|---|---|
| `E_BAD_ARGS` | rewrite | an argument is missing, out of range or unknown |
| `E_WRONG_QUEUE_TYPE` | rewrite | `read` on a queue or `peek` on a stream |
| `E_VHOST_NOT_SERVED` | ask the user | the profile does not serve that vhost |
| `E_PROTOCOL_NOT_CONFIGURED` | ask the user | set `stream_url` or `amqp_url` |
| `E_TIER` | ask the user | `peek` on a profile with tier `read` |
| `E_DELIVERY_LIMIT` | ask the user | a peek could dead-letter or drop messages of this quorum queue |
| `E_UNMASK_OFF` | ask the user | `--unmask` while unmasked output is off in the console |
| `E_STALE` | rewrite | hub mode: the queue changed between the approval and the run; nothing was read: ask again |
| `E_UNSAFE_NAME` | stop | a vhost or name that is, or holds, a `.` or `..` path segment |
| `E_POLICY` | stop | anything else the policy refuses |

## Doctor

```sh
lockmq doctor [--profile P] [--fix [--yes]]
```

Checks the config, the separated setup, the project's `.lockmq` (owned by
the console account, mode 0755, `pii.toml` owned by the console account,
the files readable), each profile (tier
first; `peek` flagged NOT READ-ONLY), whether its console runs (with its
start command), and the agent wiring.

`--fix` repairs `.lockmq` as root: the launcher runs `lockmq doctor --fix`
as root in the image. The repair enters the directory and never follows a symbolic link;
a `.lockmq` that is itself a link, a directory or file owned by root and a
file with more than one link are reported, not repaired. `--yes` skips
the question.

## Audit log

`~/.local/state/lockmq/audit.log` of the console account (mode 0600): one
JSON line per event (`login`, `policy`, `catalog`, `refused`, `approved`,
`denied`, `auto`, `timeout`, `run`, `logout`) with the operation, vhost,
target and message count, the agent's intent and the client's uid. Never a
message body, header or property value.
