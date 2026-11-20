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

// Where the process meets the operating system and the build: everything run
// takes from outside, and the real values of it.
//
// One reason to change: what the process takes from outside.

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"runtime/debug"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
)

func main() { os.Exit(run(os.Args[0], os.Args[1:], osProcess())) }

// process is the operating-system and build dependencies run takes from its
// caller.
type process struct {
	stdout, stderr io.Writer
	lookupEnv      func(key string) (string, bool)
	readFile       func(name string) ([]byte, error)
	listen         func(network, address string) (net.Listener, error)
	notifyContext  func(parent context.Context, signals ...os.Signal) (context.Context, context.CancelFunc)
	keaTransport   http.RoundTripper
	// adoptLogger makes the configured logger the process-wide one, which
	// also routes the log package through it: net/http's client writes
	// there, outside any ErrorLog.
	adoptLogger func(*slog.Logger)
	build       buildSource
	now         func() time.Time
	// runtimeCollectors describe the process itself.
	runtimeCollectors []prometheus.Collector
}

// osProcess is the process run gets from the operating system and the build.
func osProcess() process {
	return process{
		stdout:        os.Stdout,
		stderr:        os.Stderr,
		lookupEnv:     os.LookupEnv,
		readFile:      os.ReadFile,
		listen:        net.Listen,
		notifyContext: signal.NotifyContext,
		keaTransport:  http.DefaultTransport,
		adoptLogger:   slog.SetDefault,
		build:         buildSource{version: version, revision: revision, readInfo: debug.ReadBuildInfo},
		now:           time.Now,
		runtimeCollectors: []prometheus.Collector{
			collectors.NewGoCollector(),
			collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
		},
	}
}
