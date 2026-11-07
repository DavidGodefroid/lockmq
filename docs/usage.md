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
lockmq profile add [--non-interactive] [--json] [--project DIR] [--profile NAME] [--production]
                   [--credentials ask|vault [--vault-path P]] [--url URL] [--user USER] [--vhost V]...
                   [--amqp-url URL] [--stream-url URL] [--tier read|peek]
```

Adds one profile to the project's `.lockmq/config.toml`. No form of it
takes, asks for or writes a secret: the console asks for the password at
its start (`ask`, the default) or reads it from Vault (`vault`, at
`--vault-path`).

| Flag | Config key | Meaning |
|---|---|---|
| `--profile NAME` | the table name | default `dev` |
| `--url URL` | `url` | management API, `http(s)://host:port` |
| `--user USER` | `user` | broker user (empty: asked when the console starts) |
| `--vhost V` | `vhosts` | repeatable (none: every vhost the account sees) |
| `--amqp-url URL` | `amqp_url` | AMQP URL for stream reads |
| `--stream-url URL` | `stream_url` | stream protocol URL |
| `--credentials ask\|vault` | `credentials` | where the console gets the password |
| `--vault-path P` | `vault_path` | with `--credentials vault` only |
| `--production` | `production` | a production profile |
| `--tier read\|peek` | `tier` | default `read` |

Without `--non-interactive`, in a terminal, it asks for what the flags did
not give (it may offer the keychain outside a container), and the peek
question:

```
Allow reading messages of classic and quorum queues? Not strictly read-only:
messages are requeued and marked redelivered, and may count toward a quorum
queue's delivery limit. [y/N]
```

The default is no: `tier = "read"`. Without a terminal it refuses rather
than wait for an answer. `--non-interactive` never reads stdin: `--url` is
then required, and `--credentials` takes `ask` or `vault` only.

The profile is validated before it is written: a flag named like a secret
(`--password`, ...), a value that embeds a password (a URL with
`user:password@`), a positional argument and a name already in the config
are refused, and the file is left as it was. An error names the flag,
never its value. In a container the management URL defaults to
`http://host.docker.internal:15672`, and `localhost`, `127.0.0.1` and
`::1`, the container itself, are refused.

`--json` prints exactly one line of JSON on stdout, lockkit's
`verbs.Profile` on success:

```json
{"lock":"lockmq","profile":"dev","config":"/work/app/.lockmq/config.toml","production":false,"credentials":"ask"}
```

and `verbs.Error` on failure, whose `code` is `usage`, `secret`, `exists`
(the profile is already there; nothing written) or `failed`:

```json
{"lock":"lockmq","code":"exists","error":"a profile of that name is already in the project's config; nothing written"}
```

Questions and messages then go to stderr. `lox add lockmq` runs
`lockmq profile add --non-interactive --json --profile NAME ... --url URL --user USER --vhost V`,
then `lockmq init claude` and `lockmq doctor --fix --yes --json`.

Every key is described in [configuration.md](configuration.md). Changing the
file later: tightenings (a lower limit, `peek` → `read`, a removed vhost, a
new mask rule) apply at once; loosenings wait for you in the console
(`:review`).

## Auto rules

`lox run` (lockhub) decides the lock's tickets in auto mode by default and
asks the human only for what matters; each profile tunes it with an `auto`
key, one rule per action:

```toml
[profiles.local]
url  = "http://127.0.0.1:15672"
auto = "full-auto"          # every action auto: the local broker is left to the agent

[profiles.prod]
url        = "https://mq.example.com:15671"
production = true
[profiles.prod.auto]
connect = "ask"             # default: a human approves the first connection
list    = "auto"            # queues, exchanges, bindings, counts
read    = "auto"            # stream reads (no offset stored, nothing removed)
peek    = "ask"             # default: queue reads, requeued (a human approves each)
get     = "ask"             # consuming (ack) a message
publish = "ask"
purge   = "ask"             # or deny
delete  = "deny"
```

| Value | Auto mode | Manual mode |
|---|---|---|
| `auto` | `lox run` approves by its rule, no question | asked |
| `ask` | asked: the ticket is `never_auto` (the lock refuses an auto rule's approval); `connect`: the first connect ticket of each console run and of each approved policy is `never_auto`, the reconnections after a human's approval may be approved by a rule | asked |
| `deny` | refused by the lock before any ticket (`E_AUTO_DENIED`); `connect = "deny"`: the console never logs in | refused |

Defaults, when a profile sets no rule: `connect` and `peek` ask (a peek
requeues messages and raises their delivery count), `list` and `read` auto,
`get`, `publish`, `purge` and `delete` ask. lockmq is
read-only: `get`, `publish`, `purge` and `delete` name no operation of this
release; their rules are validated and kept, so that a profile is locked
down before any such operation exists. Unknown actions and values are
refused. An unmasked read (`unmask`) is asked whatever the rule.

The lock enforces `deny` and `ask` itself; `auto` only lets the decider
approve without a question. Each ticket is a version-2 ticket of kind
`action` whose verb is the action (`list`, `read`, `peek`) and subject the
profile and the masked target; a `list` under `auto` runs at once, without
a ticket, as catalogue reads always did. In hub mode, the broker session
serves the agent only once its **connect ticket** is approved: the first
request of each session (the console's login, a reconnection) sends one
(kind `connect`: the profile, the management endpoint `host:port`, the
credential source, never a secret) and waits for its decision; the requests
after it share it. Under `connect = "ask"` the lock takes only a human's
decision on the first connect ticket of each console run and of each
approved policy fingerprint (the ticket is signed `never_auto`); a
reconnection after that may be approved by `lox run`'s rule. A change of the
approved policy asks the connection again. While a connect ticket waits,
the agent's other requests, `plan` included, wait behind it (up to the ticket's
10 minutes). The console still logs in at its start, where the password
is typed. The audit records the profile's rule (`auto_rule`, such as
`auto.peek=ask`) and who decided (`decided_by`: `auto:<rule id>` of the
decider, or `human`), connect tickets included (`op: connect`).

The rules are part of the profile's policy: a looser rule (`deny` < `ask` <
`auto`) is a loosening the console asks for at its start (`:review`), with
the typed profile name on a production profile; a tighter one applies at
once. Only the human edits `.lockmq/config.toml` on the host.

```sh
lockmq profile rules [--json] [--project DIR] [--profile NAME]
```

prints the effective rules of each profile, and with `--json` one line for
`lox run`:

```json
{"lock":"lockmq","actions":["connect","list","read","peek","get","publish","purge","delete"],"profiles":[{"profile":"prod","production":true,"full_auto":false,"rules":{"connect":"ask","delete":"deny",...},"ids":{"connect":"auto.connect=ask",...},"source":"approved","pending_review":true,"file_rules":{"connect":"auto",...}}]}
```

The rules are those of the policy the console approved (`source:
"approved"`) when this account can read it, never an edit of the config
files the human has not approved: those files are in the project, which the
agent can write. Files whose rules differ are flagged `pending_review`, with
`file_rules` beside. A profile with no approved policy readable here (none
yet, or a console run by a separated service account) reports the files'
rules (`source: "files"`). Either way the lock does not rely on this output:
a ticket its approved rules leave to the human is signed `never_auto`, and
the lock refuses an auto rule's approval of it.

## Agents

```sh
lockmq init [claude|codex|cursor|gemini ...]
```

Writes the project's agent files (default: the agents found on the
machine); it never asks, and running it again changes nothing: the MCP server entry (`lockmq mcp`), the Claude Code skill, the
`AGENTS.md` / `GEMINI.md` section, the Cursor rule, and an example
`.lockmq/config.toml` when there is none. Claude Code may call the catalogue
tools and `lockmq status` without a prompt; `lockmq_read`, `lockmq_peek` and
`lockmq_pii_add` always ask.

## The console

```sh
lockmq console --hub URL [--hub-plain-http] --hub-key-file FILE --instance NAME
              [--profile P] [--project DIR] [--allow-unmask] [--show-results]
              [--allowed-uids UID,...]
```

The console is launched by `lox run` only (lockhub's CLI), which writes the
hub flags: without `--hub`, `--hub-key-file` and `--instance` it refuses to
start ("launched by `lox run` only"). There is no standalone console and no
auto mode. In order: the isolation checks,
the policy review (first start: describe and approve; later: tightenings
apply, loosenings are asked), the production banner and the typed profile
name, the user and the password (or Vault), the connection
(management API, then the stream readers), the privilege audit of the broker
user (`administrator` or `policymaker`: refused on production, typed
`continue` elsewhere), then the socket.

`--allowed-uids UID,...` lets these uids connect besides the members of `lockmq-clients`,
as `allowed_uids` in the image's `/etc/lockmq/system.toml` does: an agent in a container of
its own whose uid has no account in the console's image
([docs/docker.md](docker.md#agent-in-its-own-container)). Comma-separated decimal uids;
root (0), 65534 (nobody), leading zeros and the console's own uid are refused; the console prints them at
start.

Keys and commands while it runs (lockkit's console):

| Key / command | Does |
|---|---|
| `:review` | show the pending loosening and the change requests of clients, apply or refuse |
| `:status` | the policy in force, the session |
| Ctrl-O, `:results` | print each read and peek result in clear in the console (the agent still gets it masked) |
| `:unmask` | let clients ask for unmasked output (`--unmask`), each such request approved on lockhub, never auto-approved |
| `:quit`, Ctrl-C | end the session |

**Hub mode.** With `--hub URL --hub-key-file FILE --instance NAME` (the three are required;
`lox run` writes them), the broker session waits for its connect ticket and reads and peeks
(and metadata reads a profile asks for: [auto rules](#auto-rules)) are decided on lockhub, never on this
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
is `hub`. `lockmq hub-key --instance NAME` prints the instance key for `lox run` to pin
(made once in the console account's home, `0600`).
`lox run` also passes `--hub-plain-http` for its `http://lockhub:8443` on the hub's own network:
without it an `http://` hub URL other than loopback is refused, and with it the URL must still
be a service name without a dot or a private address.

**Supported lockhub versions.** Which [lockhub](https://github.com/DavidGodefroid/lockhub) releases
(its `lox` CLI, named `lock` before lockhub v0.3.0, and its hub image) each lockmq release works with (under `lox run`; `lox up` before lockhub v0.10.0, deprecated since):

| lockmq | lockkit | lockhub |
|---|---|---|
| v0.8.1 | v0.14.1 | as v0.8.0 |
| v0.8.0 | v0.13.0 | `lox run` (lockhub#121) or a hub on lockkit v0.13.0 or later: tickets are version 2 (kinds `connect` and `action`, `lockkit` ticket kinds), which an older hub refuses |
| v0.6.1 | v0.10.0 | as v0.6.0 |
| v0.6.0 | v0.10.0 | as v0.5.0 |
| v0.5.0 | v0.9.1 | v0.3.0 or later (`lox`): the console runs under `lox up` only (deprecated since lockhub v0.10.0, now `lox run`), and the launcher's agent commands read its `.lockfamily/up.json` (version 1) |
| v0.4.3 | v0.8.1 | as v0.4.2 |
| v0.4.2 | v0.7.2 | as v0.4.1 |
| v0.4.1 | v0.6.0 | as v0.4.0; `allowed_uids` (`--allowed-uids`, for lea's agents) from lockhub v0.2.4 |
| v0.4.0 | v0.6.0 | v0.2.0 or later: hub mode, `devices: true` (paired phones), reveals (`reveal: true`, Ctrl-O) and renewals |
| v0.3.0 | v0.3.1 | v0.2.0 or later: hub mode only; leave `devices` and `reveal` off |
| v0.2.0 and older | older than v0.3.1 | none (no hub mode) |

lockhub runs these consoles from v0.2.0 on (the console account, a
terminal, the reviewed profile copy; the CLI was named `lock` before v0.3.0); lockhub v0.1.x cannot run them. A console
older than a feature `lockfamily.yaml` asks of it refuses the flag and does not start: leave
`devices`/`reveal`/`allowed_uids` off for it. A lock on lockkit older than v0.5.0 ignores the hub's reveal and
renewal requests, which then time out. lockhub's CI runs a lockmq image under its CLI since
lockhub#91 (on lockhub's main, in no lockhub release yet): a host `lockmq mcp`, lockmq v0.5.0's
launcher and image, reads a stream message once its ticket is approved. Otherwise the table
follows each release's lockkit and lockhub's
[CHANGELOG](https://github.com/DavidGodefroid/lockhub/blob/main/CHANGELOG.md) ("Supported locks",
v0.2.0).

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
(`event: renew`, with the stored request). `lox run` also passes `--facade-keys-file FILE`,
the local facades whose Ctrl-O may reveal a ticket's clear screen, sealed to them (audited
`event: reveal`). A console started again without the hub flags withdraws the plans its hub
mode left pending and refuses a run that names a `ticket` with `withdrawn` (plan again; the
human approves on this terminal): `ticket` is a hub-mode param only.

## Client commands

Every client command takes `--profile P` (optional when one profile is
configured), `--instance NAME` and `--json`. `--instance NAME` reaches one
lock among several of this type that lea mounts (a name is lowercase letters,
digits and `-`); without it the client takes `$LOCKMQ_INSTANCE`, else the one
lock in `$LEA_LOCKMQ_INSTANCES`. It is not the hub instance name that
`lockmq console` and `lockmq hub-key` take with the same flag, which is the
name `lox run` gives the lock. Exit codes: 0 ok, 1 refused, denied or failed, 2 no
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
| `lockmq read STREAM --from first\|last\|N\|TIMESTAMP [--max N] [--intent TEXT] [--unmask] [--dry-run]` | stream messages, masked; approved on lockhub |
| `lockmq peek QUEUE [--count N] [--intent TEXT] [--unmask] [--dry-run]` | NOT read-only; tier `peek` only; approved every time |
| `lockmq pii list`, `lockmq pii add "RULE"` | the mask and allow rules; add a mask rule |
| `lockmq request "CHANGE"` | queue a change for the human (`tier=peek`, `limits.read_max=50`) |
| `lockmq logout` | end the console's session |

`--dry-run` asks the console for its verdict and the approval screen without
asking you anything and without reading a message: `run`, `hub` or `refused`
with its code.

A mask rule's text: `queue=VHOST/GLOB` (or `exchange=`), then one of
`body=$.json.path`, `header=NAME-GLOB` or `property=NAME`, e.g.
`queue=orders/* body=$.customer.email`. The vhost `/` is written `//name`.

The broker reports stream positions per chunk: `last_offset` is the first
offset of the stream's last committed chunk, so the stream can hold up to one
chunk more and a lag can be that much higher (`precision` in the output).

`--unmask` asks for the messages in clear. It is refused (`E_UNMASK_OFF`)
unless you allowed it in the console (`:unmask` or `--allow-unmask`); allowed,
the request shows `PII: UNMASKED` on its screen, is approved every time, never
by the hub's auto mode, and its audit record says `"unmasked":true`.

`--intent` is one line telling you why; it is shown on the approval screen
and kept in the audit log.

## MCP tools

`lockmq mcp [--profile P] [--instance NAME]` serves the same operations to the agent on stdio:
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
| `E_AUTO_DENIED` | ask the user | the profile's auto rules deny the action (`deny`, see [Auto rules](#auto-rules)) |
| `E_STALE` | rewrite | hub mode: the queue changed between the approval and the run; nothing was read: ask again |
| `E_UNSAFE_NAME` | stop | a vhost or name that is, or holds, a `.` or `..` path segment |
| `E_POLICY` | stop | anything else the policy refuses |

## Doctor

```sh
lockmq doctor [--profile P] [--instance NAME] [--fix [--yes]] [--json]
```

Checks the config, the separated setup, the project's `.lockmq` as `lox run`
copies it (yours, as are the files and directories in it, none of them
writable by group or others, no link), each profile (tier first; `peek`
flagged NOT READ-ONLY), whether its console runs (with its start command),
and the agent wiring. In the image `doctor` runs as the agent account and
checks `.lockmq` for your uid, which the launcher passes.

`--fix` makes `.lockmq` what `lox run` copies: it takes group and others'
write away there, and gives back to you what an older `lockmq doctor --fix`
gave the console account (`.lockmq` itself and a copy of `pii.toml`). The
launcher runs `lockmq doctor --fix` as root in the image, for those repairs
only: before any check it becomes the agent account, so the checks are a
`doctor`'s without `--fix`. The repair enters each directory and never
follows a symbolic link; a link, a file or directory of another account
(root's included) and a file of the console account with more than one link
are reported, not repaired. `--fix` asks before the repair; `--yes` skips
the question. Without a terminal `--fix` needs `--yes`, and `--yes` without
`--fix` is a usage error.

The exit status is 0 exactly when no check fails: a console that does not
run yet and unwired agents are warnings. `--json` prints one line, lockkit's
`verbs.Doctor`: `ok`, the repairs made (`fixed`) and every check (`check`,
`status` `ok|warn|fail`, `detail`, `fix`); a doctor that could not check (a
usage error) prints a `verbs.Error` instead.

## Audit log

`~/.local/state/lockmq/audit.log` of the console account (mode 0600): one
JSON line per event (`login`, `policy`, `catalog`, `refused`, `approved`,
`denied`, `auto`, `timeout`, `run`, `logout`) with the operation, vhost,
target and message count, the agent's intent and the client's uid. Never a
message body, header or property value.
