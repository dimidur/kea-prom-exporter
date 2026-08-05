package main

import (
	"context"
	"io"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Tests for server.go: serving /metrics and draining on a signal.

func TestGraceIsActuallyWaitedOut(t *testing.T) {
	// The grace duration was untestable through the drain test, because
	// http.Server.Shutdown never aborts in-flight connections -- so replacing
	// grace with a nanosecond changed nothing observable. Timing the shutdown
	// is what distinguishes "waited" from "the parameter is ignored".

	// Arrange
	// Released via Cleanup, not after the wait: releasing afterwards means
	// a shutdown that ignores the grace hangs until the package timeout
	// instead of failing in milliseconds.
	release := make(chan struct{})
	var releaseOnce sync.Once
	t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })
	started := make(chan struct{})
	mux := http.NewServeMux()
	mux.HandleFunc("/hang", func(w http.ResponseWriter, _ *http.Request) {
		close(started)
		<-release
	})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	srv := &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: time.Second}

	const grace = 300 * time.Millisecond
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan time.Duration, 1)
	go func() {
		start := time.Now()
		_ = serveUntilSignal(ctx, srv, grace, nil)
		done <- time.Since(start)
	}()
	go func() {
		for range 50 {
			if resp, err := http.Get("http://" + addr + "/hang"); err == nil {
				defer resp.Body.Close()
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
	}()
	<-started

	// Act
	cancel()
	elapsed := <-done
	releaseOnce.Do(func() { close(release) })

	// Assert -- it must wait out the grace, and not much beyond it.
	if elapsed < grace {
		t.Errorf("shutdown took %v with a %v grace; the grace is being ignored", elapsed, grace)
	}
	if elapsed > grace+2*time.Second {
		t.Errorf("shutdown took %v with a %v grace; it is waiting far longer than asked", elapsed, grace)
	}
}

func TestSignalHandlingIsReleasedOnShutdown(t *testing.T) {
	// Restoring default signal handling is what makes an impatient second
	// SIGTERM kill the process instead of being swallowed. Every other test
	// passes nil for the callback, so dropping the call went unnoticed.

	// Arrange
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	srv := &http.Server{Addr: addr, ReadHeaderTimeout: time.Second}

	var released atomic.Bool
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = serveUntilSignal(ctx, srv, time.Second, func() { released.Store(true) })
	}()
	// Give the listener a moment to bind before asking it to stop.
	time.Sleep(50 * time.Millisecond)

	// Act
	cancel()
	<-done

	// Assert
	if !released.Load() {
		t.Error("shutdown did not restore default signal handling; a second SIGTERM would be swallowed")
	}
}

func TestServeUntilSignalDrainsInFlightRequests(t *testing.T) {
	// The point of the grace period: a request already being served must
	// finish, and shutting down must not be reported as a failure.
	// Arrange
	started := make(chan struct{})
	mux := http.NewServeMux()
	mux.HandleFunc("/slow", func(w http.ResponseWriter, _ *http.Request) {
		close(started)
		time.Sleep(150 * time.Millisecond)
		_, _ = w.Write([]byte("done"))
	})

	// Bind first so the test knows the port, then hand the server the address.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	srv := &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: time.Second}

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- serveUntilSignal(ctx, srv, 5*time.Second, nil) }()

	body := make(chan string, 1)
	go func() {
		// Retry briefly: serveUntilSignal binds asynchronously to this goroutine.
		for range 50 {
			resp, err := http.Get("http://" + addr + "/slow")
			if err != nil {
				time.Sleep(10 * time.Millisecond)
				continue
			}
			defer resp.Body.Close()
			b, _ := io.ReadAll(resp.Body)
			body <- string(b)
			return
		}
		body <- "never connected"
	}()

	// Act
	<-started
	cancel() // signal arrives mid-request

	// Assert
	if got := <-body; got != "done" {
		t.Errorf("in-flight request returned %q, want it to complete with \"done\"", got)
	}
	if err := <-done; err != nil {
		t.Errorf("serveUntilSignal returned %v, want nil on a clean shutdown", err)
	}
}

func TestServeUntilSignalGraceExpiryIsNotAFailure(t *testing.T) {
	// A grace shorter than the in-flight work is the designed outcome of a
	// grace period, not a process failure -- returning an error here made
	// every `docker stop` of a busy exporter exit non-zero and look like a
	// crash to Kubernetes and to alerting.
	// Arrange -- the handler is released via Cleanup, not after the wait:
	// releasing afterwards means a shutdown that ignores the grace hangs until
	// the package timeout instead of failing in milliseconds.
	mux := http.NewServeMux()
	release := make(chan struct{})
	var releaseOnce sync.Once
	t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })
	started := make(chan struct{})
	mux.HandleFunc("/hang", func(w http.ResponseWriter, _ *http.Request) {
		close(started)
		<-release
	})

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	srv := &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: time.Second}

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- serveUntilSignal(ctx, srv, 50*time.Millisecond, nil) }()

	go func() {
		for range 50 {
			if resp, err := http.Get("http://" + addr + "/hang"); err == nil {
				defer resp.Body.Close()
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
	}()
	// Act
	<-started
	cancel()

	// Assert
	if err := <-done; err != nil {
		t.Errorf("serveUntilSignal returned %v, want nil when only the grace expired", err)
	}
	releaseOnce.Do(func() { close(release) })
}

func TestServeUntilSignalReportsABindFailure(t *testing.T) {
	// A port already in use must surface as an error, not as a "listening"
	// line followed by silence.
	// Arrange
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()

	srv := &http.Server{Addr: ln.Addr().String(), ReadHeaderTimeout: time.Second}

	// Act + Assert
	if err := serveUntilSignal(t.Context(), srv, time.Second, nil); err == nil {
		t.Error("serveUntilSignal returned nil for an address already in use")
	}
}
