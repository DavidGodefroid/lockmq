# lockmq configuration

Profiles, auto rules and PII rules: every key of the files in `.lockmq/`,
and the company's [managed configuration](#managed-configuration) that bounds
them.
The commands that write them are in [usage.md](usage.md).

## Profiles

Profiles live in `.lockmq/config.toml` at the project root. No file holds a
secret: a URL with a password is refused.

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
tickets_per_window = 30     # tickets a human decides per window
ticket_window     = "5m"
max_pending       = 30      # tickets pending at once
```

`[profiles.NAME.auto]` shapes `lox run`'s auto mode for the profile, one
rule per action, `auto`, `ask` or `deny` (`auto = "full-auto"` sets them all
to `auto`, for a local broker left to the agent):

```toml
[profiles.prod.auto]
connect = "ask"    # default: a human approves the first connection
list    = "auto"   # queues, exchanges, bindings, counts
read    = "auto"   # stream reads
peek    = "ask"    # default: requeued queue reads, a human approves each
purge   = "deny"   # get, publish, purge, delete: ask by default (no such operation yet)
```

See [usage.md](usage.md#auto-rules).

An `[profiles.NAME.ssh]` table reaches a broker behind a bastion: the console
opens the tunnel itself and checks the bastion's host key; streams are then
read over AMQP.

## All profile keys

| Key | Default | Notes |
|---|---|---|
| `url` | required | management API, `http://` or `https://`; no userinfo |
| `amqp_url`, `stream_url` | none | optional; an operation that needs a missing one says which key to set |
| `vhosts` | every vhost the account sees | the only vhosts served |
| `user` | asked at start | with `credentials = "vault"`: the secret's username |
| `credentials` | `ask` | `ask`, `keychain` or `vault` |
| `vault_path` | required with `vault` | e.g. `rabbitmq/creds/<role>` |
| `credentials_ttl` | none | at least `1m`: the secret is asked again once it is that old |
| `tls` | `verify-full` | `disable`, `prefer`, `require`, `verify-ca`, `verify-full` |
| `tls_ca`, `tls_cert`, `tls_key` | none | a private CA; a client certificate (both or neither) |
| `tier` | `read` | `read` or `peek` |
| `production` | `false` | typed approval, `redact-unknown` body mode |
| `unconfined` | `false` | `true` serves agents that do not run confined (see [below](#confined-agents)); refused on a production profile; switching it on is a loosening |
| `detectors` | `card`, `email`, `iban`, `phone` | also `be_niss`, `fr_nir`, `nl_bsn`, `us_ssn`, `ip`, `secret`, `person_name`; `[]` disables them |
| `body_mode` | `redact-unknown` on production, `detect` otherwise | |
| `limits.request_timeout` | 10s | |
| `limits.read_max` | 20 | ceiling 200 |
| `limits.peek_max` | 5 | ceiling 20 |
| `limits.delivery_headroom` | 3 | lowering it is a loosening |
| `limits.max_body_bytes` | 16 384 | per message after masking |
| `limits.max_output_bytes` | 65 536 | messages dropped from the end and counted |
| `limits.list_page_size` | 100 | |
| `limits.idle_timeout`, `limits.max_session` | 20m, 4h | session ends after this long idle / in all |
| `limits.tickets_per_window`, `limits.ticket_window` | 30 per `5m` | hub tickets a human decides, opened per window at most; a ticket an auto rule of `lox run` decides leaves this count, and a safety cap of 300 tickets per window counts every one ([ticket limits](usage.md#ticket-limits)); more tickets or a shorter window is a loosening |
| `limits.max_pending` | 30 | hub tickets pending a decision at once at most; raising it is a loosening |
| `auto` | `connect`/`peek` ask, `list`/`read` auto, `get`/`publish`/`purge`/`delete` ask | `"full-auto"` or a table of `auto`/`ask`/`deny`; a looser rule is a loosening |
| `ssh.host`, `ssh.port`, `ssh.user`, `ssh.auth`, `ssh.key`, `ssh.credentials` | port 22 | as in locksql: `auth` is `key`, `agent` or `password` |


Changing the file later: tightenings (a lower limit, `peek` → `read`, a
removed vhost, a new mask rule) apply at once; loosenings (more tickets, a
shorter `ticket_window` or more pending tickets among them) wait for you in
the console (`:review`).

### Confined agents

The console serves only agents that run confined (lockkit#133): the client
checks at `hello` that it runs in a container, that no rootful container
runtime socket (`/var/run/docker.sock` and the like) is reachable, that the
Lock Family's state directory is not readable, and that the project's
`.lockfamily/` is not writable; the console also refuses a peer holding a
group that owns a runtime socket. An agent run on the host, or in a container
that mounts the Docker socket, is refused with `CodeUnconfined` and its
reasons; an older client that sends no report is refused as `old_client`.
Client and console must be the same lockmq version.

`unconfined = true` lets such an agent in on a profile that is not
production, for a local broker:

```toml
[profiles.dev]
url        = "http://127.0.0.1:15672"
unconfined = true   # the lock then protects against mistakes, not against a compromised agent
```

It is a loosening, approved in the console; the console prints a red banner
and marks every audit record and hub ticket of an unconfined connection. A
production profile with `unconfined = true` is refused, and the managed
file's `[family] require_confined_agent = true` refuses unconfined agents whatever the
profile says.

## PII rules

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

## Managed configuration

A company can bound every lockmq of a machine with the Lock Family's
managed file, `/etc/lockfamily/managed.toml` on Linux and
`/Library/Application Support/lockfamily/managed.toml` on macOS (schema,
signing and deployment:
[lockfamily docs/managed.md](https://github.com/DavidGodefroid/lockfamily/blob/main/docs/managed.md)).
The file and its directory must be owned by root and writable by root
only; a badly owned, invalid or badly signed file is a refusal to start,
never a file ignored. lockmq reads its `[lockmq]` section:

```toml
version = 1
organisation = "Acme"

[lockmq.bounds]                    # the loosest policy allowed
tier = "read"
vhosts = ["orders", "billing"]
credentials = ["vault"]
hosts = ["*.mq.acme.internal"]
limits = { read_max = 50, max_session = "8h" }
detectors = ["email", "iban"]      # always on, added to every profile

[[lockmq.bounds.pii_mask]]         # always on, added to the PII rules
queue = ["orders/*"]
body  = "$.customer.email"

[lockmq.defaults]                  # for the keys a profile leaves unset
tls = "verify-full"
limits = { read_max = 20 }

[lockmq.profiles.orders-prod]      # served as written
url = "https://mq.acme.internal:15671"
production = true
vhosts = ["orders"]
```

Precedence: hard-coded floor > managed bounds and profiles > project file >
user file > managed defaults > built-in defaults.

- **Profiles**: a managed profile is served as written (the defaults fill
  what it leaves unset). A profile of the same name in the project or user
  config is refused at load, naming it: rename yours.
- **Defaults**: `[lockmq.defaults]` takes every profile key, `limits.*`
  included; a value of the config files wins over it.
- **Bounds**: the merged profile is checked against `[lockmq.bounds]` at
  the console's start and at every policy poll (2 s). A profile beyond a
  bound is refused with no prompt, citing the key
  (`blocked by company policy: lockmq.bounds.tier = read (profile dev: tier = peek)`),
  audited `refused-managed`, and no human can approve past it. A loosening
  edited in while the console runs is refused at the next poll, never
  pending. A duration or size beyond its bound is a violation, never clamped.
  A managed default or a managed profile beyond the bounds is a load error.
- `lockmq managed show` prints the file, its fingerprint and signature, the
  bounds, the defaults and the managed profiles; `lockmq managed check
  [--profile P]` checks the profiles as the console does, with no connection
  and no credential: one `blocked by company policy: …` line per violation
  and exit 3, else `ok: …` and exit 0 (`lox run` runs it before it starts
  the lock). `lockmq doctor` has a `managed` line.

| Bound key | Type | Looser (refused) |
|---|---|---|
| `production` | `true` | a profile that is not production |
| `tier` | `read` or `peek` | a higher tier (`peek` under `read`) |
| `tls` | TLS mode, a floor | a weaker mode (`disable` < `prefer` < `require` < `verify-ca` < `verify-full`) |
| `body_mode` | `"redact-unknown"` | `detect` |
| `vhosts` | allowed vhosts | a vhost outside the list, or no `vhosts` (every vhost) |
| `credentials` | allowed modes (`ask`, `keychain`, `vault`) | a mode outside the list |
| `hosts` | host patterns (`*` any run) | a host of `url`, `amqp_url`, `stream_url` or `ssh.host` matching none |
| `credentials_ttl` | duration ceiling | longer, or none (kept for the session) |
| `auto.<action>` (`connect`, `list`, `read`, `peek`, `get`, `publish`, `purge`, `delete`) | `deny`, `ask` or `auto`, a ceiling | a looser rule (`deny` < `ask` < `auto`) |
| `limits.read_max`, `limits.peek_max`, `limits.max_body_bytes`, `limits.max_output_bytes`, `limits.list_page_size` | integer ceiling | a larger value (the built-in default counts when the profile sets none) |
| `limits.request_timeout`, `limits.idle_timeout`, `limits.max_session` | duration ceiling | a longer duration |
| `detectors` | required detectors | none: they are added to every profile |
| `[[pii_mask]]` | mask rules (`queue`, `exchange`, one of `body`/`header`/`property`, `mode`), as `[[mask]]` | none: they are added to the rules in force; a `[[allow]]` that could clear one is refused at load, and a user mask of the same target takes the managed mode |

`[family] vault = { required = true }` acts as `credentials = ["vault"]`,
cited as `family.vault.required`.
