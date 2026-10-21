"""Detached supervisor. Invoked by preview.py; never shares application data."""
import base64
import hashlib
import http.cookies
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
import json
import os
from pathlib import Path
import secrets
import signal
import socket
import struct
import subprocess
import sys
import threading
import time
from urllib.parse import urlsplit
import uuid

from .core import (clean_environment, git, lock, private_dir, probe, process_alive,
                   process_identity, read_json, request, sha256, source_identity, write_json)


class FixtureServer(ThreadingHTTPServer):
    daemon_threads = True
    block_on_close = False


class FixtureHandler(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"
    def log_message(self, *_):
        pass

    def do_HEAD(self):
        self.send_response(200)
        self.end_headers()

    def do_GET(self):
        if self.path == "/ws" and self.headers.get("Upgrade", "").lower() == "websocket":
            key = self.headers.get("Sec-WebSocket-Key", "")
            accept = base64.b64encode(hashlib.sha1((key + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11").encode()).digest()).decode()
            self.send_response(101)
            self.send_header("Upgrade", "websocket")
            self.send_header("Connection", "Upgrade")
            self.send_header("Sec-WebSocket-Accept", accept)
            self.end_headers()
            self.close_connection = True
            try:
                while True:
                    header = self.rfile.read(2)
                    if len(header) != 2:
                        return
                    opcode, length = header[0] & 15, header[1] & 127
                    if not header[0] & 128 or not header[1] & 128:
                        return
                    if length == 126:
                        length = struct.unpack("!H", self.rfile.read(2))[0]
                    elif length == 127:
                        length = struct.unpack("!Q", self.rfile.read(8))[0]
                    if length > 65535:
                        return
                    mask, payload = self.rfile.read(4), self.rfile.read(length)
                    if len(mask) != 4 or len(payload) != length:
                        return
                    payload = bytes(value ^ mask[index % 4] for index, value in enumerate(payload))
                    if opcode == 8:
                        self.wfile.write(b"\x88\x00")
                        self.wfile.flush()
                        return
                    if opcode not in (1, 2, 9):
                        return
                    response = bytes([0x80 | (10 if opcode == 9 else opcode)])
                    response += bytes([length]) if length < 126 else b"\x7e" + struct.pack("!H", length)
                    self.wfile.write(response + payload)
                    self.wfile.flush()
            except (OSError, struct.error):
                return
        if self.path == "/events":
            self.send_response(200)
            self.send_header("Content-Type", "text/event-stream")
            self.send_header("Cache-Control", "no-store")
            self.end_headers()
            try:
                while True:
                    self.wfile.write(b"data: preview-event\n\n")
                    self.wfile.flush()
                    time.sleep(1)
            except OSError:
                return
        payload = (f"<!doctype html><html><head><title>Preview application {self.server.label}</title></head>"
                   f"<body><h1>Preview application {self.server.label}</h1>"
                   f"<p>Build: {self.server.build_id}</p><a href='/events'>SSE events</a>"
                   "<form method='post' action='/upload'><button>Send sample request</button></form>"
                   "</body></html>").encode()
        self.send_response(200)
        self.send_header("Content-Type", "text/html; charset=utf-8")
        self.send_header("Content-Length", str(len(payload)))
        self.end_headers()
        self.wfile.write(payload)

    def do_POST(self):
        size = int(self.headers.get("Content-Length", "0"))
        if size > 2 * 1024 * 1024:
            self.send_error(413)
            return
        payload = json.dumps({"bytes": len(self.rfile.read(size)), "buildID": self.server.build_id}).encode()
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(payload)))
        self.end_headers()
        self.wfile.write(payload)


def cookie_from(headers, name):
    cookie = http.cookies.SimpleCookie()
    cookie.load(next((v for k, v in headers.items() if k.lower() == "set-cookie"), ""))
    if name not in cookie:
        raise RuntimeError("Expected authentication cookie not returned")
    return f"{name}={cookie[name].value}"


class Worker:
    def __init__(self, home, binding):
        self.home, self.binding = home, binding
        self.root, self.runtime = Path(binding["root"]), Path(binding["runtime"])
        self.children, self.handles, self.upstreams = [], [], []
        self.stopping = False
        self.applications = []

    def phase(self, phase, detail=None):
        write_json(self.runtime / "status.json", {"phase": phase, "detail": detail,
                                                   "applications": self.applications, "updatedAt": time.time()})

    def start(self, command, label, extra=None):
        handle = open(self.runtime / (label + ".log"), "ab", buffering=0)
        self.handles.append(handle)
        child = subprocess.Popen(command, cwd=self.root, env={**clean_environment(), **(extra or {})},
                                 stdin=subprocess.DEVNULL, stdout=handle, stderr=handle, start_new_session=True)
        identity = process_identity(child.pid)
        if not identity:
            raise RuntimeError(f"{label} exited before process registration")
        self.children.append((child, identity))
        write_json(self.runtime / "children.json", [identity for child, identity in self.children if child.poll() is None])
        return child

    def run_command(self, command, label, extra=None):
        child = self.start(command, label, extra)
        try:
            if child.wait() != 0:
                raise RuntimeError(f"{label} failed; inspect {self.runtime / (label + '.log')}")
        except BaseException:
            # Stop the build while still holding build.lock, before another
            # worktree can acquire it and rewrite the same dependency caches.
            if child.poll() is None:
                os.killpg(child.pid, signal.SIGTERM)
                try:
                    child.wait(timeout=10)
                except subprocess.TimeoutExpired:
                    os.killpg(child.pid, signal.SIGKILL)
                    child.wait(timeout=5)
            raise

    def wait(self, predicate, seconds=60):
        deadline = time.monotonic() + seconds
        while time.monotonic() < deadline:
            if any(child.poll() is not None for child, _ in self.children):
                # Completed build commands are removed before starting services.
                raise RuntimeError("Managed process exited; inspect private instance logs")
            try:
                if predicate():
                    return
            except (OSError, ValueError):
                pass
            time.sleep(0.25)
        raise RuntimeError("Readiness deadline exceeded; inspect DNS, certificates and instance logs")

    def build(self):
        self.phase("queued")
        with lock(self.home / "build.lock"):
            self.phase("building")
            before = source_identity(self.root)
            build_id = uuid.uuid4().hex
            build = {"buildID": build_id, "instanceID": self.binding["id"], "source": before,
                     "builtAt": time.time()}
            destination = private_dir(self.runtime / "builds" / build_id)
            self.run_command(["make", "frontend"], "frontend", {"NEXT_TELEMETRY_DISABLED": "1"})
            write_json(self.root / "internal/web/dist/__preview.json", build)
            self.run_command(["go", "build", "-trimpath", "-ldflags",
                              f"-X main.version=preview-{build_id} -X main.commit={before['commit']} -X main.date={int(build['builtAt'])}",
                              "-o", str(destination / "tunnel"), "./cmd/tunnel"], "build", {"CGO_ENABLED": "0"})
            after = source_identity(self.root)
            if before != after:
                raise RuntimeError("Source changed during build; stop editing and refresh")
            build["binarySHA256"] = sha256(destination / "tunnel")
            write_json(self.runtime / "build.json", build)
            self.children = []
            write_json(self.runtime / "children.json", [])
            return build, destination / "tunnel"

    def api(self, path, method="GET", data=None):
        status, _, body = request(self.binding, "/api" + path, method, data, self.cookie)
        if status < 200 or status >= 300:
            raise RuntimeError(f"Fixture API {method} {path}: HTTP {status}")
        return json.loads(body) if body else None

    def seed(self, binary, build):
        credentials = read_json(self.runtime / "credentials.json")
        status, headers, _ = request(self.binding, "/api/login", "POST", credentials)
        if status != 200:
            raise RuntimeError("Preview administrator login failed")
        self.cookie = cookie_from(headers, "tunnel_console")
        fixtures = read_json(self.runtime / "fixtures.json", [])
        for index, label in enumerate(("A", "B")):
            server = FixtureServer(("127.0.0.1", 0), FixtureHandler)
            server.label, server.build_id = label, build["buildID"]
            self.upstreams.append(server)
            threading.Thread(target=server.serve_forever, daemon=True).start()
            port = server.server_port
            if index < len(fixtures):
                fixture = fixtures[index]
            else:
                site = self.api("/sites", "POST", {"name": "Preview site " + label})
                # Persist each created object immediately so a failed startup can resume.
                fixture = {"siteID": site["site"]["id"]}
                fixtures.append(fixture)
                write_json(self.runtime / "fixtures.json", fixtures)
            if "agentID" not in fixture:
                agent = self.api(f"/sites/{fixture['siteID']}/agents", "POST", {"name": "Preview Agent " + label})
                fixture.update({"agentID": agent["agent"]["id"], "token": agent["token"]})
                write_json(self.runtime / "fixtures.json", fixtures)
            target = f"http://127.0.0.1:{port}"
            if "serviceID" not in fixture:
                service = self.api("/services", "POST", {"name": "Preview application " + label,
                                                        "siteID": fixture["siteID"], "url": target})
                fixture["serviceID"] = service["id"]
                write_json(self.runtime / "fixtures.json", fixtures)
            else:
                self.api("/services/" + fixture["serviceID"], "PATCH",
                         {"name": "Preview application " + label, "url": target})
            agent_config = self.runtime / ("agent-" + label + ".json")
            write_json(agent_config, {"serverURL": self.binding["origin"], "siteID": fixture["siteID"],
                                      "instanceID": fixture["agentID"],
                                      "allow": [{"cidr": "127.0.0.1/32", "ports": [port]}]})
            self.start([str(binary), "agent", "--config", str(agent_config)], "agent-" + label,
                       {"TUNNEL_AGENT_TOKEN": fixture["token"]})
            self.applications.append({"name": label, "id": fixture["serviceID"],
                                      "url": self.binding["accessURL"].replace("://", "://" + fixture["serviceID"] + ".")})
        self.wait(lambda: all(any(agent["id"] == fixture["agentID"] and agent["online"]
                                 for agent in self.api("/agents")) for fixture in fixtures))
        # Check ticket redemption and the content actually reached through each Agent.
        for app in self.applications:
            launch = urlsplit(self.api("/services/" + app["id"] + "/launch", "POST")["url"])
            status, headers, _ = request(self.binding, launch.path + "?" + launch.query, host=launch.netloc,
                                          public=not self.binding["local"])
            if status not in (302, 303):
                raise RuntimeError("Application ticket redemption failed")
            cookie = cookie_from(headers, "tunnel_access")
            status, _, body = request(self.binding, "/", cookie=cookie, host=launch.netloc,
                                       public=not self.binding["local"])
            if status != 200 or build["buildID"].encode() not in body:
                raise RuntimeError("Application Agent path or build identity failed")
        self.api("/logout", "POST")

    def stop_children(self):
        for child, identity in reversed(self.children):
            if process_alive(identity):
                try:
                    os.killpg(child.pid, signal.SIGTERM)
                    child.wait(timeout=10)
                except subprocess.TimeoutExpired:
                    if process_alive(identity):
                        os.killpg(child.pid, signal.SIGKILL)
                    child.wait(timeout=5)
                except ProcessLookupError:
                    pass
        for server in self.upstreams:
            server.shutdown()
            server.server_close()
        for handle in self.handles:
            handle.close()

    def run(self):
        os.umask(0o077)
        write_json(self.runtime / "worker.json", process_identity(os.getpid()))

        def stop(_signal, _frame):
            self.stopping = True
            raise InterruptedError("Preview stopped")

        signal.signal(signal.SIGTERM, stop)
        signal.signal(signal.SIGINT, stop)
        try:
            build, binary = self.build()
            self.phase("starting")
            credentials = read_json(self.runtime / "credentials.json")
            if not credentials:
                credentials = {"username": "preview-admin", "password": secrets.token_urlsafe(24)}
                write_json(self.runtime / "credentials.json", credentials)
            server_config = {"listen": f"127.0.0.1:{self.binding['port']}", "publicURL": self.binding["origin"],
                             "accessURL": self.binding["accessURL"], "storage": self.binding["storage"],
                             "dataDir": str(self.runtime / "data"), "adminUsername": credentials["username"],
                             "probeIntervalSeconds": 0}
            # Fail on an occupied port before starting a server that could be mistaken for ours.
            with socket.socket() as check:
                # Match Go's listener behavior: a recently closed preview may
                # leave accepted connections in TIME_WAIT without a live server.
                check.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
                check.bind(("127.0.0.1", self.binding["port"]))
                check.listen(1)
            write_json(self.runtime / "server.json", server_config)
            self.start([str(binary), "server", "--config", str(self.runtime / "server.json")], "server",
                       {"TUNNEL_ADMIN_PASSWORD": credentials["password"]})
            self.wait(lambda: probe(self.binding, build)["ok"])
            if not self.binding["local"]:
                self.phase("checking_public")
                self.wait(lambda: probe(self.binding, build, public=True)["ok"], seconds=90)
            self.phase("seeding")
            self.seed(binary, build)
            self.phase("running")
            while True:
                if any(child.poll() is not None for child, _ in self.children):
                    raise RuntimeError("Server or Agent exited; inspect private logs and refresh")
                time.sleep(1)
        except Exception as error:
            self.phase("stopped" if self.stopping else "failed", str(error))
            if not self.stopping:
                print(type(error).__name__ + ": " + str(error), flush=True)
        finally:
            # Do not allow a second signal to interrupt cleanup.
            signal.signal(signal.SIGTERM, signal.SIG_IGN)
            signal.signal(signal.SIGINT, signal.SIG_IGN)
            self.stop_children()


def main():
    home, runtime = map(Path, sys.argv[1:3])
    binding = read_json(runtime / "binding.json")
    # Hold a per-worktree lease for the entire runtime, including builds.
    root_key = __import__("hashlib").sha256(binding["root"].encode()).hexdigest()
    with lock(home / "locks" / (root_key + ".lock")):
        Worker(home, binding).run()


if __name__ == "__main__":
    main()
