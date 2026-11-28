#!/usr/bin/env bash
# Commits raised coverage floors to main, on top of the commit they were
# measured on: docs/design.md, "Coverage is held to a ratchet". GitHub refuses
# the commit once main has moved on, which is tolerated, since the run for the
# newer commit raises the floors; any other refusal fails.
#
# The floors come from a job that runs the tests and their dependencies, so
# they are committed only if they keep the ratchet's rule against the
# baseline committed at that commit: every floor a number from 0 to 100, cap
# and buffer unchanged, no floor lower than before, and a file's floors
# dropped only once the file is gone.
#
# Usage:
#   scripts/commit-floors.sh <measured-commit> <floors> <baseline>
#
# measured-commit  the commit the floors were measured on
# floors           the raised coverage-baseline.json
# baseline         coverage-baseline.json in a checkout of that commit; a
#                  file whose floors are dropped is looked for beside it
#
# Needs GH_TOKEN and GITHUB_REPOSITORY. Exit status: 1 when the floors break
# the ratchet's rule, main refuses the commit without having moved on or
# cannot be read after refusing it, or GITHUB_REPOSITORY is not set; 2 on a
# usage error.
set -euo pipefail
shopt -s inherit_errexit

if [[ "$#" -ne 3 ]]; then
	echo "usage: $0 <measured-commit> <floors> <baseline>" >&2
	exit 2
fi
measured="$1"
floors="$2"
baseline="$3"
repo="${GITHUB_REPOSITORY:?GITHUB_REPOSITORY is not set}"

# Each floor lies between its old one (0 for a new file) and 100. jq orders
# null and booleans below every number and strings, arrays and objects above,
# so those bounds admit numbers alone. The two maps must be objects, since
# to_entries reads an array too; a value jq cannot index makes it fail, which
# fails the check as well.
if ! jq -e --slurpfile old "${baseline}" '
	($old[0]) as $o
	| .cap == $o.cap and .buffer == $o.buffer
	and (.files | type) == "object" and (.totals | type) == "object"
	and all(.totals | to_entries[];
		.value >= ($o.totals[.key] // 0) and .value <= 100)
	and all(.files | to_entries[]; .key as $path | .value | to_entries
		| all(.[]; .value >= ($o.files[$path][.key] // 0) and .value <= 100))
	' "${floors}" >/dev/null 2>&1; then
	echo "::error::the raised floors do not keep the ratchet's rule; nothing committed"
	exit 1
fi
dropped="$(jq -r --slurpfile new "${floors}" \
	'(.files | keys) - ($new[0].files | keys) | .[]' "${baseline}")"
while IFS= read -r path; do
	if [[ -n "${path}" && -e "$(dirname "${baseline}")/${path}" ]]; then
		echo "::error::the raised floors drop ${path}, which still exists; nothing committed"
		exit 1
	fi
done <<<"${dropped}"

if "$(dirname "$0")/commit-file.sh" main "${measured}" coverage-baseline.json \
	"${floors}" "ci: raise the coverage floors [skip ci]"; then
	exit 0
fi
if ! landed="$(gh api "repos/${repo}/commits/main" --jq .sha)"; then
	echo "::error::main refused the raised floors, and cannot be read to say why"
	exit 1
fi
if [[ "${landed}" != "${measured}" ]]; then
	echo "::notice::main moved on during this run; its next run raises the floors"
	exit 0
fi
echo "::error::main refused the raised floors without having moved on"
exit 1
