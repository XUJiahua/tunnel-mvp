#!/usr/bin/env python3
"""Exercise v0.1.1 -> current JSON/SQLite and rollback using real processes.

Use only local, disposable data. The two binaries must be supplied explicitly;
no production configuration or credentials are read. Output never includes state,
cookies, passwords or Agent tokens.
"""
import argparse
import hashlib
import http.client
import json
import os
from pathlib import Path
import shutil
import subprocess
import tempfile
import threading
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from urllib.parse import urlsplit


class Upstream(BaseHTTPRequestHandler):
    def do_GET(self):
        body = b"legacy-upgrade-tunnel-ok"
        self.send_response(200)
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def log_message(self, *_):
        pass


class Drill:
    def __init__(self, directory, old, new):
        self.directory, self.old, self.new = directory, old, new
        self.children, self.logs = [], []
        self.password = "disposable-upgrade-password-123"
        self.upstream = ThreadingHTTPServer(("127.0.0.1", 0), Upstream)
        threading.Thread(target=self.upstream.serve_forever, daemon=True).start()
        # A reservation discovers a free port; readiness/exit checks catch a race.
        reservation = ThreadingHTTPServer(("127.0.0.1", 0), Upstream)
        self.port = reservation.server_port
        reservation.server_close()
        self.url = f"http://localhost:{self.port}"
        self.config = directory / "server.json"
        self.config.write_text(json.dumps({
            "listen": f"127.0.0.1:{self.port}", "publicURL": self.url,
            "accessURL": f"http://apps.localhost:{self.port}",
            "dataDir": str(directory / "data"),
        }))
        self.cookie = ""

    def start(self, binary, args, extra=None):
        # Inherited TUNNEL_* configuration must not touch another data directory.
        env = {k: v for k, v in os.environ.items() if not k.startswith("TUNNEL_")}
        env.update(extra or {})
        log = (self.directory / f"process-{len(self.logs)}.log").open("wb")
        self.logs.append(log)
        child = subprocess.Popen([str(binary), *args], env=env, stdout=log, stderr=log)
        self.children.append(child)
        return child

    @staticmethod
    def stop(child):
        if child.poll() is None:
            child.terminate()
            try:
                child.wait(timeout=10)
            except subprocess.TimeoutExpired:
                child.kill()
                child.wait(timeout=5)
                raise RuntimeError("process required forced termination")

    @staticmethod
    def wait(predicate, child=None, seconds=40):
        deadline = time.monotonic() + seconds
        while time.monotonic() < deadline:
            if child and child.poll() is not None:
                raise RuntimeError("fixture process exited before readiness")
            try:
                if predicate():
                    return
            except (OSError, http.client.HTTPException):
                pass
            time.sleep(0.1)
        raise RuntimeError("fixture readiness timed out")

    def request(self, path, method="GET", data=None, cookie=None, host=None):
        headers = {"Host": host or f"localhost:{self.port}", "Origin": self.url}
        headers["Cookie"] = self.cookie if cookie is None else cookie
        body = None
        if data is not None:
            body = json.dumps(data)
            headers["Content-Type"] = "application/json"
        conn = http.client.HTTPConnection("127.0.0.1", self.port, timeout=5)
        try:
            conn.request(method, path, body, headers)
            response = conn.getresponse()
            return response.status, dict(response.getheaders()), response.read()
        finally:
            conn.close()

    def api(self, path, method="GET", data=None, cookie=None):
        status, _, body = self.request("/api" + path, method, data, cookie)
        if not 200 <= status < 300:
            raise RuntimeError(f"API {method} {path} returned {status}")
        return json.loads(body)

    def login(self, name="admin"):
        status, headers, _ = self.request("/api/login", "POST", {
            "username": name, "password": self.password,
        }, cookie="")
        if status != 200:
            raise RuntimeError(f"login returned {status}")
        return headers["Set-Cookie"].split(";", 1)[0]

    def server(self, binary, storage=None):
        args = ["server", "-c", str(self.config)]
        if storage:
            args.extend(["--storage", storage, "--probe-interval", "0"])
        child = self.start(binary, args, {"TUNNEL_ADMIN_PASSWORD": self.password})
        self.wait(lambda: self.request("/healthz", cookie="")[0] == 200, child)
        self.cookie = self.login()
        return child

    def access(self, service, cookie):
        launch = self.api(f"/services/{service['id']}/launch", "POST", cookie=cookie)
        destination = urlsplit(launch["url"])
        path = destination.path + ("?" + destination.query if destination.query else "")
        status, headers, _ = self.request(path, cookie="", host=destination.netloc)
        if status != 303:
            raise RuntimeError(f"ticket exchange returned {status}")
        access = headers["Set-Cookie"].split(";", 1)[0]
        status, _, body = self.request("/", cookie=access, host=destination.netloc)
        if status != 200 or body != b"legacy-upgrade-tunnel-ok":
            raise RuntimeError(f"real tunnel access returned {status}")
        return access, destination.netloc

    def command(self, args, expected_success=True):
        env = {k: v for k, v in os.environ.items() if not k.startswith("TUNNEL_")}
        p = subprocess.run([str(self.new), "storage", *args], env=env,
                           capture_output=True, timeout=40)
        if (p.returncode == 0) != expected_success:
            raise RuntimeError(f"storage {args[0]} unexpected exit {p.returncode}")

    def check(self, site, service, reader_id, denied_id):
        self.wait(lambda: any(v["id"] == site["id"] and v.get("online")
                              for v in self.api("/sites")))
        reader = self.login("reader")
        me = self.api("/me", cookie=reader)
        if me["id"] != reader_id or me["siteIDs"] != [site["id"]]:
            raise RuntimeError("legacy member identity/grants changed")
        apps = self.api("/services", cookie=reader)
        if len(apps) != 1 or apps[0]["id"] != service["id"]:
            raise RuntimeError("legacy application identity/visibility changed")
        denied = self.login("denied")
        if self.api("/me", cookie=denied)["id"] != denied_id:
            raise RuntimeError("denied member identity changed")
        if self.api("/services", cookie=denied):
            raise RuntimeError("ungranted member gained application visibility")
        if self.request(f"/api/services/{service['id']}/launch", "POST", cookie=denied)[0] < 400:
            raise RuntimeError("ungranted member gained gateway entry")
        return self.access(service, reader)

    def run(self):
        old_server = self.server(self.old)
        site_result = self.api("/sites", "POST", {"name": "Legacy site"})
        site, token = site_result["site"], site_result["token"]
        service = self.api("/services", "POST", {
            "name": "Legacy application", "siteID": site["id"],
            "url": f"http://127.0.0.1:{self.upstream.server_port}",
        })
        reader = self.api("/users", "POST", {
            "username": "reader", "password": self.password, "siteIDs": [site["id"]],
        })
        denied = self.api("/users", "POST", {
            "username": "denied", "password": self.password, "siteIDs": [],
        })
        agent_config = self.directory / "agent.json"
        agent_config.write_text(json.dumps({"serverURL": self.url, "siteID": site["id"],
            "allow": [{"cidr": "127.0.0.0/8", "ports": [self.upstream.server_port]}]}))
        self.start(self.old, ["agent", "-c", str(agent_config)], {"TUNNEL_AGENT_TOKEN": token})
        old_access, app_host = self.check(site, service, reader["id"], denied["id"])
        old_cookie = self.cookie
        self.stop(old_server)
        data, backup = self.directory / "data", self.directory / "pre-upgrade"
        shutil.copytree(data, backup)
        legacy_state = json.loads((backup / "state.json").read_text())
        legacy_bytes = (backup / "state.json").read_bytes()
        steps = []
        for storage in ("json", "sqlite"):
            shutil.rmtree(data)
            shutil.copytree(backup, data)
            if storage == "sqlite":
                self.command(["migrate", "--data-dir", str(data)])
                if (data / "state.json").read_bytes() != legacy_bytes:
                    raise RuntimeError("migration modified legacy JSON")
            server = self.server(self.new, storage)
            if self.request("/api/me", cookie=old_cookie)[0] != 401:
                raise RuntimeError("pre-upgrade console session survived")
            if self.request("/", cookie=old_access, host=app_host)[0] != 401:
                raise RuntimeError("pre-upgrade access session survived")
            self.check(site, service, reader["id"], denied["id"])
            self.command(["inspect", "--data-dir", str(data), "--storage", storage], False)
            # Force a real current-format durable write before backup/rollback.
            self.api(f"/sites/{site['id']}", "PATCH", {"name": "Upgraded site"})
            self.stop(server)
            state_backup = self.directory / f"{storage}.backup.json"
            ops_backup = self.directory / f"{storage}.operations.tar.gz"
            self.command(["export", "--data-dir", str(data), "--storage", storage,
                          "--output", str(state_backup)])
            self.command(["validate", "--input", str(state_backup)])
            exported = json.loads(state_backup.read_text())["state"]
            for collection, fields in (("users", ("id", "passwordHash", "siteIDs")),
                                       ("sites", ("id", "tokenHash")),
                                       ("services", ("id", "siteID", "url"))):
                actual = {v["id"]: v for v in exported[collection]}
                for previous in legacy_state[collection]:
                    if any(actual[previous["id"]].get(k) != previous.get(k) for k in fields):
                        raise RuntimeError(f"legacy {collection} fields changed")
            self.command(["export-operations", "--data-dir", str(data), "--output", str(ops_backup)])
            self.command(["validate-operations", "--input", str(ops_backup)])
            target = self.directory / f"restored-{storage}"
            target_storage = "sqlite" if storage == "json" else "json"
            self.command(["restore", "--input", str(state_backup), "--data-dir", str(target),
                          "--storage", target_storage])
            self.command(["restore-operations", "--input", str(ops_backup), "--data-dir", str(target)])
            shutil.rmtree(data)
            shutil.copytree(target, data)
            restored = self.server(self.new, target_storage)
            self.check(site, service, reader["id"], denied["id"])
            if not any(v["name"] == "Upgraded site" for v in self.api("/sites")):
                raise RuntimeError("restored state lost current-format update")
            self.stop(restored)
            steps.append({"backend": storage, "crossBackendRestore": target_storage, "result": "pass"})
        # Rollback deliberately restores the original whole directory and old
        # binary. New-format state must never be fed to the permissive old writer.
        shutil.rmtree(data)
        shutil.copytree(backup, data)
        rollback = self.server(self.old)
        self.check(site, service, reader["id"], denied["id"])
        self.stop(rollback)
        if (data / "state.json").read_bytes() != legacy_bytes:
            raise RuntimeError("rollback changed original state")
        return {"upgrade": steps, "legacyAgentReconnect": "pass", "rollback": "pass",
                "sessionInvalidation": "pass", "credentialHashesAndIDs": "pass",
                "ungrantedMemberDenied": "pass", "offlineLock": "pass"}

    def close(self):
        for child in reversed(self.children):
            self.stop(child)
        self.upstream.shutdown()
        self.upstream.server_close()
        for log in self.logs:
            log.close()


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--old-binary", required=True, type=Path)
    parser.add_argument("--new-binary", required=True, type=Path)
    parser.add_argument("--output", required=True, type=Path)
    args = parser.parse_args()
    old, new = args.old_binary.resolve(strict=True), args.new_binary.resolve(strict=True)
    if args.output.exists():
        parser.error("output must be a new file")
    old_version = subprocess.check_output([str(old), "version"], text=True).strip()
    if not old_version.startswith("tunnel 0.1.1 (commit: 020224911d5fdff6e007ca6811371d55939e0076,"):
        parser.error("old binary must report the published v0.1.1 version and commit")
    with tempfile.TemporaryDirectory(prefix="tunnel-upgrade-") as temporary:
        drill = Drill(Path(temporary), old, new)
        try:
            result = drill.run()
        finally:
            drill.close()
    result.update({"oldVersion": old_version,
                   "newVersion": subprocess.check_output([str(new), "version"], text=True).strip(),
                   "oldBinarySHA256": hashlib.file_digest(old.open("rb"), "sha256").hexdigest(),
                   "newBinarySHA256": hashlib.file_digest(new.open("rb"), "sha256").hexdigest(),
                   "scope": "local HTTP, real v0.1.1 server/Agent; no external DNS/TLS/IdP or target OS validation"})
    with args.output.open("x") as output:
        json.dump(result, output, indent=2)
        output.write("\n")
    print("PASS v0.1.1 upgrade: JSON, SQLite, credentials, authorization, real legacy Agent, cross-backend restore and rollback")


if __name__ == "__main__":
    main()
