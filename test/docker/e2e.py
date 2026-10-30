#!/usr/bin/env python3
"""End-to-end test of the lockmq image, following docs/docker.md.

A RabbitMQ container and the console container share a network; the
console runs in a pseudo-terminal this script answers (policy, password,
approvals); the agent's commands run in throwaway containers that share
only the socket volume and the project. It also checks the image's
accounts and modes, and that the console refuses a peer that is neither
lockmq nor in lockmq-clients.

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
RUN_VOL, HOME_VOL = "lockmq-e2e-run", "lockmq-e2e-home"
ADMIN, ADMIN_PASS = "admin", "e2e-admin-pass"
RO, RO_PASS = "lockmq-ro", "e2e-ro-pass"
MGMT = "http://127.0.0.1:15699"
PAYLOAD = '{"order":"e2e-1"}'

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
    sh("docker", "volume", "rm", "-f", RUN_VOL, HOME_VOL, check_=False)
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

    def __init__(self, project, *extra):
        self.out = b""
        self.lock = threading.Lock()
        self.answered = {}
        self.pid, self.fd = pty.fork()
        if self.pid == 0:
            os.execvp("docker", ["docker", "run", "-it", "--rm", "--name", CONSOLE, "--user", "lockmq",
                                 "--network", NET, "-v", f"{project}:{project}",
                                 "-v", f"{RUN_VOL}:/run/lockmq", "-v", f"{HOME_VOL}:/home/lockmq",
                                 IMAGE, "console", "--project", project, *extra])
        threading.Thread(target=self.pump, daemon=True).start()

    def pump(self):
        answers = [("Apply these changes? [y/N]", b"y\r"), ("Password for", RO_PASS.encode() + b"\r"),
                   ('Type "continue" to proceed', b"continue\r"), ("Approve? [y/N]", b"y\r")]
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
        answers = "\n".join(["dev", f"http://{BROKER}:15672", f"amqp://{BROKER}:5672", "", "", RO, "", "", ""]) + "\n"
        r = sh("docker", "run", "-i", "--rm", "--user", me, "-e", "HOME=/tmp", *mount, IMAGE, "setup", input=answers)
        check("host.docker.internal" in r.stdout and "keychain" not in r.stdout, "setup knows it runs in a container")
        r = sh("docker", "run", "--rm", "--user", "0", *mount, IMAGE, "doctor", "--fix", "--yes", check_=False)
        st = os.stat(os.path.join(project, ".lockmq"))
        check(st.st_uid == 10011 and (st.st_mode & 0o777) == 0o755, f"doctor --fix gives .lockmq to uid 10011, 0755\n{r.stdout}{r.stderr}")
        check(os.stat(os.path.join(project, ".lockmq", "config.toml")).st_mode & 0o004 != 0, "config.toml readable by the console")

        # Run the console.
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

        r = agent(project, "queues", "--json")
        check(r.returncode == 0 and "orders" in r.stdout, f"catalogue call (queues)\n{r.stdout}{r.stderr}")

        r = agent(project, "read", "orders", "--from", "first", "--max", "1", "--json")
        check(r.returncode == 0 and "e2e-1" in r.stdout, f"stream read approved in the console\n{r.stdout}{r.stderr}")
        check(con.answered.get("Approve? [y/N]", 0) >= 1, "the read was approved in the console")

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
    finally:
        cleanup()
    if failures:
        print(f"\n{len(failures)} failure(s)")
        sys.exit(1)
    print("\nall ok")


if __name__ == "__main__":
    main()
