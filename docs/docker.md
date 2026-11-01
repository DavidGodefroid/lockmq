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
  is a named volume, shared by the console container and the agent's
  containers (read-only there): `lox up`'s socket volume of the lock
  (`lockfamily-<hash>_<instance>-run`, named in `.lockfamily/up.json`). The
  kernel checks every peer on the socket as on a host.

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
| `profile add` | you, with `HOME=/tmp` | |
| the agent commands: `mcp`, `status`, `queues`, `read`, `peek`, `doctor`... | `agent` | the socket volume of the workspace's lockmq lock that `lox up` runs, read-only ([below](#the-socket-of-lox-ups-lock)) |
| `doctor --fix` | root for its repairs only, then `agent` for every check, as `doctor` | as `doctor` |
| everything else: `help`... | `agent` | none |

The socket name is derived from the project path, so the console and the
agent see the same one, and the console's hints name real paths. The agent
account must enter the project directory, the only one of the path that
keeps its host mode in the container: mode 0755, the usual, does it
(`lockmq doctor` reports a project it cannot enter). `doctor` checks
`.lockmq/` for your uid, which the launcher passes as `LOCKMQ_HOST_UID`.

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

### The socket of lox up's lock

The consoles run hub-attached only, started by `lox up` (lockhub): each
under compose, with its socket in the volume
`lockfamily-<8 hex of sha256(workspace)>_<instance>-run`, which `lox up`
names in `<workspace>/.lockfamily/up.json` (lockhub's `docs/lox-up.md`).
The agent commands mount that volume on `/run/lockmq`, read-only
(connecting to a socket needs no write), and no other: without it they stop
(exit 2) and say why, and what to do (`lox up`), never falling back to the
fixed `lockmq-run`. The launcher takes the volume only after the checks
lea makes for its own agents:

- `up.json` is found in the nearest directory up from here whose
  `.lockfamily` holds one (that directory is the workspace); the
  `.lockfamily` directory and `up.json` are no link, are yours, and
  neither group nor others may write them; `up.json` is lox up's state
  (version 1, as `lox up` writes it: two-space indented JSON, the values
  the launcher reads unescaped) of this very workspace (its `workspace`,
  and its `project` `lockfamily-<8 hex of sha256(workspace)>`);
- the lock is the workspace's only lockmq lock, or the one `--instance
  NAME` names (the launcher's own option, taken out of the command line;
  with several and no `--instance`, the error names them). In
  `.mcp.json`, add it to the arguments: `"args": ["mcp", "--instance",
  "NAME"]`;
- its socket volume is the one `lox up` names for it, its clients group
  the image's (10010);
- Docker confirms the volume: lox up's labels (compose's project and
  volume, `dev.lockfamily.role=socket`, its instance, type and workspace),
  read as JSON and compared whole; and every container that mounts it is
  lox up's own (compose ran it from lox up's state directory: its
  `project.working_dir` is `$XDG_STATE_HOME/lockfamily/<project>` or
  `~/.local/state/lockfamily/<project>`, its `config_files` `-` or that
  directory's `compose.yaml`) or mounts it read-only (the agents', as the
  launcher's own). A compose file written in the workspace, with lox up's
  project name and volume, or any container mounting the volume
  read-write, could serve the socket from an image of its own: the
  launcher then trusts none of that socket.

`up.json` only names the lock: what is mounted is what Docker's labels
prove. Anything else fails closed.

What these checks are for: mistakes (a stale or copied `up.json`, another
workspace's lock, a volume or compose file of the same name) and files
others than you can write. They are no boundary against an agent that
runs as you on the host with Docker access (Claude Code started in a
terminal usually does): it can write `up.json`, make volumes and
containers with any labels, or `docker exec` into lox up's containers.
Even then it holds none of the console's credentials and cannot sign a
decision (the hub signs them, the console checks the hub's key), but it
could serve an impostor socket to another agent. To contain the agent
itself, run it in lea, whose containers have no Docker access.

The socket's name derives from the project (`.lockmq/config.toml` up from
the working directory) and the profile: run the agent from the workspace,
whose `.lockmq` lox up's console serves.

### Paths

A project path, `$HOME` (for `init`) or a `VAULT_CACERT` path holding `:` is
refused: docker `-v` separates on it. `LOCKMQ_DOCKER_ARGS` is split on
spaces and never globbed. With `LOCKMQ_READONLY_PROJECT=1`, `.lockmq/` must
be a directory whose physical path is itself; the launcher checks it twice,
the second time right before `docker run`, but cannot close the instant
between that check and Docker's own mount: an agent that can replace
`.lockmq` in that instant could redirect the console's writable mount.
Mount `.lockmq` read-only in the agents' containers to close it.

On macOS (Docker Desktop), `lockmq doctor --fix` giving back to you a
`.lockmq/` an older release gave to uid 10011 (a `chown` through Docker
Desktop's file sharing) is untested so far.

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
use `lockmq console`, `lockmq init`, `lockmq profile add` (formerly
`lockmq setup`, removed); the `lockmq-run` and
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

Add a broker profile:

```sh
lockmq profile add
```

It asks for the management API URL, the AMQP and stream URLs, the vhosts,
the broker user and where the password comes from, never for the password
itself, and appends the profile to `.lockmq/config.toml`. Flags give the
answers instead, and `--non-interactive` never asks
(`lockmq profile add --non-interactive --url http://host.docker.internal:15672 --user ro`;
[usage](usage.md#profiles)). Inside a container, `localhost` is the
container itself, and `profile add` knows it:

- a broker on your machine is `host.docker.internal` (the default), which
  the launcher maps to your machine;
- a broker in another container is reached by its name on a shared network
  (`LOCKMQ_DOCKER_ARGS="--network NAME" lockmq console`), as in
  [A broker in a container](#a-broker-in-a-container);
- `profile add` refuses `localhost`, `127.0.0.1` and `::1`;
- `credentials = "keychain"` needs an OS keychain, which the container does
  not have: use `credentials = "ask"`, or `"vault"` with the image below;
- a remote broker takes TLS URLs (`https`, `amqps`,
  `rabbitmq-stream+tls`), checked against the image's CA certificates
  (`tls = "verify-full"` by default); a production profile on a remote host
  refuses plain ones.

Then check the setup:

```sh
lockmq doctor --fix --yes
```

`.lockmq/` stays yours: `lox up` reads it as you and copies only your own
regular files and directories that neither group nor others can write
(lockhub's `docs/lox-up.md`, "Profiles: a reviewed copy"); the console reads
that copy, never the project, and `lox up` shows you every line of its
TOML files before the copy. `doctor` reports what `lox up` would refuse;
`--fix` takes group and others' write away from your files and
directories there, and gives back to you what an older `lockmq doctor
--fix` gave the console account (uid 10011: `.lockmq/` itself and a copy of
`pii.toml`). Those repairs run as root (the launcher starts `doctor --fix`
as root); every check then runs as `agent`, as a `doctor` without `--fix`
does. A link, or a file of another account, is never repaired: replace it
with your own. The repair enters each directory and never follows a
symbolic link. On a fresh setup `doctor` exits 0: the console's line is a
warning while `lox up` runs none.

## Run the console

The console runs hub-attached only: lockhub's `lox up` starts it, with its
hub flags (`lockmq console` on its own refuses to start: "launched by `lox
up` only"). From the workspace:

```sh
lox add lockmq events
lox up
lox attach events   # the console's own questions: policy review, password (detach: Ctrl-P Ctrl-Q)
lox approve         # the agent's reads and peeks
```

The rest of this page describes the launcher's own console container
(`lockmq console` with the hub flags `lox up` writes): its options,
`--profile P`, `--show-results` or `--allow-unmask`, and
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

The console account needs no `vault login`: at start the console asks
`Vault token for <VAULT_ADDR>:` in its terminal (no echo) and keeps the
token in memory for the session, never in `lockmq-home` or anywhere on
disk.

Get the token without storing it, then paste it at the prompt:

```sh
vault login -method=oidc -no-store -token-only
```

`-no-store -token-only` prints the token and writes no `~/.vault-token`.
A plain `vault login` would store the token in your account, where the
agent runs: the agent could read it and fetch the login from Vault itself,
bypassing the console (`lockmq doctor` fails on it). For the same reason,
do not leave `VAULT_TOKEN` set in that shell. When Vault refuses a read
(`not authenticated or not allowed`: the token is expired or revoked, or
its policy may not read `vault_path`), the console asks for a token again;
three refusals in a row stop the connection.

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

`lockmq doctor` runs as `agent` (`--fix` makes its repairs as root first); `docker run --rm --user lockmq -v
lockmq-run:/run/lockmq -v lockmq-home:/home/lockmq
ghcr.io/davidgodefroid/lockmq:X.Y.Z doctor` checks the console account.

## Agent in its own container

When the agent itself runs in a container that cannot reach the Docker
socket, the launcher cannot start the MCP server: the agent runs the
`lockmq` binary inside its own container instead, as a client of the
console container. The two containers share only the socket volume.

The console checks every peer by uid against its own container's accounts,
where the agent's uid has none: name it when you start the console, with
the release image as it is:

```sh
lockmq console --allowed-uids 1000
```

or at build time, in an image of your own:

```sh
docker build -t lockmq --build-arg ALLOWED_UIDS=1000 .
LOCKMQ_IMAGE=lockmq lockmq console
```

`--allowed-uids` and `ALLOWED_UIDS` are comma-separated lists of uids: the
build writes them to `allowed_uids` in the image's `/etc/lockmq/system.toml`,
the flag adds them to it for that console only (the console prints them at
start). Both refuse root (0), the console's own uid (10011) and 65534
(nobody, the kernel's overflow uid). Only whoever starts the console sets
them: the human, through the launcher (which passes the flag on) or
through `lox up` (lockhub v0.2.4 or later: a lock's `allowed_uids` in the
lockfamily.yaml `lox up` reviews with you); no client can. It is the uid the kernel reports
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
`lockmq-clients` (nor in `ALLOWED_UIDS` or `--allowed-uids`) is refused.

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
