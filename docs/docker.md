# Running lockmq in Docker

lockmq runs in Docker, with nothing on your machine but Docker and the
`lockmq` launcher. The launcher is a short shell script
([`scripts/lockmq`](../scripts/lockmq)): every command you type,
`lockmq console`, `lockmq version`, `lockmq mcp` and the others, runs in
the lockmq image, never in a binary of its own on your machine. The image
holds the separated setup that keeps the console apart from the agent:

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

## Install

Download the install script from the release you install, read it, then
run it with that release:

```sh
V=vX.Y.Z
curl -fsSLO "https://raw.githubusercontent.com/DavidGodefroid/lockmq/$V/scripts/install.sh"
less install.sh
LOCKMQ_VERSION=$V sh install.sh
```

It checks the release's cosign signature and checksum, and installs the
launcher as `/usr/local/bin/lockmq` (with `sudo` when needed; set
`LOCKMQ_INSTALL_DIR` for another directory). The launcher runs the image of
its own release, `ghcr.io/davidgodefroid/lockmq:X.Y.Z`, which Docker pulls
on first use: `install.sh` checks the image's cosign signature too (and its
`-vault` variant's) and writes the verified digests into the launcher, which
runs the image by digest, never by a tag that could be moved. Linux and
macOS (Docker Desktop); on Windows, install it in WSL 2 with Docker
Desktop's WSL integration and run it there.

```sh
lockmq version    # the launcher's version and its image's
lockmq update     # the latest release: a new launcher, then docker pull
lockmq update vX.Y.Z
```

`lockmq update` checks the signatures and the checksum as `install.sh`
does, and needs cosign the same way. With `LOCKMQ_SKIP_COSIGN=1` nothing is
verified and the launcher runs the image by its tag.

### What runs where

The launcher mounts the project (the nearest directory up from where you
are that holds `.lockmq/`, or that directory itself) at its own path, and
runs each command with the account it needs:

| Command | Runs as | Volumes |
|---|---|---|
| `console` | `lockmq`, in a container of its own per project (`lockmq-console-<n>`; `LOCKMQ_CONSOLE_NAME` names it), with `--add-host=host.docker.internal:host-gateway`, no log driver, no capability, `no-new-privileges`, a read-only root with a 64 MB `/tmp`, at most 256 processes and 1 GB of memory | `lockmq-run`, `lockmq-home` |
| `init` | you (your uid and gid), with your home mounted: it writes the agents' files | |
| `setup` | you, with `HOME=/tmp` | |
| `doctor --fix` | root | |
| everything else: `mcp`, `status`, `queues`, `read`, `peek`, `doctor`, `help`... | `agent` | `lockmq-run` |

The socket name is derived from the project path, so the console and the
agent see the same one, and the console's hints name real paths. The
console account must enter and list the project directory, the only one of
the path that keeps its host mode in the container: mode 0755, the usual,
does it (`lockmq doctor` reports a project it cannot enter).

The launcher reads:

- `LOCKMQ_IMAGE`: the image to run instead of the release's, for example
  your own build (below);
- `LOCKMQ_VAULT=1`: the image with the `vault` CLI
  ([Credentials from Vault](#credentials-from-vault));
- `LOCKMQ_DOCKER_ARGS`: more `docker run` arguments, split on spaces, for
  example `LOCKMQ_DOCKER_ARGS="--network lockmq-net" lockmq console`;
- `LOCKMQ_READONLY_PROJECT=1`: the console mounts the project read-only and
  only `.lockmq/` writable (it must then be a directory, not a link), for a
  project your agents write;
- `VAULT_ADDR`, `VAULT_NAMESPACE`: passed to the console; `VAULT_CACERT`
  too, with its file mounted read-only at its own path.

The launcher never mounts the Docker socket.

### Paths

A project path, `$HOME` (for `init`) or a `VAULT_CACERT` path holding `:` is
refused: docker `-v` separates on it. `LOCKMQ_DOCKER_ARGS` is split on
spaces and never globbed. With `LOCKMQ_READONLY_PROJECT=1`, `.lockmq/` must
be a directory whose physical path is itself; the launcher checks it twice,
the second time right before `docker run`, but cannot close the instant
between that check and Docker's own mount: an agent that can replace
`.lockmq` in that instant could redirect the console's writable mount.
Mount `.lockmq` read-only in the agents' containers to close it.

On macOS (Docker Desktop), `lockmq doctor --fix` giving `.lockmq/` to uid
10011 (a `chown` through Docker Desktop's file sharing) is untested so far.

### Your own build

From a clone, or without cloning:

```sh
docker build -t lockmq .
docker build -t lockmq https://github.com/DavidGodefroid/lockmq.git
```

then `LOCKMQ_IMAGE=lockmq lockmq ...`. The launcher of a clone
(`sh scripts/lockmq ...`) runs the image tagged `lockmq` by default.
`--build-arg VERSION=vX.Y.Z` sets the version that `lockmq version` prints.
The base images are pinned by digest in the `Dockerfile`.

### Migrating

From the former Docker guide: its `docker run ... lockmq ...` commands keep
working, but the launcher replaces them. Install it, then
use `lockmq console`, `lockmq init`, `lockmq setup`; the `lockmq-run` and
`lockmq-home` volumes are the same. An agent entry that runs
`sh -c 'exec docker run ... lockmq mcp'` keeps working; `lockmq init` now
writes `lockmq mcp`, which goes through the launcher.

From a host install (the release archives and packages of v0.1.0, then
`sudo lockmq install`): `install.sh` replaces the binary with the launcher,
and the image refuses `lockmq install` and `lockmq update`. What the host
install created stays until you remove it: the `lockmq` account and the
`lockmq-clients` group, `/etc/lockmq/`, the socket directory (`/run/lockmq`,
`/etc/tmpfiles.d/lockmq.conf` on Linux), and the console account's state in
its home (approved policy, audit log). Remove a `.deb`, `.rpm` or `.apk`
package with your package manager first: releases no longer publish any.

## Set up a project

In the project directory, `init` writes `.lockmq/config.toml` with a
commented example profile, and the agent files (here Claude Code's):

```sh
lockmq init claude
```

The MCP command it writes, `lockmq mcp`, is the launcher: the agent starts
the MCP server in a throwaway container ([Wire the agent](#wire-the-agent)).

Add a broker profile with the wizard:

```sh
lockmq setup
```

It asks for the management API URL, the AMQP and stream URLs, the vhosts,
the broker user and where the password comes from, never for the password
itself, and appends the profile to `.lockmq/config.toml`. Inside a
container, `localhost` is the container itself, and the wizard knows it:

- a broker on your machine is `host.docker.internal` (the default), which
  the launcher maps to your machine;
- a broker in another container is reached by its name on a shared network
  (`LOCKMQ_DOCKER_ARGS="--network NAME" lockmq console`), as in
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
where the launcher runs the repair as root:

```sh
lockmq doctor --fix --yes
```

It makes uid 10011 the owner of `.lockmq/` (mode 0755), replaces a
`pii.toml` it does not own with a copy it owns (review its rules: the agent
may have changed them), and makes `config.toml` readable; `config.toml`
stays yours to edit (the console shows any change to the policy and asks you
to approve it). The repair enters the directory and never follows a
symbolic link. The checks of the console fail from this container, which
runs no console: read the `project` lines. Run it again whenever `.lockmq/`
is created anew, by `lockmq init` for example.

## Run the console

In a terminal you keep in view, from the project directory:

```sh
lockmq console
```

Add `--profile P`, `--show-results`, `--auto` or `--allow-unmask`, and
`LOCKMQ_DOCKER_ARGS="--network NAME"` for a broker container. The
`lockmq-home` volume keeps the console account's state from one run to the
next: the approved policy, the audit log and `~/.ssh/known_hosts` for an
`ssh` profile. A key for an `ssh` tunnel must be in that volume, owned by
`lockmq` with mode 0600.

## Credentials from Vault

`credentials = "vault"` runs the `vault` CLI in the console's container. The
default image leaves it out (it adds some 550 MB); the release's `-vault`
image has it (`ghcr.io/davidgodefroid/lockmq:X.Y.Z-vault`, checked against
the pinned Vault release sums), which `LOCKMQ_VAULT=1` selects. For your own
build, `docker build --target vault -t lockmq:vault .` and
`LOCKMQ_IMAGE=lockmq:vault`.

The console reads `VAULT_ADDR` and `VAULT_NAMESPACE` from its environment,
which the launcher passes on:

```sh
LOCKMQ_VAULT=1 VAULT_ADDR=https://vault.example.com lockmq console
```

`VAULT_CACERT` names a file on your machine: the launcher mounts it
read-only in the console's container, at the same path.

Log in once as the console account, with its home volume; the token stays
in `lockmq-home`, out of the agents' reach:

```sh
docker run -it --rm --user lockmq -e VAULT_ADDR=https://vault.example.com \
  -v lockmq-home:/home/lockmq --entrypoint vault \
  ghcr.io/davidgodefroid/lockmq:X.Y.Z-vault login -method=oidc
```

Log in again when the token expires: the console then reports it.

## Wire the agent

`lockmq init claude` wrote `lockmq mcp` as the MCP command in `.mcp.json`:
the agent runs the launcher, which starts the MCP server in a throwaway
container, as `agent`, with the socket volume and the project mounted. Start
the agent at the project root, or below it.

```json
{
  "mcpServers": {
    "lockmq": {
      "type": "stdio",
      "command": "lockmq",
      "args": ["mcp"]
    }
  }
}
```

For every project instead, `claude mcp add --scope user lockmq -- lockmq
mcp`. Gemini CLI and Cursor take the same `command` and `args` in the files
`lockmq init` writes for them (`.gemini/settings.json`, `.cursor/mcp.json`),
and Codex in the `[mcp_servers.lockmq]` table `lockmq init codex` prints for
`~/.codex/config.toml`. Keep the timeouts there (`timeout`,
`tool_timeout_sec`): the console waits up to 5 minutes for an approval.

The client commands run the same way: `lockmq status`, `lockmq queues`...

`lockmq doctor` runs as `agent`; `docker run --rm --user lockmq -v
lockmq-run:/run/lockmq -v lockmq-home:/home/lockmq
ghcr.io/davidgodefroid/lockmq:X.Y.Z doctor` checks the console account.

## Agent in its own container

When the agent itself runs in a container that cannot reach the Docker
socket, the launcher cannot start the MCP server: the agent runs the
`lockmq` binary inside its own container instead, as a client of the
console container. The two containers share only the socket volume.

The console checks every peer by uid against its own container's accounts,
where the agent's uid has none: name it at build time.

```sh
docker build -t lockmq --build-arg ALLOWED_UIDS=1000 .
LOCKMQ_IMAGE=lockmq lockmq console
```

`ALLOWED_UIDS` is a comma-separated list of uids, written to `allowed_uids`
in the image's `/etc/lockmq/system.toml`; the build refuses root (0) and the
console's own uid (10011). It is the uid the kernel reports
for the agent's process, as the console sees it: the agent container's uid
when neither container remaps user namespaces. Every process of that uid,
in any container that mounts the socket volume, may then use the console:
name only the agent's.

The agent's image needs what a client host has:

- the `lockmq` binary, not the launcher: the one in the release archive
  (`lockmq_X.Y.Z_linux_ARCH.tar.gz`, checked as `install.sh` checks it),
  the same version as the console's image;
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

and start the console on it: `LOCKMQ_DOCKER_ARGS="--network lockmq-net"
lockmq console`. A broker that
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
shell commands in your account usually can: the launcher it runs for the
MCP server is itself a `docker run`. The console cannot see this (its terminal belongs to the
container), so treat Docker mode like the in-agent approval it improves on:
it keeps credentials out of the agent's reach and the approval in your
terminal, but it does not stop an agent determined to go around it. For that
boundary, run the agent itself in a container with no access to the Docker
socket ([Agent in its own container](#agent-in-its-own-container)).
