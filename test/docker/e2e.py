#!/usr/bin/env python3
"""End-to-end test of the lockmq image, following docs/docker.md.

A RabbitMQ container and the console container share a network; the
console runs in a pseudo-terminal this script answers (policy, password),
attached to a hub as `lox run` starts it (the hub flags are required; the
hub named here is not running, so the first catalogue call waits for its
connect ticket, as does a read: the decision on a real hub is
lockhub's own end-to-end test); the agent's commands run in
throwaway containers that share only the socket volume and the project.
It also checks the image's accounts and modes, that the console refuses to
start without its hub, that it refuses a peer that is neither lockmq nor in
lockmq-clients, and that it enforces the company's managed file
(/etc/lockfamily/managed.toml, root-owned, in a volume of its own).

    python3 test/docker/e2e.py IMAGE

Needs docker and curl; uses the names lockmq-e2e-* and removes them.
Standard library only.
"""

import base64
import json
import os
import pty
import select
import subprocess
import sys
import tempfile
import threading
import time
import urllib.request

IMAGE = sys.argv[1] if len(sys.argv) > 1 else "lockmq"
NET, BROKER, CONSOLE = "lockmq-e2e-net", "lockmq-e2e-rabbit", "lockmq-e2e-console"
RUN_VOL, HOME_VOL, MANAGED_VOL = "lockmq-e2e-run", "lockmq-e2e-home", "lockmq-e2e-managed"
ADMIN, ADMIN_PASS = "admin", "e2e-admin-pass"
RO, RO_PASS = "lockmq-ro", "e2e-ro-pass"
MGMT = "http://127.0.0.1:15699"
PAYLOAD = '{"order":"e2e-1"}'
HUB_URL, INSTANCE = "http://lockmq-e2e-hub:8443", "mq-e2e"

failures = []


def check(ok, what):
    print(("ok   " if ok else "FAIL ") + what, flush=True)
    if not ok:
        failures.append(what)


def sh(*args, check_=True, **kw):
    r = subprocess.run(args, capture_output=True, text=True, **kw)
    if check_ and r.returncode != 0:
        raise SystemExit(f"{' '.join(args)}: exit {r.returncode}\n{r.stdout}{r.stderr}")
    return r


def api(method, path, body=None):
    req = urllib.request.Request(MGMT + path, method=method,
                                 data=None if body is None else json.dumps(body).encode())
    req.add_header("Authorization", "Basic " + base64.b64encode(f"{ADMIN}:{ADMIN_PASS}".encode()).decode())
    req.add_header("Content-Type", "application/json")
    with urllib.request.urlopen(req, timeout=10) as r:
        return r.status, r.read()


def cleanup():
    sh("docker", "rm", "-f", CONSOLE, BROKER, check_=False)
    sh("docker", "volume", "rm", "-f", RUN_VOL, HOME_VOL, MANAGED_VOL, check_=False)
    sh("docker", "network", "rm", NET, check_=False)


def image_checks():
    def in_image(script):
        return sh("docker", "run", "--rm", "--user", "0", "--entrypoint", "sh", IMAGE, "-c", script).stdout.strip()

    who = sh("docker", "run", "--rm", "--entrypoint", "id", IMAGE, "-un").stdout.strip()
    check(who == "agent", f"default user is agent ({who})")
    check(in_image("stat -c '%a %U:%G' /run/lockmq") == "2710 lockmq:lockmq-clients", "socket dir 2710 lockmq:lockmq-clients")
    check(in_image("stat -c '%a %U' /home/lockmq") == "700 lockmq", "console home 0700")
    check(in_image("stat -c '%a %U:%G' /usr/local/bin/lockmq /etc/lockmq/system.toml") == "755 root:root\n644 root:root",
          "binary and system.toml root-owned")
    check(in_image("id -Gn lockmq") == "lockmq", "console account not in lockmq-clients")
    check(in_image("id -Gn agent | tr ' ' '\\n' | grep -x lockmq-clients") == "lockmq-clients", "agent in lockmq-clients")
    check(in_image("command -v sudo su-exec doas || true") == "", "no sudo in the image")
    check(in_image("grep -c allowed_uids /etc/lockmq/system.toml || true") == "0", "no allowed_uids by default")


def start_broker():
    sh("docker", "network", "create", NET)
    sh("docker", "run", "-d", "--name", BROKER, "--network", NET, "-p", "127.0.0.1:15699:15672",
       "-e", f"RABBITMQ_DEFAULT_USER={ADMIN}", "-e", f"RABBITMQ_DEFAULT_PASS={ADMIN_PASS}",
       "rabbitmq:4.1-management")
    deadline = time.time() + 180
    while True:
        try:
            if api("GET", "/api/overview")[0] == 200:
                break
        except Exception:
            pass
        if time.time() > deadline:
            raise SystemExit("broker not ready:\n" + sh("docker", "logs", BROKER, check_=False).stdout[-3000:])
        time.sleep(2)
    api("PUT", f"/api/users/{RO}", {"password": RO_PASS, "tags": "monitoring"})
    api("PUT", f"/api/permissions/%2F/{RO}", {"configure": "^$", "write": "^$", "read": "^(orders)$"})
    api("PUT", "/api/queues/%2F/orders", {"durable": True, "arguments": {"x-queue-type": "stream"}})
    api("POST", "/api/exchanges/%2F/amq.default/publish",
        {"properties": {}, "routing_key": "orders", "payload": PAYLOAD, "payload_encoding": "string"})


class Console:
    """The console in a pseudo-terminal, answering its prompts."""

    def __init__(self, project, *extra, docker=()):
        hub = ["--hub", HUB_URL, "--hub-plain-http", "--hub-key-file", os.path.join(project, "hub.pub"), "--instance", INSTANCE]
        self.out = b""
        self.lock = threading.Lock()
        self.answered = {}
        self.pid, self.fd = pty.fork()
        if self.pid == 0:
            os.execvp("docker", ["docker", "run", "-it", "--rm", "--name", CONSOLE, "--user", "lockmq",
                                 "--network", NET, "-v", f"{project}:{project}",
                                 "-v", f"{RUN_VOL}:/run/lockmq", "-v", f"{HOME_VOL}:/home/lockmq", *docker,
                                 IMAGE, "console", "--project", project, *hub, *extra])
        threading.Thread(target=self.pump, daemon=True).start()

    def pump(self):
        answers = [("Apply these changes? [y/N]", b"y\r"), ("Password for", RO_PASS.encode() + b"\r"),
                   ('Type "continue" to proceed', b"continue\r")]
        while True:
            try:
                r, _, _ = select.select([self.fd], [], [], 0.2)
                if not r:
                    continue
                chunk = os.read(self.fd, 4096)
            except OSError:
                return
            if not chunk:
                return
            with self.lock:
                self.out += chunk
                text = self.out.decode("utf-8", "replace")
                for prompt, answer in answers:
                    n = text.count(prompt)
                    if n > self.answered.get(prompt, 0):
                        self.answered[prompt] = n
                        os.write(self.fd, answer)

    def text(self):
        with self.lock:
            return self.out.decode("utf-8", "replace")

    def stop(self):
        sh("docker", "rm", "-f", CONSOLE, check_=False)


def write_managed(body, mode="0644"):
    """Writes the managed file as root into its volume: 0:0, mode, the
    directory 0755 (a company's MDM or package does the same on a host)."""
    sh("docker", "run", "--rm", "-i", "--user", "0", "-v", f"{MANAGED_VOL}:/etc/lockfamily", "--entrypoint", "sh", IMAGE, "-c",
       "cat > /etc/lockfamily/managed.toml && chown 0:0 /etc/lockfamily /etc/lockfamily/managed.toml && "
       f"chmod 0755 /etc/lockfamily && chmod {mode} /etc/lockfamily/managed.toml", input=body)


def managed_checks(project):
    """[lockmq.bounds] tier = "read": the console prints the managed line
    and serves a read profile; managed check exits 3 on a peek profile; a
    managed file others can write is a refusal to start."""
    mount = ["-v", f"{MANAGED_VOL}:/etc/lockfamily:ro"]
    write_managed('version = 1\norganisation = "Acme E2E"\n\n[lockmq.bounds]\ntier = "read"\n')
    con = Console(project, docker=mount)
    deadline, up = time.time() + 120, False
    while time.time() < deadline:
        if agent(project, "status").returncode == 0:
            up = True
            break
        time.sleep(2)
    check(up and "Managed by Acme E2E (sha256:" in con.text(), "managed: the console prints the managed line and serves a read profile")
    con.stop()
    if failures:
        print(con.text()[-4000:])
    sh("docker", "wait", CONSOLE, check_=False)

    peek = tempfile.mkdtemp(prefix="lockmq-e2e-peek-")
    os.chmod(peek, 0o755)
    os.makedirs(os.path.join(peek, ".lockmq"))
    with open(os.path.join(peek, ".lockmq", "config.toml"), "w") as f:
        f.write(f'[profiles.dev]\nurl = "http://{BROKER}:15672"\ntier = "peek"\n')
    r = sh("docker", "run", "--rm", "--user", f"{os.getuid()}:{os.getgid()}", "-e", "HOME=/tmp", *mount,
           "-v", f"{peek}:{peek}", "-w", peek, IMAGE, "managed", "check", check_=False)
    check(r.returncode == 3 and r.stdout.strip() == "blocked by company policy: lockmq.bounds.tier = read (profile dev: tier = peek)",
          f"managed check exits 3 on a peek profile under tier = read\n{r.stdout}{r.stderr}")

    write_managed('version = 1\norganisation = "Acme E2E"\n\n[lockmq.bounds]\ntier = "read"\n', mode="0666")
    con = Console(project, docker=mount)
    deadline = time.time() + 60
    while time.time() < deadline and "must be writable by root only" not in con.text():
        time.sleep(1)
    check("must be writable by root only" in con.text() and agent(project, "status").returncode != 0,
          "a managed file others can write is a refusal to start")
    con.stop()
    if failures:
        print(con.text()[-4000:])
    sh("docker", "wait", CONSOLE, check_=False)


def agent(project, *args, user=None, extra=()):
    cmd = ["docker", "run", "--rm", "-v", f"{project}:{project}", "-w", project, "-v", f"{RUN_VOL}:/run/lockmq"]
    if user:
        cmd += ["--user", user]
    cmd += list(extra) + [IMAGE] + list(args)
    return subprocess.run(cmd, capture_output=True, text=True, timeout=360)


def main():
    cleanup()
    try:
        image_checks()
        start_broker()
        project = tempfile.mkdtemp(prefix="lockmq-e2e-")
        os.chmod(project, 0o755)  # the console account enters the project
        me = f"{os.getuid()}:{os.getgid()}"
        mount = ["-v", f"{project}:{project}", "-w", project]

        # Set up a project (docs/docker.md).
        r = sh("docker", "run", "--rm", "--user", me, "-e", "HOME=/tmp", *mount, IMAGE, "init", "claude")
        check(os.path.exists(os.path.join(project, ".lockmq", "config.toml")), "init writes .lockmq/config.toml")
        check(os.stat(os.path.join(project, ".lockmq")).st_uid == os.getuid(), "init files belong to the caller")
        # lox add's calls: profile add --non-interactive --json, doctor --fix --yes --json.
        r = sh("docker", "run", "--rm", "--user", me, "-e", "HOME=/tmp", *mount, IMAGE, "profile", "add",
               "--non-interactive", "--json", "--profile", "dev", "--url", f"http://{BROKER}:15672",
               "--amqp-url", f"amqp://{BROKER}:5672", "--user", RO)
        prof = json.loads(r.stdout)
        check(prof.get("profile") == "dev" and prof.get("credentials") == "ask" and prof.get("lock") == "lockmq",
              f"profile add --non-interactive --json writes the profile\n{r.stdout}{r.stderr}")
        r = sh("docker", "run", "--rm", "--user", me, "-e", "HOME=/tmp", *mount, IMAGE, "profile", "add",
               "--non-interactive", "--json", "--profile", "dev", "--url", f"http://{BROKER}:15672", check_=False)
        check(r.returncode != 0 and json.loads(r.stdout).get("code") == "exists", f"profile add keeps an existing profile\n{r.stdout}{r.stderr}")
        # As the launcher runs it: root for the repairs, your uid for the
        # checks of .lockmq, which then run as the agent account.
        host_uid = ["-e", f"LOCKMQ_HOST_UID={os.getuid()}"]
        r = sh("docker", "run", "--rm", "--user", "0", *host_uid, *mount, IMAGE, "doctor", "--fix", "--yes", "--json", check_=False)
        doc = json.loads(r.stdout)
        check(r.returncode == 0 and doc.get("ok") is True, f"doctor --fix --yes --json passes on a fresh setup\n{r.stdout}{r.stderr}")

        def lox_up_copies(why):
            # lox run copies only your own files and directories that no one
            # else can write (lockhub's docs/lox-run.md).
            bad = []
            for d, dirs, files in os.walk(os.path.join(project, ".lockmq")):
                for p in [d] + [os.path.join(d, f) for f in files]:
                    st = os.lstat(p)
                    if st.st_uid != os.getuid() or st.st_mode & 0o022:
                        bad.append(f"{p} uid {st.st_uid} mode {st.st_mode & 0o777:04o}")
            check(not bad, f"{why}: .lockmq is yours, written by you only {bad}\n{r.stdout}{r.stderr}")

        lox_up_copies("doctor --fix")
        # What an older doctor --fix left (.lockmq and pii.toml given to the
        # console account) is given back to you.
        with open(os.path.join(project, ".lockmq", "pii.toml"), "w") as f:
            f.write("[[mask]]\n")
        sh("docker", "run", "--rm", "--user", "0", "--entrypoint", "chown", *mount, IMAGE, "10011",
           os.path.join(project, ".lockmq"), os.path.join(project, ".lockmq", "pii.toml"))
        r = sh("docker", "run", "--rm", *host_uid, *mount, IMAGE, "doctor", "--json", check_=False)
        check(r.returncode == 1 and "as an older lockmq doctor --fix left it" in r.stdout, f"doctor reports the console account's .lockmq\n{r.stdout}{r.stderr}")
        r = sh("docker", "run", "--rm", "--user", "0", *host_uid, *mount, IMAGE, "doctor", "--fix", "--yes", "--json", check_=False)
        doc = json.loads(r.stdout)
        check(r.returncode == 0 and doc.get("ok") is True and doc.get("fixed"), f"doctor --fix gives it back\n{r.stdout}{r.stderr}")
        lox_up_copies("doctor --fix after an older one")
        os.remove(os.path.join(project, ".lockmq", "pii.toml"))

        # Without its hub the console refuses to start: launched by lox run only.
        r = sh("docker", "run", "--rm", "--user", "lockmq", "-v", f"{project}:{project}",
               IMAGE, "console", "--project", project, check_=False)
        check(r.returncode != 0 and "launched by `lox run` only" in r.stdout + r.stderr, f"console without --hub refused\n{r.stdout}{r.stderr}")

        # Run the console, attached to a hub (its pinned key; the hub itself
        # is not running).
        with open(os.path.join(project, "hub.pub"), "w") as f:
            f.write(base64.b64encode(os.urandom(32)).decode() + "\n")
        os.chmod(os.path.join(project, "hub.pub"), 0o644)
        con = Console(project)
        deadline, up = time.time() + 120, False
        while time.time() < deadline:
            if agent(project, "status").returncode == 0:
                up = True
                break
            time.sleep(2)
        check(up, "lockmq status from the agent's container")
        if not up:
            print(con.text()[-4000:])
            raise SystemExit("the console did not come up")

        # The broker session serves the agent only once its connect ticket
        # is approved on the hub, which is not running: the first request
        # waits for the decision (fail closed) and returns nothing.
        r = agent(project, "8", "lockmq", "queues", "--json", extra=["--entrypoint", "timeout"])
        # Ended by timeout's SIGTERM while waiting ("context canceled"),
        # not refused for another cause.
        check(r.returncode != 0 and "context canceled" in r.stderr and "orders" not in r.stdout, f"catalogue call waits for its connect ticket, nothing listed\n{r.stdout}{r.stderr}")
        check("waits for its connect ticket" in con.text() and "auto.connect=ask" in con.text(), "the console names the connect ticket and its rule")

        # A stream read reads the queue's properties first: it waits for the
        # connect ticket too, and nothing is read.
        r = agent(project, "8", "lockmq", "read", "orders", "--from", "first", "--max", "1", "--json", extra=["--entrypoint", "timeout"])
        check(r.returncode != 0 and "context canceled" in r.stderr and "e2e-1" not in r.stdout, f"stream read waits for its connect ticket, nothing read\n{r.stdout}{r.stderr}")
        check("Approve? [y/N]" not in con.text(), "the read is never asked in the console")
        check(f"Requests are decided on lockhub at {HUB_URL} as {INSTANCE}" in con.text(), "the console names its hub")

        # Peers outside the setup.
        r = agent(project, "status", user="4242")
        check(r.returncode != 0, f"uid 4242, no group: refused (exit {r.returncode})")
        r = agent(project, "status", user="4242", extra=["--group-add", "10010"])
        check(r.returncode != 0, f"uid 4242 with gid 10010 but no account: refused by the console (exit {r.returncode}) {r.stderr.strip()}")
        # The console's own account may connect (lockkit's peer check: it
        # already holds the credentials); no other account outside the group.
        r = agent(project, "status", user="10011")
        check(r.returncode == 0, f"the console's own uid is accepted, as lockkit allows (exit {r.returncode})")
        check(agent(project, "status").returncode == 0, "console still serves the agent")

        # No credential reaches the agent's side.
        r = agent(project, "sh", "-c", "grep -rl " + RO_PASS + " /run/lockmq . 2>/dev/null; true", extra=["--entrypoint", ""])
        check(r.stdout.strip() == "", "no password in the socket volume or the project")
        con.stop()
        if failures:
            print(con.text()[-4000:])

        # --allowed-uids: the release image as it is serves the uid of an
        # agent in a container of its own (no account in the image), for
        # this console only; no other such uid.
        sh("docker", "wait", CONSOLE, check_=False)
        con = Console(project, "--allowed-uids", "4242")
        out4242 = None
        deadline = time.time() + 120
        while time.time() < deadline:
            r = agent(project, "status", user="4242", extra=["--group-add", "10010"])
            if r.returncode == 0:
                out4242 = r
                break
            time.sleep(2)
        check(out4242 is not None, "--allowed-uids 4242: uid 4242 with gid 10010 served")
        check("allowed uids besides lockmq-clients (--allowed-uids): 4242" in con.text(), "the console prints the allowed uids")
        r = agent(project, "status", user="4243", extra=["--group-add", "10010"])
        check(r.returncode != 0, f"--allowed-uids 4242: uid 4243 still refused (exit {r.returncode})")
        check(agent(project, "status").returncode == 0, "--allowed-uids: the image's agent still served")
        con.stop()
        if failures:
            print(con.text()[-4000:])
        r = sh("docker", "run", "--rm", "--user", "lockmq", "-v", f"{RUN_VOL}:/run/lockmq", "-v", f"{HOME_VOL}:/home/lockmq",
               IMAGE, "console", "--allowed-uids", "0", check_=False)
        check(r.returncode != 0 and "leading zero" in r.stdout + r.stderr, f"--allowed-uids 0 refused\n{r.stdout}{r.stderr}")

        sh("docker", "wait", CONSOLE, check_=False)
        managed_checks(project)
    finally:
        cleanup()
    if failures:
        print(f"\n{len(failures)} failure(s)")
        sys.exit(1)
    print("\nall ok")


if __name__ == "__main__":
    main()
