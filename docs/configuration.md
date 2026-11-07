# lockmq configuration

Profiles, auto rules and PII rules: every key of the files in `.lockmq/`.
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
| `auto` | `connect`/`peek` ask, `list`/`read` auto, `get`/`publish`/`purge`/`delete` ask | `"full-auto"` or a table of `auto`/`ask`/`deny`; a looser rule is a loosening |
| `ssh.host`, `ssh.port`, `ssh.user`, `ssh.auth`, `ssh.key`, `ssh.credentials` | port 22 | as in locksql: `auth` is `key`, `agent` or `password` |


Changing the file later: tightenings (a lower limit, `peek` → `read`, a
removed vhost, a new mask rule) apply at once; loosenings wait for you in the
console (`:review`).

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
