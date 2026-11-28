# kea-prom-exporter

[![CI](https://github.com/dimidur/kea-prom-exporter/actions/workflows/ci.yml/badge.svg?branch=main)](https://github.com/dimidur/kea-prom-exporter/actions/workflows/ci.yml?query=branch%3Amain)
[![Version](https://img.shields.io/github/v/tag/dimidur/kea-prom-exporter?sort=semver&label=version)](https://github.com/dimidur/kea-prom-exporter/tags)
[![Docker pulls](https://img.shields.io/docker/pulls/dimidur/kea-prom-exporter?label=docker%20pulls)](https://hub.docker.com/r/dimidur/kea-prom-exporter)
[![Coverage](https://img.shields.io/endpoint?url=https%3A%2F%2Fdimidur.github.io%2Fkea-prom-exporter%2Fcoverage.json)](docs/design.md#coverage-is-held-to-a-ratchet)
[![Tested with Kea](https://img.shields.io/badge/tested%20with%20Kea-3.2.1-blue)](docs/design.md#compatibility-is-checked-against-a-real-kea)

Prometheus exporter for **ISC Kea DHCP 3.x**, written in Go.

Surfaces lease-pool utilisation, packet counters and, the reason this exists,
**HA peer state**, so a failed-over or partner-down DHCP cluster is visible in
Prometheus and alertable like anything else.

## Status

**Early, but working.** Verified end-to-end against **both nodes** of a live
**Kea 3.2.0** hot-standby HA pair: every metric below returns real data. The
one path not exercised against real hardware is **multiple HA relationships**
(hub-and-spoke), which needs a topology this project has no lab for; it is
covered by constructed `status-get` payloads only, shaped to the example in the
Kea Administrator Reference Manual (ARM).
The scope is a headline metric set rather than full statistic coverage; see
the roadmap.

Unit tests run mostly against **response fixtures captured from that same live
Kea**, so the parser is pinned to the shape Kea actually emits, including the
per-pool statistic dimension introduced in 3.x. Where a state cannot be
captured without breaking a production DHCP server (allocation failures, a
downed partner, a hub), the payload is constructed instead, and CONTRIBUTING
says how those are derived.

### Supported Kea version

The latest stable Kea **3.x** release, currently tested against **Kea 3.2.1**.
How CI checks it, and why, is in
[docs/design.md](docs/design.md#compatibility-is-checked-against-a-real-kea).

Stable means an even minor version. ISC ships odd minors (3.1, 3.3) as
[development releases][isc-versions] that are EOL as soon as the next stable
lands, so a 3.3 is not supported even while it is newer than 3.2.

[isc-versions]: https://kb.isc.org/docs/aa-00896

## Metrics

| metric | labels | meaning |
| --- | --- | --- |
| `kea_dhcp4_addresses_assigned` | `subnet` | leases currently issued |
| `kea_dhcp4_addresses_capacity` | `subnet` | pool size |
| `kea_dhcp4_addresses_declined` | `subnet` | addresses currently declined |
| `kea_dhcp4_packets_received_total` | `type` | DHCPv4 packets in, by op |
| `kea_dhcp4_packets_sent_total` | `type` | DHCPv4 packets out, by op |
| `kea_dhcp4_packets_dropped_total` | *(none)* | inbound packets dropped, **total** |
| `kea_dhcp4_packets_dropped_by_reason_total` | `reason` | the same drops, by reason; see the warning below |
| `kea_dhcp4_allocation_failures_by_scope_total` | `scope` | server-wide allocation failures, partitioned by lookup scope |
| `kea_dhcp4_allocation_failures_by_cause_total` | `cause` | the **same** failures, partitioned by cause |
| `kea_dhcp4_ha_enabled` | *(none)* | 1 when Kea reports an HA relationship, 0 when the hook is not loaded |
| `kea_dhcp4_ha_local_state_info` | `relationship`, `peer`, `state`, `role`, `mode` | HA state of this peer |
| `kea_dhcp4_ha_partner_state_info` | `relationship`, `peer`, `state`, `role` | HA state of the partner; **absent until in touch** |
| `kea_dhcp4_ha_partner_in_touch` | `relationship`, `peer` | 1 once Kea has a state for the partner, including `unavailable` |
| `kea_dhcp4_ha_partner_last_contact_seconds` | `relationship`, `peer` | seconds since the last successful heartbeat to the partner; **absent until in touch** |
| `kea_dhcp4_ha_communication_interrupted` | `relationship`, `peer` | 1 when HA communication is broken |
| `kea_up` | *(none)* | 1 only when **every** control command succeeded |
| `kea_command_up` | `command` | 1 when that specific command succeeded |
| `kea_scrape_errors_total` | `command` | cumulative failures, per command |
| `kea_scrape_duration_seconds` | *(none)* | time spent collecting from Kea, emitted even when the scrape failed |
| `kea_exporter_unhandled_statistics` | *(none)* | count of Kea statistics with no mapping here, or whose value is not a number |
| `kea_exporter_build_info` | `version`, `revision`, `goversion` | build identity (value always 1) |

### Counting failures without double-counting them

Kea records one event in several statistics at once, so some of these metrics
deliberately do **not** share a label. Reading them as if they did doubles the
numbers.

What follows are the rules for whoever reads these metrics. Why the exporter is
built to them is in [docs/design.md](docs/design.md).

**Allocation failures.** Kea counts a single failure twice: once by scope
(`shared-network` or `subnet`) and once by cause (`no-pools` or
`attempts-exhausted`). Scope and cause are two *different* partitions of the
same events, and each sums to the real total on its own:

```promql
# Either of these is the real failure rate. Adding them together is not.
sum(rate(kea_dhcp4_allocation_failures_by_scope_total[5m]))
sum(rate(kea_dhcp4_allocation_failures_by_cause_total[5m]))

# Why did allocation fail?
sum by (cause) (rate(kea_dhcp4_allocation_failures_by_cause_total[5m]))
```

`attempts-exhausted` is this exporter's label for Kea's `v4-allocation-fail`.
Despite what Kea's ARM says, that statistic is not the total: it is a peer of
`no-pools`, counting failures after at least one allocation attempt.

Several statistics are deliberately **not** exported (why is in
[docs/design.md](docs/design.md)):

- the `pkt4-received` / `pkt4-sent` grand totals;
- the server-wide `assigned-addresses` and `declined-addresses`;
- the per-pool statistics;
- `v4-allocation-fail-classes`;
- the per-subnet `subnet[N].v4-allocation-fail*` keys: the allocation-failure
  metrics are server-wide.

**Dropped packets.** The reasons sum to the total:

```promql
sum(kea_dhcp4_packets_dropped_by_reason_total) == kea_dhcp4_packets_dropped_total
```

Use one or the other, never their sum, and alert on the total.

**Declined addresses.** `kea_dhcp4_addresses_declined` is a subset of
`kea_dhcp4_addresses_assigned`, not a sibling of it: Kea keeps declined leases
counted as assigned. `assigned / capacity` is already right, and
`assigned + declined` is not.

### HA

Every HA metric except `kea_dhcp4_ha_enabled` carries the labels
`relationship` and `peer`, and a daemon with several relationships emits one
series per relationship. `kea_dhcp4_ha_enabled` stays unlabelled: it describes
the daemon, not a relationship.

`kea_dhcp4_ha_partner_last_contact_seconds` and
`kea_dhcp4_ha_partner_state_info` are deliberately **absent** until Kea has a
state for the partner, rather than 0 and an empty state. Pair them with
`kea_dhcp4_ha_partner_in_touch`, which is 1 from then on, until Kea's HA
service restarts.

Kea records the partner as `unavailable` after a failed exchange with it, such
as a heartbeat, and that state is exported: it is the one to alert on. A
partner never reached shows it too, with the age counted from the HA service's
start.

**`relationship` and `peer`.** Kea supports several HA relationships per daemon
(hub-and-spoke), and all of them are exported. Kea gives them no names, so both
labels are derived from the server names it does report:

- **`relationship`** is the lower of the two names, so that **both nodes of a
  pair compute the same value**. It is unique per relationship, so it alone
  tells one relationship from another.
- **`peer`** is the partner's `server-name`. It names the *other* end, so a
  sample carrying `peer=X` was observed from X's partner. On the node whose
  name sorts lower, it is the name `relationship` does not show; on the other
  node the two are equal.

Why the lower name is unique and agrees across both nodes is in
[docs/design.md](docs/design.md#ha-relationship-labels). That agreement matters
when you run one exporter per Kea node, which is the normal deployment. Each
node sees the *other* as its peer, so `peer` alone differs between the two
views of one relationship and cannot join them:

```promql
# one relationship, two exporters: same relationship, different peer
kea_dhcp4_ha_partner_in_touch{relationship="kea-primary", peer="kea-standby"} 1  # from the primary
kea_dhcp4_ha_partner_in_touch{relationship="kea-primary", peer="kea-primary"} 1  # from the standby

max by (relationship) (kea_dhcp4_ha_communication_interrupted)  # one series per relationship
```

A relationship is consequently named after one of its two members, which from
the other member's point of view is the far server. Alerting is unaffected,
since you almost always want to know *which* node lost contact, and that is
per-`instance` anyway.

On a backup server or in `passive-backup` mode the partner metrics are
**absent** rather than 0, and both labels fall back to the local server's name.
**The join property does not hold there**: each node names the relationship
after itself, so the two views do not share a `relationship` value. This
applies to every member in `passive-backup` mode, and to a backup peer inside
an otherwise ordinary hot-standby or load-balancing relationship.

**Caveat on `mode`, latent.** Every relationship reports the *first*
relationship's mode, which is harmless today
([docs/design.md](docs/design.md#ha-relationship-labels)).

### Build identity

`--version` prints the same identity as `kea_exporter_build_info`. For a
released image both come from the build; for a local `go build` the version
reads `dev` and the revision comes from Go's VCS stamping, suffixed `-dirty`
when the working tree had uncommitted changes.

### Where the numbers come from

On each `/metrics` request the exporter POSTs `statistic-get-all` and
`status-get` to the `kea-dhcp4` HTTP control socket and translates the replies.
Requests that arrive while a round is in progress share it.

**Unknown statistic keys are logged once and ignored, never fatal.** A future
Kea release that adds statistics degrades to *missing* metrics rather than *no*
metrics, and `kea_exporter_unhandled_statistics` counts them. A stock Kea
already reports some statistics this exporter does not map, and more with each
subnet, so compare the count against your server's own baseline rather than
alerting on it being above 0.

Extending coverage starts with a row in one of the tables in
[`statmap.go`](statmap.go), whose comments list what else a new metric needs.

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

Every flag that configures the running exporter has an environment equivalent,
and an explicit flag always wins over the environment. A value the environment
cannot supply, such as an unparseable `KEA_TIMEOUT` or an unrecognised
`LOG_LEVEL`, is reported at `WARN` and the default is kept, rather than being
applied silently or zeroed. The exporter takes flags only: a positional
argument is refused with exit 2, as an unknown flag is.

| Flag | Env | Default | Notes |
| --- | --- | --- | --- |
| `--listen` | `LISTEN` | `:9547` | |
| `--kea-url` | `KEA_URL` | `http://127.0.0.1:8001/` | |
| `--kea-user` | `KEA_USER` | *(empty)* | empty disables basic auth |
| `--kea-password-file` | `KEA_PASSWORD_FILE` | *(empty)* | preferred over the inline form |
| `--kea-password` | `KEA_PASSWORD` | *(empty)* | visible in `docker inspect` |
| `--kea-timeout` | `KEA_TIMEOUT` | `5s` | **per control command**, and a scrape sends its commands one after another; zero or less means `5s` |
| `--kea-service` | `KEA_SERVICE` | `dhcp4` | only `dhcp4` is implemented |
| `--log-level` | `LOG_LEVEL` | `info` | `debug`, `info`, `warn`, `error` |
| `--log-format` | `LOG_FORMAT` | `text` | `text` or `json` |
| `--version` | *(none)* | *(none)* | prints build identity and exits; deliberately has no env form |

`--log-level=debug` additionally names each Kea statistic the exporter has no
mapping for or whose value is not a number, and each mapped statistic left out
of a scrape because its value was empty or null. The unhandled ones are always
counted in `kea_exporter_unhandled_statistics`, so drift after a Kea upgrade is
visible without turning debug logging on.

On `SIGTERM` or `SIGINT` the exporter stops accepting connections and lets
in-flight scrapes finish before exiting. The grace period is one effective
`--kea-timeout` (`5s` when it is zero or less) for every control command a
scrape sends, which is the longest a legitimate scrape can take, plus one
second. It exits 0 even if that grace expires; a slow shutdown is not a crash.
The same span bounds every `/metrics` response: one still being written that
long after its request was read is cut off, so a scraper that stops reading
cannot hold up the others.

## Docker

Released images are on Docker Hub as
[`dimidur/kea-prom-exporter`](https://hub.docker.com/r/dimidur/kea-prom-exporter),
for `linux/amd64` and `linux/arm64`. Each stable release is tagged with its
version (`0.3.0`). Its minor line's tag (`0.3`) points at the highest stable
release of that line, and `latest` at the highest overall, so a patch to an
older line leaves `latest` alone. A prerelease (`1.2.0-rc1`) gets only its
own tag. Pin a version. Releases after 0.3.0 have
[release notes](https://github.com/dimidur/kea-prom-exporter/releases) saying
what each one changed.

The image runs as **uid 65532** (`nonroot`), so a mounted password file must be
readable by that user; a `0600` file owned by your account is not.

```bash
VERSION=0.3.0                  # the release to run
chmod 0644 kea-password        # or: chown 65532 kea-password
docker run --rm -p 9547:9547 \
  -e KEA_URL=http://host.docker.internal:8001/ \
  -e KEA_USER=kea-ops \
  -e KEA_PASSWORD_FILE=/run/secrets/kea-password \
  -v ./kea-password:/run/secrets/kea-password:ro \
  "dimidur/kea-prom-exporter:$VERSION"
```

Under Kubernetes `PodSecurity: restricted` no `securityContext` override is
needed; the image already declares a non-root user.

To build the image yourself, `docker build -t kea-prom-exporter:dev .` and run
that tag instead. Multi-arch build:

```bash
REGISTRY=registry.example.com/you   # where your build goes, not Docker Hub's
docker buildx build --platform linux/amd64,linux/arm64 \
  --build-arg VERSION=dev --build-arg REVISION="$(git rev-parse HEAD)" \
  -t "$REGISTRY/kea-prom-exporter:dev" --push .
```

Without those build args the image reports `version=dev revision=unknown`:
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

### v0.1: green CI, scrapes a live Kea peer

- [x] Commit `go.sum` so CI's `git diff --exit-code go.mod go.sum` step succeeds.
- [x] Verify against a live Kea peer: confirmed on 3.2.0 in hot-standby HA;
  every declared metric returns real data.
- [x] Unit tests over captured `statistic-get-all` + `status-get` fixtures, so
  regressions surface in CI without a live Kea.
- [x] Tagged `v0.1.0` with a multi-arch image published from CI.

### v0.5: feature completeness

- [ ] Full `statistic-get-all` coverage: `reclaimed-*`, `*-lease-reuses`,
  `*-reservation-conflicts`, and the rest of the long tail.
  (`v4-allocation-fail-*`, the `pkt4-*-drop` reasons and `declined-addresses`
  are done.)
- [ ] Per-pool subnet metrics (`subnet[N].pool[M].*`), the Kea 3.x dimension.
- [x] HA partner-side state (`remote.last-state`, `remote.role`) as a labelled
  metric, and every relationship exported rather than only the first.
- [ ] HA served scopes (`local.scopes`, `remote.last-scopes`), which say which
  peer is actually answering during a failover.
- [ ] DHCPv6 support, mirroring the dhcp4 collector.
- [ ] Multi-target scraping: one process, several Kea peers, selected by
  `?target=<peer>` (the blackbox-exporter pattern).

### Contributions welcome

- [ ] TLS to the Kea control socket (Kea 3.x supports `socket-type: https`
  with mutual TLS).
- [ ] Schema-driven metric mapping, so new Kea statistics don't need a
  recompile.
- [ ] Kea-version detection at startup, warning when the target reports a major
  version this hasn't been validated against.

## Non-goals

- **Stork-server compatibility.** This speaks Prometheus only, with no gRPC
  stream to a Stork server. If you want Stork, run Stork.
- **isc-dhcp support.** Kea 3.x only; the legacy `isc-dhcpd` is a different
  daemon with a different management surface.
- **General-purpose Kea CLI.** Read-only: it sends only commands that read
  state. No `config-set`, no `lease-add`.

## Alternatives

- [mweinelt/kea-exporter](https://github.com/mweinelt/kea-exporter): the
  established Python exporter for Kea. As of v0.7.0 its parser predates the
  Kea 3.x per-pool statistic dimension.
- [ISC Stork](https://gitlab.isc.org/isc-projects/stork): the vendor's own
  monitoring stack. Its agent can expose Prometheus metrics, though HA state is
  surfaced through Stork's own channel rather than in that metric set.

## Support

If this is useful to you, you can sponsor the work:

[![Sponsor](https://img.shields.io/badge/Sponsor-%E2%9D%A4-db61a2?logo=github-sponsors&logoColor=white)](https://github.com/sponsors/dimidur)

## License

[MIT](LICENSE)
