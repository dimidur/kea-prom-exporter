#!/usr/bin/env bash
# Fake docker for scripts/test-scripts.sh, keeping its state in ${FAKE}: logs
# its arguments to docker-calls and exits with the number in docker-status
# (default 0).
set -euo pipefail
fake="${FAKE:?FAKE is not set: this fake runs under scripts/test-scripts.sh}"

printf '%s\n' "$*" >>"${fake}/docker-calls"
status=0
if [[ -f "${fake}/docker-status" ]]; then
	status="$(cat "${fake}/docker-status")"
fi
exit "${status}"
