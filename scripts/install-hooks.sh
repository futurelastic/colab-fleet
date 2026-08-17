#!/bin/sh
# install-hooks.sh — points git at .githooks/ (idempotent, run once per repo per machine).
# core.hooksPath lives in .git/config, so it is per-machine config and does not sync.
set -e

repo_root="$(git rev-parse --show-toplevel 2>/dev/null)" || {
  echo "Not a git repo. Run 'git init' first."
  exit 1
}
cd "$repo_root"

chmod +x .githooks/* 2>/dev/null || true
git config core.hooksPath .githooks

echo "core.hooksPath = .githooks (pre-commit hook enabled)."
