#!/usr/bin/env bats
# Tests for scripts/classify-release.sh.

# shellcheck source-path=SCRIPTDIR source=helpers.bash
source "${BATS_TEST_DIRNAME:?}/helpers.bash"

script="${BATS_TEST_DIRNAME:?}/../classify-release.sh"

# One row: the tag, the tags pushed so far, and what it may move.
classify_case() {
	echo "# classify $1 among [$2]"
	# Arrange
	fresh
	# shellcheck disable=SC2086 # the tags are a space-separated list
	tags_pushed $2

	# Act
	run --separate-stderr "${script}" "$1"

	# Assert
	assert_success
	assert_output "$3"
}

@test "classify: what each release may move" {
	# Each row is one way a release could move `latest` or its line's tag
	# when it should not, or fail to when it should. The tag itself counts
	# whether GitHub lists it yet or not, semver order is numeric (0.10 above
	# 0.9), and only stable tags compete, so a prerelease or junk tag sorting
	# above a release must not hold it back.
	classify_case v0.4.0 "v0.3.1 v0.4.0" $'stable=true\nhighest=true\nline-highest=true'
	classify_case v0.3.2 "v0.4.0 v0.3.2" $'stable=true\nhighest=false\nline-highest=true'
	classify_case v0.3.1 "v0.3.2 v0.3.1 v0.4.0" $'stable=true\nhighest=false\nline-highest=false'
	classify_case v0.10.0 "v0.9.0" $'stable=true\nhighest=true\nline-highest=true'
	classify_case v0.1.5 "v0.10.0 v0.1.4" $'stable=true\nhighest=false\nline-highest=true'
	classify_case v1.0.0-rc1 "v0.9.0" $'stable=false\nhighest=false\nline-highest=false'
	classify_case v1.0 "v0.9.0" $'stable=false\nhighest=false\nline-highest=false'
	classify_case v0.1.0 "" $'stable=true\nhighest=true\nline-highest=true'
	classify_case v0.4.0 "v0.5.0-rc1 v0.4.1-rc1 v0.4.9x" \
		$'stable=true\nhighest=true\nline-highest=true'
}

@test "classify: reads every page of tags" {
	# GitHub lists 100 tags a page, so past 100 tags the highest can be on a
	# later one; missing it would move `latest` back to a lower release.

	# Arrange
	tags_pushed v0.3.1
	tags_on_page_2 v0.5.0

	# Act
	run --separate-stderr "${script}" v0.4.0

	# Assert
	assert_success
	assert_output $'stable=true\nhighest=false\nline-highest=true'
}

@test "classify: says why a release moves nothing" {
	# A release that moves nothing looks like a broken run unless its log
	# says why.

	# Arrange
	tags_pushed v0.3.2 v0.4.0

	# Act
	run --separate-stderr "${script}" v0.3.1

	# Assert
	assert_success
	assert_stderr $'v0.3.1 is below v0.4.0, so it moves nothing that names the latest\nv0.3.1 is below v0.3.2, so its line\'s tag stays'
}

@test "classify: a prerelease reads no tags" {
	# A prerelease moves nothing whatever else is tagged, so it must not
	# depend on reading the tags: their read failing cannot fail its release.

	# Arrange: no tag list to read

	# Act
	run --separate-stderr "${script}" v1.0.0-rc1

	# Assert
	assert_success
	assert_output $'stable=false\nhighest=false\nline-highest=false'
}

@test "classify: --stable-only answers stable alone, reading nothing" {
	# Whether a tag is stable is fixed with the tag, so a job that needs only
	# that must not fail on the tags read before it has done anything.

	# Arrange: no tag list to read

	# Act
	run --separate-stderr "${script}" --stable-only v0.4.0

	# Assert
	assert_success
	assert_output stable=true
}

@test "classify: --stable-only on a prerelease" {
	# The same one definition of stable, whichever mode answers.

	# Arrange: no tag list to read

	# Act
	run --separate-stderr "${script}" --stable-only v1.0.0-rc1

	# Assert
	assert_success
	assert_output stable=false
}

@test "classify: fails when the tags cannot be read" {
	# A stable release that cannot see the other tags cannot know it is the
	# highest, and taking none for an answer would move `latest` to it.

	# Arrange: no tag list to read

	# Act
	run --separate-stderr "${script}" v0.4.0

	# Assert
	assert_failure 1
	assert_output ""
	assert_stderr_line "cannot read the repository's tags"
}

# One malformed call.
classify_usage_case() {
	echo "# classify [$*]"
	# Arrange
	fresh
	tags_pushed v0.4.0

	# Act
	run --separate-stderr "${script}" "$@"

	# Assert
	assert_failure 2
	assert_output ""
}

@test "classify: usage errors" {
	# Without the tag the script would classify nothing and print false for
	# all three, which a workflow would take for an answer.
	classify_usage_case
	classify_usage_case --stable-only
	classify_usage_case v0.4.0 extra
}

@test "classify: fails without GITHUB_REPOSITORY" {
	# Without GITHUB_REPOSITORY the tags would be read from no repository.

	# Arrange
	tags_pushed v0.4.0

	# Act
	run --separate-stderr env -u GITHUB_REPOSITORY "${script}" v0.4.0

	# Assert
	assert_failure 1
	assert_output ""
	assert_stderr --partial "GITHUB_REPOSITORY is not set"
}
