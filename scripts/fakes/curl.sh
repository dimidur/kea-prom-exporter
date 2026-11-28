#!/usr/bin/env bash
# Fake curl for scripts/test-scripts.sh, keeping its state in ${FAKE}.
#
# Writes an SVG titled `label=... message=... color=...` to its -o file,
# from its --data-urlencode values, and logs those values to curl-calls, its
# URL to curl-urls and its --retry, --max-time and --max-filesize to
# curl-limits. It writes curl-body instead when that exists, whatever its
# size, as curl does when a server sends no length. When curl-fails exists
# the server answers with an error: like curl, it then fails only with -f,
# and otherwise writes the server's error page and succeeds.
set -euo pipefail
fake="${FAKE:?FAKE is not set: this fake runs under scripts/test-scripts.sh}"

out=""
data=()
limits=()
fail_on_error=false
while (($#)); do
	case "$1" in
	-o) out="$2"; shift 2 ;;
	--data-urlencode) data+=("$2"); shift 2 ;;
	--retry | --max-time | --max-filesize) limits+=("$1 $2"); shift 2 ;;
	--*) shift ;;
	-*)
		if [[ "$1" == *f* ]]; then
			fail_on_error=true
		fi
		shift
		;;
	*) printf '%s\n' "$1" >>"${fake}/curl-urls"; shift ;;
	esac
done
printf '%s\n' "${data[*]}" >>"${fake}/curl-calls"
printf '%s\n' "${limits[*]}" >>"${fake}/curl-limits"

if [[ -f "${fake}/curl-fails" ]]; then
	if [[ "${fail_on_error}" == true ]]; then
		echo "curl: (22) The requested URL returned error: 503" >&2
		exit 22
	fi
	echo '<html>503 Service Unavailable</html>' >"${out}"
	exit 0
fi
if [[ -f "${fake}/curl-body" ]]; then
	cat "${fake}/curl-body" >"${out}"
else
	printf '<svg xmlns="http://www.w3.org/2000/svg"><title>%s</title></svg>\n' \
		"${data[*]}" >"${out}"
fi
