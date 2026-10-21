#!/usr/bin/env bash
set -euo pipefail
cd "$(git rev-parse --show-toplevel)"
git config --local core.hooksPath .githooks
chmod +x .githooks/pre-commit
printf 'Git hooks installed; full verification remains an explicit command and CI gate.\n'
