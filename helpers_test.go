package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"runtime/debug"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
)

// Helpers the whole test suite shares.
//
// The fixtures in testdata/ are real responses captured from a live Kea
// 3.2.0 control socket, with hostnames replaced by neutral peer names.
// Testing against captured output rather than hand-written JSON is the
// point: it pins the exporter to the shape Kea actually emits, including
// the per-pool statistic dimension that only appears on 3.x.

// fixture returns the contents of testdata/name, failing the test if it
// cannot be read.
func fixture(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	return data
}

// fixtureOrNone returns the contents of testdata/name, or nil when name is
// empty: the reply a fakeKea fails its command with.
func fixtureOrNone(t *testing.T, name string) []byte {
	t.Helper()
	if name == "" {
		return nil
	}
	return fixture(t, name)
}

// fixtureStatistics decodes a captured statistic-get-all fixture to its
// statistics, keyed by name, each decoded as Statistic: json.RawMessage for
// Kea's raw sample encoding, [][]json.RawMessage for its samples, newest
// first, each a value and a timestamp.
func fixtureStatistics[Statistic any](t *testing.T, name string) map[string]Statistic {
	t.Helper()
	var reply []struct {
		Arguments map[string]Statistic `json:"arguments"`
	}
	if err := json.Unmarshal(fixture(t, name), &reply); err != nil {
		t.Fatalf("decode fixture %s: %v", name, err)
	}
	if len(reply) == 0 {
		t.Fatalf("fixture %s holds no reply", name)
	}
	return reply[0].Arguments
}

// fakeListener is a net.Listener with no socket behind it. Accept fails with
// acceptErr when that is set, and otherwise hands out the connections dial
// makes until Close.
type fakeListener struct {
	conns     chan net.Conn
	closed    chan struct{}
	once      sync.Once
	acceptErr error
}

// newFakeListener returns an open fakeListener.
func newFakeListener() *fakeListener {
	return &fakeListener{conns: make(chan net.Conn), closed: make(chan struct{})}
}

// dial connects to whatever is accepting on l, over an in-memory pipe, and
// returns the client's end.
func (l *fakeListener) dial(t *testing.T) net.Conn {
	t.Helper()
	client, server := net.Pipe()
	t.Cleanup(func() { _ = client.Close() })
	select {
	case l.conns <- server:
		return client
	case <-time.After(testWait):
		t.Fatal("timed out waiting for the server to accept")
		return nil
	}
}

// Accept returns acceptErr when that is set, and otherwise the next
// connection dial makes, or net.ErrClosed once the listener is closed.
func (l *fakeListener) Accept() (net.Conn, error) {
	if l.acceptErr != nil {
		return nil, l.acceptErr
	}
	select {
	case conn := <-l.conns:
		return conn, nil
	case <-l.closed:
		return nil, net.ErrClosed
	}
}

// Close stops Accept handing out connections. It is safe to call more than
// once.
func (l *fakeListener) Close() error {
	l.once.Do(func() { close(l.closed) })
	return nil
}

// Addr is the address the listener is bound to: 127.0.0.1:9547, whatever
// address it was asked for.
func (l *fakeListener) Addr() net.Addr {
	return &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 9547}
}

// trackedBody is a response body that records whether it was closed and how
// much of it was still unread at that moment. A read after Close is what a
// real response body refuses, so only bytes consumed before Close count.
type trackedBody struct {
	*strings.Reader
	closed        bool
	unreadAtClose int
}

// newTrackedBody returns an unclosed trackedBody holding content.
func newTrackedBody(content string) *trackedBody {
	return &trackedBody{Reader: strings.NewReader(content)}
}

// Close records how much of the body was still unread the first time.
func (b *trackedBody) Close() error {
	if !b.closed {
		b.unreadAtClose = b.Len()
	}
	b.closed = true
	return nil
}

// stubTransport stands in for the network under an http.Client. A round trip
// shows the request to observe when that is set, then fails with the request
// context's error once that context is done, as a real transport does, and
// with err when it is set; otherwise it answers with status and body.
type stubTransport struct {
	status  int
	body    *trackedBody
	err     error
	observe func(req *http.Request)
}

// RoundTrip answers req as described on stubTransport.
func (s stubTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if s.observe != nil {
		s.observe(req)
	}
	if err := req.Context().Err(); err != nil {
		return nil, err
	}
	if s.err != nil {
		return nil, s.err
	}
	return &http.Response{
		StatusCode: s.status,
		Header:     http.Header{},
		Body:       s.body,
		Request:    req,
	}, nil
}

// fakeServer is an httpServer with no socket behind it. Serve returns
// serveResult at once when that is set, and otherwise blocks until Shutdown,
// then returns serveResultAtShutdown, or http.ErrServerClosed when that is
// nil; either way it records that it returned. Shutdown returns
// shutdownResult and records the context it was given.
type fakeServer struct {
	serveResult           error
	serveResultAtShutdown error
	shutdownResult        error
	stopped               chan struct{}

	serveReturned       bool
	shutdownCalled      bool
	shutdownDeadline    time.Time
	shutdownHadDeadline bool
	shutdownCtxErr      error
}

// newFakeServer returns a fakeServer that serves until Shutdown and shuts
// down cleanly.
func newFakeServer() *fakeServer { return &fakeServer{stopped: make(chan struct{})} }

// Serve returns as described on fakeServer.
func (s *fakeServer) Serve() error {
	defer func() { s.serveReturned = true }()
	if s.serveResult != nil {
		return s.serveResult
	}
	<-s.stopped
	if s.serveResultAtShutdown != nil {
		return s.serveResultAtShutdown
	}
	return http.ErrServerClosed
}

// Shutdown records ctx, ends Serve and returns shutdownResult.
func (s *fakeServer) Shutdown(ctx context.Context) error {
	s.shutdownCalled = true
	s.shutdownDeadline, s.shutdownHadDeadline = ctx.Deadline()
	s.shutdownCtxErr = ctx.Err()
	close(s.stopped)
	return s.shutdownResult
}

// Errors the fakes fail with: a server's listener, a shutdown, and a Kea
// that is not there.
var (
	errAcceptFailed   = errors.New("accept: too many open files")
	errShutdownFailed = errors.New("close listener: bad file descriptor")
	errNoKea          = errors.New("no Kea in this test")
)

// signalledContext returns a context whose shutdown signal has already
// arrived.
func signalledContext(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	return ctx
}

// serverErrorProbe is a message written to a server's ErrorLog to find out
// where the server's own errors are logged.
const serverErrorProbe = "accept failed"

// assertServerErrorLogged fails the test unless logs holds serverErrorProbe at
// level ERROR.
func assertServerErrorLogged(t *testing.T, logs *bytes.Buffer) {
	t.Helper()
	if !strings.Contains(logs.String(), `level=ERROR msg="`+serverErrorProbe) {
		t.Errorf("log = %q, want the server's error at level ERROR on the given logger", logs.String())
	}
}

// envOf is a lookupEnv that serves variables from a map.
func envOf(vars map[string]string) func(string) (string, bool) {
	return func(name string) (string, bool) {
		value, ok := vars[name]
		return value, ok
	}
}

// filesOf is a readFile that serves files from a map, and reports any other
// name as not existing.
func filesOf(files map[string]string) func(string) ([]byte, error) {
	return func(name string) ([]byte, error) {
		contents, ok := files[name]
		if !ok {
			return nil, fmt.Errorf("open %s: %w", name, fs.ErrNotExist)
		}
		return []byte(contents), nil
	}
}

// sentinelBuildInfo is build info Go could have stamped into the binary.
func sentinelBuildInfo() (*debug.BuildInfo, bool) {
	return &debug.BuildInfo{
		GoVersion: "go0.0.0-sentinel",
		Settings:  []debug.BuildSetting{{Key: "vcs.revision", Value: "abc123"}},
	}, true
}

// countingCollector counts its collections.
type countingCollector struct {
	desc        *prometheus.Desc
	collections *atomic.Int32
}

// Describe sends the collector's only descriptor.
func (c countingCollector) Describe(out chan<- *prometheus.Desc) { out <- c.desc }

// Collect counts the collection and sends one sample.
func (c countingCollector) Collect(out chan<- prometheus.Metric) {
	c.collections.Add(1)
	out <- prometheus.MustNewConstMetric(c.desc, prometheus.GaugeValue, 1)
}

// runtimeProbe stands in for a runtime collector: wherever it is registered,
// /metrics carries the sample runtimeProbeSample.
func runtimeProbe() prometheus.Collector {
	return countingCollector{prometheus.NewDesc("runtime_probe", "Stands in for a runtime collector.", nil, nil), &atomic.Int32{}}
}

// runtimeProbeSample is the line runtimeProbe adds to /metrics.
const runtimeProbeSample = "runtime_probe 1"

// assertAlreadyRegistered fails the test unless reg refuses second because an
// equal collector is already registered, which is how a test sees that one
// is.
func assertAlreadyRegistered(t *testing.T, reg prometheus.Registerer, name string, second prometheus.Collector) {
	t.Helper()
	var already prometheus.AlreadyRegisteredError
	if err := reg.Register(second); !errors.As(err, &already) {
		t.Errorf("registering a second %s collector: err = %v, want AlreadyRegisteredError", name, err)
	}
}

// testBuild is the build identity for tests that do not look at it.
var testBuild = buildID{version: "test", revision: "test", goVersion: "go-test"}

// stoppedClock is a clock that never moves, for tests that do not time
// anything.
func stoppedClock() time.Time { return time.Time{} }

// testReport is how a collector under test reports on itself: to logger, as
// testBuild, by a clock that never moves.
func testReport(logger *slog.Logger) selfReport {
	return selfReport{log: logger, build: testBuild, now: stoppedClock}
}

// testClock is a clock that moves only when told to.
type testClock struct{ current time.Time }

// now is the clock's current time.
func (c *testClock) now() time.Time { return c.current }

// advance moves the clock on by step.
func (c *testClock) advance(step time.Duration) { c.current = c.current.Add(step) }

// quietLogger discards everything, for tests that do not look at the log.
var quietLogger = slog.New(slog.DiscardHandler)

// capturingLogger returns a text logger that records records at level and
// above, and the buffer it writes them to.
func capturingLogger(level slog.Level) (*slog.Logger, *bytes.Buffer) {
	var logs bytes.Buffer
	return slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: level})), &logs
}

// healthyKea answers each command fakeKea knows with its captured healthy
// reply.
func healthyKea(t *testing.T) *fakeKea {
	t.Helper()
	return newFakeKea(fixture(t, "statistic-get-all.json"), fixture(t, "status-get.json"))
}

// recordedCall is one command a fakeKea received, with what its context held
// when it arrived.
type recordedCall struct {
	command     string
	hadDeadline bool
	ctxErr      error
}

// fakeKea stands in for the Kea client, so the collector's logic is tested
// without a server. statistic-get-all is answered with statistics and
// status-get with status, each read by interpretResponse as the body of an
// HTTP 200, as keaClient.call reads Kea's reply. A nil reply fails that
// command, and so do any other command and a done context. Every call is
// recorded with what its context held, and passed to onCall when that is
// set; Collect may run concurrently, so the record is locked.
type fakeKea struct {
	statistics, status []byte
	onCall             func(command string)

	mu    sync.Mutex
	calls []recordedCall
}

// newFakeKea returns a fakeKea answering statistic-get-all with statistics
// and status-get with status.
func newFakeKea(statistics, status []byte) *fakeKea {
	return &fakeKea{statistics: statistics, status: status}
}

// call records the command and answers it as described on fakeKea.
func (f *fakeKea) call(ctx context.Context, command string) (*keaResponse, error) {
	_, hadDeadline := ctx.Deadline()
	f.mu.Lock()
	f.calls = append(f.calls, recordedCall{command: command, hadDeadline: hadDeadline, ctxErr: ctx.Err()})
	f.mu.Unlock()
	if f.onCall != nil {
		f.onCall(command)
	}

	// A done context fails the call, as it does for keaClient.call.
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("kea call %q: %w", command, err)
	}

	var reply []byte
	switch command {
	case cmdStatisticGetAll:
		reply = f.statistics
	case cmdStatusGet:
		reply = f.status
	}
	if reply == nil {
		return nil, noReply(command)
	}
	return interpretResponse(command, http.StatusOK, bytes.NewReader(reply))
}

// noReply is the error fakeKea fails command with when it has no reply for
// it.
func noReply(command string) error {
	return fmt.Errorf("kea call %q: no reply configured", command)
}

// recorded returns the calls received so far.
func (f *fakeKea) recorded() []recordedCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.calls)
}

// loggedUnhandled returns the statistic named by each "unhandled statistic"
// line in logs, in the order logged. The debug log is where the collector
// reports them.
func loggedUnhandled(logs string) []string {
	var keys []string
	for line := range strings.SplitSeq(logs, "\n") {
		if !strings.Contains(line, "unhandled statistic") {
			continue
		}
		if _, rest, found := strings.Cut(line, " statistic="); found {
			key, _, _ := strings.Cut(rest, " ")
			keys = append(keys, strings.Trim(key, `"`))
		}
	}
	return keys
}

// serve runs one request through handler and returns the recorded response.
func serve(handler http.Handler, path string) *httptest.ResponseRecorder {
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, path, nil))
	return recorder
}

// sumFamily totals every sample in one metric family.
func sumFamily(t *testing.T, source prometheus.Collector, name string) float64 {
	t.Helper()
	return sumOf(gather(t, source), name)
}

// sumOf totals every sample in the family called name.
func sumOf(families []*dto.MetricFamily, name string) float64 {
	var total float64
	for _, family := range families {
		if family.GetName() != name {
			continue
		}
		for _, metric := range family.GetMetric() {
			total += metric.GetCounter().GetValue() + metric.GetGauge().GetValue()
		}
	}
	return total
}

// gather registers source on a pedantic registry, which also checks its
// descriptors, and returns what one scrape of it yields.
func gather(t *testing.T, source prometheus.Collector) []*dto.MetricFamily {
	t.Helper()
	reg := prometheus.NewPedanticRegistry()
	if err := reg.Register(source); err != nil {
		t.Fatalf("register: %v", err)
	}
	families, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	return families
}

// testWait is how long a test waits for something it expects before failing,
// rather than hanging until the package timeout.
const testWait = 5 * time.Second

// receive waits for a value on values, failing the test rather than hanging
// until the package timeout if none arrives.
func receive[T any](t *testing.T, values <-chan T, what string) T {
	t.Helper()
	select {
	case value := <-values:
		return value
	case <-time.After(testWait):
		t.Fatalf("timed out waiting for %s", what)
		var zero T
		return zero
	}
}

// collectAll runs source.Collect with a reader already draining the channel,
// and returns everything it emitted. Collect writes synchronously, so a
// buffered channel with no concurrent reader deadlocks the moment the metric
// count exceeds the buffer, and the failure is a hung test with no output, not
// an assertion. Sizing a buffer "big enough" only moves the cliff.
func collectAll(source prometheus.Collector) []prometheus.Metric {
	metrics := make(chan prometheus.Metric)
	var collected []prometheus.Metric
	done := make(chan struct{})
	go func() {
		defer close(done)
		for metric := range metrics {
			collected = append(collected, metric)
		}
	}()
	source.Collect(metrics)
	close(metrics)
	<-done
	return collected
}

// upOnly narrows the collector to kea_up so ToFloat64 sees a single metric.
type upOnly struct{ collector *collector }

// Describe sends kea_up's descriptor only.
func (u upOnly) Describe(out chan<- *prometheus.Desc) { out <- u.collector.up }

// Collect sends kea_up from one collection of the wrapped collector.
func (u upOnly) Collect(out chan<- prometheus.Metric) {
	for _, metric := range collectAll(u.collector) {
		if strings.Contains(metric.Desc().String(), `"kea_up"`) {
			out <- metric
		}
	}
}
