#!/bin/sh
# install-hooks.sh — installs the racevis pre-commit lint hook.
# Run once after cloning: bash scripts/install-hooks.sh

set -e

HOOK_SRC="scripts/pre-commit"
HOOK_DST=".git/hooks/pre-commit"

if [ ! -d ".git" ]; then
  echo "Error: run this from the racevis repo root"
  exit 1
fi

cp "$HOOK_SRC" "$HOOK_DST"
chmod +x "$HOOK_DST"
echo "Pre-commit hook installed at $HOOK_DST"
echo "Every 'git commit' will now run golangci-lint before committing."
