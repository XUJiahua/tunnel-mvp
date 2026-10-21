#!/usr/bin/env python3
"""Render or provision this project's dedicated Cloudflare Tunnel on this host."""
import argparse
from concurrent.futures import ThreadPoolExecutor
import json
from pathlib import Path
import shutil
import subprocess
import sys

from preview.core import private_dir, routes, settings, state_home, write_json
from preview.network import connect_tls


def unit_quote(value):
    return '"' + str(value).replace("\\", "\\\\").replace('"', '\\"').replace("%", "%%") + '"'


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("command", choices=["plan", "provision", "status", "check"])
    parser.add_argument("--origincert", type=Path, default=Path.home() / ".cloudflared/cert.cubbber-dev.pem")
    parser.add_argument("--name", default="tunnel-mvp-preview")
    args = parser.parse_args()
    home = private_dir(state_home())
    config = settings(home)
    if args.command == "status":
        subprocess.run(["systemctl", "--user", "status", "tunnel-mvp-preview.service", "--no-pager"], check=True)
        return
    if args.command == "check":
        names = [route["hostname"].replace("*.", "preview-probe.")
                 for route in routes(config)["ingress"] if "hostname" in route]

        def check(hostname):
            try:
                with connect_tls(hostname, 443):
                    return {"hostname": hostname, "tls": True}
            except OSError as error:
                return {"hostname": hostname, "tls": False, "error": str(error)}

        with ThreadPoolExecutor(max_workers=8) as pool:
            checks = list(pool.map(check, names))
        write_json(home / "host-check.json", checks)
        print(json.dumps(checks, indent=2))
        if not all(item["tls"] for item in checks):
            raise ValueError("DNS or edge TLS checks failed; see host-check.json")
        return
    plan = routes(config)
    plan["domain"] = config["domain"]
    plan["tunnelName"] = args.name
    plan["dnsRecords"] = [route["hostname"] for route in plan["ingress"] if "hostname" in route]
    # The complete reviewable plan is written before any external changes.
    write_json(home / "host-plan.json", plan)
    if args.command == "plan":
        print(json.dumps(plan, indent=2))
        return
    cloudflared = shutil.which("cloudflared")
    if not cloudflared or not args.origincert.is_file():
        raise ValueError("cloudflared and a zone-scoped origin certificate are required")
    # Check service management before creating remote resources.
    subprocess.run(["systemctl", "--user", "is-active", "default.target"], check=True)
    command = [cloudflared, "--origincert", str(args.origincert.resolve()), "tunnel"]

    def tunnels():
        return json.loads(subprocess.check_output([*command, "list", "--output", "json"]))

    existing = next((t for t in tunnels() if t["name"] == args.name and t.get("deleted_at", "0001").startswith("0001")), None)
    if not existing:
        subprocess.run([*command, "create", args.name], check=True)
        existing = next(t for t in tunnels() if t["name"] == args.name)
    identity = existing["id"]
    credentials = Path.home() / ".cloudflared" / (identity + ".json")
    if not credentials.is_file():
        raise ValueError("Dedicated tunnel exists but its credentials are not on this host")
    tunnel_config = {"tunnel": identity, "credentials-file": str(credentials), "ingress": plan["ingress"]}
    write_json(home / "tunnel.json", tunnel_config)
    subprocess.run([cloudflared, "--config", str(home / "tunnel.json"), "tunnel", "ingress", "validate"], check=True)
    for hostname in plan["dnsRecords"]:
        # No --overwrite-dns: conflicting records are preserved and reported.
        subprocess.run([*command, "route", "dns", identity, hostname], check=True)
    unit = Path.home() / ".config/systemd/user/tunnel-mvp-preview.service"
    unit.parent.mkdir(parents=True, exist_ok=True)
    start = [cloudflared, "--no-autoupdate", "--config", str(home / "tunnel.json"), "tunnel", "run", identity]
    unit.write_text("[Unit]\nDescription=tunnel-mvp development preview ingress\nAfter=network-online.target\n\n"
                    "[Service]\nExecStart=" + " ".join(map(unit_quote, start)) +
                    "\nRestart=on-failure\nRestartSec=5\nUMask=0077\n\n[Install]\nWantedBy=default.target\n")
    subprocess.run(["systemctl", "--user", "daemon-reload"], check=True)
    subprocess.run(["systemctl", "--user", "enable", "--now", "tunnel-mvp-preview.service"], check=True)
    # Repeated provisioning must apply any port or route changes to the connector.
    subprocess.run(["systemctl", "--user", "restart", "tunnel-mvp-preview.service"], check=True)
    write_json(home / "host-provisioned.json", {"tunnelID": identity, "tunnelName": args.name,
                                               "dnsRecords": plan["dnsRecords"],
                                               "certificateCoverage": "not_verified"})
    print(f"Dedicated tunnel and {len(plan['dnsRecords'])} DNS records configured. "
          "Application edge certificates still require verification.")


if __name__ == "__main__":
    try:
        main()
    except (OSError, ValueError, subprocess.SubprocessError, StopIteration) as error:
        print(f"preview-host: {error}", file=sys.stderr)
        sys.exit(1)
