# Running lockmq in Docker

Docker is the way to run lockmq: nothing to install on your machine but
Docker. Other deployment modes exist in the code but are no longer
documented.

The image holds the separated setup that keeps the console apart from the
agent:

- the console runs as `lockmq` (uid 10011), the only account that holds the
  broker credentials;
- the agent's MCP server and CLI run as `agent` (uid 10012), a member of
  `lockmq-clients` (gid 10010);
- `/etc/lockmq/system.toml` is owned by root and the binary by root; the
  image has no `sudo`;
- the socket directory `/run/lockmq` (mode 2710, `lockmq:lockmq-clients`)
  is a named volume, `lockmq-run`, shared by the console container and the
  agent's containers. The kernel checks every peer on the socket as on a
  host.

The ids differ from the locksql image's, so that one agent image can be a
client of both.

## Build

From a clone:

```sh
docker build -t lockmq .
```

or without cloning:

```sh
docker build -t lockmq https://github.com/DavidGodefroid/lockmq.git
```

`--build-arg VERSION=v0.1.0` sets the version that `lockmq version` prints.
The base images are pinned by digest in the `Dockerfile`.

## Set up a project

Mount the project at its own path (`-v "$PWD:$PWD" -w "$PWD"`): the socket name
is derived from the project path, so the console and the agent must see the
same one, and the console's hints then name real paths. The console account
must enter and list the project directory, the only one of the path that
keeps its host mode in the container: mode 0755, the usual, does it (`lockmq
doctor` reports a project it cannot enter).

In the project directory, `init` writes `.lockmq/config.toml` with a
commented example profile, and the agent files (here Claude Code's):

```sh
docker run --rm --user "$(id -u):$(id -g)" -e HOME=/tmp \
  -v "$PWD:$PWD" -w "$PWD" lockmq init claude
```

`--user` and `HOME` make the files yours. `init` writes `lockmq mcp` as the
MCP command, which is not on your machine: replace the `lockmq` entry as in
[Wire the agent](#wire-the-agent).

Add a broker profile with the wizard:

```sh
docker run -it --rm --user "$(id -u):$(id -g)" -e HOME=/tmp \
  -v "$PWD:$PWD" -w "$PWD" lockmq setup
```

It asks for the management API URL, the AMQP and stream URLs, the vhosts,
the broker user and where the password comes from, never for the password
itself, and appends the profile to `.lockmq/config.toml`. Inside a
container, `localhost` is the container itself, and the wizard knows it:

- a broker on your machine is `host.docker.internal` (the default: Docker
  Desktop; on Linux add `--add-host=host.docker.internal:host-gateway` to the
  console's `docker run`);
- a broker in another container is reached by its name on a shared network
  (`--network NAME` on the console's `docker run`), as in
  [A broker in a container](#a-broker-in-a-container);
- the wizard refuses `localhost`, `127.0.0.1` and `::1`;
- `credentials = "keychain"` needs an OS keychain, which the container does
  not have: use `credentials = "ask"`, or `"vault"` with the image below;
- a remote broker takes TLS URLs (`https`, `amqps`,
  `rabbitmq-stream+tls`), checked against the image's CA certificates
  (`tls = "verify-full"` by default); a production profile on a remote host
  refuses plain ones.

Then give `.lockmq/` to the console account. The console writes `pii.toml`
there, and a directory your account owns would let the agent replace that
file, and with it the masking rules. The account exists only in the image,
so run the repair there, as root:

```sh
docker run --rm --user 0 -v "$PWD:$PWD" -w "$PWD" lockmq doctor --fix --yes
```

It makes uid 10011 the owner of `.lockmq/` (mode 0755), replaces a
`pii.toml` it does not own with a copy it owns (review its rules: the agent
may have changed them), and makes `config.toml` readable; `config.toml` stays yours to edit (the console shows
any change to the policy and asks you to approve it). The repair enters the
directory and never follows a symbolic link. The checks of the console fail
from this container, which runs no console: read the `project` lines. Run
it again whenever `.lockmq/` is created anew, by `lockmq init` for example.

## Run the console

In a terminal you keep in view, from the project directory:

```sh
docker run -it --rm --name lockmq-console --user lockmq \
  -v "$PWD:$PWD" \
  -v lockmq-run:/run/lockmq \
  -v lockmq-home:/home/lockmq \
  lockmq console --project "$PWD"
```

Add `--network NAME` for a broker container,
`--add-host=host.docker.internal:host-gateway` for a broker on your Linux
machine, and `--profile P`, `--show-results`, `--auto` or `--allow-unmask`
after `console`. The `lockmq-home` volume keeps the console account's state
from one run to the next: the approved policy, the audit log and
`~/.ssh/known_hosts` for an `ssh` profile. A key for an `ssh` tunnel must be
in that volume, owned by `lockmq` with mode 0600.

## Credentials from Vault

`credentials = "vault"` runs the `vault` CLI in the console's container. The
default image leaves it out (it adds some 550 MB); the `vault` target has it,
checked against the pinned release sums:

```sh
docker build --target vault -t lockmq .
```

The console reads `VAULT_ADDR` (and `VAULT_CACERT`, `VAULT_NAMESPACE`) from
its environment: add `-e VAULT_ADDR=https://vault.example.com` to its
`docker run`. Log in once as the console account, with the same volumes; the
token stays in `lockmq-home`, out of the agents' reach:

```sh
docker run -it --rm --user lockmq -e VAULT_ADDR=https://vault.example.com \
  -v lockmq-home:/home/lockmq --entrypoint vault lockmq login -method=oidc
```

Log in again when the token expires: the console then reports it.

## Wire the agent

The agent starts the MCP server in a throwaway container, as `agent`, with
the socket volume and the project mounted. `sh -c` expands `$PWD`, the
directory the agent runs in (start the agent at the project root).

Claude Code, for every project:

```sh
claude mcp add --scope user lockmq -- \
  sh -c 'exec docker run -i --rm -v "$PWD:$PWD" -w "$PWD" -v lockmq-run:/run/lockmq lockmq mcp'
```

or for one project, in `.mcp.json`:

```json
{
  "mcpServers": {
    "lockmq": {
      "type": "stdio",
      "command": "sh",
      "args": ["-c", "exec docker run -i --rm -v \"$PWD:$PWD\" -w \"$PWD\" -v lockmq-run:/run/lockmq lockmq mcp"]
    }
  }
}
```

Gemini CLI and Cursor take the same `command` and `args` in the files
`lockmq init` writes for them (`.gemini/settings.json`, `.cursor/mcp.json`),
and Codex in the `[mcp_servers.lockmq]` table `lockmq init codex` prints for
`~/.codex/config.toml`. Keep the timeouts there (`timeout`,
`tool_timeout_sec`): the console waits up to 5 minutes for an approval.

The client commands run the same way:

```sh
docker run --rm -v "$PWD:$PWD" -w "$PWD" -v lockmq-run:/run/lockmq lockmq status
docker run --rm -v "$PWD:$PWD" -w "$PWD" -v lockmq-run:/run/lockmq lockmq queues
```

`lockmq doctor` runs as `agent` by default and as the console account with
`--user lockmq`.

## Agent in its own container

When the agent itself runs in a container that cannot reach the Docker
socket, it cannot start `docker run` for the MCP server: it runs the
`lockmq` binary inside its own container instead, as a client of the
console container. The two containers share only the socket volume.

The console checks every peer by uid against its own container's accounts,
where the agent's uid has none: name it at build time.

```sh
docker build -t lockmq --build-arg ALLOWED_UIDS=1000 .
```

`ALLOWED_UIDS` is a comma-separated list of uids, written to `allowed_uids`
in the image's `/etc/lockmq/system.toml`; the build refuses root (0) and the
console's own uid (10011). It is the uid the kernel reports
for the agent's process, as the console sees it: the agent container's uid
when neither container remaps user namespaces. Every process of that uid,
in any container that mounts the socket volume, may then use the console:
name only the agent's.

The agent's image needs what a client host has:

- the `lockmq` binary, the same version as the console's;
- `/etc/lockmq/system.toml`, owned by root, mode 0644, with the image's
  `service_user`, `client_group` and `socket_dir`: without it the client looks
  for the socket in the user's runtime directory;
- an account `lockmq` with uid 10011: the client refuses a socket served by
  any uid but `service_user`'s;
- a group `lockmq-clients` with gid 10010 that holds the agent's account:
  the socket directory is mode 2710 and the socket 0660, group
  `lockmq-clients`.

Start the agent's container with the socket volume and the project at the
same path as the console's, for example:

```sh
docker run … -v lockmq-run:/run/lockmq -v "$PWD:$PWD" -w "$PWD" agent-image
```

`lockmq init claude` then needs no change: the `lockmq mcp` it writes in
`.mcp.json` runs inside the agent's container. Check from there with
`lockmq status`.

## A broker in a container

Put the broker and the console on one network, and name the broker by its
container name in the profile:

```sh
docker network create lockmq-net
docker run -d --name rabbitmq --network lockmq-net rabbitmq:4.1-management
```

```toml
[profiles.dev]
url      = "http://rabbitmq:15672"
amqp_url = "amqp://rabbitmq:5672"
```

and add `--network lockmq-net` to the console's `docker run`. A broker that
publishes its ports on your machine is `host.docker.internal` instead.

The stream protocol connects to the address the broker **advertises**, not
the one in `stream_url`: set `stream.advertised_host` and
`stream.advertised_port` in the broker's configuration to what the console's
container can reach (the broker's container name and port 5552 on a shared
network), or leave `stream_url` out and read streams over AMQP.

Create the read-only user once, with the container's admin:

```sh
docker exec rabbitmq rabbitmqctl add_user lockmq-ro 'choose-a-password'
docker exec rabbitmq rabbitmqctl set_user_tags lockmq-ro monitoring
docker exec rabbitmq rabbitmqctl set_permissions -p / lockmq-ro '^$' '^$' '^(order-events)$'
```

The last argument is the `read` permission: list the streams lockmq may
read (and the queues, only for a profile with tier `peek`; `read` also
allows a purge, see the security model).

Never give the agent `docker exec` on the broker container: it is a shell
with the broker's own rights.

## What Docker does and does not separate

Inside Docker the boundary is the same as on a host: the agent's container
holds no credential, has no broker connection and reaches the console only
through the socket; a process that is neither the console account nor in
`lockmq-clients` (nor in `ALLOWED_UIDS`) is refused.

Your own account is another matter. Whoever can run `docker` commands can
`docker attach` to the console and type an approval, `docker exec` into it
and read its memory, or open the `lockmq-home` volume. An agent that runs
shell commands in your account usually can: the MCP server above is itself a
`docker run`. The console cannot see this (its terminal belongs to the
container), so treat Docker mode like the in-agent approval it improves on:
it keeps credentials out of the agent's reach and the approval in your
terminal, but it does not stop an agent determined to go around it. For that
boundary, run the agent itself in a container with no access to the Docker
socket ([Agent in its own container](#agent-in-its-own-container)).
