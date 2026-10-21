#!/usr/bin/env python3
"""Stable development entry points; the existing Makefile and CI remain authoritative."""
import argparse
import json
import os
from pathlib import Path
import shutil
import subprocess
import sys
import time

from preview.core import clean_environment, lock, private_dir, state_home

ROOT = Path(__file__).resolve().parents[1]


def run(command, cwd=ROOT, env=None):
    print("+ " + " ".join(map(str, command)), flush=True)
    subprocess.run(command, cwd=cwd, env=clean_environment() if env is None else env, check=True)


def infrastructure():
    run([sys.executable, "-m", "unittest", "discover", "-s", "scripts/tests", "-v"])


def doctor():
    checks = []
    for name, command in (("Go", ["go", "version"]), ("Node", ["node", "--version"]),
                          ("pnpm", ["corepack", "pnpm", "--version"]), ("Python", [sys.executable, "--version"]),
                          ("make", ["make", "--version"]), ("Git", ["git", "--version"])):
        try:
            result = subprocess.run(command, cwd=ROOT / "web", capture_output=True, text=True, timeout=45)
            version = result.stdout.strip().splitlines()[0] if result.stdout.strip() else ""
            ok = result.returncode == 0
            if name == "Go":
                import re
                match = re.search(r"go(\d+)\.(\d+)\.(\d+)", version)
                ok = bool(ok and match and tuple(map(int, match.groups())) >= (1, 27, 2))
            if name == "Node":
                ok = ok and int(version.lstrip("v").split(".")[0]) >= 24
            if name == "pnpm":
                ok = ok and version == "12.14.0"
            checks.append({"tool": name, "ok": ok, "version": version,
                           "detail": result.stderr.strip()[-500:] if not ok else None})
        except (OSError, subprocess.TimeoutExpired, ValueError) as error:
            checks.append({"tool": name, "ok": False, "detail": str(error)})
    print(json.dumps({"checks": checks, "optional": {name: bool(shutil.which(name))
                                                   for name in ("cloudflared", "docker", "openssl")}}, indent=2))
    return 0 if all(check["ok"] for check in checks) else 1


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("command", choices=["doctor", "install", "verify"])
    parser.add_argument("--profile", choices=["core", "infrastructure", "browser", "postgres", "ha", "upgrade", "release"], default="core")
    parser.add_argument("--postgres", choices=["15", "18"], default="18")
    args = parser.parse_args()
    if args.command == "doctor":
        return doctor()
    home = private_dir(state_home())
    if args.command == "install":
        with lock(home / "build.lock"):
            run(["corepack", "pnpm", "install", "--frozen-lockfile"], ROOT / "web")
            run(["go", "mod", "download"])
            run(["corepack", "pnpm", "exec", "playwright", "install", "chromium"], ROOT / "web")
        run(["bash", "scripts/install-git-hooks.sh"])
        return 0
    if args.profile == "infrastructure":
        infrastructure()
        return 0
    # Existing browser fixtures and the HA deployment lab use fixed ports.
    # One host-wide lease serializes their verification across all worktrees.
    with lock(home / "verify.lock"), lock(home / "build.lock"):
        if args.profile == "core":
            infrastructure()
            run(["make", "build"])
            run(["make", "test"])
            print("Core checks passed. PostgreSQL, browser, HA, upgrade and release profiles were not executed.")
        elif args.profile == "browser":
            run(["make", "build"])
            for storage in ("json", "sqlite"):
                env = {**clean_environment(), "TUNNEL_TEST_STORAGE": storage}
                # Keycloak is deliberately outside this development environment.
                run(["corepack", "pnpm", "exec", "playwright", "test"], ROOT / "web", env)
            print("Browser checks completed; inspect skips. Keycloak and cluster acceptance require their own environments.")
        elif args.profile == "postgres":
            if not os.environ.get("TUNNEL_TEST_POSTGRES_DSN"):
                raise ValueError("postgres requires TUNNEL_TEST_POSTGRES_DSN pointing to an isolated test service")
            env = {**clean_environment(), "TUNNEL_REQUIRE_POSTGRES_TESTS": "1",
                   "TUNNEL_TEST_POSTGRES_DSN": os.environ["TUNNEL_TEST_POSTGRES_DSN"]}
            run(["go", "test", "-race", "-count=1", "-timeout=15m", "./internal/state", "./internal/control", "./cmd/tunnel", "-run", "Postgres"], env=env)
            print("PostgreSQL Go checks completed. PG15/18 browser and HA matrix remain separate CI gates.")
        elif args.profile == "ha":
            reports = private_dir(home / "reports")
            identity = str(time.time_ns())
            run([sys.executable, "scripts/verify-ha-deployment.py", "--postgres", args.postgres,
                 "--runtime", str(home / ("ha-" + identity)), "--report", str(reports / ("ha-" + identity + ".json"))])
        elif args.profile == "upgrade":
            run(["make", "build"])
            destination = home / ("upgrade-" + str(time.time_ns()))
            run(["bash", "scripts/verify-upgrade.sh", str(ROOT / "bin/tunnel"), str(destination)])
        elif args.profile == "release":
            run(["make", "release-snapshot"])
    return 0


if __name__ == "__main__":
    try:
        sys.exit(main())
    except (OSError, ValueError, subprocess.SubprocessError) as error:
        print(f"dev: {error}", file=sys.stderr)
        sys.exit(1)
