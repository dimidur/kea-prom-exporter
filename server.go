package main

// Process lifecycle: serving /metrics and draining on a signal.
//
// One reason to change: how the exporter serves and shuts down.

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"time"
)

// serveUntilSignal runs srv until ctx is cancelled, then drains it within
// grace. Split out of main so the shutdown path is reachable from a test.
//
// releaseSignals restores default signal handling once shutdown has begun, so
// an impatient second SIGTERM kills the process immediately instead of being
// swallowed. It may be nil.
func serveUntilSignal(ctx context.Context, srv *http.Server, grace time.Duration, releaseSignals func()) error {
	// Listen before announcing: srv.ListenAndServe binds inside the goroutine,
	// so logging there prints a success line even when the bind fails.
	ln, err := net.Listen("tcp", srv.Addr)
	if err != nil {
		return err
	}
	slog.Info("listening", "addr", ln.Addr().String())

	serveErr := make(chan error, 1)
	go func() {
		err := srv.Serve(ln)
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
		slog.Info("shutting down", "grace", grace)
		shutdownCtx, cancel := context.WithTimeout(context.Background(), grace)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			// The grace expiring is the designed outcome of a grace period,
			// not a process failure: exiting non-zero here would make every
			// `docker stop` of a busy exporter look like a crash.
			if errors.Is(err, context.DeadlineExceeded) {
				slog.Warn("shutdown grace expired with requests still in flight", "grace", grace)
				return nil
			}
			return err
		}
		return nil
	}
}
