#!/bin/bash
# Prepares a Claude Code cloud session for working on Carryon: downloads the Go modules and
# installs restic, which the engine tests run against. Does nothing outside cloud sessions.
set -euo pipefail

if [ "${CLAUDE_CODE_REMOTE:-}" != "true" ]; then
  exit 0
fi

cd "${CLAUDE_PROJECT_DIR:-$(dirname "$0")/../..}"

# Keep AI attribution out of commits made here (AGENTS.md, "Pull requests").
git config core.hooksPath .claude/git-hooks

go mod download

RESTIC_VERSION=0.19.1
if ! restic version 2>/dev/null | grep -q "^restic ${RESTIC_VERSION} "; then
  case "$(uname -m)" in
    x86_64) arch=amd64 ;;
    aarch64) arch=arm64 ;;
    *) echo "session-start: no restic build for $(uname -m); skipping" >&2; exit 0 ;;
  esac
  file="restic_${RESTIC_VERSION}_linux_${arch}.bz2"
  base="https://github.com/restic/restic/releases/download/v${RESTIC_VERSION}"
  tmp=$(mktemp -d)
  trap 'rm -rf "$tmp"' EXIT
  curl -fsSL --retry 3 -o "$tmp/$file" "$base/$file"
  curl -fsSL --retry 3 -o "$tmp/SHA256SUMS" "$base/SHA256SUMS"
  (cd "$tmp" && grep " ${file}\$" SHA256SUMS | sha256sum -c --quiet -)
  bunzip2 "$tmp/$file"
  install -m 755 "$tmp/${file%.bz2}" /usr/local/bin/restic
fi
restic version
