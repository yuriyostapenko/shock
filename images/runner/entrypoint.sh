#!/bin/sh
# Keep Claude Code's temp directory (scratchpad, task state) on the persistent
# home. The CLI refuses a symlinked /tmp/claude-<uid>; CLAUDE_CODE_TMPDIR is
# the supported override.
set -eu
export CLAUDE_CODE_TMPDIR="${CLAUDE_CODE_TMPDIR:-${HOME}/.cache/claude-tmp}"
mkdir -p -m 0700 "${CLAUDE_CODE_TMPDIR}"
exec claude "$@"
