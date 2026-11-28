#!/usr/bin/env bash
# Draws one README badge and commits it to the badges branch, from which the
# README links it. Why it decides at run time whether, and for the version
# what, to publish, and fetches the SVG rather than drawing it: docs/design.md,
# "The badges are committed by CI".
#
# Usage:
#   scripts/badge.sh coverage <commit> <percent>
#   scripts/badge.sh version
#
# coverage  <percent> as coverage.svg, only while <commit> is still main's
#           newest; main being ahead only by commits that start no run of
#           their own, marked with one of GitHub's skip markers as the
#           coverage bot's floors are, still counts as <commit>
# version   the tag of the repository's Latest release, as version.svg
#
# Each attempt reads the branch's head first, then the source (main's
# comparison or the Latest release), and commits against that head, which
# GitHub refuses once the head has moved; a refused attempt starts over. That
# order is what keeps the older of two racing runs off the branch (same
# heading). The SVG is shields.io's static badge; badges sit at the branch's
# top level, and one the branch already holds byte for byte is not committed
# again.
#
# Needs GH_TOKEN and GITHUB_REPOSITORY. Exit status: 1 when GitHub or
# shields.io cannot be read, shields.io's answer is not a badge to commit,
# every attempt is refused, or GITHUB_REPOSITORY is not set; 2 on a usage
# error, including a percentage above 100.
set -euo pipefail
shopt -s inherit_errexit

usage() {
	echo "usage: $0 coverage <commit> <percent> | $0 version" >&2
	exit 2
}

case "${1:-}" in
coverage)
	[[ "$#" -eq 3 && "$3" =~ ^[0-9]+([.][0-9]+)?$ ]] || usage
	jq -ne --argjson coverage "$3" '$coverage <= 100' >/dev/null || usage
	;;
version) [[ "$#" -eq 1 ]] || usage ;;
*) usage ;;
esac
mode="$1"
repo="${GITHUB_REPOSITORY:?GITHUB_REPOSITORY is not set}"
scripts="$(dirname "$0")"
# The markers in a commit message that make GitHub start no workflow for it.
skip='\[(skip ci|ci skip|no ci|skip actions|actions skip)\]'
attempts=3
# A shields.io badge is about 1.3 KB, and starts with this root element.
max_size=16384
root='<svg xmlns="http://www.w3.org/2000/svg"'
svg="$(mktemp)"
trap 'rm -f "${svg}"' EXIT

# The coverage badge's colour: shields.io's brightgreen from 90 %, yellow
# from 75 %, red below.
coverage_colour() {
	jq -nr --argjson coverage "$1" '
		if $coverage >= 90 then "brightgreen" elif $coverage >= 75 then "yellow"
		else "red" end'
}

# Sets path, label, message and colour from what GitHub holds now, or exits
# when there is nothing to publish.
read_badge() {
	local comparison others tag
	if [[ "${mode}" == coverage ]]; then
		if ! comparison="$(gh api "repos/${repo}/compare/$2...main")"; then
			echo "cannot compare $2 with main" >&2
			exit 1
		fi
		others="$(jq --arg skip "${skip}" \
			'[.commits[] | select(.commit.message | test($skip) | not)] | length' \
			<<<"${comparison}")"
		if [[ "${others}" != 0 ]]; then
			echo "::notice::main moved on; the run for its newest commit publishes the coverage badge"
			exit 0
		fi
		path=coverage.svg
		label=coverage
		message="$3%"
		colour="$(coverage_colour "$3")"
	else
		if ! tag="$(gh api "repos/${repo}/releases/latest" --jq .tag_name)"; then
			echo "cannot read the Latest release" >&2
			exit 1
		fi
		path=version.svg
		label=version
		message="${tag}"
		colour=blue
	fi
}

# Fetches the badge into ${svg} and sets drawn to its blob. The SVG is not
# pinned, so it is committed only if it is small, has an SVG root and holds
# nothing that could run or link: no script, foreignObject, href or event
# handler.
draw() {
	local start size
	if ! curl -fsS --retry 3 --max-time 30 --max-filesize "${max_size}" -o "${svg}" \
		--get --data-urlencode "label=${label}" --data-urlencode "message=${message}" \
		--data-urlencode "color=${colour}" https://img.shields.io/static/v1; then
		echo "cannot draw ${path}: shields.io did not answer" >&2
		exit 1
	fi
	size="$(wc -c <"${svg}")"
	if ((size > max_size)); then
		echo "cannot draw ${path}: shields.io's answer is over ${max_size} bytes" >&2
		exit 1
	fi
	start="$(head -c "${#root}" "${svg}")"
	if [[ "${start}" != "${root}" ]]; then
		echo "cannot draw ${path}: shields.io answered with something other than an SVG" >&2
		exit 1
	fi
	if grep -Eiq '<script|<foreignobject|href=|[[:space:]]on[a-z]+[[:space:]]*=' "${svg}"; then
		echo "cannot draw ${path}: shields.io's SVG holds script, a link or an event handler" >&2
		exit 1
	fi
	drawn="$(git hash-object "${svg}")"
}

for ((attempt = 1; attempt <= attempts; attempt++)); do
	if ! tip="$(gh api "repos/${repo}/git/ref/heads/badges" --jq .object.sha)"; then
		echo "cannot read the badges branch; it must exist before a badge is committed" >&2
		exit 1
	fi
	read_badge "$@"
	draw
	if ! tree="$(gh api "repos/${repo}/git/trees/${tip}")"; then
		echo "cannot read the files on the badges branch" >&2
		exit 1
	fi
	committed="$(jq -r --arg path "${path}" '.tree[] | select(.path == $path) | .sha' \
		<<<"${tree}")"
	if [[ "${committed}" == "${drawn}" ]]; then
		echo "${path} is unchanged"
		exit 0
	fi
	if "${scripts}/commit-file.sh" badges "${tip}" "${path}" "${svg}" \
		"badges: ${label} ${message}"; then
		exit 0
	fi
	echo "attempt ${attempt} to commit ${path} was refused" >&2
	if ((attempt < attempts)); then
		sleep 5
	fi
done
echo "the badges branch refused ${path} ${attempts} times" >&2
exit 1
