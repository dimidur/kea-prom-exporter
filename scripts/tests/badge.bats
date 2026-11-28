#!/usr/bin/env bats
# Tests for scripts/badge.sh.

# shellcheck source-path=SCRIPTDIR source=helpers.bash
source "${BATS_TEST_DIRNAME:?}/helpers.bash"

script="${BATS_TEST_DIRNAME:?}/../badge.sh"
compare=repos_owner_repo_compare_abc...main
latest=repos_owner_repo_releases_latest

# A badges branch at the given tip, whose tree holds the path=blob entries.
badges_branch() {
	local tip="$1"
	shift
	echo "{\"object\":{\"sha\":\"${tip}\"}}" >"${FAKE}/repos_owner_repo_git_ref_heads_badges"
	jq -n '{tree: [$ARGS.positional[] | split("=") | {path: .[0], sha: .[1]}]}' \
		--args "$@" >"${FAKE}/repos_owner_repo_git_trees_${tip}"
}

# main ahead of abc by one commit per given message.
main_ahead_by() {
	jq -n '{commits: [$ARGS.positional[] | {commit: {message: .}}]}' \
		--args "$@" >"${FAKE}/${compare}"
}

latest_release() {
	echo "{\"tag_name\":\"$1\"}" >"${FAKE}/${latest}"
}

# Sets blob to the blob of the badge the fake curl draws for these values.
drawn_blob() {
	blob="$(printf '<svg xmlns="http://www.w3.org/2000/svg"><title>%s</title></svg>\n' "$*" |
		git hash-object --stdin)"
}

notice="::notice::main moved on; the run for its newest commit publishes the coverage badge"

@test "badge coverage: commits main's newest figure" {
	# The coverage of main's newest commit goes to coverage.svg, drawn by
	# shields.io's static badge, on the branch head it read.

	# Arrange
	main_ahead_by
	badges_branch tip1 README.md=other

	# Act
	run --separate-stderr "${script}" coverage abc 97.3

	# Assert
	assert_success
	assert_requests 1
	assert_logged curl-calls "label=coverage message=97.3% color=brightgreen"
	assert_logged curl-urls "https://img.shields.io/static/v1"
	assert_input .branch.branchName '"badges"'
	assert_input .expectedHeadOid '"tip1"'
	assert_input '.fileChanges.additions[0].path' '"coverage.svg"'
	assert_input '.fileChanges.additions[0].contents | @base64d' \
		'"<svg xmlns=\"http://www.w3.org/2000/svg\"><title>label=coverage message=97.3% color=brightgreen</title></svg>\n"'
	assert_input .message.headline '"badges: coverage 97.3%"'
}

@test "badge coverage: each fetch is bounded in time and size" {
	# The job's timeout is set above the script's worst case, which holds
	# only while each fetch is retried and bounded as here; and an answer far
	# larger than a badge is not even downloaded where curl can tell.

	# Arrange
	main_ahead_by
	badges_branch tip1

	# Act
	run --separate-stderr "${script}" coverage abc 97.3

	# Assert
	assert_success
	assert_logged curl-limits "--retry 3 --max-time 30 --max-filesize 16384"
}

# One band: a percentage and the colour it is drawn in.
colour_case() {
	echo "# colour of $1"
	# Arrange
	fresh
	main_ahead_by
	badges_branch tip1

	# Act
	run --separate-stderr "${script}" coverage abc "$1"

	# Assert
	assert_success
	assert_logged curl-calls "label=coverage message=$1% color=$2"
}

@test "badge coverage: colour bands" {
	# At and either side of each threshold, since an off-by-one there shows
	# a passing figure in the wrong colour.
	colour_case 100 brightgreen
	colour_case 99.74 brightgreen
	colour_case 90 brightgreen
	colour_case 89.99 yellow
	colour_case 75 yellow
	colour_case 74.9 red
	colour_case 0 red
}

# One run of commits on main that start no workflow of their own.
skipped_case() {
	echo "# main ahead by [$*]"
	# Arrange
	fresh
	main_ahead_by "$@"
	badges_branch tip1

	# Act
	run --separate-stderr "${script}" coverage abc 97.3

	# Assert
	assert_success
	assert_requests 1
}

@test "badge coverage: commits that start no run of their own are not newer" {
	# Raising the floors is itself a commit on main, made after the figure
	# was measured, with a skip marker so it starts no run; counting it, or
	# any commit GitHub starts no workflow for, would leave no run to publish.
	skipped_case "ci: raise the coverage floors [skip ci]"
	skipped_case "docs: typo [ci skip]" "chore: x [no ci]"
	skipped_case "[skip actions] y" "z [actions skip]"
}

# One way main can have moved on: the messages of the commits it is ahead by.
stale_case() {
	echo "# main ahead by [$*]"
	# Arrange
	fresh
	main_ahead_by "$@"
	badges_branch tip1

	# Act
	run --separate-stderr "${script}" coverage abc 97.3

	# Assert
	assert_success
	assert_output "${notice}"
	assert_requests 0
	assert_logged curl-calls ""
}

@test "badge coverage: a newer merge's run publishes instead" {
	# Of two merges finishing out of order, the older must not replace the
	# newer one's figure, and must not fail the run either. Only GitHub's own
	# bracketed markers skip a run.
	stale_case "fix: a merge"
	stale_case "ci: raise the coverage floors [skip ci]" "fix: a merge"
	stale_case "mentions skip ci without brackets"
	stale_case "[skip-ci] is not a marker"
}

@test "badge coverage: a merge landing during the retry stops it" {
	# The version badge moved the head meanwhile, and on the retry main has
	# moved on too, so the newer run's figure is the one to keep.

	# Arrange
	main_ahead_by
	badges_branch tip1
	after_reading "${compare}" badges_branch tip2
	after_reading "${compare}" main_ahead_by "fix: a merge"

	# Act
	run --separate-stderr "${script}" coverage abc 97.3

	# Assert
	assert_success
	assert_output "${notice}"
	assert_requests 1
}

@test "badge coverage: fails when main cannot be compared" {
	# Not knowing whether main moved on is not the same as it not having
	# moved, so the badge is left alone and the job fails.

	# Arrange
	badges_branch tip1

	# Act
	run --separate-stderr "${script}" coverage abc 97.3

	# Assert
	assert_failure 1
	assert_requests 0
	assert_logged curl-calls ""
	assert_stderr_line "cannot compare abc with main"
}

@test "badge version: commits the Latest release" {
	# The version badge shows GitHub's Latest release, whichever run draws
	# it, so a late re-run of an older release draws the newer one.

	# Arrange
	latest_release v0.5.0
	badges_branch tip1

	# Act
	run --separate-stderr "${script}" version

	# Assert
	assert_success
	assert_requests 1
	assert_logged curl-calls "label=version message=v0.5.0 color=blue"
	assert_input '.fileChanges.additions[0].path' '"version.svg"'
}

@test "badge version: of two releases racing, the higher one's badge stays" {
	# Just after the lower release reads Latest, the higher one sets Latest
	# and commits its badge. The lower one's commit must be refused, and its
	# retry find the higher badge in place. Reading Latest before the branch
	# head would instead commit the lower version on the new head, and GitHub
	# would accept it.

	# Arrange
	latest_release v0.3.1
	badges_branch tip1
	drawn_blob label=version message=v0.4.0 color=blue
	after_reading "${latest}" badges_branch tip2 "version.svg=${blob}"
	after_reading "${latest}" latest_release v0.4.0

	# Act
	run --separate-stderr "${script}" version

	# Assert
	assert_success
	assert_output "version.svg is unchanged"
	assert_requests 1
	assert_logged curl-calls \
		$'label=version message=v0.3.1 color=blue\nlabel=version message=v0.4.0 color=blue'
}

@test "badge version: fails with no Latest release" {
	# A repository with no Latest release has no version to show.

	# Arrange
	badges_branch tip1

	# Act
	run --separate-stderr "${script}" version

	# Assert
	assert_failure 1
	assert_requests 0
	assert_logged curl-calls ""
	assert_stderr_line "cannot read the Latest release"
}

@test "badge: an unchanged badge is not committed again" {
	# Every release and most merges redraw the same badge; committing it again
	# would add a commit to the branch for nothing.

	# Arrange
	latest_release v0.5.0
	drawn_blob label=version message=v0.5.0 color=blue
	badges_branch tip1 "version.svg=${blob}"

	# Act
	run --separate-stderr "${script}" version

	# Assert
	assert_success
	assert_output "version.svg is unchanged"
	assert_requests 0
}

@test "badge: a changed badge is committed" {
	# A different figure replaces the badge the branch holds.

	# Arrange
	latest_release v0.5.0
	drawn_blob label=version message=v0.4.0 color=blue
	badges_branch tip1 "version.svg=${blob}"

	# Act
	run --separate-stderr "${script}" version

	# Assert
	assert_success
	assert_requests 1
}

@test "badge: a refused commit is retried on the new head" {
	# The other badge landing between reading the head and committing is
	# expected when a release and a merge finish together. The next attempt
	# must commit on the new head; reusing the old one is refused every time.

	# Arrange
	latest_release v0.5.0
	badges_branch tip1
	after_reading "${latest}" badges_branch tip2 coverage.svg=other

	# Act
	run --separate-stderr "${script}" version

	# Assert
	assert_success
	assert_requests 2
	assert_input .expectedHeadOid '"tip2"' 2
	assert_logged sleeps 5
}

@test "badge: gives up after three refusals, waiting only between them" {
	# A branch that keeps refusing is a fault to report, not to retry forever,
	# and nothing is gained by waiting after the last attempt.

	# Arrange
	latest_release v0.5.0
	badges_branch tip1
	printf '1\n1\n1\n' >"${FAKE}/graphql-statuses"

	# Act
	run --separate-stderr "${script}" version

	# Assert
	assert_failure 1
	assert_requests 3
	assert_logged sleeps $'5\n5'
	assert_stderr_line "the badges branch refused version.svg 3 times"
}

@test "badge: fails when the branch is missing" {
	# The branch is created by hand, once; without it there is nothing to
	# commit on, and saying so beats GitHub's bare 404.

	# Arrange
	latest_release v0.5.0

	# Act
	run --separate-stderr "${script}" version

	# Assert
	assert_failure 1
	assert_requests 0
	assert_stderr_line "cannot read the badges branch; it must exist before a badge is committed"
}

@test "badge: fails without committing when the branch's files cannot be read" {
	# Not knowing what the branch holds must not mean committing blindly.

	# Arrange
	latest_release v0.5.0
	echo '{"object":{"sha":"tip1"}}' >"${FAKE}/repos_owner_repo_git_ref_heads_badges"

	# Act
	run --separate-stderr "${script}" version

	# Assert
	assert_failure 1
	assert_requests 0
	assert_stderr_line "cannot read the files on the badges branch"
}

@test "badge: fails without committing when shields.io is down" {
	# Without a drawn badge there is nothing to commit; the job fails so the
	# badge is not silently left behind.

	# Arrange
	latest_release v0.5.0
	badges_branch tip1
	: >"${FAKE}/curl-fails"

	# Act
	run --separate-stderr "${script}" version

	# Assert
	assert_failure 1
	assert_requests 0
	assert_stderr_line "cannot draw version.svg: shields.io did not answer"
}

@test "badge: fails without committing an answer that is not an SVG" {
	# An error page served with a 200 would otherwise be committed and shown
	# as a broken image.

	# Arrange
	latest_release v0.5.0
	badges_branch tip1
	echo '<html>maintenance</html>' >"${FAKE}/curl-body"

	# Act
	run --separate-stderr "${script}" version

	# Assert
	assert_failure 1
	assert_requests 0
	assert_stderr_line "cannot draw version.svg: shields.io answered with something other than an SVG"
}

@test "badge: fails without committing an svg element that is not an SVG root" {
	# Without the SVG namespace on the root, the file is not the image the
	# README expects, whatever its first tag is called.

	# Arrange
	latest_release v0.5.0
	badges_branch tip1
	echo '<svg width="1"/>' >"${FAKE}/curl-body"

	# Act
	run --separate-stderr "${script}" version

	# Assert
	assert_failure 1
	assert_requests 0
	assert_stderr_line "cannot draw version.svg: shields.io answered with something other than an SVG"
}

# One SVG body shields.io might answer with that must not be committed.
unsafe_case() {
	echo "# answer holding [$1]"
	# Arrange
	fresh
	latest_release v0.5.0
	badges_branch tip1
	printf '<svg xmlns="http://www.w3.org/2000/svg">%s</svg>\n' "$1" >"${FAKE}/curl-body"

	# Act
	run --separate-stderr "${script}" version

	# Assert
	assert_failure 1
	assert_requests 0
	assert_stderr_line "cannot draw version.svg: shields.io's SVG holds script, a link or an event handler"
}

@test "badge: fails without committing an SVG that could run or link" {
	# The SVG is fetched unpinned, so a compromised or changed shields.io must
	# not get script, embedded HTML, a link or an event handler onto the
	# branch, in any letter case.
	unsafe_case '<script>alert(1)</script>'
	unsafe_case '<SCRIPT>alert(1)</SCRIPT>'
	unsafe_case '<foreignObject><p>x</p></foreignObject>'
	unsafe_case '<a href="https://example.org">x</a>'
	unsafe_case '<use xlink:href="#x"/>'
	unsafe_case '<rect onload="x()"/>'
	unsafe_case '<rect ONCLICK = "x()"/>'
}

@test "badge: fails without committing an answer far larger than a badge" {
	# A badge is about 1.3 KB; curl cannot cap an answer whose length the
	# server does not send, so the size is checked once it has arrived.

	# Arrange
	latest_release v0.5.0
	badges_branch tip1
	printf '<svg xmlns="http://www.w3.org/2000/svg">%20000s</svg>\n' '' >"${FAKE}/curl-body"

	# Act
	run --separate-stderr "${script}" version

	# Assert
	assert_failure 1
	assert_requests 0
	assert_stderr_line "cannot draw version.svg: shields.io's answer is over 16384 bytes"
}

# One malformed call, against a fake that would otherwise publish.
badge_usage_case() {
	echo "# badge [$*]"
	# Arrange
	fresh
	main_ahead_by
	latest_release v0.5.0
	badges_branch tip1

	# Act
	run --separate-stderr "${script}" "$@"

	# Assert
	assert_failure 2
	assert_logged curl-calls ""
}

@test "badge: usage errors" {
	# A malformed call fails before anything is read or drawn. The percentage
	# must be a plain decimal, since it is both a jq number and badge text,
	# and no more than 100.
	badge_usage_case
	badge_usage_case release
	badge_usage_case version extra
	badge_usage_case coverage abc
	badge_usage_case coverage abc ninety
	badge_usage_case coverage abc 9e1
	badge_usage_case coverage abc 97.
	badge_usage_case coverage abc 97.3x
	badge_usage_case coverage abc 97.3 extra
	badge_usage_case coverage abc 100.01
	badge_usage_case coverage abc 101
}

@test "badge: fails without GITHUB_REPOSITORY" {
	# Without GITHUB_REPOSITORY every request would name the wrong repository.

	# Arrange
	latest_release v0.5.0
	badges_branch tip1

	# Act
	run --separate-stderr env -u GITHUB_REPOSITORY "${script}" version

	# Assert
	assert_failure 1
	assert_logged curl-calls ""
	assert_stderr --partial "GITHUB_REPOSITORY is not set"
}
