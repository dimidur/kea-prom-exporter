#!/usr/bin/env bash
# Fake go for scripts/test-scripts.sh, keeping its state in ${FAKE}.
#
# `go list -m` prints example.com/m. `go test` logs its arguments to
# go-calls, copies go-profile to its -coverprofile path when that exists, and
# exits with the number in go-test-status (default 0).
set -euo pipefail
fake="${FAKE:?FAKE is not set: this fake runs under scripts/test-scripts.sh}"

case "$1" in
list) echo example.com/m ;;
test)
	printf '%s\n' "$*" >>"${fake}/go-calls"
	for arg in "$@"; do
		if [[ "${arg}" == -coverprofile=* && -f "${fake}/go-profile" ]]; then
			cp "${fake}/go-profile" "${arg#*=}"
		fi
	done
	status=0
	if [[ -f "${fake}/go-test-status" ]]; then
		status="$(cat "${fake}/go-test-status")"
	fi
	exit "${status}"
	;;
*) exit 1 ;;
esac
