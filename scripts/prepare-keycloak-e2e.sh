#!/usr/bin/env bash
set -euo pipefail
# Run on Linux/macOS with Java 21 available; no Docker daemon is needed.
keycloak_e2e_root="${1:?Usage: prepare-keycloak-e2e.sh OUTPUT_DIRECTORY}"
keycloak_e2e_version=26.8.0
keycloak_e2e_sha256=9e41da899f838a58cd510fc98ed4f7cadc715aed5683e42aca20a0c9a2a3980a
mkdir -p "$keycloak_e2e_root"
keycloak_e2e_archive="$keycloak_e2e_root/keycloak.tar.gz"
if [ ! -f "$keycloak_e2e_archive" ]; then
  curl --fail --location --retry 3 --output "$keycloak_e2e_archive" "https://github.com/keycloak/keycloak/releases/download/$keycloak_e2e_version/keycloak-$keycloak_e2e_version.tar.gz"
fi
python3 - "$keycloak_e2e_archive" "$keycloak_e2e_sha256" <<'PY'
import hashlib,sys
with open(sys.argv[1],'rb') as f:
    digest=hashlib.file_digest(f,'sha256').hexdigest()
if digest!=sys.argv[2]:
    raise SystemExit('Keycloak SHA-256 mismatch; remove archive and retry')
print('Keycloak 26.8.0 SHA-256: PASS')
PY
mkdir -p "$keycloak_e2e_root/server"
tar --no-same-owner -xzf "$keycloak_e2e_archive" -C "$keycloak_e2e_root/server" --strip-components=1
printf 'KEYCLOAK_HOME=%s/server\n' "$keycloak_e2e_root"
