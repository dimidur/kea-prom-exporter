#!/usr/bin/env bats
# Tests for scripts/commit-floors.sh.

# shellcheck source-path=SCRIPTDIR source=helpers.bash
source "${BATS_TEST_DIRNAME:?}/helpers.bash"

script="${BATS_TEST_DIRNAME:?}/../commit-floors.sh"

# The baseline committed at the measured commit, in a checkout of it.
committed() {
	mkdir -p "${FAKE}/repo"
	jq -n "$1" >"${FAKE}/repo/coverage-baseline.json"
}

# The floors the test job raised.
raised() {
	printf '%s\n' "$1" >"${FAKE}/floors.json"
}

# Runs the script on the measured commit abc.
commit_floors() {
	run --separate-stderr "${script}" abc "${FAKE}/floors.json" \
		"${FAKE}/repo/coverage-baseline.json"
}

old='{cap: 90, buffer: 2, files: {"a.go": {statements: 70}}, totals: {statements: 70}}'
new='{"cap":90,"buffer":2,"files":{"a.go":{"statements":73}},"totals":{"statements":73}}'

@test "commit-floors: commits on main at the measured commit" {
	# The floors land on main on top of the commit they were measured on, with
	# the skip marker, so they start no run of their own.

	# Arrange
	committed "${old}"
	raised "${new}"

	# Act
	commit_floors

	# Assert
	assert_success
	assert_requests 1
	assert_input '"\(.branch.branchName) \(.expectedHeadOid)"' '"main abc"'
	assert_input '.fileChanges.additions[0].path' '"coverage-baseline.json"'
	assert_input .message.headline '"ci: raise the coverage floors [skip ci]"'
}

@test "commit-floors: a new file's floor and a gone file's dropped one pass" {
	# Onboarding and pruning are the ratchet's own changes.

	# Arrange
	committed '{cap: 90, buffer: 2, files: {"gone.go": {statements: 90}}, totals: {statements: 70}}'
	raised '{"cap":90,"buffer":2,"files":{"new.go":{"statements":73}},"totals":{"statements":70}}'

	# Act
	commit_floors

	# Assert
	assert_success
	assert_requests 1
}

# One set of floors that breaks the ratchet's rule.
broken_case() {
	echo "# raised [$1]"
	# Arrange
	fresh
	committed "${old}"
	raised "$1"

	# Act
	commit_floors

	# Assert
	assert_failure 1
	assert_requests 0
	assert_output "::error::the raised floors do not keep the ratchet's rule; nothing committed"
}

@test "commit-floors: floors that break the ratchet's rule are not committed" {
	# They come from a job that runs the tests' dependencies, which could
	# write anything there; main must never get lower, malformed or
	# reconfigured floors from it.
	broken_case '{"cap":90,"buffer":2,"files":{"a.go":{"statements":0}},"totals":{"statements":73}}'
	broken_case '{"cap":90,"buffer":2,"files":{"a.go":{"statements":73}},"totals":{"statements":69}}'
	broken_case '{"cap":100,"buffer":2,"files":{"a.go":{"statements":73}},"totals":{"statements":73}}'
	broken_case '{"cap":90,"buffer":5,"files":{"a.go":{"statements":73}},"totals":{"statements":73}}'
	broken_case '{"cap":90,"buffer":2,"files":{"a.go":{"statements":101}},"totals":{"statements":73}}'
	broken_case '{"cap":90,"buffer":2,"files":{"a.go":{"statements":73}},"totals":{"statements":101}}'
	broken_case '{"cap":90,"buffer":2,"files":{"a.go":{"statements":null}},"totals":{"statements":73}}'
	broken_case '{"cap":90,"buffer":2,"files":{"b.go":{"statements":-1}},"totals":{"statements":73}}'
	broken_case '{"cap":90,"buffer":2,"files":{"a.go":{"statements":"73"}},"totals":{"statements":73}}'
	broken_case '{"cap":90,"buffer":2,"files":{"a.go":73},"totals":{"statements":73}}'
	broken_case '{"cap":90,"buffer":2,"files":[],"totals":{"statements":73}}'
	broken_case '{"cap":90,"buffer":2,"files":{"a.go":{"statements":73}},"totals":[]}'
	broken_case '[]'
	broken_case 'not json'
}

@test "commit-floors: a file's floors are not dropped while it exists" {
	# Dropping a floor is pruning only for a file that is gone; otherwise it
	# removes the file's protection.

	# Arrange
	committed "${old}"
	: >"${FAKE}/repo/a.go"
	raised '{"cap":90,"buffer":2,"files":{},"totals":{"statements":73}}'

	# Act
	commit_floors

	# Assert
	assert_failure 1
	assert_requests 0
	assert_output "::error::the raised floors drop a.go, which still exists; nothing committed"
}

@test "commit-floors: main having moved on passes, and says why" {
	# A merge during the run makes GitHub refuse the commit; that merge's own
	# run raises the floors, so this one passes. The notice must be the whole
	# of stdout, on a line of its own, or the runner does not read it.

	# Arrange
	committed "${old}"
	raised "${new}"
	printf '1\n' >"${FAKE}/graphql-statuses"
	echo '{"sha":"newer"}' >"${FAKE}/repos_owner_repo_commits_main"

	# Act
	commit_floors

	# Assert
	assert_success
	assert_output "::notice::main moved on during this run; its next run raises the floors"
}

@test "commit-floors: a refusal with main in place fails" {
	# A refusal with main still at the measured commit is a fault (a ruleset, a
	# token), and passing would leave the floors stale with nothing to show.

	# Arrange
	committed "${old}"
	raised "${new}"
	printf '1\n' >"${FAKE}/graphql-statuses"
	echo '{"sha":"abc"}' >"${FAKE}/repos_owner_repo_commits_main"

	# Act
	commit_floors

	# Assert
	assert_failure 1
	assert_output "::error::main refused the raised floors without having moved on"
}

@test "commit-floors: a refusal with main unreadable fails" {
	# After a refusal, not knowing whether main moved is no reason to pass.

	# Arrange
	committed "${old}"
	raised "${new}"
	printf '1\n' >"${FAKE}/graphql-statuses"

	# Act
	commit_floors

	# Assert
	assert_failure 1
	assert_output "::error::main refused the raised floors, and cannot be read to say why"
}

@test "commit-floors: usage error" {
	# Without the committed baseline there is nothing to check the floors
	# against.

	# Arrange
	committed "${old}"
	raised "${new}"

	# Act
	run --separate-stderr "${script}" abc "${FAKE}/floors.json"

	# Assert
	assert_failure 2
	assert_requests 0
}

@test "commit-floors: fails without GITHUB_REPOSITORY" {
	# Without GITHUB_REPOSITORY, a refusal could not be told from main having
	# moved on.

	# Arrange
	committed "${old}"
	raised "${new}"

	# Act
	run --separate-stderr env -u GITHUB_REPOSITORY "${script}" abc "${FAKE}/floors.json" \
		"${FAKE}/repo/coverage-baseline.json"

	# Assert
	assert_failure 1
	assert_requests 0
	assert_stderr --partial "GITHUB_REPOSITORY is not set"
}
