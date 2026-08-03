// Prometheus exporter for ISC Kea DHCPv4 (Kea 3.x).
//
// On each /metrics scrape the exporter POSTs `statistic-get-all` and
// `status-get` to the kea-dhcp4 daemon's HTTP control socket (basic
// auth) and translates the responses into Prometheus metrics.
//
// Scope: a headline metric set covering lease pool utilisation, packet
// counters, and HA peer state -- not full statistic coverage. Forward-compatible
// by design — an unknown statistic key is logged once and ignored
// rather than failing the scrape, so a Kea release that adds new
// statistics degrades to missing metrics instead of no metrics. The names go
// to the debug log; the count is always exported as
// kea_exporter_unhandled_statistics.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

func main() {
	flag.Parse()
	if *showVersion {
		id := buildIdentity()
		fmt.Printf("kea-prom-exporter %s (revision %s, %s)\n", id.version, id.revision, id.goVersion)
		return
	}
	envProblems := applyEnv(flag.CommandLine, os.LookupEnv)

	logger, logProblems := newLogger(*logLevel, *logFormat)
	slog.SetDefault(logger)
	// Deferred until the logger exists, and every problem is reported rather
	// than only the last one.
	for _, err := range append(envProblems, logProblems...) {
		slog.Warn("configuration problem", "err", err)
	}

	secret, err := loadPassword(*keaPassFile, *keaPass)
	if err != nil {
		fatal("could not load the Kea password", "err", err)
	}
	if *keaUser != "" && secret == "" {
		fatal("kea-user is set but no password was provided; use --kea-password-file or --kea-password")
	}
	// Only dhcp4 is mapped. Without this, --kea-service=dhcp6 returns a valid
	// response whose every key is unmapped: an empty metric set under a
	// kea_dhcp4_ namespace, reported as kea_up 1.
	if *scrapeService != "dhcp4" {
		fatal("unsupported --kea-service; only dhcp4 is implemented", "service", *scrapeService)
	}

	k := &keaClient{
		url:      *keaURL,
		user:     *keaUser,
		password: secret,
		service:  *scrapeService,
		timeout:  *httpTimeout,
		client: &http.Client{
			// Generous relative to the per-request context deadline, which is
			// what actually bounds a call; this is a backstop.
			Timeout: *httpTimeout * 2,
		},
	}

	reg := prometheus.NewRegistry()
	reg.MustRegister(
		newCollector(k),
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
	)

	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.HandlerFor(reg, promhttp.HandlerOpts{
		Registry: reg,
		// Overlapping scrapes -- two Prometheus servers, a readiness probe, a
		// human with curl -- would each drive their own pair of control-socket
		// requests at a daemon that is also answering DHCP. CoalesceGather
		// makes concurrent scrapes share one collection instead, so the socket
		// sees one round trip per cycle no matter how many scrapers there are.
		// MaxRequestsInFlight was the wrong instrument: it counts /metrics
		// requests rather than Kea requests, and it answers the excess with 503
		// -- which reaches Prometheus as up=0, indistinguishable from Kea
		// actually being down, and drops the per-command diagnostics with it.
		//
		// Joined scrapers observe the same snapshot and timestamp. At a scrape
		// interval measured in tens of seconds that is not observable.
		CoalesceGather: true,
		// Surfaces the reason behind promhttp_metric_handler_errors_total,
		// which otherwise counts failures without ever saying why.
		ErrorLog: slog.NewLogLogger(slog.Default().Handler(), slog.LevelError),
	}))
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintln(w, "kea-prom-exporter — see /metrics")
	})

	srv := &http.Server{
		Addr:              *listenAddr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}

	// A scrape runs the two control commands sequentially, each bounded by
	// --kea-timeout, so the slowest legitimate in-flight request takes twice
	// that. A grace shorter than the work it is waiting for cuts the response
	// off anyway -- which is the thing graceful shutdown exists to prevent.
	grace := 2**httpTimeout + time.Second

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := serveUntilSignal(ctx, srv, grace, stop); err != nil {
		fatal("http server failed", "err", err)
	}
}
