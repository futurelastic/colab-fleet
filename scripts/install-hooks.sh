#!/bin/sh
# install-hooks.sh — trỏ git tới .githooks/ (idempotent, chạy 1 lần/repo/máy).
# core.hooksPath nằm trong .git/config nên là cấu hình per-machine, không sync.
set -e

repo_root="$(git rev-parse --show-toplevel 2>/dev/null)" || {
  echo "❌ Không phải git repo. Chạy 'git init' trước."
  exit 1
}
cd "$repo_root"

chmod +x .githooks/* 2>/dev/null || true
git config core.hooksPath .githooks

echo "✅ core.hooksPath = .githooks (hook pre-commit gitleaks đã bật)."
