#!/usr/bin/env python3
"""Prepare raw Agent executables and unsigned manifest; signing is a separate offline step."""
import argparse
import datetime
import hashlib
import json
from pathlib import Path
import re
import shutil
import urllib.parse

p = argparse.ArgumentParser(description=__doc__)
p.add_argument('--artifact', action='append', required=True, help='GOOS/GOARCH=/absolute/path/to/raw/executable')
p.add_argument('--directory', required=True, help='New output directory; never overwritten')
p.add_argument('--base-url', required=True, help='Direct public HTTPS URL serving this directory')
p.add_argument('--version', required=True, help='Exact main.version embedded in the binaries')
p.add_argument('--sequence', type=int, required=True)
p.add_argument('--expires-hours', type=int, default=24)
a = p.parse_args()
url = urllib.parse.urlparse(a.base_url)
if (url.scheme != 'https' or not url.hostname or url.username or url.query or url.fragment
        or not re.fullmatch(r'[A-Za-z0-9][A-Za-z0-9.+_-]{0,79}', a.version)
        or not 0 < a.sequence < 2**64 or not 1 <= a.expires_hours <= 168):
    p.error('HTTPS URL, safe version, positive uint64 sequence and 1–168 hour expiry required')
artifacts = []
seen = set()
sources = []
for value in a.artifact:
    platform, sep, name = value.partition('=')
    if not sep or platform not in {f'{o}/{r}' for o in ('linux', 'darwin', 'windows') for r in ('amd64', 'arm64')} or platform in seen:
        p.error('Each supported platform must appear at most once')
    source = Path(name)
    if source.is_symlink() or not source.is_file() or not 0 < source.stat().st_size <= 128 * 1024 * 1024:
        p.error('Artifact must be a regular raw executable of 1–128 MiB')
    seen.add(platform)
    sources.append((platform, source))
out = Path(a.directory)
out.mkdir(mode=0o700, parents=True, exist_ok=False)
for platform, source in sources:
    goos, arch = platform.split('/')
    name = f'tunnel_{a.version}_{goos}_{arch}' + ('.exe' if goos == 'windows' else '')
    target = out / name
    with source.open('rb') as src, target.open('xb') as dst:
        shutil.copyfileobj(src, dst)
    target.chmod(0o700)
    with target.open('rb') as data:
        digest = hashlib.file_digest(data, 'sha256').hexdigest()
    artifacts.append(dict(platform=platform, url=a.base_url.rstrip('/') + '/' + name,
                          sha256=digest, size=target.stat().st_size))
manifest = dict(format=1, sequence=a.sequence, version=a.version,
                expiresAt=(datetime.datetime.now(datetime.timezone.utc) + datetime.timedelta(hours=a.expires_hours)).isoformat().replace('+00:00', 'Z'),
                artifacts=artifacts)
with (out / 'manifest.json').open('x') as f:
    json.dump(manifest, f, indent=2)
print(f'Prepared {len(artifacts)} executable(s) and unsigned manifest.json; sign before publishing')
