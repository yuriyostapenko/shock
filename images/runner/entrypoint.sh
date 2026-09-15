#!/bin/sh
# Container start, before the CLI runs: keep Claude Code's temp directory
# (scratchpad, task state, edit diffs) on the persistent home so it survives
# sleep. The CLI refuses a symlinked /tmp/claude-<uid>, so it is pointed at a
# real directory via CLAUDE_CODE_TMPDIR instead.
set -eu
export CLAUDE_CODE_TMPDIR="${CLAUDE_CODE_TMPDIR:-${HOME}/.cache/claude-tmp}"
mkdir -p -m 0700 "${CLAUDE_CODE_TMPDIR}"
exec claude "$@"
