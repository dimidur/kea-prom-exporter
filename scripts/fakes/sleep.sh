#!/usr/bin/env bash
# Fake sleep for scripts/test-scripts.sh: logs its argument to ${FAKE}/sleeps
# and returns at once.
set -euo pipefail
fake="${FAKE:?FAKE is not set: this fake runs under scripts/test-scripts.sh}"

printf '%s\n' "$*" >>"${fake}/sleeps"
