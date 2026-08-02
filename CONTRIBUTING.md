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

```bash
go mod tidy
go vet ./...
go test -race ./...
go build -o kea-prom-exporter .
```

CI additionally runs `gofmt`, `staticcheck`, `govulncheck` and `gosec`. All four
are expected to be silent — please keep them that way rather than adding
suppressions, unless the finding is genuinely a false positive, in which case
annotate it with the reason (there is one `#nosec` in the tree, and it explains
itself).

## Tests without a Kea instance

`testdata/` holds **real** `statistic-get-all` and `status-get` responses
captured from a live Kea, with hostnames replaced. That is deliberate: parsing
real output is what this exporter gets wrong if it gets anything wrong, and
hand-written JSON tends to encode the author's assumptions rather than the
daemon's behaviour.

If you add support for statistics that the existing fixtures don't contain,
please add a fixture captured from a real Kea rather than composing one by hand.

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
