"""Exercise ownership, allocation races, lifecycle cleanup and real HTTP probes."""
from concurrent.futures import ThreadPoolExecutor
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
import importlib.util
import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import threading
import unittest
from unittest.mock import patch

SCRIPTS = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(SCRIPTS))
from preview.core import (allocate, binding_key, process_alive, process_identity, probe,
                          read_json, registry, routes, settings, source_identity, write_json)
from preview.worker import FixtureHandler, FixtureServer, cookie_from
import base64
import http.client
import socket
import ssl

spec = importlib.util.spec_from_file_location("preview_cli", SCRIPTS / "preview.py")
cli = importlib.util.module_from_spec(spec)
spec.loader.exec_module(cli)


class PreviewTests(unittest.TestCase):
    def setUp(self):
        # Git hooks export repository/index variables; fixtures must never inherit them.
        git_environment = patch.dict(os.environ, {key: value for key, value in os.environ.items()
                                                if not key.startswith("GIT_")}, clear=True)
        git_environment.start()
        self.addCleanup(git_environment.stop)
        self.temporary = tempfile.TemporaryDirectory()
        self.directory = Path(self.temporary.name)
        self.home = self.directory / "state"

    def tearDown(self):
        self.temporary.cleanup()

    def repository(self):
        root = self.directory / "repo"
        root.mkdir()
        for args in (["init", "-q"], ["config", "user.email", "preview@example.invalid"],
                     ["config", "user.name", "Preview test"]):
            subprocess.run(["git", "-C", str(root), *args], check=True, stdout=subprocess.DEVNULL)
        (root / "tracked").write_text("original")
        subprocess.run(["git", "-C", str(root), "add", "."], check=True)
        subprocess.run(["git", "-C", str(root), "-c", "core.hooksPath=/dev/null", "commit", "-qm", "fixture"], check=True)
        return root

    def test_parallel_allocations_are_unique_and_idempotent(self):
        roots = [self.directory / f"repo-{i}" for i in range(4)]
        with ThreadPoolExecutor(max_workers=4) as pool:
            bindings = list(pool.map(lambda root: allocate(self.home, root), roots))
        self.assertEqual(set(range(4)), {binding["slot"] for binding in bindings})
        self.assertEqual(4, len(registry(self.home)["bindings"]))
        self.assertEqual(bindings[0], allocate(self.home, roots[0]))
        with self.assertRaises(ValueError):
            allocate(self.home, self.directory / "fifth")

    def test_slot_zero_main_and_conflicting_requests(self):
        root = self.directory / "repo"
        binding = allocate(self.home, root, slot=0)
        self.assertEqual("https://console-dev0.cubbber.dev", binding["origin"])
        self.assertEqual("https://dev0.app.cubbber.dev", binding["accessURL"])
        self.assertEqual(30500, binding["port"])
        main = allocate(self.home, root, main=True)
        self.assertEqual("https://console.cubbber.dev", main["origin"])
        self.assertEqual("https://app.cubbber.dev", main["accessURL"])
        self.assertEqual(30510, main["port"])
        self.assertNotEqual(binding_key(root), binding_key(root, True))
        with self.assertRaises(ValueError):
            allocate(self.home, self.directory / "other", slot=0)
        with self.assertRaises(ValueError):
            allocate(self.home, root, slot=1)
        with self.assertRaises(ValueError):
            allocate(self.home, root, local=True)

    def test_routes_and_certificate_namespaces_keep_main_last(self):
        ingress = routes(settings(self.home))["ingress"]
        self.assertEqual(11, len(ingress))
        for i in range(4):
            self.assertEqual(f"console-dev{i}.cubbber.dev", ingress[2*i]["hostname"])
            self.assertEqual(f"*.dev{i}.app.cubbber.dev", ingress[2*i+1]["hostname"])
            self.assertEqual(ingress[2*i]["service"], ingress[2*i+1]["service"])
        self.assertEqual("*.app.cubbber.dev", ingress[-2]["hostname"])
        self.assertEqual({"service": "http_status:404"}, ingress[-1])

    def test_configuration_rejects_port_overlap_and_unknown_keys(self):
        write_json(self.home / "config.json", {"mainPort": 30500})
        with self.assertRaises(ValueError):
            settings(self.home)
        write_json(self.home / "config.json", {"password": "must-not-be-configured-here"})
        with self.assertRaises(ValueError):
            settings(self.home)

    def test_retired_slots_cannot_be_allocated_or_restarted_but_can_be_destroyed(self):
        root = self.directory / "repo"
        for slot in (4, 9):
            with self.assertRaises(ValueError):
                allocate(self.home, root, slot=slot)
        binding = allocate(self.home, root)
        state = registry(self.home)
        state["bindings"][binding_key(root)]["slot"] = 4
        write_json(self.home / "registry.json", state)
        with self.assertRaises(ValueError):
            allocate(self.home, root)
        env = {**os.environ, "TUNNEL_PREVIEW_HOME": str(self.home)}
        subprocess.run([sys.executable, str(SCRIPTS / "preview.py"), "destroy", "--root", str(root)],
                       env=env, check=True, stdout=subprocess.DEVNULL)
        self.assertFalse(Path(binding["runtime"]).exists())
        self.assertEqual({}, registry(self.home)["bindings"])

    def test_cli_exports_only_four_preview_certificate_namespaces(self):
        env = {**os.environ, "TUNNEL_PREVIEW_HOME": str(self.home)}
        command = [sys.executable, str(SCRIPTS / "preview.py")]
        coverage = json.loads(subprocess.check_output([*command, "certificates"], env=env))
        self.assertEqual("*.cubbber.dev", coverage["consoleCoverage"])
        self.assertEqual(["*.app.cubbber.dev", *[f"*.dev{i}.app.cubbber.dev" for i in range(4)]],
                         coverage["applicationCoverage"])
        result = subprocess.run([*command, "up", "--slot", "4"], env=env, capture_output=True)
        self.assertEqual(2, result.returncode)
        self.assertEqual({}, registry(self.home)["bindings"])

    def test_source_identity_detects_dirty_content_untracked_and_mode(self):
        root = self.repository()
        clean = source_identity(root)
        self.assertFalse(clean["dirty"])
        (root / "tracked").write_text("changed")
        changed = source_identity(root)
        self.assertTrue(changed["dirty"])
        self.assertNotEqual(clean["digest"], changed["digest"])
        (root / "untracked").write_text("new source")
        untracked = source_identity(root)
        self.assertNotEqual(changed["digest"], untracked["digest"])
        (root / "tracked").chmod(0o755)
        self.assertNotEqual(untracked["digest"], source_identity(root)["digest"])
        (root / "tracked").unlink()
        self.assertTrue(source_identity(root)["dirty"])

    def test_infrastructure_isolated_from_inherited_git_hook_environment(self):
        parent = self.repository()
        before_head = subprocess.check_output(["git", "-C", str(parent), "rev-parse", "HEAD"])
        before_index = (parent / ".git/index").read_bytes()
        before_config = (parent / ".git/config").read_bytes()
        env = {**os.environ, "GIT_DIR": str(parent / ".git"), "GIT_WORK_TREE": str(parent),
               "GIT_INDEX_FILE": str(parent / ".git/index")}
        result = subprocess.run([sys.executable, "-m", "unittest", "discover", "-s", str(SCRIPTS / "tests"),
                                 "-k", "source_identity_detects", "-v"], env=env, capture_output=True, text=True, timeout=20)
        self.assertEqual(0, result.returncode, result.stderr)
        self.assertEqual(before_head, subprocess.check_output(["git", "-C", str(parent), "rev-parse", "HEAD"]))
        self.assertEqual(before_index, (parent / ".git/index").read_bytes())
        self.assertEqual(before_config, (parent / ".git/config").read_bytes())

    def test_publish_stops_local_process_and_retains_data_after_failed_build(self):
        root = self.repository()
        binding = allocate(self.home, root, local=True, slot=0)
        runtime = Path(binding["runtime"])
        write_json(runtime / "credentials.json", {"username": "fixture", "password": "private-fixture"})
        write_json(runtime / "fixtures.json", [{"serviceID": "unchanged"}])
        (runtime / "data").mkdir()
        (runtime / "data/state.db").write_bytes(b"existing-synthetic-data")
        child = subprocess.Popen([sys.executable, "-c", "import time; time.sleep(60)"], start_new_session=True)
        try:
            write_json(runtime / "children.json", [process_identity(child.pid)])
            env = {**os.environ, "TUNNEL_PREVIEW_HOME": str(self.home)}
            result = subprocess.run([sys.executable, str(SCRIPTS / "preview.py"), "publish", "--root", str(root),
                                     "--wait", "10"], env=env, capture_output=True, text=True, timeout=25)
            self.assertEqual(1, result.returncode, result.stderr)
            child.wait(timeout=5)
            status = json.loads(result.stdout)
            self.assertEqual("failed", status["phase"])
            self.assertFalse(status["localMode"])
            public = registry(self.home)["bindings"][binding_key(root)]
            self.assertEqual(public, read_json(runtime / "binding.json"))
            self.assertEqual(binding["id"], public["id"])
            self.assertEqual(binding["port"], public["port"])
            self.assertEqual("https://console-dev0.cubbber.dev", public["origin"])
            self.assertEqual("https://dev0.app.cubbber.dev", public["accessURL"])
            self.assertEqual(b"existing-synthetic-data", (runtime / "data/state.db").read_bytes())
            self.assertEqual("private-fixture", read_json(runtime / "credentials.json")["password"])
            self.assertEqual([{"serviceID": "unchanged"}], read_json(runtime / "fixtures.json"))
            self.assertNotIn("private-fixture", result.stdout + result.stderr)
            self.assertEqual(public, cli.publish(self.home, public))
        finally:
            if child.poll() is None:
                child.terminate()
            child.wait(timeout=5)

    def test_publish_requires_existing_worktree_and_rejects_mode_options(self):
        env = {**os.environ, "TUNNEL_PREVIEW_HOME": str(self.home)}
        command = [sys.executable, str(SCRIPTS / "preview.py"), "publish", "--root", str(self.directory / "repo")]
        result = subprocess.run(command, env=env, capture_output=True)
        self.assertEqual(1, result.returncode)
        for option in (["--local"], ["--main"], ["--slot", "0"], ["--storage", "json"]):
            result = subprocess.run([*command, *option], env=env, capture_output=True)
            self.assertEqual(2, result.returncode)
        self.assertEqual({}, registry(self.home)["bindings"])

    def test_private_state_is_atomic_and_restrictive(self):
        path = self.home / "credentials.json"
        write_json(path, {"generation": 1})
        write_json(path, {"generation": 2})
        self.assertEqual({"generation": 2}, read_json(path))
        self.assertEqual(0o600, path.stat().st_mode & 0o777)
        self.assertEqual(0o700, path.parent.stat().st_mode & 0o777)
        self.assertEqual(["credentials.json"], [p.name for p in path.parent.iterdir()])

    def test_pid_reuse_guard_and_orphan_cleanup_preserve_unrelated_process(self):
        unrelated = subprocess.Popen([sys.executable, "-c", "import time; time.sleep(60)"], start_new_session=True)
        child = subprocess.Popen([sys.executable, "-c", "import time; time.sleep(60)"], start_new_session=True)
        try:
            identity = process_identity(child.pid)
            wrong = {**process_identity(unrelated.pid), "start": "not-the-same-process"}
            self.assertTrue(process_alive(identity))
            self.assertFalse(process_alive(wrong))
            binding = allocate(self.home, self.directory / "repo")
            runtime = Path(binding["runtime"])
            write_json(runtime / "worker.json", wrong)
            write_json(runtime / "children.json", [identity])
            cli.stop(binding)
            child.wait(timeout=5)
            self.assertIsNone(unrelated.poll())
            self.assertEqual("stopped", read_json(runtime / "status.json")["phase"])
            self.assertEqual(binding, allocate(self.home, self.directory / "repo"))
        finally:
            for process in (child, unrelated):
                if process.poll() is None:
                    process.terminate()
                process.wait(timeout=5)

    def test_real_http_probe_rejects_wrong_build_even_if_health_is_ok(self):
        build = {"buildID": "expected", "instanceID": "preview-a", "source": {"digest": "abc"}}

        class Handler(BaseHTTPRequestHandler):
            identity = dict(build)

            def log_message(self, *_):
                pass

            def do_GET(self):
                self.send_response(200)
                self.end_headers()
                self.wfile.write(json.dumps(self.identity).encode() if self.path == "/__preview.json" else
                                 b'{"status":"ok"}' if self.path == "/healthz" else b"<html>console</html>")

        server = ThreadingHTTPServer(("127.0.0.1", 0), Handler)
        thread = threading.Thread(target=server.serve_forever, daemon=True)
        thread.start()
        try:
            binding = {"origin": "https://console-dev0.cubbber.dev", "port": server.server_port}
            self.assertTrue(probe(binding, build)["ok"])
            Handler.identity = {**build, "buildID": "stale"}
            self.assertEqual("build_identity_mismatch", probe(binding, build)["reason"])
        finally:
            server.shutdown()
            server.server_close()
            thread.join()

    def test_public_request_tries_next_address_after_tls_timeout_without_replaying_post(self):
        from preview.core import request

        class Handler(BaseHTTPRequestHandler):
            requests = []

            def log_message(self, *_):
                pass

            def do_POST(self):
                self.requests.append(self.path)
                self.rfile.read(int(self.headers.get("Content-Length", "0")))
                self.send_response(200)
                self.send_header("Content-Length", "2")
                self.end_headers()
                self.wfile.write(b"{}")

        server = ThreadingHTTPServer(("127.0.0.1", 0), Handler)
        thread = threading.Thread(target=server.serve_forever, daemon=True)
        thread.start()
        address = (socket.AF_INET, socket.SOCK_STREAM, socket.IPPROTO_TCP, "", ("127.0.0.1", server.server_port))
        attempts = []

        def handshake(raw, server_hostname):
            self.assertEqual("preview.invalid", server_hostname)
            attempts.append(raw)
            if len(attempts) == 1:
                raise TimeoutError("First address accepts TCP but stalls TLS")
            return raw  # Only the TLS handshake is simulated; the HTTP request is real.

        try:
            with patch("urllib.request.getproxies", return_value={}), \
                    patch("socket.getaddrinfo", return_value=[address, address]), patch("ssl.create_default_context") as factory, \
                    patch("ssl._create_default_https_context", factory):
                factory.return_value.wrap_socket.side_effect = handshake
                status, _, body = request({"origin": "https://preview.invalid"}, "/single-use-ticket", "POST",
                                          {"ticket": "synthetic"}, public=True)
            self.assertEqual(200, status)
            self.assertEqual(b"{}", body)
            self.assertEqual(2, len(attempts))
            self.assertEqual(-1, attempts[0].fileno())
            self.assertEqual(["/single-use-ticket"], Handler.requests)
        finally:
            server.shutdown()
            server.server_close()
            thread.join()

    def test_public_probe_retries_read_only_checks_after_network_error(self):
        build = {"buildID": "expected", "instanceID": "preview-a", "source": {"digest": "abc"}}
        with patch("preview.core.request", side_effect=[ConnectionResetError("transient"),
                   (200, {}, json.dumps(build).encode()), (200, {}, b'{"status":"ok"}'),
                   (200, {}, b"<html>console</html>")]) as transport:
            self.assertTrue(probe({}, build, public=True)["ok"])
            self.assertEqual(4, transport.call_count)
            self.assertTrue(all(call.kwargs == {"public": True} for call in transport.call_args_list))
        with patch("preview.core.request", side_effect=ssl.SSLCertVerificationError("invalid")) as transport:
            self.assertFalse(probe({}, build, public=True)["ok"])
            self.assertEqual(1, transport.call_count)

    def test_public_request_does_not_retry_certificate_verification_failure(self):
        from preview.core import request

        class Handler(BaseHTTPRequestHandler):
            def log_message(self, *_):
                pass

        server = ThreadingHTTPServer(("127.0.0.1", 0), Handler)
        thread = threading.Thread(target=server.serve_forever, daemon=True)
        thread.start()
        address = (socket.AF_INET, socket.SOCK_STREAM, socket.IPPROTO_TCP, "", ("127.0.0.1", server.server_port))
        try:
            with patch("urllib.request.getproxies", return_value={}), \
                    patch("socket.getaddrinfo", return_value=[address, address]), patch("ssl.create_default_context") as factory, \
                    patch("ssl._create_default_https_context", factory):
                factory.return_value.wrap_socket.side_effect = ssl.SSLCertVerificationError("invalid certificate")
                with self.assertRaises(ssl.SSLCertVerificationError):
                    request({"origin": "https://preview.invalid"}, "/", public=True)
                self.assertEqual(1, factory.return_value.wrap_socket.call_count)
        finally:
            server.shutdown()
            server.server_close()
            thread.join()

    def test_public_request_uses_connect_proxy_with_destination_sni(self):
        from preview.core import request

        class Handler(BaseHTTPRequestHandler):
            protocol_version = "HTTP/1.1"
            tunnels, requests = [], []

            def log_message(self, *_):
                pass

            def do_CONNECT(self):
                self.tunnels.append(self.path)
                self.send_response(200)
                self.end_headers()
                self.close_connection = False

            def do_POST(self):
                self.requests.append(self.path)
                self.rfile.read(int(self.headers.get("Content-Length", "0")))
                self.send_response(200)
                self.send_header("Content-Length", "2")
                self.end_headers()
                self.wfile.write(b"{}")

        server = ThreadingHTTPServer(("127.0.0.1", 0), Handler)
        thread = threading.Thread(target=server.serve_forever, daemon=True)
        thread.start()
        try:
            with patch("urllib.request.getproxies", return_value={"https": f"http://127.0.0.1:{server.server_port}"}), \
                    patch("urllib.request.proxy_bypass", return_value=False), \
                    patch("ssl._create_default_https_context") as factory:
                factory.return_value.wrap_socket.side_effect = lambda raw, server_hostname: raw
                status, _, _ = request({"origin": "https://preview.invalid"}, "/ticket", "POST", public=True)
                self.assertEqual(200, status)
                self.assertEqual("preview.invalid", factory.return_value.wrap_socket.call_args.kwargs["server_hostname"])
            self.assertEqual(["preview.invalid:443"], Handler.tunnels)
            self.assertEqual(["/ticket"], Handler.requests)
        finally:
            server.shutdown()
            server.server_close()
            thread.join()

    def test_destroy_is_idempotent_and_main_is_not_archived(self):
        root = self.directory / "repo"
        binding = allocate(self.home, root)
        main = allocate(self.home, root, main=True)
        env = {**os.environ, "TUNNEL_PREVIEW_HOME": str(self.home)}
        command = [sys.executable, str(SCRIPTS / "preview.py"), "destroy", "--root", str(root), "--if-exists"]
        for _ in range(2):
            subprocess.run(command, env=env, check=True, stdout=subprocess.DEVNULL)
        self.assertFalse(Path(binding["runtime"]).exists())
        self.assertTrue(Path(main["runtime"]).exists())
        self.assertEqual([main], list(registry(self.home)["bindings"].values()))

    def test_synthetic_upstream_supports_real_http_upload_sse_and_websocket(self):
        server = FixtureServer(("127.0.0.1", 0), FixtureHandler)
        server.label, server.build_id = "A", "fixture-build"
        thread = threading.Thread(target=server.serve_forever, daemon=True)
        thread.start()
        try:
            connection = http.client.HTTPConnection("127.0.0.1", server.server_port, timeout=3)
            connection.request("POST", "/upload", b"sample")
            response = connection.getresponse()
            self.assertEqual(200, response.status)
            self.assertEqual({"bytes": 6, "buildID": "fixture-build"}, json.loads(response.read()))
            connection.close()
            connection = http.client.HTTPConnection("127.0.0.1", server.server_port, timeout=3)
            connection.request("GET", "/events")
            response = connection.getresponse()
            self.assertEqual(b"data: preview-event\n", response.readline())
            connection.close()
            with socket.create_connection(("127.0.0.1", server.server_port), timeout=3) as sock:
                key = base64.b64encode(b"0123456789abcdef").decode()
                sock.sendall(("GET /ws HTTP/1.1\r\nHost: localhost\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n"
                              f"Sec-WebSocket-Key: {key}\r\nSec-WebSocket-Version: 13\r\n\r\n").encode())
                stream = sock.makefile("rb")
                self.assertIn(b"101", stream.readline())
                while stream.readline() != b"\r\n":
                    pass
                sock.sendall(b"\x81\x84\x00\x00\x00\x00echo")
                self.assertEqual(b"\x81\x04echo", stream.read(6))
                sock.sendall(b"\x88\x80\x00\x00\x00\x00")
                self.assertEqual(b"\x88\x00", stream.read(2))
                stream.close()
        finally:
            server.shutdown()
            server.server_close()
            thread.join()

    def test_authentication_cookie_must_be_present(self):
        self.assertEqual("tunnel_console=opaque", cookie_from({"Set-Cookie": "tunnel_console=opaque; HttpOnly; Secure"}, "tunnel_console"))
        with self.assertRaises(RuntimeError):
            cookie_from({}, "tunnel_console")

    def test_failed_build_retains_binding_and_can_be_destroyed(self):
        root = self.repository()  # Deliberately has no frontend build target.
        env = {**os.environ, "TUNNEL_PREVIEW_HOME": str(self.home)}
        command = [sys.executable, str(SCRIPTS / "preview.py")]
        result = subprocess.run([*command, "up", "--root", str(root), "--local", "--slot", "0", "--wait", "10"],
                                env=env, capture_output=True, text=True, timeout=20)
        self.assertEqual(1, result.returncode, result.stderr)
        status = json.loads(result.stdout)
        self.assertFalse(status["ready"])
        self.assertEqual("failed", status["phase"])
        binding = registry(self.home)["bindings"][binding_key(root)]
        self.assertEqual(0, binding["slot"])
        subprocess.run([*command, "destroy", "--root", str(root)], env=env, check=True, stdout=subprocess.DEVNULL)
        self.assertEqual({}, registry(self.home)["bindings"])
        self.assertFalse(Path(binding["runtime"]).exists())


if __name__ == "__main__":
    unittest.main()
