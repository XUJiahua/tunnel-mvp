#!/usr/bin/env python3
"""Run a disposable Docker Compose HA/TLS deployment acceptance lab.

Requires a source checkout, Docker Compose v2, Python 3.11+, OpenSSL and free
loopback ports 28443-28445. Never connects to an existing deployment/database.
"""
import argparse
import base64
import hashlib
import http.client
import http.cookiejar
import json
import os
from pathlib import Path
import secrets
import shutil
import socket
import ssl
import subprocess
import sys
import time
import urllib.error
import urllib.parse
import urllib.request

ROOT = Path(__file__).resolve().parents[1]
PUBLIC = "https://console.tunnel.test:28443"


def command(*args, timeout=120, capture=True):
    return subprocess.run(args, check=True, text=True, timeout=timeout,
                          stdout=subprocess.PIPE if capture else None,
                          stderr=subprocess.PIPE if capture else None).stdout


def eventually(check, description, seconds=45):
    deadline = time.monotonic() + seconds
    last = None
    while time.monotonic() < deadline:
        try:
            result = check()
            if result:
                return result
        except (OSError, ValueError, urllib.error.URLError, http.client.HTTPException) as error:
            last = type(error).__name__
        time.sleep(0.3)
    raise AssertionError(f"Timed out: {description} (last error: {last})")


class Client:
    """Real verified TLS/SNI/Host and cookies; resolve only lab names to loopback."""
    def __init__(self, ca):
        self.context = ssl.create_default_context(cafile=str(ca))
        self.cookies = http.cookiejar.CookieJar()

    def opener(self, port):
        class Connection(http.client.HTTPSConnection):
            def connect(inner):
                if not (inner.host == "console.tunnel.test" or inner.host.endswith(".apps.tunnel.test")):
                    raise ValueError("Redirect escaped the lab DNS namespace")
                plain = socket.create_connection(("127.0.0.1", port), inner.timeout)
                inner.sock = inner._context.wrap_socket(plain, server_hostname=inner.host)

        class Handler(urllib.request.HTTPSHandler):
            def https_open(inner, request):
                return inner.do_open(Connection, request, context=self.context)

        class Redirect(urllib.request.HTTPRedirectHandler):
            def redirect_request(inner, request, fp, code, message, headers, newurl):
                target = urllib.parse.urlsplit(newurl)
                if target.scheme != "https" or target.port != 28443 or not (
                    target.hostname == "console.tunnel.test" or target.hostname.endswith(".apps.tunnel.test")
                ):
                    raise ValueError("Redirect escaped the lab HTTPS namespace")
                return super().redirect_request(request, fp, code, message, headers, newurl)

        return urllib.request.build_opener(urllib.request.ProxyHandler({}), Handler(),
                                          Redirect(), urllib.request.HTTPCookieProcessor(self.cookies))

    def open(self, path, body=None, port=28443):
        url = path if path.startswith("https://") else PUBLIC + path
        data = json.dumps(body).encode() if body is not None else None
        request = urllib.request.Request(url, data=data, headers={
            "Origin": PUBLIC, "Content-Type": "application/json"})
        return self.opener(port).open(request, timeout=5)

    def api(self, path, body=None, port=28443):
        with self.open("/api" + path, body, port) as response:
            return json.load(response)

    def status(self, path, port=28443):
        try:
            with self.open(path, port=port) as response:
                return response.status
        except urllib.error.HTTPError as error:
            error.close()
            return error.code

    def websocket(self, url):
        parsed = urllib.parse.urlsplit(url)
        request = urllib.request.Request(url)
        self.cookies.add_cookie_header(request)
        key = base64.b64encode(secrets.token_bytes(16)).decode()
        with socket.create_connection(("127.0.0.1", 28443), timeout=5) as plain:
            with self.context.wrap_socket(plain, server_hostname=parsed.hostname) as secure:
                headers = (f"GET /ws HTTP/1.1\r\nHost: {parsed.netloc}\r\n"
                           f"Upgrade: websocket\r\nConnection: Upgrade\r\n"
                           f"Sec-WebSocket-Key: {key}\r\nSec-WebSocket-Version: 13\r\n"
                           f"Origin: https://{parsed.netloc}\r\nCookie: {request.get_header('Cookie', '')}\r\n\r\n")
                secure.sendall(headers.encode())
                stream = secure.makefile("rb")
                assert b" 101 " in stream.readline(), "application WebSocket upgrade failed"
                received = {}
                while True:
                    line = stream.readline()
                    if line == b"\r\n":
                        break
                    assert line, "WebSocket upgrade ended before headers"
                    name, value = line.decode().split(":", 1)
                    received[name.lower()] = value.strip()
                expected = base64.b64encode(hashlib.sha1((key + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11").encode()).digest()).decode()
                assert received.get("sec-websocket-accept") == expected
                payload, mask = b"deployment-websocket", secrets.token_bytes(4)
                secure.sendall(bytes([0x81, 128 | len(payload)]) + mask +
                               bytes(byte ^ mask[i % 4] for i, byte in enumerate(payload)))
                assert stream.read(2) == bytes([0x81, len(payload)])
                assert stream.read(len(payload)) == payload, "application WebSocket echo failed"
                stream.close()


class Lab:
    def __init__(self, runtime, postgres):
        self.runtime = runtime
        self.project = "tunnel-lab-" + secrets.token_hex(6)
        self.password = secrets.token_urlsafe(24)
        self.db_password = secrets.token_hex(24)
        self.redactions = [self.password, self.db_password]
        self.postgres = postgres
        self.results = []

    def compose(self, *args, **kwargs):
        return command("docker", "compose", "--project-name", self.project,
                       "--env-file", str(self.runtime / "environment"),
                       "-f", str(ROOT / "examples/ha-lab/compose.yaml"), *args, **kwargs)

    def prepare(self):
        # Refuse overwriting any runtime; all writes stay under this new directory.
        self.runtime.mkdir(mode=0o700)
        for name in ["node-a", "node-b"]:
            (self.runtime / name).mkdir(mode=0o700)
        commit = command("git", "-C", str(ROOT), "rev-parse", "HEAD").strip()
        if command("git", "-C", str(ROOT), "status", "--porcelain").strip():
            commit += "-dirty"
        self.commit = commit
        command("bash", str(ROOT / "scripts/create-cluster-pki.sh"), str(self.runtime / "pki"),
                "node-a=node-a", "node-b=node-b")
        for node in ["node-a", "node-b"]:
            shutil.copyfile(self.runtime / "pki/ca.crt", self.runtime / "pki" / node / "ca.crt")
        # Separate trust roots for the public TLS listener and private cluster.
        command("openssl", "req", "-new", "-newkey", "ec", "-pkeyopt", "ec_paramgen_curve:P-256",
                "-nodes", "-x509", "-sha256", "-days", "2", "-subj", "/CN=Disposable public lab CA",
                "-addext", "basicConstraints=critical,CA:TRUE", "-addext", "keyUsage=critical,keyCertSign,cRLSign",
                "-keyout", str(self.runtime / "public-ca.key"), "-out", str(self.runtime / "public-ca.crt"))
        command("openssl", "req", "-new", "-newkey", "ec", "-pkeyopt", "ec_paramgen_curve:P-256",
                "-nodes", "-subj", "/CN=console.tunnel.test", "-keyout", str(self.runtime / "public.key"),
                "-out", str(self.runtime / "public.csr"))
        (self.runtime / "public.ext").write_text("basicConstraints=critical,CA:FALSE\nkeyUsage=critical,digitalSignature\nextendedKeyUsage=serverAuth\nsubjectAltName=DNS:console.tunnel.test,DNS:apps.tunnel.test,DNS:*.apps.tunnel.test\n")
        command("openssl", "x509", "-req", "-in", str(self.runtime / "public.csr"),
                "-CA", str(self.runtime / "public-ca.crt"), "-CAkey", str(self.runtime / "public-ca.key"),
                "-set_serial", "0x" + secrets.token_hex(16), "-days", "2", "-sha256",
                "-extfile", str(self.runtime / "public.ext"), "-out", str(self.runtime / "public.crt"))
        (self.runtime / "public.pem").write_bytes((self.runtime / "public.crt").read_bytes() + (self.runtime / "public.key").read_bytes())
        for letter in "ab":
            node = "node-" + letter
            config = {"listen": ":8080", "publicURL": PUBLIC,
                      "accessURL": "https://apps.tunnel.test:28443", "storage": "postgres",
                      "dataDir": "/var/lib/tunnel", "probeIntervalSeconds": 0,
                      "cluster": {"enabled": True, "nodeID": node, "listen": ":8443",
                                  "advertiseURL": f"https://{node}:8443", "caFile": "/etc/tunnel/pki/ca.crt",
                                  "certFile": "/etc/tunnel/pki/node.crt", "keyFile": "/etc/tunnel/pki/node.key"}}
            (self.runtime / f"server-{letter}.json").write_text(json.dumps(config))
        (self.runtime / "database-password").write_text(self.db_password)
        (self.runtime / "agent.json").write_text("{}")
        (self.runtime / "agent.env").write_text("")
        environment = {"LAB_RUNTIME": str(self.runtime), "LAB_UID": str(os.getuid()), "LAB_GID": str(os.getgid()),
                       "LAB_IMAGE": self.project + ":acceptance", "LAB_COMMIT": commit,
                       "LAB_POSTGRES_VERSION": self.postgres, "LAB_ADMIN_PASSWORD": self.password,
                       "LAB_POSTGRES_DSN": f"postgres://tunnel:{self.db_password}@database:5432/tunnel?sslmode=disable"}
        # Compose .env single quoting protects paths containing spaces, $, #, etc.
        for value in environment.values():
            if any(char in value for char in "'\r\n"):
                raise ValueError("Lab runtime path cannot contain quotes or newlines")
        (self.runtime / "environment").write_text("".join(f"{key}='{value}'\n" for key, value in environment.items()))
        self.client = Client(self.runtime / "public-ca.crt")

    def record(self, name, action):
        print(f"RUN {name}", flush=True)
        start = time.monotonic()
        evidence = action()
        self.results.append({"name": name, "passed": True, "seconds": round(time.monotonic() - start, 3), "evidence": evidence})
        print(f"PASS {name}", flush=True)

    def converged(self):
        def ready():
            nodes = self.client.api("/cluster/status")["nodes"]
            return len(nodes) == 2 and all(node["alive"] and node["fresh"] and not node["draining"] and
                                          node.get("report", {}).get("backlog", {}).get("pending", -1) == 0 for node in nodes) and all(
                self.client.api("/cluster/status", port=port)["localNodeID"] == node
                for port, node in [(28444, "node-a"), (28445, "node-b")])
        eventually(ready, "both nodes reporting with empty observation queues")

    def health(self, port):
        return self.client.status("/healthz", port)

    def prepare_application(self):
        self.client.api("/login", {"username": "admin", "password": self.password}, 28444)
        # Same secure console cookie must work on the other actual listener.
        assert self.client.status("/api/me", 28445) == 200
        assert self.client.api("/cluster/status", port=28444)["localNodeID"] == "node-a"
        assert self.client.api("/cluster/status", port=28445)["localNodeID"] == "node-b"
        site = self.client.api("/sites", {"name": "Disposable container site"})["site"]
        self.app = self.client.api("/services", {"name": "Private lab application", "siteID": site["id"], "url": "http://application:8080"})
        registration = self.client.api(f"/sites/{site['id']}/agents", {"name": "lab-agent"})
        self.agent_id = registration["agent"]["id"]
        self.redactions.append(registration["token"])
        (self.runtime / "agent.json").write_text(json.dumps({"serverURL": PUBLIC, "siteID": site["id"],
            "instanceID": self.agent_id, "allow": [{"cidr": "0.0.0.0/0", "host": "application", "ports": [8080]}]}))
        (self.runtime / "agent.env").write_text("TUNNEL_AGENT_TOKEN=" + registration["token"] + "\n")
        self.compose("--profile", "agents", "up", "-d", "agent")
        self.online()
        self.converged()
        self.launch()
        self.client.websocket(self.app_url)
        # Since the server has no site network, this request necessarily uses Agent.
        return {"sharedSecureSession": True, "privateApplicationHTTP": True, "applicationWebSocket": True}

    def online(self):
        eventually(lambda: any(agent["id"] == self.agent_id and agent["online"] for agent in self.client.api("/agents")), "Agent reconnected", 60)

    def launch(self):
        result = self.client.api(f"/services/{self.app['id']}/launch", {})
        with self.client.open(result["url"]) as response:
            assert response.read() == b"ha-deployment-live"
            self.app_url = response.url.rstrip("/")

    def read_app(self):
        with self.client.open(self.app_url + "/") as response:
            return response.read() == b"ha-deployment-live"

    def post_once(self):
        with self.client.open(self.app_url + "/count") as response:
            before = json.load(response)["commits"]
        with self.client.open(self.app_url + "/commit", {"nonce": secrets.token_hex(8)}) as response:
            assert response.status == 200
        with self.client.open(self.app_url + "/count") as response:
            assert json.load(response)["commits"] == before + 1

    def drain(self):
        stream = self.client.open(self.app_url + "/events")
        def event():
            for _ in range(10):
                line = stream.readline()
                if line.startswith(b"data: "):
                    return int(line[6:])
                assert line, "SSE ended unexpectedly"
            raise AssertionError("SSE did not deliver data")
        try:
            before = event()
            try:
                self.client.api("/cluster/local/drain", {"nodeID": "node-a", "draining": True}, 28445)
                raise AssertionError("misdirected drain was accepted")
            except urllib.error.HTTPError as error:
                assert error.code == 409
                error.close()
            self.client.api("/cluster/local/drain", {"nodeID": "node-a", "draining": True}, 28444)
            eventually(lambda: self.health(28444) == 503, "drained node health is 503")
            # Give active checks time to remove A; use fresh TLS/HTTP connections.
            eventually(self.read_app, "HAProxy routes new application requests away from drained A")
            for _ in range(6):
                assert event() > before, "SSE stopped during drain"
            # Every public management request must now be answered by B.
            eventually(lambda: all(self.client.api("/cluster/status")["localNodeID"] == "node-b" for _ in range(8)), "active health checks select only B")
            self.post_once()
        finally:
            stream.close()
            self.client.api("/cluster/local/drain", {"nodeID": "node-a", "draining": False}, 28444)
        self.converged()
        return {"wrongNodeStatus": 409, "drainedHealth": 503, "SSEContinued": True, "newRequestsOnB": True, "POSTExecutedOnce": True}

    def kill_owner(self):
        # HAProxy sends Agent connections to B first and application requests to A.
        # Node B's report must prove that the real Agent is attached there.
        def owner_b():
            nodes = self.client.api("/cluster/status")["nodes"]
            return any(node["nodeID"] == "node-b" and node.get("report", {}).get("agents", 0) > 0 for node in nodes)
        eventually(owner_b, "Agent initially owned by B")
        self.compose("kill", "-s", "SIGKILL", "node-b")
        self.online()
        eventually(self.read_app, "application resumes through surviving A", 60)
        self.post_once()
        self.compose("start", "node-b")
        eventually(lambda: self.health(28445) == 200, "B restarted")
        self.converged()
        return {"ownerKilled": "node-b", "AgentReconnected": True, "applicationRecovered": True, "POSTExecutedOnceAfterRecovery": True}

    def database_outage(self):
        # Pause one disposable database; no TCP error is needed for local fencing.
        self.compose("pause", "database")
        start = time.monotonic()
        try:
            eventually(lambda: self.health(28444) == 503 and self.health(28445) == 503,
                       "both nodes fence during database outage", 18)
            fenced_after = round(time.monotonic() - start, 3)
            status = self.client.status(self.app_url + "/")
            assert status == 503, f"application remained accessible during fencing: {status}"
        finally:
            self.compose("unpause", "database")
        # Fenced processes stay closed; restart both after DB is authoritative.
        self.compose("restart", "node-a", "node-b")
        eventually(lambda: self.health(28444) == 200 and self.health(28445) == 200, "nodes restarted after database restored", 60)
        self.online()
        self.converged()
        eventually(self.read_app, "application after database recovery", 60)
        self.post_once()
        return {"databaseFault": "pause", "bothNodesFencedAfterSeconds": fenced_after,
                "applicationDeniedStatus": status, "explicitNodeRestart": True, "POSTExecutedOnceAfterRecovery": True}

    def rotate_certificate(self):
        node_dir = self.runtime / "pki/node-b"
        before = hashlib.sha256((node_dir / "node.crt").read_bytes()).hexdigest()
        eventually(lambda: self.client.api("/cluster/status", port=28445)["localNodeID"] == "node-b", "B directed management listener recovered")
        self.client.api("/cluster/local/drain", {"nodeID": "node-b", "draining": True}, 28445)
        eventually(lambda: all(self.client.api("/cluster/status")["localNodeID"] == "node-a" for _ in range(8)), "LB removed B for certificate maintenance")
        self.compose("stop", "node-b")
        command("openssl", "req", "-new", "-newkey", "ec", "-pkeyopt", "ec_paramgen_curve:P-256", "-nodes",
                "-subj", "/CN=node-b", "-keyout", str(node_dir / "node.key"), "-out", str(node_dir / "rotate.csr"))
        (node_dir / "rotate.ext").write_text("basicConstraints=critical,CA:FALSE\nkeyUsage=critical,digitalSignature\nextendedKeyUsage=serverAuth,clientAuth\nsubjectAltName=URI:spiffe://tunnel-mvp/node/node-b,DNS:node-b\n")
        command("openssl", "x509", "-req", "-in", str(node_dir / "rotate.csr"), "-CA", str(self.runtime / "pki/ca.crt"),
                "-CAkey", str(self.runtime / "pki/ca.key"), "-set_serial", "0x" + secrets.token_hex(16),
                "-days", "90", "-sha256", "-extfile", str(node_dir / "rotate.ext"), "-out", str(node_dir / "node.crt"))
        for purpose in ["sslserver", "sslclient"]:
            command("openssl", "verify", "-CAfile", str(self.runtime / "pki/ca.crt"), "-purpose", purpose, str(node_dir / "node.crt"))
        (node_dir / "rotate.csr").unlink()
        (node_dir / "rotate.ext").unlink()
        self.compose("start", "node-b")
        eventually(lambda: self.health(28445) == 200, "B accepts rotated certificate")
        self.converged()
        # New Agent connection must prefer B again, forcing A -> B private mTLS.
        self.compose("restart", "agent")
        self.online()
        eventually(lambda: any(node["nodeID"] == "node-b" and node.get("report", {}).get("agents", 0) > 0
                               for node in self.client.api("/cluster/status")["nodes"]), "rotated B owns Agent again")
        self.launch()
        self.client.websocket(self.app_url)
        after = hashlib.sha256((node_dir / "node.crt").read_bytes()).hexdigest()
        assert after != before
        return {"sameCA": True, "sameNodeID": "node-b", "leafChanged": True, "verifiedPrivateTLSAfterRotation": True}

    def run(self):
        self.compose("config", "--quiet")
        self.compose("build", "node-a", timeout=900, capture=False)
        self.compose("up", "-d", "database", "node-a", "node-b", "application", "edge", timeout=180, capture=False)
        eventually(lambda: self.health(28444) == 200 and self.health(28445) == 200, "initial nodes ready", 60)
        version = self.compose("exec", "-T", "node-a", "/app/tunnel", "version").strip()
        assert self.commit in version, "container binary does not identify the built source commit"
        self.compose("exec", "-T", "node-a", "sh", "-c",
                     "test -s /usr/share/licenses/tunnel/remotedialer-LICENSE && test -s /usr/share/licenses/tunnel/libpq-LICENSE")
        # Trust must actually be enforced; OS roots cannot trust the lab issuer.
        untrusted = Client.__new__(Client)
        untrusted.context, untrusted.cookies = ssl.create_default_context(), http.cookiejar.CookieJar()
        try:
            untrusted.open("/healthz")
            raise AssertionError("untrusted public certificate was accepted")
        except urllib.error.URLError as error:
            assert isinstance(error.reason, ssl.SSLCertVerificationError)
        self.record("verified TLS, shared secure session, private HTTP and WebSocket", self.prepare_application)
        self.record("active health checks, targeted drain, SSE and single POST", self.drain)
        self.record("SIGKILL owner, Agent reconnect and application recovery", self.kill_owner)
        self.record("database pause, fail-closed fencing and explicit recovery", self.database_outage)
        self.record("same-CA leaf certificate rotation and cross-node mTLS", self.rotate_certificate)
        images = json.loads(self.compose("images", "--format", "json"))
        return {"sourceCommit": self.commit, "binaryVersion": version, "postgresMajor": self.postgres, "images": images}

    def save_logs(self):
        try:
            log = self.compose("logs", "--no-color", "--tail", "150", timeout=20)
            for value in self.redactions:
                log = log.replace(value, "[REDACTED]")
            (self.runtime / "diagnostics.log").write_text(log)
        except (OSError, subprocess.SubprocessError):
            pass


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--runtime", required=True, type=Path, help="new private directory; never reused")
    parser.add_argument("--report", type=Path, help="sanitized JSON evidence outside runtime")
    parser.add_argument("--postgres", choices=["15", "18"], default="18")
    parser.add_argument("--prepare-only", action="store_true", help="generate a lab without starting Docker")
    parser.add_argument("--keep", action="store_true", help="retain running containers and disposable DB")
    args = parser.parse_args()
    os.umask(0o077)
    runtime = args.runtime.resolve()
    if args.report and (args.report.resolve() == runtime or runtime in args.report.resolve().parents):
        parser.error("report must be outside runtime; never upload the private runtime directory")
    for executable in ["openssl", "git"] + ([] if args.prepare_only else ["docker"]):
        if not shutil.which(executable):
            parser.error(f"required executable unavailable: {executable}")
    lab = Lab(runtime, args.postgres)
    # Finish preparing before entering cleanup: a pre-existing path is untouched.
    lab.prepare()
    if args.prepare_only:
        print(f"Prepared {runtime}; see docs/ha-deployment-lab.md for start/stop commands.")
        return
    report = {"scope": "single-host disposable Compose; not multi-host or database failover acceptance", "passed": False}
    try:
        report.update(lab.run())
        report["passed"] = True
    except (AssertionError, OSError, ValueError, subprocess.SubprocessError, urllib.error.URLError, http.client.HTTPException) as error:
        # Do not print command arguments/output: credentials may occur there.
        report["failure"] = type(error).__name__
        if isinstance(error, AssertionError):
            report["failure"] += ": " + str(error)
        elif isinstance(error, urllib.error.HTTPError):
            report["failure"] += f": HTTP {error.code} at {urllib.parse.urlsplit(error.url).path}"
            try:
                message = json.loads(error.read(2048)).get("error", "")
                for value in lab.redactions:
                    message = message.replace(value, "[REDACTED]")
                report["failure"] += ": " + message
            except (ValueError, OSError):
                pass
            error.close()
        print("FAIL deployment acceptance: " + report["failure"], file=sys.stderr)
    finally:
        report["checks"] = lab.results
        lab.save_logs()
        if not args.keep:
            try:
                lab.compose("down", "--volumes", "--remove-orphans", timeout=90)
                report["containersRemoved"] = True
            except (OSError, subprocess.SubprocessError):
                report["containersRemoved"] = False
                report["passed"] = False
        if args.report:
            args.report.parent.mkdir(parents=True, exist_ok=True)
            args.report.write_text(json.dumps(report, indent=2) + "\n")
        print(json.dumps(report, indent=2), flush=True)
    if not report["passed"]:
        sys.exit(1)


if __name__ == "__main__":
    main()
