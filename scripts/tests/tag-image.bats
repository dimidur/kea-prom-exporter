#!/usr/bin/env bats
# Tests for scripts/tag-image.sh.

# shellcheck source-path=SCRIPTDIR source=helpers.bash
source "${BATS_TEST_DIRNAME:?}/helpers.bash"

script="${BATS_TEST_DIRNAME:?}/../tag-image.sh"

# One release, the tags pushed so far, and the registry call it makes.
tag_case() {
	echo "# tag-image $1 among [$2]"
	# Arrange
	fresh
	# shellcheck disable=SC2086 # the tags are a space-separated list
	tags_pushed $2

	# Act
	run --separate-stderr "${script}" registry/image "$1"

	# Assert
	assert_success
	assert_logged docker-calls "$3"
}

@test "tag-image: moves latest and the minor line's tag as far as classify allows" {
	# They move to the version just pushed only as far as classify allows,
	# and are copied from that version's manifest rather than rebuilt.
	tag_case v0.4.0 "v0.3.1" \
		"buildx imagetools create -t registry/image:latest -t registry/image:0.4 registry/image:0.4.0"
	tag_case v0.3.2 "v0.4.0 v0.3.1" \
		"buildx imagetools create -t registry/image:0.3 registry/image:0.3.2"
	tag_case v0.3.1 "v0.3.2 v0.4.0" ""
	tag_case v1.0.0-rc1 "v0.9.0" ""
}

@test "tag-image: says so when it moves nothing" {
	# A release that moves neither tag must say so, or its run reads as if
	# the step were broken.

	# Arrange
	tags_pushed v0.3.2 v0.4.0

	# Act
	run --separate-stderr "${script}" registry/image v0.3.1

	# Assert
	assert_success
	assert_output "v0.3.1 moves neither latest nor its minor line's tag"
}

@test "tag-image: moves nothing without an answer from classify" {
	# A tag that cannot be classified must not move `latest`.

	# Arrange: no tag list to read

	# Act
	run --separate-stderr "${script}" registry/image v0.4.0

	# Assert
	assert_failure 1
	assert_logged docker-calls ""
}

@test "tag-image: fails with docker's status when the registry refuses" {
	# A registry that refuses the tags fails the job, so the release is not
	# taken for complete.

	# Arrange
	tags_pushed v0.3.1
	echo 3 >"${FAKE}/docker-status"

	# Act
	run --separate-stderr "${script}" registry/image v0.4.0

	# Assert
	assert_failure 3
}

# One malformed call.
tag_usage_case() {
	echo "# tag-image [$*]"
	# Arrange
	fresh
	tags_pushed v0.3.1

	# Act
	run --separate-stderr "${script}" "$@"

	# Assert
	assert_failure 2
	assert_logged docker-calls ""
}

@test "tag-image: usage errors" {
	# The version tag is the tag without its v, so a tag without one would
	# name an image that was never pushed.
	tag_usage_case registry/image
	tag_usage_case registry/image 0.4.0
	tag_usage_case registry/image v0.4.0 extra
}
