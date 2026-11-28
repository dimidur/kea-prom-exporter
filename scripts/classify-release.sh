#!/usr/bin/env bash
# Decides what a release tag may move, from the tags pushed so far:
# docs/design.md, "Only the highest release names the latest".
#
# Usage:
#   scripts/classify-release.sh [--stable-only] <tag>
#
# Prints, as key=value lines for GITHUB_OUTPUT:
#   stable        the tag is vX.Y.Z, with no suffix
#   highest       stable, and not below any stable tag
#   line-highest  stable, and not below any stable tag in its minor line
#
# --stable-only prints stable alone, which is fixed with the tag, and reads
# nothing. Otherwise the repository's tags are read for a stable tag, and the
# tag itself counts whether GitHub lists it yet or not.
#
# Needs GH_TOKEN and GITHUB_REPOSITORY. Exit status: 1 when the tags cannot be
# read or GITHUB_REPOSITORY is not set; 2 on a usage error.
set -euo pipefail
shopt -s inherit_errexit

stable_only=false
if [[ "${1:-}" == --stable-only ]]; then
	stable_only=true
	shift
fi
if [[ "$#" -ne 1 ]]; then
	echo "usage: $0 [--stable-only] <tag>" >&2
	exit 2
fi
tag="$1"
repo="${GITHUB_REPOSITORY:?GITHUB_REPOSITORY is not set}"
pattern='^v[0-9]+\.[0-9]+\.[0-9]+$'

stable=false
if [[ "${tag}" =~ ${pattern} ]]; then
	stable=true
fi
if [[ "${stable_only}" == true ]]; then
	echo "stable=${stable}"
	exit 0
fi

highest=false
line_highest=false
if [[ "${stable}" == true ]]; then
	if ! tags="$(gh api --paginate "repos/${repo}/tags?per_page=100" --jq '.[].name')"; then
		echo "cannot read the repository's tags" >&2
		exit 1
	fi
	candidates="$(printf '%s\n%s\n' "${tags}" "${tag}" | grep -E "${pattern}")"
	top="$(sort -V <<<"${candidates}" | tail -n 1)"
	line="${tag%.*}."
	line_top="$(awk -v line="${line}" 'index($0, line) == 1' <<<"${candidates}" |
		sort -V | tail -n 1)"
	if [[ "${top}" == "${tag}" ]]; then
		highest=true
	else
		echo "${tag} is below ${top}, so it moves nothing that names the latest" >&2
	fi
	if [[ "${line_top}" == "${tag}" ]]; then
		line_highest=true
	else
		echo "${tag} is below ${line_top}, so its line's tag stays" >&2
	fi
fi
printf 'stable=%s\nhighest=%s\nline-highest=%s\n' \
	"${stable}" "${highest}" "${line_highest}"
