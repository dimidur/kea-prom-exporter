package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"slices"
	"strings"
	"sync"
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

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	return data
}

// fixtureArguments decodes a captured statistic-get-all fixture to its
// statistics, keyed by name, each still in Kea's raw sample encoding.
func fixtureArguments(t *testing.T, name string) map[string]json.RawMessage {
	t.Helper()
	var reply []struct {
		Arguments map[string]json.RawMessage `json:"arguments"`
	}
	if err := json.Unmarshal(fixture(t, name), &reply); err != nil {
		t.Fatalf("decode fixture %s: %v", name, err)
	}
	return reply[0].Arguments
}

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
// recorded with what its context held; Collect may run concurrently, so the
// record is locked.
type fakeKea struct {
	statistics, status []byte

	mu    sync.Mutex
	calls []recordedCall
}

func newFakeKea(statistics, status []byte) *fakeKea {
	return &fakeKea{statistics: statistics, status: status}
}

func (f *fakeKea) call(ctx context.Context, command string) (*keaResponse, error) {
	_, hadDeadline := ctx.Deadline()
	f.mu.Lock()
	f.calls = append(f.calls, recordedCall{command: command, hadDeadline: hadDeadline, ctxErr: ctx.Err()})
	f.mu.Unlock()

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
		return nil, fmt.Errorf("kea call %q: no reply configured", command)
	}
	return interpretResponse(command, http.StatusOK, bytes.NewReader(reply))
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
	for _, line := range strings.Split(logs, "\n") {
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

// receive waits for a value on ch, failing the test rather than hanging
// until the package timeout if none arrives.
func receive[T any](t *testing.T, ch <-chan T, what string) T {
	t.Helper()
	select {
	case value := <-ch:
		return value
	case <-time.After(5 * time.Second):
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
	ch := make(chan prometheus.Metric)
	var collected []prometheus.Metric
	done := make(chan struct{})
	go func() {
		defer close(done)
		for metric := range ch {
			collected = append(collected, metric)
		}
	}()
	source.Collect(ch)
	close(ch)
	<-done
	return collected
}

// upOnly narrows the collector to kea_up so ToFloat64 sees a single metric.
type upOnly struct{ collector *collector }

func (u upOnly) Describe(ch chan<- *prometheus.Desc) { ch <- u.collector.up }
func (u upOnly) Collect(ch chan<- prometheus.Metric) {
	for _, metric := range collectAll(u.collector) {
		if strings.Contains(metric.Desc().String(), `"kea_up"`) {
			ch <- metric
		}
	}
}
