#!/usr/bin/env bash
# Run from the merged checkout to rebuild the binary used by the user service.
set -euo pipefail
repo="$(cd "$(dirname "$0")/.." && pwd)"
cd "$repo"
if [[ "$(git branch --show-current)" != main ]] || [[ -n "$(git status --porcelain)" ]]; then
  echo 'Use a clean main checkout after merging the Messages change.' >&2
  exit 1
fi
git fetch origin main
if [[ "$(git rev-parse HEAD)" != "$(git rev-parse origin/main)" ]]; then
  echo 'Update main to origin/main before restarting.' >&2
  exit 1
fi
stage="$(mktemp "$repo/.herd-remote-build.XXXXXX")"
trap 'rm -f "$stage"' EXIT
CGO_ENABLED=0 "${GO:-go}" build -o "$stage" .
chmod 755 "$stage"
mv "$stage" "$repo/herd-remote"
systemctl --user restart herd-remote.service
systemctl --user is-active herd-remote.service
