# Contributing

Thanks for looking. The job here is narrow: turn what Kea's control socket
reports into Prometheus metrics, with HA state as the part that matters most.

## Reporting a problem

Include:

- your **Kea version** (`kea-dhcp4 -V`, or `version-get` against the control socket)
- whether the **HA hook** is loaded: HA metrics are skipped entirely when it
  isn't, which looks identical to "the metrics are missing"
- the exporter's log output; unmapped statistics are logged once each and are
  usually the explanation for a metric you expected but didn't get

`kea_up 0` means at least one control command failed;
`kea_command_up{command="..."}` says which, and
`kea_scrape_errors_total{command="..."}` counts them. If the exporter is up but
a *specific* metric is absent, that's the unmapped-statistic path instead, and
the log will say so.

## Development

The `go` line in `go.mod` is the only place the Go version is set, down to the
patch: CI installs it and the image build compiles with it. The `golang` base
tag in the `Dockerfile` only bootstraps that build and is updated on its own; it
does not need to match. With the default `GOTOOLCHAIN`, an older local Go
downloads the `go.mod` version, so expect a download on your first build. A
newer local Go runs as itself, and `go test` fails on it; run with
`GOTOOLCHAIN=go<version>` in that case.

```bash
go mod tidy   # CI runs this and fails if it produces a diff
go vet -tags compat ./...
go test -race ./...
go build -o kea-prom-exporter .
```

CI additionally runs formatting, lint and security checks, listed in `ci.yml`.
Every one is expected to be silent; please keep them that way rather than
adding suppressions, unless the finding is genuinely a false positive, in which
case annotate it, stating the reason in the annotation itself.
Their versions are pinned under `.github/`; when reproducing a CI failure
locally, run the pinned version rather than `@latest`.

A change that drops statement coverage below a floor in
`coverage-baseline.json` fails; add tests rather than lower a floor.
`scripts/coverage-ratchet.sh` runs the same check locally.

## Tests without a Kea instance

`testdata/` holds **real** `statistic-get-all` and `status-get` responses
captured from a live Kea, with hostnames replaced. That is deliberate: parsing
real output is what this exporter gets wrong if it gets anything wrong, and
hand-written JSON tends to encode the author's assumptions rather than the
daemon's behaviour.

Before constructing or editing one, read [docs/design.md](docs/design.md): it
says what the exporter relies on in Kea's output.

If you add support for statistics that the existing fixtures don't contain,
please add a fixture captured from a real Kea rather than composing one by hand.

There is one carve-out, and it is narrow. Some statistics only appear once
something has gone wrong (the per-subnet allocation failures appear only after
a lease request fails), and inducing that on a production DHCP server to take
a capture is not reasonable. `testdata/statistic-get-all-with-failures.json`
covers those. It is *derived from* the real capture rather than written from
scratch: the same sample shape, with the per-subnet allocation-failure keys
added and counters raised to values that keep the exporter's invariants: both
allocation-failure partitions sum to the same total, and the drop reasons sum
to the drop total. It does not satisfy every constraint Kea has: it counts
failures in both scopes for its one subnet, which one Kea configuration would
not. If you need to extend it, derive it the same way; do not hand-write a
fresh document, because the failure mode is exactly the one this section warns
about: a fixture that encodes what you believe instead of what the daemon does.

The same carve-out covers `testdata/status-get-partner-down.json`, derived
from the standby capture, and the inline `status-get` payloads in `ha_test.go`,
for HA topologies that cannot be captured either: a downed partner, a backup
server, and a hub with several relationships. **Check them against what Kea
accepts before trusting them**: a payload the daemon would reject makes a test
that passes while proving nothing. Two constraints are enforced at config time,
not in the `status-get` response:

- Server names must be unique across *all* relationships.
- More than one relationship requires *every* one to be hot-standby.

Two fields of one partner also have to agree: with the partner in touch,
`communication-interrupted: true` needs an `age` of at least
`max-response-delay` divided by 1000 and rounded down (60 by default).

Shape a new HA payload on the hub-and-spoke example in the Kea Administrator
Reference Manual (ARM) and satisfy yourself the daemon would accept it.

## Tests against a real Kea

`compat_test.go` runs only with `-tags compat`, against a running Kea.
`.github/compat/kea.sh` starts and stops one in Docker as the `compat` workflow
does; its header lists the variables it and the test read, and the `env` block
of `compat.yml` has their values.

Mapping a statistic listed in `unmappedStatistics`, in `contract_test.go`,
means removing it from that list in the same change.

A fix for a break the check found comes with a unit test that reproduces it,
built from the replies the failing run saved as its `kea-replies` artifact.

## Documentation

A design decision goes in [docs/design.md](docs/design.md): what the exporter
does, and what in Kea's output it relies on, not how Kea works inside. Code and
test comments point to its heading rather than repeat it.

## Test style

Every test opens with a comment stating the behaviour under test and why it is
worth pinning, then uses `// Arrange` / `// Act` / `// Assert` section markers.
Keep both: the comment says why the test exists, the markers say where to look.

```go
func TestSomething(t *testing.T) {
    // Why this matters, in one or two lines.

    // Arrange
    collector := newCollector(newFakeKea(statistics, status), testReport(quietLogger))

    // Act
    got := collectAll(collector)

    // Assert
    if len(got) == 0 { ... }
}
```

Table-driven tests mark the sections inside the subtest, where the acting
happens, not around the table literal.

A test that cannot fail is worse than no test. When adding one, make it fail
first: break the code it guards and watch it go red.

## Things worth knowing before changing the collector

- **`Collect` can run concurrently.** The Prometheus collector contract allows
  it, even though the server here makes overlapping scrapes share one call.
  Per-collector state must be safe under that: the scrape error counter is
  `atomic.Uint64` for exactly this reason, and there is a test that fails under
  `-race` if it regresses.
- **`Collect` must not start goroutines.** A panic inside `Collect` fails that
  scrape, and any sharing it; a panic on a goroutine it starts is not recovered
  and kills the exporter.
- **Unknown statistics must never fail a scrape.** They are logged once and
  skipped. A Kea release that adds statistics should cost you *missing* metrics,
  never *no* metrics.
- **Per-pool statistics stay ignored.** A test guards this against a fixture
  that really does contain them. Why they are a breakdown of the per-subnet
  ones is in [docs/design.md](docs/design.md#totals-of-other-series-are-not-exported).
- **Metric names are an API.** Renaming one breaks dashboards and alerts
  downstream, so treat it as a breaking change.

## Releases

Maintainer only: tag `vX.Y.Z` and CI builds and publishes multi-arch images.

## License

Contributions are accepted under the [MIT License](LICENSE).
