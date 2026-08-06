# Contributing

Thanks for looking. The job here is narrow: turn what Kea's control socket
reports into Prometheus metrics, with HA state as the part that matters most.

## Reporting a problem

Include:

- your **Kea version** (`kea-dhcp4 -V`, or `version-get` against the control socket)
- whether the **HA hook** is loaded — HA metrics are skipped entirely when it
  isn't, which looks identical to "the metrics are missing"
- the exporter's log output; unmapped statistics are logged once each and are
  usually the explanation for a metric you expected but didn't get

`kea_up 0` means at least one control command failed;
`kea_command_up{command="..."}` says which, and
`kea_scrape_errors_total{command="..."}` counts them. If the exporter is up but
a *specific* metric is absent, that's the unmapped-statistic path instead, and
the log will say so.

## Development

The required Go version is the `go` directive in `go.mod`, and it names a full
patch version rather than a minor one — the patch gets pinned when a Go release
fixes a stdlib advisory. An older toolchain downloads the pinned one rather
than failing, so expect a toolchain download on your first build.

```bash
go mod tidy   # CI runs this and fails if it produces a diff
go vet ./...
go test -race ./...
go build -o kea-prom-exporter .
```

CI additionally runs `gofmt`, `staticcheck`, `govulncheck` and `gosec`. All four
are expected to be silent — please keep them that way rather than adding
suppressions, unless the finding is genuinely a false positive, in which case
annotate it with the reason (there is one `#nosec` in the tree, and it explains
itself). Their versions are pinned in `ci.yml`; when reproducing a CI failure
locally, run the pinned version rather than `@latest`.

## Tests without a Kea instance

`testdata/` holds **real** `statistic-get-all` and `status-get` responses
captured from a live Kea, with hostnames replaced. That is deliberate: parsing
real output is what this exporter gets wrong if it gets anything wrong, and
hand-written JSON tends to encode the author's assumptions rather than the
daemon's behaviour.

Before constructing or editing one, read
[docs/kea-behaviour.md](docs/kea-behaviour.md). It records what Kea actually
guarantees — including the constraints that make a plausible-looking payload
one the daemon would refuse to start with.

If you add support for statistics that the existing fixtures don't contain,
please add a fixture captured from a real Kea rather than composing one by hand.

There is one carve-out, and it is narrow. Some statistics only appear once
something has gone wrong — Kea does not create the per-subnet
`v4-allocation-fail*` observations until a lease request actually fails — and
inducing that on a production DHCP server to take a capture is not reasonable.
`testdata/statistic-get-all-with-failures.json` covers those. It is *derived
from* the real capture rather than written from scratch: the same keys, the
same sample shape and history, with the failure counters raised to values that
satisfy the relationships Kea's source guarantees. If you need to extend it,
derive it the same way; do not hand-write a fresh document, because the failure
mode is exactly the one this section warns about — a fixture that encodes what
you believe instead of what the daemon does.

The same carve-out covers the inline `status-get` payloads in `ha_test.go`, for
HA topologies that cannot be captured either: a downed partner, a backup
server, and a hub with several relationships. **Check them against the Kea
source before trusting them** — a payload the daemon would reject makes a test
that passes while proving nothing. Two constraints are easy to violate and are
enforced at config time, not in the `status-get` response:

- Server names must be unique across *all* relationships
  (`HAConfigParser::parseOne`, `HARelationshipMapper::map`).
- More than one relationship requires *every* one to be hot-standby
  (`HAConfigParser::validateRelationships`).

Shape a new HA payload on the ARM's hub-and-spoke example and satisfy yourself
the daemon would accept it. Cite Kea by symbol name rather than line number:
line numbers rot silently and a stale-but-precise-looking citation is worse
than none.

## Test style

Every test opens with a comment stating the behaviour under test and why it is
worth pinning, then uses `// Arrange` / `// Act` / `// Assert` section markers.
Keep both: the comment says why the test exists, the markers say where to look.

```go
func TestSomething(t *testing.T) {
    // Why this matters, in one or two lines.

    // Arrange
    srv := keaStub(t, ...)

    // Act
    got := collectAll(c)

    // Assert
    if len(got) == 0 { ... }
}
```

Table-driven tests mark the sections inside the subtest, where the acting
happens — not around the table literal.

A test that cannot fail is worse than no test. When adding one, make it fail
first — break the code it guards and watch it go red. The metric-count floor
and the double-counting guards were each written that way, and one earlier
version of the channel test was replaced because it turned out it could not
fail.

## Things worth knowing before changing the collector

- **`Collect` can run concurrently.** Prometheus calls it once per scrape and
  scrapes can overlap. Per-collector state must be safe under that — the scrape
  error counter is `atomic.Uint64` for exactly this reason, and there is a test
  that fails under `-race` if it regresses.
- **Unknown statistics must never fail a scrape.** They are logged once and
  skipped. A Kea release that adds statistics should cost you *missing* metrics,
  never *no* metrics.
- **Per-subnet and per-pool statistics coexist in Kea 3.x.** `subnet[N].x` and
  `subnet[N].pool[M].x` both appear; counting both double-counts utilisation.
  A test guards this against a fixture that really does contain per-pool keys.
- **Metric names are an API.** Renaming one breaks dashboards and alerts
  downstream, so treat it as a breaking change.

## Releases

Maintainer only: tag `vX.Y.Z` and CI builds and publishes multi-arch images.

## License

Contributions are accepted under the [MIT License](LICENSE).
