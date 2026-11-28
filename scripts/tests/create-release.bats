#!/usr/bin/env bats
# Tests for scripts/create-release.sh.

# shellcheck source-path=SCRIPTDIR source=helpers.bash
source "${BATS_TEST_DIRNAME:?}/helpers.bash"

script="${BATS_TEST_DIRNAME:?}/../create-release.sh"

@test "create-release: leaves an existing release alone" {
	# A re-run must not touch a release that exists: its notes may have been
	# edited by hand, and gh cannot regenerate them in place.

	# Arrange
	: >"${FAKE}/release-v0.5.0"

	# Act
	run --separate-stderr "${script}" v0.5.0 true true

	# Assert
	assert_success
	assert_output "release v0.5.0 already exists; leaving it as it is"
	assert_logged release-calls "release view v0.5.0 --repo owner/repo"
}

# One classification and the release gh is asked to create for it.
create_case() {
	echo "# create-release stable=$1 highest=$2"
	# Arrange
	fresh

	# Act
	run --separate-stderr "${script}" v0.5.0 "$1" "$2"

	# Assert
	assert_success
	assert_logged release-calls $'release view v0.5.0 --repo owner/repo\n'"release create v0.5.0 --repo owner/repo --generate-notes --verify-tag $3"
}

@test "create-release: flags for each classification" {
	# Latest is stated either way, since gh would otherwise choose by date
	# and could pick a patch to an older line; a prerelease is marked so it
	# never takes Latest; --verify-tag stops gh creating a missing tag.
	create_case true true --latest=true
	create_case true false --latest=false
	create_case false false "--latest=false --prerelease"
}

# One malformed call.
create_usage_case() {
	echo "# create-release [$*]"
	# Arrange
	fresh

	# Act
	run --separate-stderr "${script}" "$@"

	# Assert
	assert_failure 2
	assert_logged release-calls ""
}

@test "create-release: usage errors" {
	# The flags are built from the two answers, so anything but true or false
	# would reach gh as a malformed --latest.
	create_usage_case v0.5.0 true
	create_usage_case v0.5.0 yes true
	create_usage_case v0.5.0 true 1
	create_usage_case v0.5.0 true true extra
}

@test "create-release: fails without GITHUB_REPOSITORY" {
	# Without GITHUB_REPOSITORY gh would pick a repository from the checkout.

	# Arrange: the release does not exist yet

	# Act
	run --separate-stderr env -u GITHUB_REPOSITORY "${script}" v0.5.0 true true

	# Assert
	assert_failure 1
	assert_logged release-calls ""
	assert_stderr --partial "GITHUB_REPOSITORY is not set"
}
