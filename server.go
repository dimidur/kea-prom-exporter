package main

// Process lifecycle: serving /metrics and draining on a signal.
//
// One reason to change: how the exporter serves and shuts down.

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// readHeaderTimeout bounds how long a client may take to send its request
// headers. Without it a connection that never finishes them is held open
// indefinitely; a scraper sends its headers at once, so seconds are ample.
const readHeaderTimeout = 5 * time.Second

// buildServer returns the server for addr: /metrics serves reg, and a failure
// to gather it is logged to logger.
func buildServer(addr string, reg *prometheus.Registry, logger *slog.Logger) *http.Server {
	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.HandlerFor(reg, promhttp.HandlerOpts{
		Registry: reg,
		// Overlapping scrapes (two Prometheus servers, a readiness probe, a
		// human with curl) would each drive their own round of scrapeCommands
		// at a daemon that is also answering DHCP. CoalesceGather makes
		// concurrent scrapes share one collection instead, so the socket sees
		// one round per cycle no matter how many scrapers there are.
		// MaxRequestsInFlight is the wrong instrument here: it counts /metrics
		// requests rather than Kea requests, and it answers the excess with
		// 503, which reaches Prometheus as up=0, indistinguishable from Kea
		// actually being down, and drops the per-command diagnostics with it.
		//
		// Joined scrapers observe the same snapshot and timestamp. At a scrape
		// interval measured in tens of seconds that is not observable.
		CoalesceGather: true,
		// Surfaces the reason behind promhttp_metric_handler_errors_total,
		// which otherwise counts failures without ever saying why.
		ErrorLog: slog.NewLogLogger(logger.Handler(), slog.LevelError),
	}))
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintln(w, "kea-prom-exporter: see /metrics")
	})

	return &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: readHeaderTimeout,
	}
}

// shutdownGrace is how long a shutdown waits for in-flight scrapes, given the
// longest a scrape can take. A grace shorter than the work it is waiting for
// cuts the response off anyway, which is the thing graceful shutdown exists to
// prevent; the extra second covers writing the response out.
func shutdownGrace(slowestScrape time.Duration) time.Duration {
	return slowestScrape + time.Second
}

// serveUntilSignal runs srv until ctx is cancelled, then drains it within
// grace.
//
// releaseSignals restores default signal handling once shutdown has begun, so
// an impatient second SIGTERM kills the process immediately instead of being
// swallowed. It may be nil.
func serveUntilSignal(ctx context.Context, srv *http.Server, grace time.Duration, logger *slog.Logger, releaseSignals func()) error {
	// Listen before announcing: srv.ListenAndServe binds inside the goroutine,
	// so logging there prints a success line even when the bind fails.
	listener, err := net.Listen("tcp", srv.Addr)
	if err != nil {
		return err
	}
	logger.Info("listening", "addr", listener.Addr().String())

	serveErr := make(chan error, 1)
	go func() {
		err := srv.Serve(listener)
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		}
		serveErr <- err
	}()

	select {
	case err := <-serveErr:
		return err
	case <-ctx.Done():
		if releaseSignals != nil {
			releaseSignals()
		}
		logger.Info("shutting down", "grace", grace)
		shutdownCtx, cancel := context.WithTimeout(context.Background(), grace)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			// The grace expiring is the designed outcome of a grace period,
			// not a process failure: exiting non-zero here would make every
			// `docker stop` of a busy exporter look like a crash.
			if errors.Is(err, context.DeadlineExceeded) {
				logger.Warn("shutdown grace expired with requests still in flight", "grace", grace)
				return nil
			}
			return err
		}
		return nil
	}
}
