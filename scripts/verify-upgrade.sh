#!/usr/bin/env bash
set -euo pipefail
# Linux amd64 CI wrapper. Never reads the user's Server configuration.
upgrade_new_binary="${1:?Usage: verify-upgrade.sh NEW_BINARY NEW_OUTPUT_DIRECTORY}"
upgrade_output_directory="${2:?Usage: verify-upgrade.sh NEW_BINARY NEW_OUTPUT_DIRECTORY}"
upgrade_script_directory="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
mkdir -m 700 -- "$upgrade_output_directory"
curl --fail --location --retry 3 --output "$upgrade_output_directory/legacy.tar.gz" \
  https://github.com/XUJiahua/tunnel-mvp/releases/download/v0.1.1/tunnel_0.1.1_linux_amd64.tar.gz
python3 - "$upgrade_output_directory" <<'PY'
import hashlib,sys,tarfile
from pathlib import Path
p=Path(sys.argv[1])
with (p/'legacy.tar.gz').open('rb') as source:
    digest=hashlib.file_digest(source,'sha256').hexdigest()
if digest!='47de51ee874c2dd0f7f9782d1eb02705ca283d680f07ef669f92188576c0d69b':
    raise SystemExit('Published v0.1.1 archive SHA-256 mismatch')
with tarfile.open(p/'legacy.tar.gz') as archive:
    executable=archive.getmember('tunnel')
    if not executable.isfile():
        raise SystemExit('Legacy executable is not a regular file')
    binary=archive.extractfile(executable).read()
with (p/'tunnel-v0.1.1').open('xb') as destination:
    destination.write(binary)
(p/'tunnel-v0.1.1').chmod(0o700)
PY
python3 "$upgrade_script_directory/verify-upgrade.py" \
  --old-binary "$upgrade_output_directory/tunnel-v0.1.1" \
  --new-binary "$upgrade_new_binary" \
  --output "$upgrade_output_directory/report.json"
