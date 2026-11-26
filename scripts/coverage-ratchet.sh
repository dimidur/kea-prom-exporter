#!/usr/bin/env bash
# Coverage ratchet: the unit tests' statement coverage, per file and in total,
# held to the floors recorded in coverage-baseline.json. Why it is built this
# way: docs/design.md, "Coverage is held to a ratchet".
#
# A floor is min(coverage - buffer, cap), at least 0, rounded to 2 decimals.
# cap and buffer are kept in the baseline.
#
# Usage:
#   scripts/coverage-ratchet.sh [check|seed|bump|total] [--reuse]
#
# check   fail if coverage is more than 0.05 points below a recorded floor
#         (the default); a file with no floor yet counts only in the total
# seed    overwrite the baseline from the current run
# bump    raise the floors the current run clears by more than the buffer,
#         give a file with no floor yet its floor, and drop the floors of
#         files that are gone
# total   print only the run's total statement coverage, in percent,
#         rounded to 2 decimals
# --reuse read the profile the last run wrote instead of running the tests,
#         so a CI job runs the tests once and reuses them for every mode;
#         without a profile, the tests run anyway, their output on stderr
#
# The profile is coverage.out at the repository root, or RATCHET_PROFILE.
# Exit status: 1 when check fails or the tests it runs fail, 2 on a usage
# error, 3 when the profile holds no statements. Needs bash 4.4 or later.
set -euo pipefail
shopt -s inherit_errexit

usage="usage: $0 [check|seed|bump|total] [--reuse]"
mode="${1:-check}"
reuse="${2:-}"
case "${mode}" in
check | seed | bump | total) ;;
*)
	echo "${usage}" >&2
	exit 2
	;;
esac
if [[ -n "${reuse}" && "${reuse}" != "--reuse" ]]; then
	echo "${usage}" >&2
	exit 2
fi

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
baseline="${root}/coverage-baseline.json"
profile="${RATCHET_PROFILE:-${root}/coverage.out}"
current="$(mktemp)"
trap 'rm -f "${current}"' EXIT

if [[ "${reuse}" != "--reuse" || ! -f "${profile}" ]]; then
	echo "[ratchet] running the unit tests with coverage..." >&2
	(cd "${root}" && go test -count=1 -coverprofile="${profile}" ./... >&2)
fi
module="$(cd "${root}" && go list -m)"

# One line per file: path relative to the module, statements covered, total.
# A block can have more than one entry in a profile; it counts once, as
# covered if any of its entries has a hit.
summary="$(awk -v prefix="${module}/" '
	NR == 1 { next }
	{
		statements[$1] = $2
		if ($3 > 0) { hit[$1] = 1 }
	}
	END {
		for (block in statements) {
			file = block
			sub(/:[^:]*$/, "", file)
			if (index(file, prefix) == 1) { file = substr(file, length(prefix) + 1) }
			total[file] += statements[block]
			if (block in hit) { covered[file] += statements[block] }
		}
		for (file in total) { printf "%s %d %d\n", file, covered[file], total[file] }
	}
' "${profile}")"
if [[ -z "${summary}" ]]; then
	echo "[ratchet] ERROR: ${profile} holds no statements; the tests measured nothing." >&2
	exit 3
fi

# { files: {path: {statements}}, totals: {statements} }, in percent.
jq -Rn '
	[ inputs | split(" ") | { file: .[0], covered: (.[1] | tonumber), total: (.[2] | tonumber) } ]
	| { files: ( map({ (.file): { statements: (if .total == 0 then 100 else .covered * 100 / .total end) } })
	             | add // {} ),
	    totals: { statements: ((map(.covered) | add) * 100 / (map(.total) | add)) } }
' <<<"${summary}" >"${current}"

# Config comes from the existing baseline, else the defaults.
if [[ -f "${baseline}" ]]; then
	cap="$(jq -r '.cap // 90' "${baseline}")"
	buffer="$(jq -r '.buffer // 2' "${baseline}")"
else
	cap=90
	buffer=2
fi

ratchet_seed() {
	local files totals
	jq -nS \
		--slurpfile current "${current}" \
		--argjson cap "${cap}" --argjson buffer "${buffer}" '
		def floor_at(value): ([ ([value - $buffer, $cap] | min), 0 ] | max) | (. * 100 | round / 100);
		($current[0]) as $run
		| { cap: $cap, buffer: $buffer,
		    files: ( $run.files | to_entries
		             | map({ key: .key, value: (.value | map_values(floor_at(.))) })
		             | from_entries ),
		    totals: ($run.totals | map_values(floor_at(.))) }
	' >"${baseline}.tmp"
	mv "${baseline}.tmp" "${baseline}"
	files="$(jq '.files | length' "${baseline}")"
	totals="$(jq -c '.totals' "${baseline}")"
	echo "seeded ${files} files; totals ${totals}"
}

ratchet_check() {
	local failures total
	failures="$(jq -rn \
		--slurpfile current "${current}" --slurpfile base "${baseline}" '
		($current[0]) as $run | ($base[0]) as $floors
		| [ $floors.files | to_entries[]
		    | .key as $path | .value as $file_floors
		    | select($run.files[$path] != null)
		    | ($file_floors | to_entries[]) | .key as $metric | .value as $floor
		    | ($run.files[$path][$metric]) as $value
		    | select($value + 0.05 < $floor)
		    | "  \($path) \($metric) \($value * 100 | round / 100)% < required \($floor)%" ]
		  + [ $floors.totals | to_entries[]
		    | .key as $metric | .value as $floor | ($run.totals[$metric]) as $value
		    | select($value + 0.05 < $floor)
		    | "  TOTAL \($metric) \($value * 100 | round / 100)% < floor \($floor)%" ]
		| join("\n")
	')"
	if [[ -n "${failures}" ]]; then
		echo "coverage ratchet FAILED:"
		echo "${failures}"
		exit 1
	fi
	total="$(ratchet_total)"
	echo "coverage ratchet OK total statements ${total}% (floors held)"
}

ratchet_total() {
	jq -r '.totals.statements * 100 | round / 100' "${current}"
}

ratchet_bump() {
	local out changes
	out="$(jq -n \
		--slurpfile current "${current}" --slurpfile base "${baseline}" '
		def floor_at(value; buffer; cap): ([ ([value - buffer, cap] | min), 0 ] | max) | (. * 100 | round / 100);
		($current[0]) as $run | ($base[0]) as $floors | ($floors.buffer) as $buffer | ($floors.cap) as $cap
		| reduce ($run.files | to_entries[]) as $entry ( { base: $floors, changes: [] };
		    reduce ($entry.value | to_entries[]) as $measure (.;
		      ($measure.value) as $value | floor_at($value; $buffer; $cap) as $candidate
		      | (.base.files[$entry.key][$measure.key]) as $old_floor
		      | if $old_floor == null
		        then .base.files[$entry.key][$measure.key] = $candidate
		             | .changes += ["onboard \($entry.key) \($measure.key) -> \($candidate)%"]
		        elif ($value > $old_floor + $buffer and $candidate > $old_floor)
		        then .base.files[$entry.key][$measure.key] = $candidate
		             | .changes += ["raise \($entry.key) \($measure.key) \($old_floor)% -> \($candidate)%"]
		        else . end ) )
		| reduce ($floors.files | keys[]) as $path (.;
		    if ($run.files[$path] == null)
		    then .base.files = (.base.files | del(.[$path])) | .changes += ["prune \($path)"]
		    else . end )
		| reduce ($run.totals | to_entries[]) as $measure (.;
		    ($measure.value) as $value | floor_at($value; $buffer; $cap) as $candidate
		    | (.base.totals[$measure.key] // 0) as $old_floor
		    | if ($value > $old_floor + $buffer and $candidate > $old_floor)
		      then .base.totals[$measure.key] = $candidate
		           | .changes += ["raise TOTAL \($measure.key) \($old_floor)% -> \($candidate)%"]
		      else . end )
		| { base: (.base | { cap, buffer, files, totals }), changes }
	')"
	changes="$(jq -r '.changes | length' <<<"${out}")"
	if ((changes > 0)); then
		jq -S '.base' <<<"${out}" >"${baseline}.tmp"
		mv "${baseline}.tmp" "${baseline}"
		echo "coverage ratchet bumped ${changes} floor(s):"
		jq -r '.changes[] | "  " + .' <<<"${out}"
	else
		echo "coverage ratchet: nothing to bump"
	fi
}

"ratchet_${mode}"
