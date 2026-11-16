package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// Tests for server.go: serving /metrics and draining on a signal.

// testClient bounds every request a test makes, so a handler that never
// answers fails the test instead of hanging it.
var testClient = &http.Client{Timeout: 5 * time.Second}

// serve runs one request through handler and returns the recorded response.
func serve(handler http.Handler, path string) *httptest.ResponseRecorder {
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, path, nil))
	return recorder
}

// freeAddr returns a loopback address that nothing is listening on, for a
// server under test to bind itself.
func freeAddr(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := listener.Addr().String()
	_ = listener.Close()
	return addr
}

// releaseOnCleanup returns a channel for a handler to block on and a function
// that closes it. Cleanup closes it as well, so a failing test does not leave
// the handler and its server blocked behind it.
func releaseOnCleanup(t *testing.T) (<-chan struct{}, func()) {
	t.Helper()
	release := make(chan struct{})
	var once sync.Once
	closeRelease := func() { once.Do(func() { close(release) }) }
	t.Cleanup(closeRelease)
	return release, closeRelease
}

// holdRequest GETs path from addr in the background, retrying until the
// server under test has bound. The request stays in flight for as long as the
// handler holds it.
func holdRequest(addr, path string) {
	go func() {
		for range 50 {
			if resp, err := testClient.Get("http://" + addr + path); err == nil {
				defer resp.Body.Close()
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
	}()
}

func TestBuildServerServesTheRegistry(t *testing.T) {
	// /metrics is the whole product, and / is what a person opening the port
	// in a browser sees; it has to say where the metrics are.

	// Arrange
	reg := prometheus.NewRegistry()
	reg.MustRegister(prometheus.NewCounter(prometheus.CounterOpts{Name: "marker_total", Help: "Marker."}))
	handler := buildServer("127.0.0.1:0", reg, quietLogger).Handler

	// Act
	metrics := serve(handler, "/metrics")
	root := serve(handler, "/")

	// Assert
	if metrics.Code != http.StatusOK || !strings.Contains(metrics.Body.String(), "marker_total 0") {
		t.Errorf("/metrics = %d %q, want 200 with the registry's metrics", metrics.Code, metrics.Body)
	}
	if root.Code != http.StatusOK || root.Body.String() != "kea-prom-exporter: see /metrics\n" {
		t.Errorf("/ = %d %q, want 200 pointing at /metrics", root.Code, root.Body)
	}
}

func TestBuildServerListensWhereToldAndBoundsHeaderReads(t *testing.T) {
	// --listen must reach the server unchanged, and without a header timeout
	// a client can hold a connection open by never finishing its headers.

	// Arrange
	const addr = "127.0.0.1:9547"

	// Act
	server := buildServer(addr, prometheus.NewRegistry(), quietLogger)

	// Assert
	if server.Addr != addr {
		t.Errorf("Addr = %q, want %q", server.Addr, addr)
	}
	if server.ReadHeaderTimeout != 5*time.Second {
		t.Errorf("ReadHeaderTimeout = %v, want 5s", server.ReadHeaderTimeout)
	}
}

// blockingCollector counts its collections and holds each one until release
// is closed.
type blockingCollector struct {
	desc    *prometheus.Desc
	calls   atomic.Int32
	entered chan struct{}
	release <-chan struct{}
}

func (b *blockingCollector) Describe(ch chan<- *prometheus.Desc) { ch <- b.desc }

func (b *blockingCollector) Collect(ch chan<- prometheus.Metric) {
	if b.calls.Add(1) == 1 {
		close(b.entered)
	}
	<-b.release
	ch <- prometheus.MustNewConstMetric(b.desc, prometheus.GaugeValue, 1)
}

func TestBuildServerSharesOneCollectionBetweenConcurrentScrapes(t *testing.T) {
	// Every collection is a round of scrapeCommands to a daemon that is also
	// answering DHCP, so overlapping scrapers must share one collection
	// rather than each driving their own.

	// Arrange
	collector := &blockingCollector{
		desc:    prometheus.NewDesc("blocking", "Held until released.", nil, nil),
		entered: make(chan struct{}),
	}
	reg := prometheus.NewRegistry()
	reg.MustRegister(collector)
	handler := buildServer("127.0.0.1:0", reg, quietLogger).Handler
	var release func()
	collector.release, release = releaseOnCleanup(t)
	arrived := make(chan struct{}, 2)
	bodies := make(chan string, 2)
	scrape := func() {
		arrived <- struct{}{}
		bodies <- serve(handler, "/metrics").Body.String()
	}

	// Act
	go scrape()
	receive(t, arrived, "the first scrape to start")
	receive(t, collector.entered, "the first collection to start")
	go scrape()
	receive(t, arrived, "the second scrape to start")
	// The second request is entering the handler but has not necessarily
	// joined the collection yet. If it has not by the time the first is
	// released, it starts a second collection and the test fails; it cannot
	// pass wrongly.
	time.Sleep(100 * time.Millisecond)
	release()
	first := receive(t, bodies, "the first scrape to finish")
	second := receive(t, bodies, "the second scrape to finish")

	// Assert
	for _, body := range []string{first, second} {
		if !strings.Contains(body, "blocking 1") {
			t.Errorf("scrape returned %q, want the shared collection's metrics", body)
		}
	}
	if got := collector.calls.Load(); got != 1 {
		t.Errorf("collector ran %d times for two concurrent scrapes, want 1", got)
	}
}

// failingCollector reports a collection error for its only metric.
type failingCollector struct{ desc *prometheus.Desc }

func (f failingCollector) Describe(ch chan<- *prometheus.Desc) { ch <- f.desc }

func (f failingCollector) Collect(ch chan<- prometheus.Metric) {
	ch <- prometheus.NewInvalidMetric(f.desc, errors.New("collector exploded"))
}

func TestBuildServerLogsWhyAScrapeFailed(t *testing.T) {
	// promhttp_metric_handler_errors_total counts failed scrapes but never
	// says why; the log is the only place the reason appears.

	// Arrange
	reg := prometheus.NewRegistry()
	reg.MustRegister(failingCollector{prometheus.NewDesc("failing", "Always fails.", nil, nil)})
	logger, logs := capturingLogger(slog.LevelInfo)
	handler := buildServer("127.0.0.1:0", reg, logger).Handler

	// Act
	response := serve(handler, "/metrics")

	// Assert
	if response.Code != http.StatusInternalServerError {
		t.Errorf("/metrics = %d, want 500 when gathering fails", response.Code)
	}
	if got := logs.String(); !strings.Contains(got, "collector exploded") || !strings.Contains(got, "level=ERROR") {
		t.Errorf("log = %q, want the gather error at level ERROR", got)
	}
}

func TestShutdownGraceOutlastsTheSlowestScrape(t *testing.T) {
	// A grace shorter than the slowest scrape cuts off the very response that
	// graceful shutdown exists to deliver; the extra second is for writing the
	// response out.
	tests := []struct {
		slowest time.Duration
		want    time.Duration
	}{
		{0, time.Second},
		{time.Second, 2 * time.Second},
		{10 * time.Second, 11 * time.Second},
		{time.Minute, 61 * time.Second},
	}
	for _, tc := range tests {
		t.Run(tc.slowest.String(), func(t *testing.T) {
			// Arrange
			scrape := tc.slowest

			// Act
			grace := shutdownGrace(scrape)

			// Assert
			if grace != tc.want {
				t.Errorf("shutdownGrace(%v) = %v, want %v", scrape, grace, tc.want)
			}
		})
	}
}

func TestGraceIsActuallyWaitedOut(t *testing.T) {
	// http.Server.Shutdown never aborts in-flight connections, so the drain
	// test cannot tell a grace of five seconds from one of a nanosecond.
	// Timing the shutdown is what distinguishes "waited" from "the parameter
	// is ignored".

	// Arrange
	release, _ := releaseOnCleanup(t)
	started := make(chan struct{})
	mux := http.NewServeMux()
	mux.HandleFunc("/hang", func(_ http.ResponseWriter, _ *http.Request) {
		close(started)
		<-release
	})
	srv := &http.Server{Addr: freeAddr(t), Handler: mux, ReadHeaderTimeout: time.Second}
	const grace = 300 * time.Millisecond
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan time.Duration, 1)
	go func() {
		start := time.Now()
		_ = serveUntilSignal(ctx, srv, grace, quietLogger, nil)
		done <- time.Since(start)
	}()
	holdRequest(srv.Addr, "/hang")
	receive(t, started, "the request to reach the handler")

	// Act
	cancel()
	elapsed := receive(t, done, "the shutdown to finish")

	// Assert
	// It must wait out the grace, and not much beyond it.
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
	// passes nil for the callback, so this is the only place it is observed.

	// Arrange
	srv := &http.Server{Addr: freeAddr(t), ReadHeaderTimeout: time.Second}
	var released atomic.Bool
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = serveUntilSignal(ctx, srv, time.Second, quietLogger, func() { released.Store(true) })
	}()

	// Act
	cancel()
	receive(t, done, "the shutdown to finish")

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
	mux.HandleFunc("/slow", func(writer http.ResponseWriter, _ *http.Request) {
		close(started)
		time.Sleep(150 * time.Millisecond)
		_, _ = writer.Write([]byte("done"))
	})
	srv := &http.Server{Addr: freeAddr(t), Handler: mux, ReadHeaderTimeout: time.Second}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- serveUntilSignal(ctx, srv, 5*time.Second, quietLogger, nil) }()
	received := make(chan string, 1)
	go func() {
		// Retry briefly: the server binds in the goroutine above.
		for range 50 {
			resp, err := testClient.Get("http://" + srv.Addr + "/slow")
			if err != nil {
				time.Sleep(10 * time.Millisecond)
				continue
			}
			defer resp.Body.Close()
			body, err := io.ReadAll(resp.Body)
			if err != nil {
				received <- "read failed: " + err.Error()
				return
			}
			received <- string(body)
			return
		}
		received <- "never connected"
	}()

	// Act
	receive(t, started, "the request to reach the handler")
	cancel() // signal arrives mid-request
	got := receive(t, received, "the in-flight response")
	err := receive(t, done, "the shutdown to finish")

	// Assert
	if got != "done" {
		t.Errorf("in-flight request returned %q, want it to complete with \"done\"", got)
	}
	if err != nil {
		t.Errorf("serveUntilSignal returned %v, want nil on a clean shutdown", err)
	}
}

func TestServeUntilSignalGraceExpiryIsNotAFailure(t *testing.T) {
	// A grace shorter than the in-flight work is the designed outcome of a
	// grace period, not a process failure: an error here would make every
	// `docker stop` of a busy exporter exit non-zero and look like a crash to
	// Kubernetes and to alerting. The expiry is still worth a warning, and it
	// and the other lifecycle lines go to the logger the exporter configured
	// rather than the global default.

	// Arrange
	logger, logs := capturingLogger(slog.LevelInfo)
	release, _ := releaseOnCleanup(t)
	started := make(chan struct{})
	mux := http.NewServeMux()
	mux.HandleFunc("/hang", func(_ http.ResponseWriter, _ *http.Request) {
		close(started)
		<-release
	})
	srv := &http.Server{Addr: freeAddr(t), Handler: mux, ReadHeaderTimeout: time.Second}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- serveUntilSignal(ctx, srv, 50*time.Millisecond, logger, nil) }()
	holdRequest(srv.Addr, "/hang")
	receive(t, started, "the request to reach the handler")

	// Act
	cancel()
	err := receive(t, done, "the shutdown to finish")

	// Assert
	if err != nil {
		t.Errorf("serveUntilSignal returned %v, want nil when only the grace expired", err)
	}
	for _, line := range []string{
		`level=INFO msg=listening`,
		`level=INFO msg="shutting down"`,
		`level=WARN msg="shutdown grace expired`,
	} {
		if !strings.Contains(logs.String(), line) {
			t.Errorf("log = %q, want %s on the given logger", logs.String(), line)
		}
	}
}

func TestServeUntilSignalReportsABindFailure(t *testing.T) {
	// A port already in use must surface as an error, not as a "listening"
	// line followed by silence.

	// Arrange
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer listener.Close()
	srv := &http.Server{Addr: listener.Addr().String(), ReadHeaderTimeout: time.Second}
	logger, logs := capturingLogger(slog.LevelInfo)

	// Act
	err = serveUntilSignal(t.Context(), srv, time.Second, logger, nil)

	// Assert
	if err == nil {
		t.Error("serveUntilSignal returned nil for an address already in use")
	}
	if strings.Contains(logs.String(), "msg=listening") {
		t.Errorf("log = %q, want no listening line for a bind that failed", logs.String())
	}
}
