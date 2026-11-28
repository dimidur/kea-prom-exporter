# shellcheck shell=bash
# Shared by the script tests. Each test gets the fakes in scripts/fakes/ on
# PATH under their tools' names (each fake's header says how to script it),
# keeping their state in a fresh ${FAKE}, so nothing touches the network,
# builds or waits.

bats_require_minimum_version 1.5.0
bats_load_library bats-support
bats_load_library bats-assert

setup() {
	local bin="${BATS_TEST_TMPDIR:?}/bin" fake tool
	mkdir -p "${bin}"
	for fake in "${BATS_TEST_DIRNAME:?}/../fakes/"*.sh; do
		tool="$(basename "${fake}" .sh)"
		cp "${fake}" "${bin}/${tool}"
		chmod +x "${bin}/${tool}"
	done
	export PATH="${bin}:${PATH}"
	export GITHUB_REPOSITORY=owner/repo
	fresh
}

# A new, empty state for the fakes, for each row of a table.
fresh() {
	FAKE="$(mktemp -d "${BATS_TEST_TMPDIR:?}/fake.XXXXXX")"
	export FAKE
}

# Asserts what a fake's log holds; nothing when it was never written.
assert_logged() {
	local got=""
	if [[ -f "${FAKE}/$1" ]]; then
		got="$(cat "${FAKE}/$1")"
	fi
	assert_equal "${got}" "$2"
}

# Asserts a jq expression over a JSON file.
assert_json() {
	local got
	if [[ ! -f "$1" ]]; then
		fail "$1 does not exist"
	fi
	got="$(jq -c "$2" "$1")"
	assert_equal "${got}" "$3"
}

# Asserts that a file is byte for byte the copy taken before.
assert_unchanged() {
	if ! cmp -s "$1" "$2"; then
		fail "$2 changed"
	fi
}
