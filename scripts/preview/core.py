"""Private state, slot allocation, source identities and HTTP probes."""
import contextlib
import fcntl
import hashlib
import http.client
import json
import os
from pathlib import Path
import re
import signal
import ssl
import subprocess
import tempfile
import time
from urllib.parse import urlsplit
import uuid

from .network import PreviewHTTPSConnection


PREVIEW_SLOTS = range(4)
PROCESS_OWNER_ENV = "TUNNEL_PREVIEW_PROCESS_OWNER"


def git(root, *args):
    return subprocess.check_output(["git", "-C", str(root), *args]).decode().strip()


def state_home():
    return Path(os.environ.get("TUNNEL_PREVIEW_HOME", Path.home() / ".local/state/tunnel-preview")).expanduser().resolve()


def private_dir(path):
    path.mkdir(parents=True, exist_ok=True, mode=0o700)
    path.chmod(0o700)
    return path


def read_json(path, default=None):
    return json.loads(path.read_text()) if path.exists() else default


def write_json(path, value):
    private_dir(path.parent)
    fd, temporary = tempfile.mkstemp(dir=path.parent, prefix=".write-")
    try:
        with os.fdopen(fd, "w") as output:
            json.dump(value, output, ensure_ascii=False, indent=2)
            output.write("\n")
        os.replace(temporary, path)
    finally:
        if os.path.exists(temporary):
            os.unlink(temporary)


@contextlib.contextmanager
def lock(path):
    private_dir(path.parent)
    with open(path, "a+") as handle:
        os.chmod(path, 0o600)
        fcntl.flock(handle, fcntl.LOCK_EX)
        yield


def settings(home):
    config = {"domain": "cubbber.dev", "basePort": 30500, "mainPort": 30510}
    supplied = read_json(home / "config.json", {})
    if set(supplied) - set(config):
        raise ValueError("Unknown preview host configuration key")
    config.update(supplied)
    if not re.fullmatch(r"[a-z0-9]+(?:[-.][a-z0-9]+)+", config["domain"]):
        raise ValueError("Invalid domain")
    ports = [config["basePort"] + i for i in PREVIEW_SLOTS] + [config["mainPort"]]
    if any(type(port) is not int or not 1024 <= port <= 65535 for port in ports) or len(set(ports)) != len(PREVIEW_SLOTS) + 1:
        raise ValueError("Preview and main ports must be distinct unprivileged ports")
    return config


def slot_config(config, slot):
    domain = config["domain"]
    main = slot == "main"
    if not main and (type(slot) is not int or slot not in PREVIEW_SLOTS):
        raise ValueError("Slot must be 0–3 or main")
    return {
        "slot": slot,
        "port": config["mainPort"] if main else config["basePort"] + slot,
        "consoleHost": f"console.{domain}" if main else f"console-dev{slot}.{domain}",
        "appHost": f"app.{domain}" if main else f"dev{slot}.app.{domain}",
    }


def routes(config):
    # Specific namespaces must precede the main application's broad wildcard.
    entries = [slot_config(config, i) for i in PREVIEW_SLOTS] + [slot_config(config, "main")]
    ingress = []
    for entry in entries:
        for hostname in (entry["consoleHost"], "*." + entry["appHost"]):
            ingress.append({"hostname": hostname, "service": f"http://127.0.0.1:{entry['port']}"})
    return {"ingress": ingress + [{"service": "http_status:404"}]}


def source_identity(root):
    root = Path(root)
    names = subprocess.check_output(["git", "-C", str(root), "ls-files", "-z", "--cached", "--others", "--exclude-standard"]).split(b"\0")
    digest = hashlib.sha256()
    for raw in sorted(set(names) - {b""}):
        path = root / os.fsdecode(raw)
        digest.update(raw + b"\0")
        if path.is_symlink():
            digest.update(b"link\0" + os.fsencode(os.readlink(path)))
        elif path.is_file():
            digest.update(b"file\0" + str(path.stat().st_mode & 0o111).encode() + b"\0" + path.read_bytes())
        elif path.is_dir():
            # The repository's retained dependency is tracked normally; submodules
            # are not currently part of this project.
            digest.update(b"directory")
        else:
            digest.update(b"deleted")
        digest.update(b"\0")
    return {"commit": git(root, "rev-parse", "HEAD"), "branch": git(root, "branch", "--show-current"),
            "dirty": bool(git(root, "status", "--porcelain")), "digest": digest.hexdigest()}


def sha256(path):
    digest = hashlib.sha256()
    with open(path, "rb") as stream:
        for block in iter(lambda: stream.read(1024 * 1024), b""):
            digest.update(block)
    return digest.hexdigest()


def registry(home):
    value = read_json(home / "registry.json", {"version": 1, "bindings": {}})
    if value.get("version") != 1:
        raise ValueError("Unsupported preview registry version")
    return value


def binding_key(root, main=False):
    return str(Path(root).resolve()) + ("::main" if main else "::worktree")


def allocate(home, root, main=False, slot=None, local=False, storage="sqlite"):
    config = settings(home)
    key = binding_key(root, main)
    with lock(home / "registry.lock"):
        state = registry(home)
        if key in state["bindings"]:
            existing = state["bindings"][key]
            slot_config(config, existing["slot"])  # Retired bindings remain available for cleanup only.
            if slot is not None and existing["slot"] != slot:
                raise ValueError("Existing binding uses a different slot; destroy it before reallocating")
            if local and not existing["local"]:
                raise ValueError("Existing binding is public; destroy it before changing mode")
            return existing
        used = {b["slot"] for b in state["bindings"].values()}
        chosen = "main" if main else slot
        if chosen is None:
            chosen = next((i for i in PREVIEW_SLOTS if i not in used), None)
        if chosen is None or chosen in used:
            raise ValueError("Requested slot unavailable; stopped and failed previews retain their bindings")
        value = slot_config(config, chosen)
        identity = uuid.uuid4().hex
        value.update({"id": identity, "root": str(Path(root).resolve()), "local": local, "storage": storage,
                      "runtime": str(home / "instances" / identity), "createdAt": time.time()})
        if local:
            value["origin"] = f"http://localhost:{value['port']}"
            value["accessURL"] = f"http://apps.localhost:{value['port']}"
        else:
            value["origin"] = "https://" + value["consoleHost"]
            value["accessURL"] = "https://" + value["appHost"]
        private_dir(Path(value["runtime"]))
        write_json(Path(value["runtime"]) / "binding.json", value)
        state["bindings"][key] = value
        write_json(home / "registry.json", state)
        return value


def process_identity(pid):
    try:
        raw = Path(f"/proc/{pid}/stat").read_text()
        fields = raw[raw.rfind(")") + 2:].split()
        if fields[0] == "Z":
            return None
        return {"pid": pid, "start": fields[19], "boot": Path("/proc/sys/kernel/random/boot_id").read_text().strip()}
    except (OSError, IndexError):
        return None


def process_alive(identity):
    return bool(identity and process_identity(identity["pid"]) ==
                {key: identity[key] for key in ("pid", "start", "boot")})


def _group_members(identity, known):
    """Verify ownership even after a session leader has exited and been reaped."""
    if identity["boot"] != Path("/proc/sys/kernel/random/boot_id").read_text().strip():
        return []
    leader = process_identity(identity["pid"])
    if leader and not process_alive(identity):
        return []  # The recorded PID was reused; it cannot authorize any signal.
    members = []
    for path in Path("/proc").iterdir():
        if not path.name.isdecimal():
            continue
        try:
            raw = (path / "stat").read_text()
            fields = raw[raw.rfind(")") + 2:].split()
            if fields[0] == "Z" or fields[2:4] != [str(identity["pid"])] * 2:
                continue
            members.append({"pid": int(path.name), "start": fields[19], "boot": identity["boot"]})
        except (FileNotFoundError, ProcessLookupError):
            continue
    if not members:
        return []
    if leader or any(member in known for member in members):
        return members
    # New children inherit this random marker. A bare group number is unsafe
    # after PID reuse; legacy records without proof fail closed and retain data.
    owner = identity.get("owner")
    for member in members:
        try:
            environment = Path(f"/proc/{member['pid']}/environ").read_bytes().split(b"\0")
            if owner and f"{PROCESS_OWNER_ENV}={owner}".encode() in environment and process_alive(member):
                return members
        except (FileNotFoundError, ProcessLookupError):
            continue
    raise RuntimeError("Cannot verify orphaned process group ownership; resource binding retained")


def stop_process_groups(identities, grace=10, kill_wait=5):
    """Stop all owned group members, including descendants outliving the leader."""
    groups = [(identity, []) for identity in identities]

    def remaining(signum=None):
        active = False
        for identity, known in groups:
            members = _group_members(identity, known)
            known[:] = members
            if members:
                active = True
                if signum is not None:
                    try:
                        os.killpg(identity["pid"], signum)
                    except ProcessLookupError:
                        pass
        return active

    for signum, seconds in ((signal.SIGTERM, grace), (signal.SIGKILL, kill_wait)):
        remaining(signum)
        deadline = time.monotonic() + seconds
        while remaining() and time.monotonic() < deadline:
            time.sleep(0.05)
        if not remaining():
            return
    raise RuntimeError("Managed process group still exists; resource binding retained")


def clean_environment():
    return {key: value for key, value in os.environ.items()
            if not key.startswith(("TUNNEL_", "CLOUDFLARE_", "CF_"))}


def request(binding, path, method="GET", data=None, cookie=None, host=None, public=False, limit=2 * 1024 * 1024):
    target = urlsplit(binding["origin"])
    authority = host or target.netloc
    if public:
        parsed = urlsplit(target.scheme + "://" + authority)
        connection_type = PreviewHTTPSConnection if parsed.scheme == "https" else http.client.HTTPConnection
        connection = connection_type(parsed.hostname, parsed.port, timeout=15)
    else:
        connection = http.client.HTTPConnection("127.0.0.1", binding["port"], timeout=6)
    headers = {"Host": authority, "Origin": binding["origin"]}
    if cookie:
        headers["Cookie"] = cookie
    body = None
    if data is not None:
        body = json.dumps(data).encode()
        headers["Content-Type"] = "application/json"
    try:
        connection.request(method, path, body, headers)
        response = connection.getresponse()
        payload = response.read(limit + 1)
        if len(payload) > limit:
            raise ValueError("HTTP response exceeds probe size limit")
        return response.status, dict(response.getheaders()), payload
    finally:
        connection.close()


def probe(binding, build, public=False):
    attempts = 2 if public else 1
    for attempt in range(attempts):
        try:
            status, _, body = request(binding, "/__preview.json", public=public)
            identity = json.loads(body)
            if status != 200 or any(identity.get(key) != build.get(key) for key in ("buildID", "instanceID", "source")):
                return {"ok": False, "reason": "build_identity_mismatch"}
            status, _, body = request(binding, "/healthz", public=public)
            if status != 200 or json.loads(body).get("status") != "ok":
                return {"ok": False, "reason": "health_check_failed"}
            status, _, body = request(binding, "/", public=public)
            return {"ok": status == 200 and b"<html" in body.lower(), "reason": "checked"}
        except ssl.SSLCertVerificationError:
            return {"ok": False, "reason": "dns_tls_or_http_failure"}
        except (OSError, http.client.HTTPException):
            # These three read-only GETs can safely recover from a transient network error.
            if attempt + 1 == attempts:
                return {"ok": False, "reason": "dns_tls_or_http_failure"}
        except ValueError:
            return {"ok": False, "reason": "dns_tls_or_http_failure"}


def inspect(binding, check_public=True):
    runtime = Path(binding["runtime"])
    worker = read_json(runtime / "worker.json", {})
    build = read_json(runtime / "build.json", {})
    status = read_json(runtime / "status.json", {"phase": "stopped"})
    running = process_alive(worker)
    current = source_identity(binding["root"]) if Path(binding["root"]).exists() else None
    source_changed = not build or not current or build["source"] != current
    local = probe(binding, build) if running and build else {"ok": False, "reason": "not_running"}
    remote = (probe(binding, build, public=True) if check_public and running and build and not binding["local"]
              else {"ok": None, "reason": "local_mode" if binding["local"] else "not_checked"})
    children = read_json(runtime / "children.json", [])
    processes_ok = bool(children) and all(process_alive(child) for child in children)
    binary = runtime / "builds" / build.get("buildID", "missing") / "tunnel"
    binary_matches = bool(build and binary.is_file() and sha256(binary) == build.get("binarySHA256"))
    return {"slot": binding["slot"], "origin": binding["origin"], "accessURL": binding["accessURL"],
            "localMode": binding["local"], "storage": binding["storage"], "running": running,
            "phase": status["phase"] if running or status["phase"] == "failed" else "stopped",
            "processesOK": processes_ok, "sourceChanged": source_changed,
            "binaryMatches": binary_matches, "build": build, "local": local, "public": remote,
            "ready": running and status["phase"] == "running" and processes_ok and binary_matches
                     and not source_changed and local["ok"] and (binding["local"] or remote["ok"] is True),
            "runtime": str(runtime), "log": str(runtime / "worker.log"),
            "detail": status.get("detail"), "applications": status.get("applications", [])}
