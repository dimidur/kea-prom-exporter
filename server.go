package main

// Serving /metrics and draining on a signal.
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

// buildServer returns the server: /metrics serves reg, and a failure to gather
// it, like the server's own errors, is logged to logger. Writing a response
// fails once writeTimeout has passed since its request was read, so a client
// that stops reading cannot hold the response, or the collection it shares,
// open for longer; it does not interrupt a gather in progress.
func buildServer(reg *prometheus.Registry, logger *slog.Logger, writeTimeout time.Duration) *http.Server {
	errorLog := slog.NewLogLogger(logger.Handler(), slog.LevelError)
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
		// Joined scrapers observe the same snapshot and timestamp, and a
		// collection stays shared until every response using it is written.
		// A client that stalls while reading would therefore hand later
		// scrapers a stale snapshot; writeTimeout bounds how long that can
		// last.
		//
		// client_golang marks the option experimental, so a bump of it is
		// checked against what is described here.
		CoalesceGather: true,
		// Surfaces the reason behind promhttp_metric_handler_errors_total,
		// which otherwise counts failures without ever saying why.
		ErrorLog: errorLog,
	}))
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintln(w, "kea-prom-exporter: see /metrics")
	})

	return &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: readHeaderTimeout,
		WriteTimeout:      writeTimeout,
		ErrorLog:          errorLog,
	}
}

// shutdownGrace is how long a shutdown waits for in-flight scrapes, given the
// longest a scrape can take. A grace shorter than the work it is waiting for
// cuts the response off anyway, which is the thing graceful shutdown exists to
// prevent; the extra second covers writing the response out.
func shutdownGrace(slowestScrape time.Duration) time.Duration {
	return slowestScrape + time.Second
}

// httpServer is what serveUntilSignal needs from the server.
type httpServer interface {
	Serve() error
	Shutdown(ctx context.Context) error
}

// listeningServer is an *http.Server and the listener it serves on.
type listeningServer struct {
	*http.Server
	listener net.Listener
}

// Serve serves on the server's own listener.
func (s listeningServer) Serve() error { return s.Server.Serve(s.listener) }

// service is the assembled exporter: a server ready to serve, how long a
// shutdown waits for it, and where both log.
type service struct {
	server httpServer
	grace  time.Duration
	log    *slog.Logger
}

// serviceFor serves reg on listener, logging to logger, for scrapes that take
// at most slowestScrape. A shutdown waits shutdownGrace for them, and a
// response may take the same span: the slowest scrape plus the time to write
// it out.
func serviceFor(reg *prometheus.Registry, listener net.Listener, logger *slog.Logger, slowestScrape time.Duration) service {
	grace := shutdownGrace(slowestScrape)
	return service{
		server: listeningServer{Server: buildServer(reg, logger, grace), listener: listener},
		grace:  grace,
		log:    logger,
	}
}

// serveUntilSignal serves until ctx is cancelled, then drains the server
// within the grace.
//
// releaseSignals restores default signal handling once shutdown has begun, so
// an impatient second SIGTERM kills the process immediately instead of being
// swallowed. It returns only after Serve has: net/http's Serve returns as soon
// as Shutdown closes the listener, before in-flight requests drain.
func (s service) serveUntilSignal(ctx context.Context, releaseSignals func()) error {
	serveErr := make(chan error)
	go func() {
		err := s.server.Serve()
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		}
		serveErr <- err
	}()

	select {
	case err := <-serveErr:
		return err
	case <-ctx.Done():
		releaseSignals()
		s.log.Info("shutting down", "grace", s.grace)
		shutdownCtx, cancel := context.WithTimeout(context.Background(), s.grace)
		defer cancel()
		shutdownErr := s.server.Shutdown(shutdownCtx)
		graceExpired, err := shutdownOutcome(<-serveErr, shutdownErr)
		if graceExpired {
			s.log.Warn("shutdown grace expired with requests still in flight", "grace", s.grace)
		}
		return err
	}
}

// shutdownOutcome is what a shutdown returns, given what Serve returned and
// what Shutdown did, and whether the grace expired.
//
// A server that failed as the signal arrived has still failed. The grace
// expiring is the designed outcome of a grace period, not a process failure:
// exiting non-zero then would make every `docker stop` of a busy exporter look
// like a crash.
func shutdownOutcome(served, shutdownErr error) (graceExpired bool, err error) {
	if served != nil {
		return false, served
	}
	if errors.Is(shutdownErr, context.DeadlineExceeded) {
		return true, nil
	}
	return false, shutdownErr
}
