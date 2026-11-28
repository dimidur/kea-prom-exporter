#!/usr/bin/env bash
# Runs the tests for the scripts in this directory, scripts/tests/*.bats,
# under the bats-core, bats-support and bats-assert pinned below. Each is
# fetched once, by its commit, into the user's cache; git checks what it
# fetched against that commit's id.
#
# Usage:
#   scripts/test-scripts.sh [bats options]
#
# Exit status: bats's; git's when a pinned tool cannot be fetched.
set -euo pipefail
shopt -s inherit_errexit

# Each pin is a tag and the commit it points to; Renovate moves both.
# renovate: depName=bats-core/bats-core
bats_core=v1.14.0@eb7f42f8d608ac693d7a4b67474f6714ea68cfc5
# renovate: depName=bats-core/bats-support
bats_support=v0.3.0@24a72e14349690bcbf7c151b9d2d1cdd32d36eb1
# renovate: depName=bats-core/bats-assert
bats_assert=v2.2.4@f1e9280eaae8f86cbe278a687e6ba755bc802c1a

scripts="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
tools="${XDG_CACHE_HOME:-${HOME}/.cache}/kea-prom-exporter/bats-${bats_core#*@}-${bats_support#*@}-${bats_assert#*@}"

# Fetches a repository at the commit of its pin into ${tools}/<name>, unless
# an earlier run already has.
fetch() {
	local name="$1" commit="${2#*@}" partial
	if [[ -d "${tools}/${name}" ]]; then
		return
	fi
	partial="${tools}/${name}.partial"
	rm -rf "${partial}"
	git init -q "${partial}"
	git -C "${partial}" fetch -q --depth 1 "https://github.com/bats-core/${name}" "${commit}"
	git -C "${partial}" -c advice.detachedHead=false checkout -q FETCH_HEAD
	mv "${partial}" "${tools}/${name}"
}

mkdir -p "${tools}"
fetch bats-core "${bats_core}"
fetch bats-support "${bats_support}"
fetch bats-assert "${bats_assert}"

export BATS_LIB_PATH="${tools}"
exec "${tools}/bats-core/bin/bats" "$@" "${scripts}/tests"
