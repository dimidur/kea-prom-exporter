#!/usr/bin/env bash
# Fake gh for scripts/test-scripts.sh, keeping its state in ${FAKE}.
#
# `gh api <endpoint>` prints the file named after the endpoint, with / as _;
# with --paginate it then prints that file's .page2, as gh runs through the
# pages. --jq applies to each page. A missing file fails as GitHub's 404
# does. Once an endpoint has been read, the files in after-<that file>/
# replace those beside them, as if another run had acted meanwhile.
#
# `gh api graphql` keeps its input as graphql.<request>. Like GitHub, it
# refuses a commit whose expectedHeadOid is not the branch's ref, when the
# fake has one, and moves the ref to new<request> on success. It also exits
# with line <request> of graphql-statuses (default 0).
#
# Like gh, a failed request prints GitHub's error body on stdout, with no
# final newline, and gh's own message on stderr.
#
# `gh release` logs its arguments to release-calls; `view <tag>` succeeds
# when release-<tag> exists.
set -euo pipefail
fake="${FAKE:?FAKE is not set: this fake runs under scripts/test-scripts.sh}"

# Fails the way gh does: GitHub's body on stdout, gh's message on stderr.
fail() {
	printf '%s' "$1"
	echo "$2" >&2
	exit "${3:-1}"
}

if [[ "$1" == release ]]; then
	printf '%s\n' "$*" >>"${fake}/release-calls"
	if [[ "$2" == view && ! -f "${fake}/release-$3" ]]; then
		echo "release not found" >&2
		exit 1
	fi
	exit 0
fi

shift
endpoint=""
expr=""
paginate=false
while (($#)); do
	case "$1" in
	--jq) expr="$2"; shift 2 ;;
	-H | --input) shift 2 ;;
	--paginate) paginate=true; shift ;;
	-*) shift ;;
	*) endpoint="${endpoint:-$1}"; shift ;;
	esac
done
respond() { if [[ -n "${expr}" ]]; then jq -r "${expr}"; else cat; fi; }

if [[ "${endpoint}" == graphql ]]; then
	count="$(find "${fake}" -maxdepth 1 -name 'graphql.*' | wc -l)"
	request=$((count + 1))
	input="${fake}/graphql.${request}"
	cat >"${input}"
	branch="$(jq -r .variables.input.branch.branchName "${input}")"
	expected="$(jq -r .variables.input.expectedHeadOid "${input}")"
	ref="${fake}/repos_owner_repo_git_ref_heads_${branch}"
	if [[ -f "${ref}" ]]; then
		head="$(jq -r .object.sha "${ref}")"
		if [[ "${head}" != "${expected}" ]]; then
			fail "{\"errors\":[{\"message\":\"Expected branch to point to \\\"${expected}\\\"\"}]}" \
				"gh: Expected branch to point to \"${expected}\""
		fi
	fi
	status="$(sed -n "${request}p" "${fake}/graphql-statuses" 2>/dev/null || true)"
	if [[ "${status:-0}" != 0 ]]; then
		fail '{"errors":[{"message":"refused"}]}' "gh: refused" "${status}"
	fi
	if [[ -f "${ref}" ]]; then
		echo "{\"object\":{\"sha\":\"new${request}\"}}" >"${ref}"
	fi
	echo "{\"data\":{\"createCommitOnBranch\":{\"commit\":{\"oid\":\"new${request}\"}}}}" | respond
	exit 0
fi

file="${fake}/${endpoint//\//_}"
if [[ ! -f "${file}" ]]; then
	fail '{"message":"Not Found","status":"404"}' "gh: Not Found (HTTP 404)"
fi
respond <"${file}"
if [[ "${paginate}" == true && -f "${file}.page2" ]]; then
	respond <"${file}.page2"
fi
after="${fake}/after-${file##*/}"
if [[ -d "${after}" ]]; then
	cp "${after}/"* "${fake}/"
	rm -r "${after}"
fi
