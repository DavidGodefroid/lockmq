# lockmq safety model and limitations

The console is the enforcement point; skills, rules and tool descriptions
only guide the agent. The full model, its threats and its limits are in
[security-model.md](security-model.md). In short:

- **Separate account.** The image holds the console account, the clients
  group, the root-owned `/etc/lockmq/system.toml` and the socket directory
  shared through a volume; the console refuses to start without them.
- **Allowlist.** What is not in the catalogue does not exist.
- **Read-only by default.** Tier `read` reads metadata and streams, and moves
  nothing. Tier `peek` is not strictly read-only and is labelled as such
  everywhere.
- **Decisions outside the agent.** Every `read` and `peek` becomes a signed
  ticket on lockhub, decided by `lox run` on the profile's
  [auto rules](usage.md#auto-rules) or by you (also `lox approve`, the lea
  deck or a paired phone); no socket method can approve. By default stream
  reads pass and peeks are asked; an unmasked read is always asked. No
  decision within 5 minutes means denied.
- **Credentials.** Asked at console start (no echo), or read from Vault at
  every connection (a container has no keychain). Errors that
  leave the console are scrubbed of known secrets, URI userinfo and
  `Authorization` headers.
- **Broker user.** Use a user tagged `monitoring` with `configure "^$"` and
  `write "^$"`. RabbitMQ lets the `read` permission **purge** a queue: grant
  `read` on the streams only, and on queues only for profiles with tier
  `peek`. At start the console refuses an `administrator` or `policymaker`
  user on production, and asks for a typed `continue` elsewhere.
- **Audit.** One JSON line per login, policy change, catalogue call,
  refusal, decision, run and logout, with the agent's intent and the client's
  uid. Never a message body, header or property value.

## Peek (tier `peek`)

The management `get` takes up to `count` ready messages from the head and
requeues them in the same call. Consequences, all on the approval screen:
redelivered messages, a delivery count that grows on quorum queues (over the
limit, the message is dead-lettered or dropped), a possible change of order
for active consumers, and only ready messages at the head are visible. The
console reads the effective delivery limit (argument, policy, operator
policy; the lowest; RabbitMQ 4.x's default 20) and refuses a peek within
`delivery_headroom` of it, from the highest count it saw this session.

## Console keys

| Key / command | Does |
|---|---|
| `:review` | show the pending loosening and the change requests of clients, apply or refuse |
| `:status` | the policy in force, the session |
| Ctrl-O, `:results` | print each read and peek result in clear here (the agent still gets it masked) |
| `:unmask` | let clients ask for unmasked output (`--unmask`), each such request approved on lockhub, never auto-approved |
| `:quit`, Ctrl-C | end the session |

The switches are console-only: no client, config file or environment
variable can set them, and each change is audited.

## Limitations

- No writes of any kind (purge, move, requeue, publish, shovel), as of v0.8.x.
- RabbitMQ only; JSON and UTF-8 text bodies only, other bodies are shown as
  size and hash.
- Masking is best effort: a value in a free-text field that no detector,
  rule or name heuristic catches comes back in clear.
- A `monitoring` user cannot read runtime parameters (shovels, federation)
  nor its own permissions: `policies` says so; permissions are not audited.
- Through an ssh tunnel the stream protocol is unavailable (its client dials
  the broker's advertised address): streams are read over AMQP, and stream
  offsets and consumer lag are not available.
- The first peek of a queue cannot know the delivery counts (the count is
  only seen after a `get`); the screen says so.
- One console per profile and project, one request at a time.

## Roadmap

Not in v0.8.x, and not promised for a given release:

- body formats beyond JSON and UTF-8 text.

## Compared with an agent holding credentials

"Why does `orders.failed` fill up?" is a good question for an agent, and
answering it means looking at queues, bindings, policies, consumers and, in
the end, a few messages. Handing the agent `rabbitmqctl`, the management API
or an admin password gives it every write too (purge, delete, publish,
shovels), and every message body in clear. lockmq moves the key and the
decision **out of the agent**.

| | Agent with broker credentials | lockmq |
|---|:---:|:---:|
| Agent never sees the broker password | ❌ | ✅ |
| Agent has no broker connection | ❌ | ✅ |
| Only an allowlist of read operations exists | ❌ | ✅ |
| Every message read decided outside the agent, by your rules or by you | ❌ | ✅ |
| Message bodies, headers and properties masked | ❌ | ✅ |
| Stream reads never store an offset | ❌ | ✅ |
| Queue peek off by default, asked every time unless the profile sets `auto.peek = "auto"`, refused near the delivery limit | ❌ | ✅ |
| Policy loosening needs a human confirmation | ❌ | ✅ |
| Append-only audit log (never message content) | ❌ | ✅ |

## Highlights in full

<table>
<tr>
<td width="50%" valign="top">

**🔑 One key holder**<br>
Credentials are typed at console start or fetched
from HashiCorp Vault (the RabbitMQ secrets engine works as is). Never in argv,
env, files, logs, the socket or error messages; URI passwords are stripped
from every answer.

</td>
<td width="50%" valign="top">

**📋 Catalogue first, no prompt**<br>
Overview and alarms, queues (paged, filtered, sorted), one queue with its
arguments, DLX, TTL, delivery limit, policy, bindings and consumers;
exchanges, policies, connections, channels, stream offsets and consumer lag.

</td>
</tr>
<tr>
<td valign="top">

**👤 Messages, approved**<br>
`read` reads a stream from `first`, `last`, an offset or a timestamp with an
unnamed consumer: nothing stored, nothing moved. Each read is a ticket that
`lox run` decides on the profile's rules (auto by default) or asks you about.

</td>
<td valign="top">

**🧪 Peek, only if you allow it**<br>
Reading a classic or quorum queue requeues messages. It exists only on a
profile you set to tier `peek`, is approved by the human every time unless
you set its auto rule to `auto` (a loosening you confirm), and refused near the quorum queue's delivery limit.

</td>
</tr>
<tr>
<td valign="top">

**🕶️ Masking for messages**<br>
JSON bodies masked per leaf (`redact-unknown` on production), headers and
properties by rule and name, value detectors, `<redacted:rN.K>` references;
binary and compressed bodies reduced to size and hash.

</td>
<td valign="top">

**🛡️ OS separation**<br>
The Docker image runs the console under its own account; the kernel checks
every peer on the socket, and the console refuses to start without it.
⚠️ An agent that can run `docker` (the default MCP wiring is a `docker run`)
can attach to the console: see
[what Docker does and does not separate](docker.md#what-docker-does-and-does-not-separate).

</td>
</tr>
<tr>
<td valign="top">

**⚖️ The AI tightens, the human loosens**<br>
The agent may add mask rules or tighten a profile; any loosening (a tier, a
limit, a vhost, a URL) waits for your confirmation in the console.

</td>
<td valign="top">

**🏷️ Refusals the agent can act on**<br>
Every refusal carries a stable code, a hint and a retry rule: rewrite, ask
the user, or stop.

</td>
</tr>
</table>
