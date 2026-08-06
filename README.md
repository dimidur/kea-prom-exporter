# kea-prom-exporter

Prometheus exporter for **ISC Kea DHCP 3.x**, written in Go.

Surfaces lease-pool utilisation, packet counters, and — the reason this exists —
**HA peer state**, so a failed-over or partner-down DHCP cluster is visible in
Prometheus and alertable like anything else.

## Status

**Early, but working.** Verified end-to-end against **both nodes** of a live
**Kea 3.2.0** hot-standby HA pair: every metric below returns real data. The
one path not exercised against real hardware is **multiple HA relationships**
(hub-and-spoke), which needs a topology this project has no lab for; it is
covered by constructed `status-get` payloads only, shaped to the ARM's example.
The scope is a headline metric set rather than full statistic coverage — see
the roadmap.

Unit tests run mostly against **response fixtures captured from that same live
Kea**, so the parser is pinned to the shape Kea actually emits, including the
per-pool statistic dimension introduced in 3.x. Where a state cannot be
captured without breaking a production DHCP server — allocation failures, a
downed partner, a hub — the payload is constructed instead, and CONTRIBUTING
says how those are derived.

### Supported Kea versions

**Kea 3.2.x**, verified against 3.2.0. This is the set the source comments
mean when they say a Kea symbol or behaviour holds "across the supported
releases".

Only **stable** releases are in scope. ISC ships odd minor versions (3.1, 3.3)
as [development releases][isc-versions] that are EOL as soon as the next stable
lands; even minors are the stable line. A newer version number is therefore not
automatically a newer supported release — 3.3.0 is a development branch, while
3.2.x is current stable and 3.0.x is LTS.

Other stable branches are **untested, not unsupported** — nothing is known to
break on 3.0.x, it simply has not been exercised. Widening this list means
running the suite against that release, not editing the line.

[isc-versions]: https://kb.isc.org/docs/aa-00896

## Metrics

| metric | labels | meaning |
| --- | --- | --- |
| `kea_dhcp4_addresses_assigned` | `subnet` | leases currently issued |
| `kea_dhcp4_addresses_capacity` | `subnet` | pool size |
| `kea_dhcp4_addresses_declined` | `subnet` | addresses withdrawn by a DHCPDECLINE |
| `kea_dhcp4_packets_received_total` | `type` | DHCPv4 packets in, by op |
| `kea_dhcp4_packets_sent_total` | `type` | DHCPv4 packets out, by op |
| `kea_dhcp4_packets_dropped_total` | — | inbound packets dropped, **total** |
| `kea_dhcp4_packets_dropped_by_reason_total` | `reason` | the same drops, by reason — see the warning below |
| `kea_dhcp4_allocation_failures_by_scope_total` | `scope` | server-wide allocation failures, partitioned by lookup scope |
| `kea_dhcp4_allocation_failures_by_cause_total` | `cause` | the **same** failures, partitioned by cause |
| `kea_dhcp4_ha_enabled` | — | 1 when Kea reports an HA relationship, 0 when the hook is not loaded |
| `kea_dhcp4_ha_local_state_info` | `relationship`, `peer`, `state`, `role`, `mode` | HA state of this peer |
| `kea_dhcp4_ha_partner_state_info` | `relationship`, `peer`, `state`, `role` | HA state of the partner; **absent until in touch** |
| `kea_dhcp4_ha_partner_in_touch` | `relationship`, `peer` | 1 once the partner has been contacted |
| `kea_dhcp4_ha_partner_last_contact_seconds` | `relationship`, `peer` | seconds since last partner heartbeat; **absent until in touch** |
| `kea_dhcp4_ha_communication_interrupted` | `relationship`, `peer` | 1 when HA communication is broken |
| `kea_up` | — | 1 only when **every** control command succeeded |
| `kea_command_up` | `command` | 1 when that specific command succeeded |
| `kea_scrape_errors_total` | `command` | cumulative failures, per command |
| `kea_scrape_duration_seconds` | — | time spent collecting from Kea, emitted even when the scrape failed |
| `kea_exporter_unhandled_statistics` | — | count of Kea statistics with no mapping here |
| `kea_exporter_build_info` | `version`, `revision`, `goversion` | build identity (value always 1) |

### Counting failures without double-counting them

Kea records one event in several statistics at once, so some of these metrics
deliberately do **not** share a label. Reading them as if they did doubles the
numbers.

What follows is the operator-facing summary. The derivations behind it — which
Kea symbol each claim comes from, and where the ARM contradicts the source —
are in [docs/kea-behaviour.md](docs/kea-behaviour.md).

**Allocation failures.** Kea counts a single failure in two independent branches
(`AllocEngine::allocateUnreservedLease4` in [`alloc_engine.cc`][alloc-engine]):
`shared-network` **xor** `subnet`, then `no-pools` **xor** `attempts-exhausted`.
Scope and cause are therefore two *different* partitions of the same events, and
each sums to the real total on its own:

```promql
# Either of these is the real failure rate. Adding them together is not.
sum(rate(kea_dhcp4_allocation_failures_by_scope_total[5m]))
sum(rate(kea_dhcp4_allocation_failures_by_cause_total[5m]))

# Why did allocation fail?
sum by (cause) (rate(kea_dhcp4_allocation_failures_by_cause_total[5m]))
```

`attempts-exhausted` is this exporter's label for Kea's `v4-allocation-fail`.
Note Kea's own ARM calls that statistic the *"number of total address allocation
failures"*, which the source contradicts — it is the `else` branch of
`total_attempts == 0`, i.e. a peer of `no-pools`, not their sum. The label name
says what it actually counts.

Several statistics are deliberately **not** exported, each because exporting it
would double something already counted: the `pkt4-received` / `pkt4-sent` grand
totals, the server-wide `assigned-addresses` and `declined-addresses` (sums of
their per-subnet forms), and two allocation-failure cases.
`v4-allocation-fail-classes` counts failures where the client belonged to any
class — but `Dhcpv4Exchange::classifyPacket` adds the `ALL` class to every
packet ([`dhcp4_srv.cc`][dhcp4-srv]), so it tracks the by-cause total rather
than isolating anything. And the per-subnet `subnet[N].v4-allocation-fail*`
keys duplicate the globals shown here; the globals are used because Kea does
not create the per-subnet ones until the first failure, so a healthy server
would otherwise export no series at all.

**Dropped packets.** The reasons *are* an exact partition of the total: Kea
records a reason, then bumps the drop alongside it — see the *"Specific drop
cause stat was increased by accept\* methods"* comment in
`Dhcpv4Srv::processPacket` ([`dhcp4_srv.cc`][dhcp4-srv]). Several sites bump the
total; each records a reason at the same time, which is what keeps the partition
exact, so the two agree:

```promql
sum(kea_dhcp4_packets_dropped_by_reason_total) == kea_dhcp4_packets_dropped_total
```

Use one or the other, never their sum. The total exists separately because it
stays correct when a Kea release adds a reason this exporter does not yet know.

**Declined addresses.** `kea_dhcp4_addresses_declined` is a subset of
`kea_dhcp4_addresses_assigned`, not a sibling of it. `Dhcpv4Srv::declineLease`
([`dhcp4_srv.cc`][dhcp4-srv]) deliberately keeps declined leases counted as
assigned so that pool utilisation stays meaningful, so `assigned / capacity` is
already right and `assigned + declined` is not.

### HA

> **Breaking change.** Four of the five `kea_dhcp4_ha_*` metrics that existed
> before now carry two additional labels, `relationship` and `peer`
> (`kea_dhcp4_ha_enabled` stays unlabelled — it describes the daemon, not a
> relationship). Queries that
> match a full label set exactly need updating; queries selecting by metric
> name are unaffected. Separately, a daemon with several relationships now
> emits one series per relationship where it previously emitted one in total,
> so `sum`/`avg`/`max` over those metrics change there — that is from
> exporting every relationship, not from the labels. This is
> pre-1.0 and metric names are treated as an API, so the change is called out
> here rather than made quietly.

`kea_dhcp4_ha_partner_last_contact_seconds` is deliberately **absent** rather
than 0 before first contact — Kea reports age 0 in that state, which reads on a
dashboard as "contacted 0 seconds ago". Pair it with
`kea_dhcp4_ha_partner_in_touch`. `kea_dhcp4_ha_partner_state_info` is absent in
the same situation and for the same reason: Kea reports the partner's state as
an empty string until it has heard from it, and `state=""` is a worse lie than
no series at all. Note that `unavailable` **is** a real state and is exported.

**`relationship` and `peer`.** Kea supports several HA relationships per daemon
(hub-and-spoke), and all of them are exported. It does not, however, give them
names: at [Kea 3.2.0][ha-impl] each entry in `high-availability` is built from
just `ha-servers` and `ha-mode`, with no identifier of its own. Both labels are
therefore derived from the server names Kea does report:

- **`relationship`** is the lower of the two names, so that **both nodes of a
  pair compute the same value**. It is unique per relationship, so it alone
  tells one relationship from another.
- **`peer`** is the partner's `server-name` — it names the *other* end, so a
  sample carrying `peer=X` was observed from X's partner. It is the thing
  `relationship` deliberately hides, which is why both exist rather than one.

Both are well-defined because Kea requires peer names to be unique across every
relationship on a daemon, and [says so when rejecting a config][ha-unique]:
*"server names must be unique for different relationships"*. `relationship` is
consequently unique, and the lower-of-two rule makes it agree across both
nodes.

That last property matters when you run one exporter per Kea node, which is the
normal deployment. Each node sees the *other* as its peer, so `peer` alone
differs between the two views of one relationship and cannot join them:

```promql
# one relationship, two exporters — same relationship, different peer
kea_dhcp4_ha_partner_in_touch{relationship="kea-primary", peer="kea-standby"} 1  # from the primary
kea_dhcp4_ha_partner_in_touch{relationship="kea-primary", peer="kea-primary"} 1  # from the standby

max by (relationship) (kea_dhcp4_ha_communication_interrupted)  # one series per relationship
```

A relationship is consequently named after one of its two members, which from
the other member's point of view is the far server. [Stork][stork-svc] names a
relationship after one real member too, though it can afford an asymmetric name
because it [reconciles the two sides by scanning both][stork-status]; a
Prometheus label cannot do a set-membership match, which is why this uses the
lower of the pair rather than the local name. Alerting is unaffected either
way, since you almost always want to know *which* node lost contact, and that
is per-`instance` anyway.

On a backup server or in `passive-backup` mode Kea omits the partner half of
the response entirely, so the partner metrics are **absent** rather than 0, and
both labels fall back to the local server's name. **The join property does not
hold there**: with no partner name to compare against, each node names the
relationship after itself, so the two views do not share a `relationship`
value. This applies to every member in `passive-backup` mode, and to a backup
peer inside an otherwise ordinary hot-standby or load-balancing relationship.

**Caveat on `mode`, latent.** Kea fills in `ha-mode` for every relationship
from the *first* relationship's configuration (`HAImpl::commandProcessed` calls
`config_->get()`, which returns the first element). Today that is harmless: Kea
only permits multiple relationships when [every one of them is
hot-standby][ha-validate], so all the modes are equal and relationship 0's is
always the right answer. The label becomes wrong only if Kea lifts that
restriction — it is recorded here so the cause is known if it ever does, not
because the `mode` label is currently untrustworthy.

[alloc-engine]: https://github.com/isc-projects/kea/blob/Kea-3.2.0/src/lib/dhcpsrv/alloc_engine.cc
[dhcp4-srv]: https://github.com/isc-projects/kea/blob/Kea-3.2.0/src/bin/dhcp4/dhcp4_srv.cc
[ha-impl]: https://github.com/isc-projects/kea/blob/Kea-3.2.0/src/hooks/dhcp/high_availability/ha_impl.cc
[ha-unique]: https://github.com/isc-projects/kea/blob/Kea-3.2.0/src/hooks/dhcp/high_availability/ha_config_parser.cc
[ha-validate]: https://github.com/isc-projects/kea/blob/Kea-3.2.0/src/hooks/dhcp/high_availability/ha_config_parser.cc
[stork-svc]: https://github.com/isc-projects/stork/blob/v2.5.0/backend/server/daemons/kea/service.go
[stork-status]: https://github.com/isc-projects/stork/blob/v2.5.0/backend/server/daemons/kea/status.go

### Build identity

`--version` prints the same identity as `kea_exporter_build_info`. For a
released image both come from the build; for a local `go build` the version
reads `dev` and the revision comes from Go's VCS stamping, suffixed `-dirty`
when the working tree had uncommitted changes.

### Where the numbers come from

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
| --- | --- | --- | --- |
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

- [ ] Full `statistic-get-all` coverage: `reclaimed-*`, `*-lease-reuses`,
  `*-reservation-conflicts`, and the rest of the long tail.
  (`v4-allocation-fail-*`, the `pkt4-*-drop` reasons and `declined-addresses`
  are done.)
- [ ] Per-pool subnet metrics (`subnet[N].pool[M].*`) — the Kea 3.x dimension.
- [x] HA partner-side state (`remote.last-state`, `remote.role`) as a labelled
  metric, and every relationship exported rather than only the first.
- [ ] HA served scopes (`local.scopes`, `remote.last-scopes`), which say which
  peer is actually answering during a failover. When this lands, extract
  `collectHA`'s per-relationship body into an `emitRelationship` helper: the
  loop is already at the length where a sixth and seventh emitted field stops
  being readable, and the scopes fields go straight into it.
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
