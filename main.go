// Prometheus exporter for ISC Kea DHCPv4 (Kea 3.x).
//
// On each /metrics scrape the exporter POSTs the control commands in
// scrapeCommands to the kea-dhcp4 daemon's HTTP control socket (basic auth)
// and translates the responses into Prometheus metrics.
//
// Scope: a headline metric set covering lease pool utilisation, packet
// counters, and HA peer state, not full statistic coverage.
// Forward-compatible by design: an unknown statistic key is logged once and
// ignored rather than failing the scrape, so a Kea release that adds new
// statistics degrades to missing metrics instead of no metrics. The names go
// to the debug log; the count is always exported as
// kea_exporter_unhandled_statistics.
package main

// The composition root: reads the configuration and assembles the exporter
// from the parts the other files define.
//
// One reason to change: what the process is assembled from.

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
)

func main() {
	flag.Parse()
	if *showVersion {
		fmt.Println(buildIdentity().versionLine())
		return
	}
	envProblems := applyEnv(flag.CommandLine, os.LookupEnv)

	logger, logProblems := newLogger(*logLevel, *logFormat)
	// Before newExporter: the collector logs to slog.Default() as it stands
	// when the collector is built.
	slog.SetDefault(logger)
	// Deferred until the logger exists, and every problem is reported rather
	// than only the last one.
	for _, err := range append(envProblems, logProblems...) {
		logger.Warn("configuration problem", "err", err)
	}

	secret, err := loadPassword(*keaPassFile, *keaPass)
	if err != nil {
		fatal("could not load the Kea password", "err", err)
	}
	settings := keaClient{
		url:      *keaURL,
		user:     *keaUser,
		password: secret,
		service:  *scrapeService,
		timeout:  *httpTimeout,
	}
	if err := validateConfig(settings); err != nil {
		fatal("invalid configuration", "err", err)
	}
	srv := buildServer(*listenAddr, newExporter(newKeaClient(settings)), logger)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	grace := shutdownGrace(maxScrapeDuration(settings.timeout))
	if err := serveUntilSignal(ctx, srv, grace, logger, stop); err != nil {
		fatal("http server failed", "err", err)
	}
}

// newExporter returns the registry that /metrics serves: the Kea collector
// plus the Go runtime and process collectors.
func newExporter(client *keaClient) *prometheus.Registry {
	reg := prometheus.NewRegistry()
	reg.MustRegister(
		newCollector(client),
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
	)
	return reg
}
