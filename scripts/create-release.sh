#!/usr/bin/env bash
# Creates a tag's GitHub release, with notes generated from the merged pull
# requests and sorted by the labels in .github/release.yml. Whether it is a
# prerelease and whether it becomes Latest come from classify-release.sh:
# docs/design.md, "Only the highest release names the latest".
#
# Usage:
#   scripts/create-release.sh <tag> <stable> <highest>
#
# stable, highest  true or false, as classify-release.sh prints them
#
# An existing release is left alone: gh release edit cannot regenerate notes,
# and rewriting a release would discard edits made by hand. --verify-tag stops
# gh creating a missing tag from the default branch, and --latest is always
# given, since gh otherwise picks Latest by date as well as version.
#
# Needs GH_TOKEN and GITHUB_REPOSITORY. Exit status: gh's when the release
# cannot be created; 1 when GITHUB_REPOSITORY is not set; 2 on a usage error.
set -euo pipefail
shopt -s inherit_errexit

if [[ "$#" -ne 3 || ! "$2" =~ ^(true|false)$ || ! "$3" =~ ^(true|false)$ ]]; then
	echo "usage: $0 <tag> <true|false> <true|false>" >&2
	exit 2
fi
tag="$1"
stable="$2"
highest="$3"
repo="${GITHUB_REPOSITORY:?GITHUB_REPOSITORY is not set}"

if gh release view "${tag}" --repo "${repo}" >/dev/null 2>&1; then
	echo "release ${tag} already exists; leaving it as it is"
	exit 0
fi
flags=(--generate-notes --verify-tag "--latest=${highest}")
if [[ "${stable}" != true ]]; then
	flags+=(--prerelease)
fi
gh release create "${tag}" --repo "${repo}" "${flags[@]}"
