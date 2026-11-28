#!/usr/bin/env bash
# Commits one file to a branch through the GraphQL createCommitOnBranch
# mutation. GitHub signs such a commit, and with an app's or the workflow's
# token it is verified as that bot's.
#
# Usage:
#   scripts/commit-file.sh <branch> <expected-head> <path> <file> <headline>
#
# branch         the branch to commit to
# expected-head  the commit the branch must still be at; GitHub refuses the
#                commit otherwise
# path           where the file goes in the repository
# file           the local text file whose contents are committed; it is read
#                as UTF-8, so a binary file would be corrupted
# headline       the commit message
#
# Prints the new commit's oid, and nothing else on stdout: when GitHub refuses
# the commit, gh prints the error body to stdout without a final newline,
# which would run into a caller's next line, so that body goes to stderr.
#
# Needs GH_TOKEN and GITHUB_REPOSITORY. Exit status: gh's, non-zero, when
# GitHub refuses the commit; 1 when the file cannot be read or
# GITHUB_REPOSITORY is not set; 2 on a usage error.
set -euo pipefail
shopt -s inherit_errexit

if [[ "$#" -ne 5 ]]; then
	echo "usage: $0 <branch> <expected-head> <path> <file> <headline>" >&2
	exit 2
fi
branch="$1"
expected="$2"
path="$3"
file="$4"
headline="$5"
repo="${GITHUB_REPOSITORY:?GITHUB_REPOSITORY is not set}"
if [[ ! -r "${file}" ]]; then
	echo "cannot read ${file}" >&2
	exit 1
fi

request="$(jq -n \
	--arg repo "${repo}" --arg branch "${branch}" \
	--arg head "${expected}" --arg path "${path}" --arg headline "${headline}" \
	--rawfile contents "${file}" '
	{ query: ("mutation($input: CreateCommitOnBranchInput!) {"
	          + " createCommitOnBranch(input: $input) { commit { oid } } }"),
	  variables: { input: {
	    branch: { repositoryNameWithOwner: $repo, branchName: $branch },
	    expectedHeadOid: $head,
	    message: { headline: $headline },
	    fileChanges: { additions: [
	      { path: $path, contents: ($contents | @base64) } ] } } } }')"
if oid="$(gh api graphql --input - --jq '.data.createCommitOnBranch.commit.oid' \
	<<<"${request}")"; then
	echo "${oid}"
else
	refused=$?
	printf '%s\n' "${oid}" >&2
	exit "${refused}"
fi
