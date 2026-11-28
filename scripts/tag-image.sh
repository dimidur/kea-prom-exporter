#!/usr/bin/env bash
# Points the image's `latest` and minor-line tags at a release already pushed
# under its version tag, as far as classify-release.sh lets it:
# docs/design.md, "Only the highest release names the latest". It classifies
# immediately before moving them, so the decision is as fresh as it can be.
#
# Usage:
#   scripts/tag-image.sh <image> <tag>
#
# image  the image's repository on the registry
# tag    the release tag, v<version>; the image is already pushed as <version>
#
# The tags are moved with `docker buildx imagetools create`, which copies the
# version's manifest list, every platform included, without rebuilding.
#
# Needs GH_TOKEN, GITHUB_REPOSITORY and docker logged in to the registry.
# Exit status: classify-release.sh's when the tag cannot be classified;
# docker's when the tags cannot be moved; 2 on a usage error.
set -euo pipefail
shopt -s inherit_errexit

if [[ "$#" -ne 2 || "$2" != v* ]]; then
	echo "usage: $0 <image> v<version>" >&2
	exit 2
fi
image="$1"
tag="$2"
version="${tag#v}"

answer="$("$(dirname "$0")/classify-release.sh" "${tag}")"
targets=()
if grep -qx 'highest=true' <<<"${answer}"; then
	targets+=(-t "${image}:latest")
fi
if grep -qx 'line-highest=true' <<<"${answer}"; then
	targets+=(-t "${image}:${version%.*}")
fi
if ((${#targets[@]} == 0)); then
	echo "${tag} moves neither latest nor its minor line's tag"
	exit 0
fi
docker buildx imagetools create "${targets[@]}" "${image}:${version}"
