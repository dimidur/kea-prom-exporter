#!/usr/bin/env bash
# The Kea the compatibility check runs against, in a container named kea.
#
#   kea.sh start      start it from kea-dhcp4.json and wait until it answers
#                     and has a dropped packet to report
#   kea.sh save DIR   save its log to DIR and, if it answers, its replies to
#                     the commands the exporter sends
#   kea.sh stop       remove the container and its config, password included
#
# start and save read KEA_COMPAT_URL, KEA_COMPAT_USER and KEA_COMPAT_PASSWORD;
# start also reads KEA_IMAGE.
# The control socket listens on 127.0.0.1:8001 and DHCP on 127.0.0.1:10067/udp.
set -euo pipefail
shopt -s inherit_errexit

here=$(dirname "$0")

# kea_command prints Kea's reply to a control command, or nothing when Kea does
# not answer; curl says why on stderr.
kea_command() {
	curl -fsS --max-time 5 -u "${KEA_COMPAT_USER:?}:${KEA_COMPAT_PASSWORD:?}" \
		-H 'Content-Type: application/json' \
		-d "{\"command\": \"$1\", \"service\": [\"dhcp4\"]}" "${KEA_COMPAT_URL:?}" || true
}

# drops prints the number of packets Kea reports as dropped.
drops() {
	local reply
	reply=$(kea_command statistic-get-all)
	if ! jq -e 'type == "array"' > /dev/null 2>&1 <<< "${reply}"; then
		echo "::error::statistic-get-all did not answer with a JSON list: ${reply}" >&2
		return 1
	fi
	jq '.[0].arguments["pkt4-receive-drop"][0][0] // 0' <<< "${reply}"
}

start() {
	local config attempt reply running count
	# Kea reads the credentials from files: it refuses inline ones.
	config=$(mktemp -d)
	cp "${here}/kea-dhcp4.json" "${config}/dhcp4.json"
	printf %s "${KEA_COMPAT_USER:?}" > "${config}/control-user"
	printf %s "${KEA_COMPAT_PASSWORD:?}" > "${config}/control-password"
	docker run -d --name kea --label "kea-compat.config=${config}" \
		-p 127.0.0.1:8001:8001 -p 127.0.0.1:10067:67/udp \
		-v "${config}:/kea/config:ro" "${KEA_IMAGE:?}" -c /kea/config/dhcp4.json > /dev/null

	for attempt in $(seq 30); do
		reply=$(kea_command version-get 2> /dev/null)
		if [[ -n "${reply}" ]]; then
			break
		fi
		running=$(docker inspect -f '{{.State.Running}}' kea)
		if [[ "${running}" != true ]]; then
			echo "::error::Kea exited before answering on its control socket" >&2
			return 1
		fi
		if [[ "${attempt}" == 30 ]]; then
			echo "::error::Kea did not answer on its control socket after 30 attempts" >&2
			return 1
		fi
		sleep 1
	done

	# 240 zero bytes give Kea a drop to report: a full DHCPv4 header and a zero
	# magic cookie, which Kea counts as a parse failure. Kea discards a datagram
	# under 236 bytes before any counter moves. UDP can lose it, so it is sent
	# again until the drop shows.
	for _ in $(seq 10); do
		head -c 240 /dev/zero > /dev/udp/127.0.0.1/10067
		count=$(drops)
		if ((count > 0)); then
			return 0
		fi
		sleep 1
	done
	echo "::warning::Kea reports no drop for the malformed datagrams" >&2
}

save() {
	local dir=$1 command reply
	mkdir -p "${dir}"
	docker logs kea > "${dir}/kea.log" 2>&1
	for command in statistic-get-all status-get; do
		reply=$(kea_command "${command}")
		if [[ -n "${reply}" ]]; then
			printf '%s\n' "${reply}" > "${dir}/${command}.json"
		fi
	done
}

stop() {
	local config
	config=$(docker inspect -f '{{index .Config.Labels "kea-compat.config"}}' kea)
	docker rm -f kea > /dev/null
	# The path comes from a label, so only a directory holding start's config
	# is deleted.
	if [[ -n "${config}" && -f "${config}/dhcp4.json" ]]; then
		rm -rf "${config}"
	fi
}

case ${1-} in
start) start ;;
save) save "${2:?usage: kea.sh save DIR}" ;;
stop) stop ;;
*)
	echo "usage: kea.sh start | kea.sh save DIR | kea.sh stop" >&2
	exit 2
	;;
esac
