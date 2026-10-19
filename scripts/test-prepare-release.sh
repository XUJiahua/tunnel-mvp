#!/usr/bin/env bash
set -euo pipefail

script_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
release_test_dir=$(mktemp -d)
trap 'rm -rf "$release_test_dir"' EXIT

git init --quiet --bare --initial-branch=main "$release_test_dir/remote.git"
git init --quiet --initial-branch=main "$release_test_dir/work"
cd "$release_test_dir/work"
git config user.name release-test
git config user.email release-test@example.invalid
git remote add origin "$release_test_dir/remote.git"
git commit --quiet --allow-empty --message 'first version'
first_commit=$(git rev-parse HEAD)

if RELEASE_TAG='invalid/tag' bash "$script_dir/prepare-release.sh" >/dev/null 2>&1; then
  echo 'invalid version was accepted' >&2
  exit 1
fi
RELEASE_TAG=v0.1.0 bash "$script_dir/prepare-release.sh" >/dev/null 2>&1
[[ $(git --git-dir="$release_test_dir/remote.git" rev-parse 'refs/tags/v0.1.0^{commit}') == "$first_commit" ]]
[[ $(git cat-file -t refs/tags/v0.1.0) == tag ]]

# Retrying the same version is allowed; moving it to new code is forbidden.
RELEASE_TAG=v0.1.0 bash "$script_dir/prepare-release.sh" >/dev/null 2>&1
git commit --quiet --allow-empty --message 'second version'
if RELEASE_TAG=v0.1.0 bash "$script_dir/prepare-release.sh" >/dev/null 2>&1; then
  echo 'existing release tag was moved' >&2
  exit 1
fi
[[ $(git --git-dir="$release_test_dir/remote.git" rev-parse 'refs/tags/v0.1.0^{commit}') == "$first_commit" ]]
echo 'Release tag validation, creation, retry and immutability: PASS'
