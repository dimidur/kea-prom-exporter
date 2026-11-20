package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// Tests for server.go: serving /metrics and draining on a signal.

func TestBuildServerServesTheRegistry(t *testing.T) {
	// /metrics is the whole product, and / is what a person opening the port
	// in a browser sees; it has to say where the metrics are. The handler
	// registers its own error counter on the registry it serves.

	// Arrange
	reg := prometheus.NewRegistry()
	reg.MustRegister(prometheus.NewCounter(prometheus.CounterOpts{Name: "marker_total", Help: "Marker."}))
	handler := buildServer(reg, quietLogger, time.Minute).Handler

	// Act
	metrics := serve(handler, "/metrics")
	root := serve(handler, "/")

	// Assert
	if metrics.Code != http.StatusOK || !strings.Contains(metrics.Body.String(), "marker_total 0") {
		t.Errorf("/metrics = %d %q, want 200 with the registry's metrics", metrics.Code, metrics.Body)
	}
	if !strings.Contains(metrics.Body.String(), "promhttp_metric_handler_errors_total") {
		t.Errorf("/metrics = %q, want the handler's error counter on the served registry", metrics.Body)
	}
	if root.Code != http.StatusOK || root.Body.String() != "kea-prom-exporter: see /metrics\n" {
		t.Errorf("/ = %d %q, want 200 pointing at /metrics", root.Code, root.Body)
	}
}

// stalledWriter is a ResponseWriter whose first Write announces itself on
// writing and then waits for release, holding its response open.
type stalledWriter struct {
	header           http.Header
	writing, release chan struct{}
	stalled          bool
}

// Header is the response's header.
func (w *stalledWriter) Header() http.Header { return w.header }

// WriteHeader accepts any status.
func (w *stalledWriter) WriteHeader(int) {}

// Write stalls on the first call as described on stalledWriter.
func (w *stalledWriter) Write(body []byte) (int, error) {
	if !w.stalled {
		w.stalled = true
		close(w.writing)
		<-w.release
	}
	return len(body), nil
}

func TestConcurrentScrapesShareOneCollection(t *testing.T) {
	// Every collection is a round of scrapeCommands to a daemon that is also
	// answering DHCP, so a scrape arriving while another is still in flight
	// must share its collection rather than drive a round of its own. A
	// response that is still being written keeps its collection shared, so
	// a second scrape arriving then needs no timing to overlap the first.

	// Arrange
	var collections atomic.Int32
	reg := prometheus.NewRegistry()
	reg.MustRegister(countingCollector{prometheus.NewDesc("counted", "Counted.", nil, nil), &collections})
	handler := buildServer(reg, quietLogger, time.Minute).Handler
	first := &stalledWriter{header: http.Header{}, writing: make(chan struct{}), release: make(chan struct{})}
	firstDone := make(chan struct{})
	go func() {
		defer close(firstDone)
		handler.ServeHTTP(first, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	}()
	receive(t, first.writing, "the first scrape to start writing")

	// Act
	second := serve(handler, "/metrics")
	close(first.release)
	receive(t, firstDone, "the first scrape to finish")

	// Assert
	if got := collections.Load(); got != 1 {
		t.Errorf("collections = %d for two overlapping scrapes, want 1 shared", got)
	}
	if second.Code != http.StatusOK {
		t.Errorf("second scrape = %d, want 200 from the shared collection", second.Code)
	}
}

func TestBuildServerBoundsItsConnectionsAndLogsItsErrors(t *testing.T) {
	// Without a header timeout a client can hold a connection open by never
	// finishing its headers. Without a write timeout a client that never
	// reads holds its response open, and with it the shared collection later
	// scrapers join. The server's own errors (failed accepts, handler panics,
	// TLS handshakes) must reach the configured logger and level.

	// Arrange
	logger, logs := capturingLogger(slog.LevelError)
	const writeTimeout = 7 * time.Second

	// Act
	server := buildServer(prometheus.NewRegistry(), logger, writeTimeout)
	server.ErrorLog.Print(serverErrorProbe)

	// Assert
	if server.ReadHeaderTimeout != 5*time.Second {
		t.Errorf("ReadHeaderTimeout = %v, want 5s", server.ReadHeaderTimeout)
	}
	if server.WriteTimeout != writeTimeout {
		t.Errorf("WriteTimeout = %v, want the %v given", server.WriteTimeout, writeTimeout)
	}
	assertServerErrorLogged(t, logs)
}

// failingCollector reports a collection error for its only metric.
type failingCollector struct{ desc *prometheus.Desc }

// Describe sends the collector's only descriptor.
func (f failingCollector) Describe(out chan<- *prometheus.Desc) { out <- f.desc }

// Collect sends a metric that fails to gather.
func (f failingCollector) Collect(out chan<- prometheus.Metric) {
	out <- prometheus.NewInvalidMetric(f.desc, errors.New("collector exploded"))
}

func TestBuildServerLogsWhyAScrapeFailed(t *testing.T) {
	// promhttp_metric_handler_errors_total counts failed scrapes but never
	// says why; the log is the only place the reason appears.

	// Arrange
	reg := prometheus.NewRegistry()
	reg.MustRegister(failingCollector{prometheus.NewDesc("failing", "Always fails.", nil, nil)})
	logger, logs := capturingLogger(slog.LevelInfo)
	handler := buildServer(reg, logger, time.Minute).Handler

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
	for _, testCase := range tests {
		t.Run(testCase.slowest.String(), func(t *testing.T) {
			// Arrange
			scrape := testCase.slowest

			// Act
			grace := shutdownGrace(scrape)

			// Assert
			if grace != testCase.want {
				t.Errorf("shutdownGrace(%v) = %v, want %v", scrape, grace, testCase.want)
			}
		})
	}
}

func TestServiceForBoundsResponsesByTheShutdownGrace(t *testing.T) {
	// A shutdown waits the slowest scrape plus a second, and a response may
	// take the same span: shorter cuts off a legitimate scrape, longer lets a
	// client that stops reading hold the shared collection open.

	// Arrange
	listener := newFakeListener()
	const slowestScrape = 6 * time.Second
	const wantSpan = 7 * time.Second

	// Act
	exporter := serviceFor(prometheus.NewRegistry(), listener, quietLogger, slowestScrape)

	// Assert
	served, ok := exporter.server.(listeningServer)
	if !ok || served.listener != listener {
		t.Fatalf("the service serves %#v, want an http.Server on the given listener", exporter.server)
	}
	if exporter.grace != wantSpan || served.WriteTimeout != wantSpan {
		t.Errorf("grace %v and write timeout %v, want both %v", exporter.grace, served.WriteTimeout, wantSpan)
	}
	if exporter.log != quietLogger {
		t.Error("the service does not log to the given logger")
	}
}

func TestServeUntilSignalReturnsWhatServeFailedWith(t *testing.T) {
	// A server that stops on its own before any signal is a failure the
	// process must exit on, except for the server having been closed, which is
	// how a shutdown ends.
	tests := []struct {
		name        string
		serveResult error
		want        error
	}{
		{"a serve error is returned", errAcceptFailed, errAcceptFailed},
		{"a closed server is not an error", http.ErrServerClosed, nil},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			// Arrange
			srv := newFakeServer()
			srv.serveResult = testCase.serveResult
			exporter := service{server: srv, grace: time.Second, log: quietLogger}

			// Act
			err := exporter.serveUntilSignal(t.Context(), func() {})

			// Assert
			if !errors.Is(err, testCase.want) {
				t.Errorf("serveUntilSignal = %v, want %v", err, testCase.want)
			}
			if srv.shutdownCalled {
				t.Error("Shutdown was called although no signal arrived")
			}
		})
	}
}

func TestServeUntilSignalShutsDownWithinTheGrace(t *testing.T) {
	// On a signal, default signal handling is restored so an impatient second
	// SIGTERM kills the process, and the server gets the grace from now to
	// drain, on a context of its own rather than the cancelled one. Serve has
	// returned by the time serveUntilSignal does, so nothing is left running.

	// Arrange
	srv := newFakeServer()
	logger, logs := capturingLogger(slog.LevelInfo)
	const grace = time.Minute
	exporter := service{server: srv, grace: grace, log: logger}
	ctx := signalledContext(t)
	var released bool
	before := time.Now()

	// Act
	err := exporter.serveUntilSignal(ctx, func() { released = true })
	after := time.Now()

	// Assert
	if err != nil {
		t.Errorf("serveUntilSignal = %v, want nil on a clean shutdown", err)
	}
	if !released {
		t.Error("default signal handling was not restored; a second SIGTERM would be swallowed")
	}
	if !srv.shutdownHadDeadline || srv.shutdownCtxErr != nil {
		t.Fatalf("Shutdown context had deadline %v and error %v, want a live context with a deadline",
			srv.shutdownHadDeadline, srv.shutdownCtxErr)
	}
	if srv.shutdownDeadline.Before(before.Add(grace)) || srv.shutdownDeadline.After(after.Add(grace)) {
		t.Errorf("Shutdown deadline %v is not the grace %v from the signal", srv.shutdownDeadline, grace)
	}
	if !strings.Contains(logs.String(), `level=INFO msg="shutting down" grace=1m0s`) {
		t.Errorf("log = %q, want the shutdown and its grace on the given logger", logs.String())
	}
	if !srv.serveReturned {
		t.Error("serveUntilSignal returned while Serve was still running")
	}
}

func TestServeUntilSignalReturnsAServeFailureThatMetTheSignal(t *testing.T) {
	// A server that fails as the signal arrives has still failed; the
	// shutdown must not swallow it and let the process exit 0. The fake fails
	// only once shutdown begins, so the signal is always seen first.

	// Arrange
	srv := newFakeServer()
	srv.serveResultAtShutdown = errAcceptFailed
	exporter := service{server: srv, grace: time.Second, log: quietLogger}
	ctx := signalledContext(t)

	// Act
	err := exporter.serveUntilSignal(ctx, func() {})

	// Assert
	if !errors.Is(err, errAcceptFailed) {
		t.Errorf("serveUntilSignal = %v, want %v", err, errAcceptFailed)
	}
}

func TestShutdownOutcome(t *testing.T) {
	// A server that failed as the signal arrived has still failed, whatever
	// the shutdown did. The grace expiring is the designed outcome of a grace
	// period, so it is reported as expired rather than as a failure, and any
	// other shutdown failure is returned.
	tests := []struct {
		name             string
		served, shutdown error
		wantExpired      bool
		want             error
	}{
		{"a clean shutdown", nil, nil, false, nil},
		{"the grace expiring", nil, context.DeadlineExceeded, true, nil},
		{"any other shutdown failure", nil, errShutdownFailed, false, errShutdownFailed},
		{"a serve failure", errAcceptFailed, nil, false, errAcceptFailed},
		{"a serve failure as the grace expires", errAcceptFailed, context.DeadlineExceeded, false, errAcceptFailed},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			// Arrange
			served, shutdown := testCase.served, testCase.shutdown

			// Act
			expired, err := shutdownOutcome(served, shutdown)

			// Assert
			if expired != testCase.wantExpired || !errors.Is(err, testCase.want) {
				t.Errorf("shutdownOutcome(%v, %v) = %v, %v; want %v, %v", served, shutdown, expired, err, testCase.wantExpired, testCase.want)
			}
		})
	}
}

func TestServeUntilSignalShutdownOutcome(t *testing.T) {
	// The grace expiring is the designed outcome of a grace period, not a
	// process failure: an error there would make every `docker stop` of a busy
	// exporter exit non-zero and look like a crash. It is still worth a
	// warning. Any other shutdown failure is returned.
	tests := []struct {
		name           string
		shutdownResult error
		want           error
		wantWarning    bool
	}{
		{"a clean shutdown", nil, nil, false},
		{"the grace expiring", context.DeadlineExceeded, nil, true},
		{"any other failure", errShutdownFailed, errShutdownFailed, false},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			// Arrange
			srv := newFakeServer()
			srv.shutdownResult = testCase.shutdownResult
			logger, logs := capturingLogger(slog.LevelInfo)
			exporter := service{server: srv, grace: time.Second, log: logger}
			ctx := signalledContext(t)

			// Act
			err := exporter.serveUntilSignal(ctx, func() {})

			// Assert
			if !errors.Is(err, testCase.want) {
				t.Errorf("serveUntilSignal = %v, want %v", err, testCase.want)
			}
			warned := strings.Contains(logs.String(), `level=WARN msg="shutdown grace expired with requests still in flight" grace=1s`)
			if warned != testCase.wantWarning {
				t.Errorf("grace-expired warning logged = %v, want %v; log = %q", warned, testCase.wantWarning, logs.String())
			}
		})
	}
}
