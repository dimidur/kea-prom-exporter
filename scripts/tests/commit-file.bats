#!/usr/bin/env bats
# Tests for scripts/commit-file.sh.

# shellcheck source-path=SCRIPTDIR source=helpers.bash
source "${BATS_TEST_DIRNAME:?}/helpers.bash"

script="${BATS_TEST_DIRNAME:?}/../commit-file.sh"

@test "commit-file: sends every field of the mutation" {
	# A wrong field commits to the wrong place or on the wrong head rather
	# than failing.

	# Arrange
	printf 'contents\n' >"${FAKE}/file.txt"

	# Act
	run --separate-stderr "${script}" main abc dir/file.txt "${FAKE}/file.txt" "the headline"

	# Assert
	assert_success
	assert_output new1
	assert_input '.branch | "\(.repositoryNameWithOwner) \(.branchName)"' '"owner/repo main"'
	assert_input .expectedHeadOid '"abc"'
	assert_input .message.headline '"the headline"'
	assert_input '.fileChanges.additions[0].path' '"dir/file.txt"'
	assert_input '.fileChanges.additions[0].contents | @base64d' '"contents\n"'
}

@test "commit-file: a refusal prints nothing, and GitHub's answer goes to stderr" {
	# Callers tell a refusal by the exit status alone. gh prints GitHub's
	# error body on stdout with no final newline, and a caller's next line,
	# an annotation, would run on from it and be lost; so the body goes to
	# stderr, on a line of its own.

	# Arrange
	printf 'contents\n' >"${FAKE}/file.txt"
	printf '1\n' >"${FAKE}/graphql-statuses"

	# Act
	run --separate-stderr "${script}" main abc file.txt "${FAKE}/file.txt" headline

	# Assert
	assert_failure 1
	assert_output ""
	assert_stderr $'gh: refused\n{"errors":[{"message":"refused"}]}'
}

@test "commit-file: fails without committing a file it cannot read" {
	# A missing file must not commit an empty one, nor read as a usage error.

	# Arrange
	missing="${FAKE}/missing.txt"

	# Act
	run --separate-stderr "${script}" main abc file.txt "${missing}" headline

	# Assert
	assert_failure 1
	assert_requests 0
	assert_stderr "cannot read ${missing}"
}

@test "commit-file: usage error" {
	# A missing argument would shift every other one into the wrong field.

	# Arrange
	printf 'contents\n' >"${FAKE}/file.txt"

	# Act
	run --separate-stderr "${script}" main abc "${FAKE}/file.txt"

	# Assert
	assert_failure 2
	assert_requests 0
}

@test "commit-file: fails without GITHUB_REPOSITORY" {
	# Without GITHUB_REPOSITORY the commit would name no repository.

	# Arrange
	printf 'contents\n' >"${FAKE}/file.txt"

	# Act
	run --separate-stderr env -u GITHUB_REPOSITORY "${script}" main abc f "${FAKE}/file.txt" h

	# Assert
	assert_failure 1
	assert_requests 0
	assert_stderr --partial "GITHUB_REPOSITORY is not set"
}
