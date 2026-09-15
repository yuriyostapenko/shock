#!/bin/sh
# Container start, before the CLI runs: keep Claude Code's per-user temp
# directory (scratchpad, task state, edit diffs) on the persistent home so it
# survives sleep. /tmp itself is container-local and empty at every start.
set -eu
mkdir -p "${HOME}/.cache/claude-tmp"
ln -sfn "${HOME}/.cache/claude-tmp" "/tmp/claude-$(id -u)"
exec claude "$@"
