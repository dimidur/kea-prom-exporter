#!/usr/bin/env bats
# Tests for scripts/coverage-ratchet.sh.

# shellcheck source-path=SCRIPTDIR source=helpers.bash
source "${BATS_TEST_DIRNAME:?}/helpers.bash"

# A copy of the ratchet in a repository of its own, since it reads and writes
# the baseline beside its parent directory. Sets ratchet and baseline.
ratchet_repo() {
	mkdir -p "${FAKE}/repo/scripts"
	cp "${BATS_TEST_DIRNAME:?}/../coverage-ratchet.sh" "${FAKE}/repo/scripts/"
	ratchet="${FAKE}/repo/scripts/coverage-ratchet.sh"
	baseline="${FAKE}/repo/coverage-baseline.json"
}

# The profile the tests left, from `file:block statements count` lines in the
# module.
profile() {
	{
		echo "mode: set"
		if (($#)); then
			printf 'example.com/m/%s\n' "$@"
		fi
	} >"${FAKE}/repo/coverage.out"
}

# a.go at 75 % (3 of 4 statements), b.go at 100 % (2 of 2): 5 of 6 in total.
two_files() {
	profile "a.go:1.1,2.2 3 1" "a.go:3.1,4.2 1 0" "b.go:1.1,2.2 2 5"
}

floors() {
	jq -n "$1" >"${baseline}"
}

# A run of the tests that covers a.go fully, for the fake go to leave.
fresh_profile() {
	printf 'mode: set\nexample.com/m/a.go:1.1,2.2 1 1\n' >"${FAKE}/go-profile"
}

@test "ratchet total: covered over all statements" {
	# The figure the badge shows, to 2 decimals.

	# Arrange
	ratchet_repo
	two_files

	# Act
	run --separate-stderr "${ratchet}" total --reuse

	# Assert
	assert_success
	assert_output 83.33
}

@test "ratchet total: from RATCHET_PROFILE" {
	# RATCHET_PROFILE points the ratchet at a profile written elsewhere.

	# Arrange
	ratchet_repo
	printf 'mode: set\nexample.com/m/a.go:1.1,2.2 1 0\nexample.com/m/a.go:3.1,4.2 1 1\n' \
		>"${FAKE}/elsewhere.out"

	# Act
	run --separate-stderr env RATCHET_PROFILE="${FAKE}/elsewhere.out" "${ratchet}" total --reuse

	# Assert
	assert_success
	assert_output 50
}

@test "ratchet total: a duplicated block counts once" {
	# go test lists a block once per package that runs it, so a block counts
	# once, and as covered if any entry has a hit.

	# Arrange
	ratchet_repo
	profile "a.go:1.1,2.2 2 0" "a.go:1.1,2.2 2 1" "a.go:3.1,4.2 2 0"

	# Act
	run --separate-stderr "${ratchet}" total --reuse

	# Assert
	assert_success
	assert_output 50
}

@test "ratchet seed: a file outside the module keeps its full path" {
	# So it cannot collide with one of the module's own.

	# Arrange
	ratchet_repo
	printf 'mode: set\nother.org/x/c.go:1.1,2.2 1 1\n' >"${FAKE}/repo/coverage.out"

	# Act
	run --separate-stderr "${ratchet}" seed --reuse

	# Assert
	assert_success
	assert_json "${baseline}" '.files | keys' '["other.org/x/c.go"]'
}

@test "ratchet check: coverage at its floors passes" {
	# And says the total.

	# Arrange
	ratchet_repo
	two_files
	floors '{cap: 90, buffer: 2, files: {"a.go": {statements: 75}}, totals: {statements: 83}}'

	# Act
	run --separate-stderr "${ratchet}" check --reuse

	# Assert
	assert_success
	assert_output "coverage ratchet OK total statements 83.33% (floors held)"
}

@test "ratchet: a bare run checks and never rewrites the baseline" {
	# A bare run is what CONTRIBUTING suggests for checking locally.

	# Arrange
	ratchet_repo
	fresh_profile
	floors '{cap: 90, buffer: 2, files: {"a.go": {statements: 50}}, totals: {statements: 50}}'
	cp "${baseline}" "${FAKE}/before.json"

	# Act
	run --separate-stderr "${ratchet}"

	# Assert
	assert_success
	assert_output "coverage ratchet OK total statements 100% (floors held)"
	assert_unchanged "${FAKE}/before.json" "${baseline}"
}

# One floor for a.go, measured at 75 %, and whether check passes against it.
tolerance_case() {
	echo "# check of 75 against a floor of $1"
	# Arrange
	fresh
	ratchet_repo
	two_files
	floors "{cap: 90, buffer: 2, files: {\"a.go\": {statements: $1}}, totals: {statements: 0}}"

	# Act
	run --separate-stderr "${ratchet}" check --reuse

	# Assert
	if [[ "$2" == passes ]]; then
		assert_success
	else
		assert_failure 1
	fi
}

@test "ratchet check: up to 0.05 points under a floor is rounding" {
	# Not a lost test.
	tolerance_case 75 passes
	tolerance_case 75.05 passes
	tolerance_case 75.06 fails
}

@test "ratchet check: a file below its floor fails" {
	# Even when the total holds: a file losing its tests is what the floors
	# are for.

	# Arrange
	ratchet_repo
	two_files
	floors '{cap: 90, buffer: 2, files: {"a.go": {statements: 80}}, totals: {statements: 0}}'

	# Act
	run --separate-stderr "${ratchet}" check --reuse

	# Assert
	assert_failure 1
	assert_output $'coverage ratchet FAILED:\n  a.go statements 75% < required 80%'
}

@test "ratchet check: a total below its floor fails" {
	# The total has a floor of its own.

	# Arrange
	ratchet_repo
	two_files
	floors '{cap: 90, buffer: 2, files: {}, totals: {statements: 85}}'

	# Act
	run --separate-stderr "${ratchet}" check --reuse

	# Assert
	assert_failure 1
	assert_output $'coverage ratchet FAILED:\n  TOTAL statements 83.33% < floor 85%'
}

@test "ratchet check: new and gone files block nothing" {
	# A file with no floor yet counts only in the total, and a floor for a
	# file that is gone binds nothing, so neither blocks the change that adds
	# or removes the file.

	# Arrange
	ratchet_repo
	profile "new.go:1.1,2.2 1 0" "a.go:1.1,2.2 3 1"
	floors '{cap: 90, buffer: 2, files: {"gone.go": {statements: 90}}, totals: {statements: 0}}'

	# Act
	run --separate-stderr "${ratchet}" check --reuse

	# Assert
	assert_success
}

@test "ratchet seed: each floor the buffer below, never above the cap" {
	# Using the defaults when there is no baseline yet.

	# Arrange
	ratchet_repo
	two_files

	# Act
	run --separate-stderr "${ratchet}" seed --reuse

	# Assert
	assert_success
	assert_output 'seeded 2 files; totals {"statements":81.33}'
	assert_json "${baseline}" . \
		'{"buffer":2,"cap":90,"files":{"a.go":{"statements":73},"b.go":{"statements":90}},"totals":{"statements":81.33}}'
}

@test "ratchet bump: a floor cleared by more than the buffer rises" {
	# To the buffer below the new coverage.

	# Arrange
	ratchet_repo
	two_files
	floors '{cap: 90, buffer: 2, files: {"a.go": {statements: 70}, "b.go": {statements: 90}},
		totals: {statements: 70}}'

	# Act
	run --separate-stderr "${ratchet}" bump --reuse

	# Assert
	assert_success
	assert_output changed=true
	assert_json "${baseline}" '{files, totals}' \
		'{"files":{"a.go":{"statements":73},"b.go":{"statements":90}},"totals":{"statements":81.33}}'
}

@test "ratchet bump: coverage within the buffer leaves the baseline as it is" {
	# Byte for byte, so main gets no commit for it.

	# Arrange
	ratchet_repo
	two_files
	floors '{buffer: 2, cap: 90, files: {"a.go": {statements: 73.5}, "b.go": {statements: 90}},
		totals: {statements: 81.5}}'
	cp "${baseline}" "${FAKE}/before.json"

	# Act
	run --separate-stderr "${ratchet}" bump --reuse

	# Assert
	assert_success
	assert_output changed=false
	assert_unchanged "${FAKE}/before.json" "${baseline}"
}

@test "ratchet bump: a hand-written floor is still held to the buffer" {
	# A floor edited by hand can carry more decimals than the ratchet writes.
	# Coverage exactly the buffer above it has not cleared it, even though the
	# rounded candidate (73.01) is above the floor (73.005).

	# Arrange
	ratchet_repo
	profile "a.go:1.1,2.2 15001 1" "a.go:3.1,4.2 4999 0"
	floors '{cap: 90, buffer: 2, files: {"a.go": {statements: 73.005}},
		totals: {statements: 73.005}}'

	# Act
	run --separate-stderr "${ratchet}" bump --reuse

	# Assert
	assert_success
	assert_output changed=false
}

@test "ratchet bump: onboards new files and prunes gone ones" {
	# Each is a change to commit, and the report says which.

	# Arrange
	ratchet_repo
	profile "new.go:1.1,2.2 3 1" "new.go:3.1,4.2 1 0"
	floors '{cap: 90, buffer: 2, files: {"gone.go": {statements: 90}}, totals: {statements: 90}}'

	# Act
	run --separate-stderr "${ratchet}" bump --reuse

	# Assert
	assert_success
	assert_output changed=true
	assert_json "${baseline}" .files '{"new.go":{"statements":73}}'
	assert_stderr $'coverage ratchet bumped 2 floor(s):\n  onboard new.go statements -> 73%\n  prune gone.go'
}

@test "ratchet: a profile with no statements fails" {
	# The tests measured nothing, which must not pass as full coverage.

	# Arrange
	ratchet_repo
	profile

	# Act
	run --separate-stderr "${ratchet}" check --reuse

	# Assert
	assert_failure 3
}

@test "ratchet: with no profile to reuse, it runs the tests" {
	# It measures rather than failing.

	# Arrange
	ratchet_repo
	fresh_profile

	# Act
	run --separate-stderr "${ratchet}" total --reuse

	# Assert
	assert_success
	assert_output 100
	assert_logged go-calls "test -count=1 -coverprofile=${FAKE}/repo/coverage.out ./..."
}

@test "ratchet: without --reuse, an old profile is measured afresh" {
	# A profile left from an earlier run is reused only when asked for, so a
	# local check measures the tree as it is now.

	# Arrange
	ratchet_repo
	two_files
	fresh_profile

	# Act
	run --separate-stderr "${ratchet}" total

	# Assert
	assert_success
	assert_output 100
}

@test "ratchet: failing tests fail it" {
	# Whatever they covered.

	# Arrange
	ratchet_repo
	fresh_profile
	echo 1 >"${FAKE}/go-test-status"

	# Act
	run --separate-stderr "${ratchet}" check

	# Assert
	assert_failure 1
}

# One malformed call.
ratchet_usage_case() {
	echo "# ratchet [$*]"
	# Arrange
	fresh
	ratchet_repo
	two_files

	# Act
	run --separate-stderr "${ratchet}" "$@"

	# Assert
	assert_failure 2
	assert_logged go-calls ""
}

@test "ratchet: usage errors" {
	# Arguments are checked before anything runs, so a typo cannot run the
	# tests or rewrite the baseline in some other mode.
	ratchet_usage_case raise
	ratchet_usage_case bump --reused
}
