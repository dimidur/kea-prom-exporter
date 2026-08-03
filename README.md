# kea-prom-exporter

Prometheus exporter for **ISC Kea DHCP 3.x**, written in Go.

Surfaces lease-pool utilisation, packet counters, and — the reason this exists —
**HA peer state**, so a failed-over or partner-down DHCP cluster is visible in
Prometheus and alertable like anything else.

## Status

**Early, but working.** Verified end-to-end against a live **Kea 3.2.0** peer in
hot-standby HA: every metric below returns real data. The scope is a headline
metric set rather than full statistic coverage — see the roadmap.

Unit tests run against **response fixtures captured from that same live Kea**,
so the parser is pinned to the shape Kea actually emits, including the per-pool
statistic dimension introduced in 3.x.

## Metrics

| metric | labels | meaning |
| --- | --- | --- |
| `kea_dhcp4_addresses_assigned` | `subnet` | leases currently issued |
| `kea_dhcp4_addresses_capacity` | `subnet` | pool size |
| `kea_dhcp4_packets_received_total` | `type` | DHCPv4 packets in, by op |
| `kea_dhcp4_packets_sent_total` | `type` | DHCPv4 packets out, by op |
| `kea_dhcp4_ha_enabled` | — | 1 when Kea reports an HA relationship, 0 when the hook is not loaded |
| `kea_dhcp4_ha_local_state_info` | `state`, `role`, `mode` | HA state of this peer |
| `kea_dhcp4_ha_partner_in_touch` | — | 1 once the partner has been contacted |
| `kea_dhcp4_ha_partner_last_contact_seconds` | — | seconds since last partner heartbeat; **absent until in touch** |
| `kea_dhcp4_ha_communication_interrupted` | — | 1 when HA communication is broken |
| `kea_up` | — | 1 only when **every** control command succeeded |
| `kea_command_up` | `command` | 1 when that specific command succeeded |
| `kea_scrape_errors_total` | `command` | cumulative failures, per command |
| `kea_scrape_duration_seconds` | — | time spent collecting from Kea, emitted even when the scrape failed |
| `kea_exporter_unhandled_statistics` | — | count of Kea statistics with no mapping here |
| `kea_exporter_build_info` | `version`, `revision`, `goversion` | build identity (value always 1) |

`kea_dhcp4_ha_partner_last_contact_seconds` is deliberately **absent** rather
than 0 before first contact — Kea reports age 0 in that state, which reads on a
dashboard as "contacted 0 seconds ago". Pair it with
`kea_dhcp4_ha_partner_in_touch`.

`--version` prints the same identity as `kea_exporter_build_info`. For a
released image both come from the build; for a local `go build` the version
reads `dev` and the revision comes from Go's VCS stamping, suffixed `-dirty`
when the working tree had uncommitted changes.

On each `/metrics` request the exporter POSTs `statistic-get-all` and
`status-get` to the `kea-dhcp4` HTTP control socket and translates the replies.

**Unknown statistic keys are logged once and ignored, never fatal.** A future
Kea release that adds statistics degrades to *missing* metrics rather than *no*
metrics, and `kea_exporter_unhandled_statistics` counts them.

Extending coverage starts with a row in one of the tables in
[`statmap.go`](statmap.go) — `exactStats` for a fixed name, `subnetStats` for a
`subnet[N].` suffix, `packetStats` for a `pkt4-` direction. That decides which
`statID` a key maps to; the metric it becomes is then wired in
[`collector.go`](collector.go): a `statID`, a descriptor field, its `NewDesc`,
a line in `Describe`, and a `statMetrics` row. The tables remove the branching,
not the wiring, and the tests fail if the two halves disagree.

## Build

```bash
go build -o kea-prom-exporter .
```

## Run

Configuration comes from flags or environment variables; env vars are usually
easier in containers.

```bash
# Reads the control-socket password from a file, so it never appears in the
# environment or in `docker inspect`.
./kea-prom-exporter \
  --listen=:9547 \
  --kea-url=http://127.0.0.1:8001/ \
  --kea-user=kea-ops \
  --kea-password-file=/etc/kea/control-password
```

Equivalent environment form:

```bash
LISTEN=:9547 \
KEA_URL=http://127.0.0.1:8001/ \
KEA_USER=kea-ops \
KEA_PASSWORD_FILE=/etc/kea/control-password \
./kea-prom-exporter
```

Every flag has an environment equivalent, and an explicit flag always wins over
the environment. A value the environment cannot supply — an unparseable
`KEA_TIMEOUT`, an unrecognised `LOG_LEVEL` — is reported at `WARN` and the
default is kept, rather than being applied silently or zeroed.

| Flag | Env | Default | Notes |
|---|---|---|---|
| `--listen` | `LISTEN` | `:9547` | |
| `--kea-url` | `KEA_URL` | `http://127.0.0.1:8001/` | |
| `--kea-user` | `KEA_USER` | *(empty)* | empty disables basic auth |
| `--kea-password-file` | `KEA_PASSWORD_FILE` | *(empty)* | preferred over the inline form |
| `--kea-password` | `KEA_PASSWORD` | *(empty)* | visible in `docker inspect` |
| `--kea-timeout` | `KEA_TIMEOUT` | `5s` | **per control command**, and a scrape issues two |
| `--kea-service` | `KEA_SERVICE` | `dhcp4` | only `dhcp4` is implemented |
| `--log-level` | `LOG_LEVEL` | `info` | `debug`, `info`, `warn`, `error` |
| `--log-format` | `LOG_FORMAT` | `text` | `text` or `json` |
| `--version` | — | — | prints build identity and exits; deliberately has no env form |

`--log-level=debug` additionally names each Kea statistic the exporter has no
mapping for. The count is always exported as
`kea_exporter_unhandled_statistics`, so drift after a Kea upgrade is visible
without turning debug logging on.

On `SIGTERM` or `SIGINT` the exporter stops accepting connections and lets
in-flight scrapes finish before exiting, within a grace period of
`2 × --kea-timeout + 1s` — the longest a legitimate scrape can take. It exits 0
even if that grace expires; a slow shutdown is not a crash.

## Docker

The image runs as **uid 65532** (`nonroot`), so a mounted password file must be
readable by that user — a `0600` file owned by your account is not.

```bash
docker build -t kea-prom-exporter:dev .
chmod 0644 kea-password        # or: chown 65532 kea-password
docker run --rm -p 9547:9547 \
  -e KEA_URL=http://host.docker.internal:8001/ \
  -e KEA_USER=kea-ops \
  -e KEA_PASSWORD_FILE=/run/secrets/kea-password \
  -v ./kea-password:/run/secrets/kea-password:ro \
  kea-prom-exporter:dev
```

Under Kubernetes `PodSecurity: restricted` no `securityContext` override is
needed; the image already declares a non-root user.

Multi-arch build:

```bash
docker buildx build --platform linux/amd64,linux/arm64 \
  --build-arg VERSION=0.1.0 --build-arg REVISION="$(git rev-parse HEAD)" \
  -t dimidur/kea-prom-exporter:0.1.0 --push .
```

Without those build args the image reports `version=dev revision=unknown` —
`.dockerignore` keeps `.git` out of the build context, so Go cannot stamp the
revision itself. The release workflow supplies both automatically.

## Scraping it

Prometheus:

```yaml
scrape_configs:
  - job_name: kea-exporter
    scrape_interval: 60s
    static_configs:
      - targets: ["kea-host:9547"]
```

Grafana Alloy:

```hcl
prometheus.scrape "kea_exporter" {
  targets         = [{ "__address__" = "host-gateway:9547", "job" = "kea-exporter" }]
  scrape_interval = "60s"
  forward_to      = [prometheus.remote_write.central.receiver]
}
```

## Roadmap

Grouped by milestone rather than date.

### v0.1 — green CI, scrapes a live Kea peer

- [x] Commit `go.sum` so CI's `git diff --exit-code go.mod go.sum` step succeeds.
- [x] Verify against a live Kea peer — confirmed on 3.2.0 in hot-standby HA;
  every declared metric returns real data.
- [x] Unit tests over captured `statistic-get-all` + `status-get` fixtures, so
  regressions surface in CI without a live Kea.
- [ ] Tagged `v0.1.0` with a multi-arch image published from CI.

### v0.5 — feature completeness

- [ ] Full `statistic-get-all` coverage: `v4-allocation-fail-*`, `reclaimed-*`,
  `*-lease-reuses`, `*-reservation-conflicts`, `pkt4-admin-filtered`, and the
  rest of the long tail.
- [ ] Per-pool subnet metrics (`subnet[N].pool[M].*`) — the Kea 3.x dimension.
- [ ] HA partner-side state and scopes (`remote.last-state`, `remote.scopes`)
  as labelled metrics, so failover events read cleanly in Grafana.
- [ ] DHCPv6 support, mirroring the dhcp4 collector.
- [ ] Multi-target scraping: one process, several Kea peers, selected by
  `?target=<peer>` (the blackbox-exporter pattern).

### Contributions welcome

- [ ] TLS to the Kea control socket (Kea 3.1.x supports `socket-type: https`
  with mutual TLS).
- [ ] Schema-driven metric mapping, so new Kea statistics don't need a
  recompile.
- [ ] Kea-version detection at startup, warning when the target reports a major
  version this hasn't been validated against.

## Non-goals

- **Stork-server compatibility.** This speaks Prometheus only — no gRPC stream
  to a Stork server. If you want Stork, run Stork.
- **isc-dhcp support.** Kea 3.x only; the legacy `isc-dhcpd` is a different
  daemon with a different management surface.
- **General-purpose Kea CLI.** Read-only: it issues `statistic-get-all` and
  `status-get`, nothing else. No `config-set`, no `lease-add`.

## Alternatives

- [mweinelt/kea-exporter](https://github.com/mweinelt/kea-exporter) — the
  established Python exporter for Kea. As of v0.7.0 its parser predates the
  Kea 3.x per-pool statistic dimension.
- [ISC Stork](https://gitlab.isc.org/isc-projects/stork) — the vendor's own
  monitoring stack. Its agent can expose Prometheus metrics, though HA state is
  surfaced through Stork's own channel rather than in that metric set.

## Support

If this is useful to you, you can sponsor the work:

[![Sponsor](https://img.shields.io/badge/Sponsor-%E2%9D%A4-db61a2?logo=github-sponsors&logoColor=white)](https://github.com/sponsors/dimidur)

## License

[MIT](LICENSE)
