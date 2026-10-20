#!/usr/bin/env python3
"""Verify the six release packages and their manifest before publishing."""
import argparse
import gzip
import hashlib
from pathlib import Path
import re
import tarfile
import zipfile


def verify(directory: Path, version: str | None = None) -> None:
    archives = sorted([*directory.glob('*.tar.gz'), *directory.glob('*.zip')])
    if len(archives) != 6:
        raise ValueError(f'Expected six packages, found {len(archives)}')
    versions, platforms = set(), set()
    for archive in archives:
        match = re.fullmatch(r'tunnel_(.+)_(linux|darwin|windows)_(amd64|arm64)\.(tar\.gz|zip)', archive.name)
        if not match:
            raise ValueError(f'Unexpected package name: {archive.name}')
        package_version, system, arch, extension = match.groups()
        versions.add(package_version)
        platforms.add((system, arch))
        if extension != ('zip' if system == 'windows' else 'tar.gz'):
            raise ValueError(f'Incorrect format: {archive.name}')
    expected_platforms = {(system, arch) for system in ('linux', 'darwin', 'windows') for arch in ('amd64', 'arm64')}
    if platforms != expected_platforms or len(versions) != 1 or (version and versions != {version}):
        raise ValueError(f'Unexpected platform/version matrix: {platforms}, {versions}')

    checksums = {}
    for line in (directory / 'checksums.txt').read_text().splitlines():
        digest, name = line.split()
        if not re.fullmatch('[0-9a-f]{64}', digest) or name in checksums:
            raise ValueError('Invalid or duplicated checksum entry')
        checksums[name] = digest
    if set(checksums) != {archive.name for archive in archives}:
        raise ValueError('Checksum manifest must contain exactly the six packages')

    required = {'README.md', 'CHANGELOG.md', 'examples/server.json', 'examples/agent.json', 'docs/operations.md', 'docs/design/storage.md', 'docs/storage-operations.md', 'docs/design/groups-invitations.md', 'docs/groups-operations.md', 'docs/design/administrators.md', 'docs/administrator-operations.md', 'docs/design/organizations-roles.md', 'docs/organization-operations.md', 'docs/design/resources-targets.md', 'docs/resource-operations.md', 'docs/design/monitoring-history.md', 'docs/monitoring-operations.md', 'docs/design/audit-index.md', 'docs/audit-operations.md', 'docs/third-party-dependencies.md', 'docs/licenses/remotedialer-LICENSE', 'docs/design/session-lifecycle.md', 'docs/session-operations.md', 'docs/design/oidc.md', 'docs/oidc-operations.md', 'examples/oidc/server.production.json', 'examples/oidc/server.development.json', 'examples/oidc/keycloak.compose.yaml'}
    required.update({
        'docs/release-readiness.md', 'docs/upgrade-v0.1.1.md',
        'docs/design/storage-queries.md', 'docs/storage-query-operations.md', 'docs/capacity/storage-queries.txt',
        'scripts/verify-upgrade.py', 'scripts/verify-upgrade.sh',
        'docs/design/agent-instances.md', 'docs/agent-operations.md',
        'docs/design/agent-remote-policy.md', 'docs/agent-policy-operations.md',
        'docs/design/agent-updates.md', 'docs/agent-update-operations.md',
        'docs/design/session-pools.md', 'docs/connection-mode-operations.md',
        'docs/design/delegated-roles.md', 'docs/delegated-role-operations.md',
        'docs/design/incremental-sqlite.md', 'docs/sqlite-incremental-operations.md',
        'docs/design/audit-rotation.md', 'docs/audit-rotation-operations.md',
        'docs/design/keycloak-session-sync.md', 'docs/keycloak-sync-operations.md',
        'docs/design/application-oidc.md', 'docs/application-oidc-operations.md',
        'docs/capacity-report.md', 'docs/roadmap.md',
        'docs/design/user-queries.md', 'docs/user-query-operations.md', 'docs/capacity/user-queries.txt',
        'docs/design/resource-queries.md', 'docs/resource-query-operations.md', 'docs/capacity/resource-queries.txt',
        'docs/design/control-plane-ha.md', 'docs/ha-operations.md', 'docs/e2e-acceptance.md',
        'examples/oidc-app/README.md', 'examples/oidc-app/app.go',
        'examples/oidc-app/main.go', 'examples/oidc-app/go.mod', 'examples/oidc-app/go.sum',
    })
    for archive in archives:
        with archive.open('rb') as source:
            actual = hashlib.file_digest(source, 'sha256').hexdigest()
        if actual != checksums[archive.name]:
            raise ValueError(f'SHA-256 mismatch: {archive.name}')
        binary = 'tunnel.exe' if '_windows_' in archive.name else 'tunnel'
        if archive.suffix == '.zip':
            with zipfile.ZipFile(archive) as package:
                names = set(package.namelist())
                bad_file = package.testzip()
                if bad_file:
                    raise ValueError(f'ZIP CRC failure: {archive.name}: {bad_file}')
        else:
            # Consume gzip through its footer to detect a missing or corrupt end.
            with gzip.open(archive, 'rb') as source:
                while source.read(1024 * 1024):
                    pass
            with tarfile.open(archive) as package:
                names = set(package.getnames())
        if not required.union({binary}).issubset(names):
            raise ValueError(f'Missing binary, documentation or examples: {archive.name}')
        print(f'PASS {archive.name}: SHA-256, archive integrity and package contents')


if __name__ == '__main__':
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('directory', type=Path)
    parser.add_argument('--version')
    args = parser.parse_args()
    verify(args.directory, args.version)
