#!/usr/bin/env python3
"""Manage persistent, isolated tunnel previews on a Linux development host."""
import argparse
import json
import os
from pathlib import Path
import shutil
import signal
import subprocess
import sys
import time

from preview.core import (PREVIEW_SLOTS, allocate, binding_key, clean_environment, git, inspect, lock, private_dir,
                          process_alive, process_identity, read_json, registry, routes, settings,
                          slot_config, state_home, write_json)


def stop(binding):
    runtime = Path(binding["runtime"])
    worker = read_json(runtime / "worker.json", {})
    children = read_json(runtime / "children.json", [])
    if process_alive(worker):
        try:
            os.kill(worker["pid"], signal.SIGTERM)
        except ProcessLookupError:
            pass
        deadline = time.monotonic() + 25
        while process_alive(worker) and time.monotonic() < deadline:
            time.sleep(0.1)
    # Reclaim orphaned children after an interrupted or killed supervisor.
    for identity in children + [worker]:
        if process_alive(identity):
            try:
                if identity is worker:
                    os.kill(identity["pid"], signal.SIGKILL)
                else:
                    os.killpg(identity["pid"], signal.SIGTERM)
            except ProcessLookupError:
                pass
    deadline = time.monotonic() + 10
    while any(process_alive(child) for child in children) and time.monotonic() < deadline:
        time.sleep(0.1)
    for child in children:
        if process_alive(child):
            try:
                os.killpg(child["pid"], signal.SIGKILL)
            except ProcessLookupError:
                pass
    if any(process_alive(child) for child in children + [worker]):
        raise RuntimeError("Managed process still exists; resource binding retained")
    write_json(runtime / "status.json", {"phase": "stopped", "updatedAt": time.time()})


def start(home, binding):
    runtime = Path(binding["runtime"])
    # Registry is authoritative after a mode change interrupted before startup.
    write_json(runtime / "binding.json", binding)
    write_json(runtime / "status.json", {"phase": "queued", "updatedAt": time.time()})
    environment = clean_environment()
    environment["PYTHONPATH"] = str(Path(__file__).resolve().parent)
    with open(runtime / "worker.log", "ab", buffering=0) as output:
        child = subprocess.Popen([sys.executable, "-m", "preview.worker", str(home), str(runtime)],
                                 cwd=binding["root"], env=environment, stdin=subprocess.DEVNULL,
                                 stdout=output, stderr=output, start_new_session=True)
    identity = process_identity(child.pid)
    if not identity:
        raise RuntimeError("Preview supervisor could not start")
    write_json(runtime / "worker.json", identity)


def publish(home, binding):
    slot_config(settings(home), binding["slot"])
    stop(binding)
    public = {**binding, "local": False, "origin": "https://" + binding["consoleHost"],
              "accessURL": "https://" + binding["appHost"]}
    with lock(home / "registry.lock"):
        state = registry(home)
        key = binding_key(binding["root"])
        if state["bindings"].get(key) != binding:
            raise ValueError("Preview binding changed; retry with the current registry")
        state["bindings"][key] = public
        write_json(home / "registry.json", state)
    return public


def main():
    os.umask(0o077)
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("command", choices=["up", "refresh", "publish", "status", "url", "stop", "destroy", "list",
                                             "routes", "dns", "certificates", "credentials", "smoke"])
    parser.add_argument("--root", type=Path, default=Path(__file__).resolve().parents[1])
    parser.add_argument("--slot", type=int, choices=PREVIEW_SLOTS)
    parser.add_argument("--main", action="store_true", help="Manage the independent main-version binding")
    parser.add_argument("--local", action="store_true", help="Use localhost without public DNS/TLS")
    parser.add_argument("--storage", choices=["json", "sqlite"], default=None)
    parser.add_argument("--wait", type=int, default=300, help="Seconds to wait; worker persists after timeout")
    parser.add_argument("--if-exists", action="store_true")
    parser.add_argument("--all", action="store_true", help="With status, inspect every binding")
    parser.add_argument("--tunnel", help="Tunnel UUID for generated CNAME records")
    args = parser.parse_args()
    if args.main and (args.slot is not None or args.local):
        parser.error("--main cannot be combined with --slot or --local")
    if args.command == "publish" and (args.main or args.local or args.slot is not None or args.storage is not None):
        parser.error("publish uses the existing worktree binding; omit --main, --local, --slot and --storage")
    if args.wait < 0:
        parser.error("--wait must be nonnegative")
    home = private_dir(state_home())
    config = settings(home)
    if args.command == "routes":
        print(json.dumps(routes(config), indent=2))
        return 0
    if args.command == "dns":
        if not args.tunnel or not __import__("re").fullmatch(r"[0-9a-fA-F]{8}(?:-[0-9a-fA-F]{4}){3}-[0-9a-fA-F]{12}", args.tunnel):
            parser.error("dns requires --tunnel UUID")
        records = [{"type": "CNAME", "name": entry["hostname"], "content": args.tunnel + ".cfargotunnel.com", "proxied": True}
                   for entry in routes(config)["ingress"] if "hostname" in entry]
        print(json.dumps(records, indent=2))
        return 0
    if args.command == "certificates":
        print(json.dumps({"consoleCoverage": "*." + config["domain"],
                          "applicationCoverage": ["*." + slot_config(config, slot)["appHost"]
                                                  for slot in ["main", *PREVIEW_SLOTS]]}, indent=2))
        return 0
    if args.command == "list":
        print(json.dumps(list(registry(home)["bindings"].values()), indent=2))
        return 0
    if args.command == "status" and args.all:
        print(json.dumps([inspect(binding) for binding in registry(home)["bindings"].values()], indent=2))
        return 0
    root = args.root.resolve()
    key = binding_key(root, args.main)
    # The operation lease prevents refresh/stop/destroy races on one binding.
    operation_key = __import__("hashlib").sha256(key.encode()).hexdigest()
    with lock(home / "operations" / (operation_key + ".lock")):
        binding = registry(home)["bindings"].get(key)
        if args.command in ("up", "refresh", "publish"):
            if args.command == "publish" and not binding:
                raise ValueError("No preview bound to this checkout; run up first")
            if args.main and (git(root, "branch", "--show-current") != "main" or git(root, "status", "--porcelain")):
                raise ValueError("Main preview requires a clean checkout on branch main")
            if binding and args.storage is not None and binding["storage"] != args.storage:
                raise ValueError("Storage is immutable for a binding; use a new isolated preview")
            binding = allocate(home, root, args.main, args.slot, args.local, args.storage or "sqlite")
            running = process_alive(read_json(Path(binding["runtime"]) / "worker.json", {}))
            if args.command == "publish":
                binding = publish(home, binding)
                running = False
            elif args.command == "refresh":
                stop(binding)
                running = False
            if not running:
                stop(binding)  # Clean any orphans before restarting a failed worker.
                start(home, binding)
            deadline = time.monotonic() + args.wait
            while time.monotonic() < deadline:
                state = read_json(Path(binding["runtime"]) / "status.json", {})
                if state.get("phase") in ("running", "failed"):
                    break
                time.sleep(0.5)
            result = inspect(binding)
            print(json.dumps(result, indent=2))
            return 0 if result["ready"] else 1
        if not binding:
            if args.if_exists and args.command in ("stop", "destroy"):
                return 0
            raise ValueError("No preview bound to this checkout; run up first")
        runtime = Path(binding["runtime"])
        if args.command == "status":
            print(json.dumps(inspect(binding), indent=2))
        elif args.command == "url":
            print(binding["origin"])
        elif args.command == "credentials":
            # Deliberate local-only disclosure; never part of status or logs.
            print(json.dumps(read_json(runtime / "credentials.json", {"error": "Not initialized"}), indent=2))
        elif args.command == "smoke":
            if not inspect(binding)["ready"]:
                raise ValueError("Preview must be ready before browser smoke; refresh after editing")
            subprocess.run(["node", str(Path(__file__).with_name("preview-smoke.mjs")), str(runtime)],
                           cwd=root, check=True)
        elif args.command in ("stop", "destroy"):
            stop(binding)
            if args.command == "destroy":
                # Only the UUID directory registered by this manager is eligible.
                if runtime.parent != home / "instances" or runtime.name != binding["id"] or runtime.is_symlink():
                    raise ValueError("Unsafe instance path; refusing cleanup")
                shutil.rmtree(runtime)
                with lock(home / "registry.lock"):
                    state = registry(home)
                    del state["bindings"][key]
                    write_json(home / "registry.json", state)
            print(json.dumps({"slot": binding["slot"], "result": args.command}))
    return 0


if __name__ == "__main__":
    try:
        sys.exit(main())
    except (ValueError, RuntimeError, OSError, subprocess.SubprocessError) as error:
        print(f"preview: {error}", file=sys.stderr)
        sys.exit(1)
