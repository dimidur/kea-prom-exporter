package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

// Tests for collector.go, plus the helpers the whole suite shares.
//
// The fixtures in testdata/ are real responses captured from a live Kea
// 3.2.0 control socket, with hostnames replaced by neutral peer names.
// Testing against captured output rather than hand-written JSON is the
// point: it pins the exporter to the shape Kea actually emits, including
// the per-pool statistic dimension that only appears on 3.x.

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	return b
}

// keaStub serves the captured fixtures, dispatching on the command in the
// request body exactly as Kea does.
func keaStub(t *testing.T, statistics, status []byte) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// io.ReadAll, not a single Read into a ContentLength-sized buffer:
		// Read is allowed to return short, which silently dispatched the
		// request to the "unexpected command" branch.
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read request body: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.Contains(string(body), "statistic-get-all"):
			_, _ = w.Write(statistics)
		case strings.Contains(string(body), "status-get"):
			_, _ = w.Write(status)
		default:
			http.Error(w, "unexpected command", http.StatusBadRequest)
		}
	}))
}

func TestCollectAgainstRealKeaOutput(t *testing.T) {
	// Arrange
	srv := keaStub(t, fixture(t, "statistic-get-all.json"), fixture(t, "status-get.json"))
	defer srv.Close()

	c := newCollector(&keaClient{url: srv.URL, client: srv.Client()})

	// Exact values, not "the family is non-empty". The previous version of
	// this test asserted only presence, which let a mislabelled aggregate
	// counter through (pkt4-sent emitted as type="sent" beside the real
	// types, doubling any sum()).
	// Act
	expected := `
# HELP kea_dhcp4_addresses_assigned Currently assigned IPv4 addresses in the pool. Includes declined addresses (dhcp4_srv.cc:4455-4458 at Kea-3.2.0): Kea keeps them assigned so pool-utilisation stays meaningful, so do not add kea_dhcp4_addresses_declined to this.
# TYPE kea_dhcp4_addresses_assigned gauge
kea_dhcp4_addresses_assigned{subnet="1"} 23
# HELP kea_dhcp4_addresses_capacity Pool size: total IPv4 addresses available in the subnet.
# TYPE kea_dhcp4_addresses_capacity gauge
kea_dhcp4_addresses_capacity{subnet="1"} 121
`
	// Assert
	if err := testutil.CollectAndCompare(c, strings.NewReader(expected),
		"kea_dhcp4_addresses_assigned", "kea_dhcp4_addresses_capacity"); err != nil {
		t.Errorf("subnet metrics: %v", err)
	}

	haExpected := `
# HELP kea_dhcp4_ha_communication_interrupted 1 if HA communication with the partner is interrupted, else 0.
# TYPE kea_dhcp4_ha_communication_interrupted gauge
kea_dhcp4_ha_communication_interrupted 0
# HELP kea_dhcp4_ha_enabled 1 when Kea reports an HA relationship, 0 when the HA hook is not loaded.
# TYPE kea_dhcp4_ha_enabled gauge
kea_dhcp4_ha_enabled 1
# HELP kea_dhcp4_ha_local_state_info HA state of this peer (info-metric; value always 1; state in label).
# TYPE kea_dhcp4_ha_local_state_info gauge
kea_dhcp4_ha_local_state_info{mode="hot-standby",role="primary",state="hot-standby"} 1
# HELP kea_dhcp4_ha_partner_in_touch 1 once this peer has been in contact with its HA partner, else 0.
# TYPE kea_dhcp4_ha_partner_in_touch gauge
kea_dhcp4_ha_partner_in_touch 1
# HELP kea_dhcp4_ha_partner_last_contact_seconds Seconds since the last successful heartbeat from the HA partner. Absent until the partner has been contacted at least once.
# TYPE kea_dhcp4_ha_partner_last_contact_seconds gauge
kea_dhcp4_ha_partner_last_contact_seconds 2
`
	if err := testutil.CollectAndCompare(c, strings.NewReader(haExpected),
		"kea_dhcp4_ha_local_state_info", "kea_dhcp4_ha_communication_interrupted",
		"kea_dhcp4_ha_enabled", "kea_dhcp4_ha_partner_in_touch",
		"kea_dhcp4_ha_partner_last_contact_seconds"); err != nil {
		t.Errorf("HA metrics: %v", err)
	}
}

func TestPacketTypesDoNotIncludeTheAggregate(t *testing.T) {
	// Kea reports pkt4-sent as the grand total alongside pkt4-offer-sent and
	// pkt4-ack-sent. Emitting it as type="sent" makes sum() return double.
	// Arrange
	raw := fixture(t, "statistic-get-all.json")
	var resp []struct {
		Arguments map[string]json.RawMessage `json:"arguments"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		t.Fatalf("decode fixture: %v", err)
	}
	aggregate, ok := mostRecentValue(resp[0].Arguments["pkt4-sent"])
	if !ok {
		t.Fatal("fixture has no pkt4-sent aggregate to guard against")
	}

	srv := keaStub(t, raw, fixture(t, "status-get.json"))
	defer srv.Close()
	c := newCollector(&keaClient{url: srv.URL, client: srv.Client()})
	reg := prometheus.NewPedanticRegistry()
	if err := reg.Register(c); err != nil {
		t.Fatalf("register: %v", err)
	}
	// Act
	mfs, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}

	// Assert
	var sum float64
	for _, mf := range mfs {
		if mf.GetName() != "kea_dhcp4_packets_sent_total" {
			continue
		}
		for _, m := range mf.GetMetric() {
			for _, l := range m.GetLabel() {
				if l.GetName() == "type" && (l.GetValue() == "sent" || l.GetValue() == "received") {
					t.Errorf("aggregate exported as a packet type: type=%q", l.GetValue())
				}
			}
			sum += m.GetCounter().GetValue()
		}
	}
	if sum != aggregate {
		t.Errorf("per-type packets sum to %v, Kea's aggregate is %v; they must agree", sum, aggregate)
	}
}

func TestMetricNamesSatisfyPromlint(t *testing.T) {
	// Catches convention breaches automatically -- e.g. a gauge carrying the
	// _total suffix, which is reserved for counters.
	// Arrange
	srv := keaStub(t, fixture(t, "statistic-get-all.json"), fixture(t, "status-get.json"))
	defer srv.Close()
	c := newCollector(&keaClient{url: srv.URL, client: srv.Client()})
	// Act
	problems, err := testutil.CollectAndLint(c)
	if err != nil {
		t.Fatalf("lint: %v", err)
	}
	// Assert
	for _, p := range problems {
		t.Errorf("promlint: %s: %s", p.Metric, p.Text)
	}
}

func TestPartialFailureIsVisible(t *testing.T) {
	// statistic-get-all fails, status-get succeeds. kea_up must be 0 -- the
	// alternative is reporting healthy while every lease metric is missing --
	// and the per-command series must say which half broke.
	// Arrange
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if strings.Contains(string(body), "statistic-get-all") {
			http.Error(w, "boom", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(fixture(t, "status-get.json"))
	}))
	defer srv.Close()

	c := newCollector(&keaClient{url: srv.URL, client: srv.Client()})
	reg := prometheus.NewPedanticRegistry()
	if err := reg.Register(c); err != nil {
		t.Fatalf("register: %v", err)
	}
	// Act
	mfs, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}

	got := map[string]float64{}
	for _, mf := range mfs {
		for _, m := range mf.GetMetric() {
			name := mf.GetName()
			for _, l := range m.GetLabel() {
				if l.GetName() == "command" {
					name += "{" + l.GetValue() + "}"
				}
			}
			if g := m.GetGauge(); g != nil {
				got[name] = g.GetValue()
			}
		}
	}
	// Assert
	if got["kea_up"] != 0 {
		t.Errorf("kea_up = %v on partial failure, want 0", got["kea_up"])
	}
	if got["kea_command_up{statistic-get-all}"] != 0 {
		t.Errorf("kea_command_up{statistic-get-all} = %v, want 0", got["kea_command_up{statistic-get-all}"])
	}
	if got["kea_command_up{status-get}"] != 1 {
		t.Errorf("kea_command_up{status-get} = %v, want 1", got["kea_command_up{status-get}"])
	}
}

func TestScrapeSucceedsAgainstRealOutput(t *testing.T) {
	// Arrange
	srv := keaStub(t, fixture(t, "statistic-get-all.json"), fixture(t, "status-get.json"))
	defer srv.Close()

	c := newCollector(&keaClient{url: srv.URL, client: srv.Client()})

	// Act + Assert
	if got := testutil.ToFloat64(upOnly{c}); got != 1 {
		t.Errorf("kea_up = %v, want 1 when both commands succeed", got)
	}
}

// collectAll runs c.Collect with a reader already draining the channel, and
// returns everything it emitted. Collect writes synchronously, so a buffered
// channel with no concurrent reader deadlocks the moment the metric count
// exceeds the buffer -- and the failure is a hung test with no output, not an
// assertion. Sizing a buffer "big enough" only moves the cliff.
func collectAll(c prometheus.Collector) []prometheus.Metric {
	ch := make(chan prometheus.Metric)
	var collected []prometheus.Metric
	done := make(chan struct{})
	go func() {
		defer close(done)
		for m := range ch {
			collected = append(collected, m)
		}
	}()
	c.Collect(ch)
	close(ch)
	<-done
	return collected
}

// upOnly narrows the collector to kea_up so ToFloat64 sees a single metric.
type upOnly struct{ c *collector }

func (u upOnly) Describe(ch chan<- *prometheus.Desc) { ch <- u.c.up }
func (u upOnly) Collect(ch chan<- prometheus.Metric) {
	for _, m := range collectAll(u.c) {
		if strings.Contains(m.Desc().String(), `"kea_up"`) {
			ch <- m
		}
	}
}

func TestUnreachableKeaReportsDownRatherThanFailing(t *testing.T) {
	// A dead control socket must produce kea_up 0, not a panic and not an
	// empty scrape: "down" has to be observable.
	// Arrange
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "nope", http.StatusInternalServerError)
	}))
	defer srv.Close()

	c := newCollector(&keaClient{url: srv.URL, client: srv.Client()})

	// Act + Assert
	if got := testutil.ToFloat64(upOnly{c}); got != 0 {
		t.Errorf("kea_up = %v, want 0 when the control socket errors", got)
	}
}

func TestPerPoolStatisticsAreNotDoubleCounted(t *testing.T) {
	// Kea 3.x reports both subnet[N].assigned-addresses and
	// subnet[N].pool[M].assigned-addresses. Counting both would inflate
	// utilisation; the per-pool keys must be skipped.
	// Arrange
	raw := fixture(t, "statistic-get-all.json")
	var resp []struct {
		Arguments map[string]json.RawMessage `json:"arguments"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		t.Fatalf("decode fixture: %v", err)
	}
	var perPool int
	for k := range resp[0].Arguments {
		if strings.Contains(k, ".pool[") {
			perPool++
		}
	}
	if perPool == 0 {
		t.Fatal("fixture lost its per-pool statistics; this guard is now vacuous")
	}

	srv := keaStub(t, raw, fixture(t, "status-get.json"))
	defer srv.Close()
	c := newCollector(&keaClient{url: srv.URL, client: srv.Client()})
	reg := prometheus.NewPedanticRegistry()
	if err := reg.Register(c); err != nil {
		t.Fatalf("register: %v", err)
	}
	// Act
	mfs, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}

	// One assigned-addresses series per subnet, never per pool.
	// Assert
	subnets := map[string]struct{}{}
	for k := range resp[0].Arguments {
		if strings.HasPrefix(k, "subnet[") && strings.HasSuffix(k, "].assigned-addresses") {
			subnets[k[:strings.Index(k, "]")]] = struct{}{}
		}
	}
	for _, mf := range mfs {
		if mf.GetName() != "kea_dhcp4_addresses_assigned" {
			continue
		}
		if len(mf.GetMetric()) != len(subnets) {
			t.Errorf("kea_dhcp4_addresses_assigned has %d series, want %d (one per subnet, per-pool keys excluded)",
				len(mf.GetMetric()), len(subnets))
		}
	}
}

func TestConcurrentScrapesAreRaceFree(t *testing.T) {
	// Prometheus may call Collect concurrently when scrapes overlap. The
	// scrape-error counter used to be a package-level uint64 incremented
	// without synchronisation, which `go test -race` flags here.
	// Arrange
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "down", http.StatusInternalServerError)
	}))
	defer srv.Close()

	c := newCollector(&keaClient{url: srv.URL, client: srv.Client()})
	// Act
	const scrapes = 8
	var wg sync.WaitGroup
	for range scrapes {
		wg.Add(1)
		go func() {
			defer wg.Done()
			collectAll(c)
		}()
	}
	wg.Wait()

	// Assert
	if got := c.statErrorCount.Load(); got != scrapes {
		t.Errorf("statErrorCount = %d after %d failing scrapes, want %d", got, scrapes, scrapes)
	}
}

func TestUnparseableStatisticIsLoggedOnceNotDroppedSilently(t *testing.T) {
	// Kea also reports string- and duration-typed statistics. They cannot
	// become a float, but an absent metric with nothing explaining why is a
	// worse outcome than a one-line log.
	// Arrange
	stats := []byte(`[{"result":0,"arguments":{
	  "pkt4-ack-sent":[[1,"2026-08-02 20:04:57"]],
	  "some-string-stat":[["not-a-number","2026-08-02 20:04:57"]]}}]`)
	srv := keaStub(t, stats, fixture(t, "status-get.json"))
	defer srv.Close()

	c := newCollector(&keaClient{url: srv.URL, client: srv.Client()})
	// Act
	collectAll(c)

	// Assert
	c.unhandledMu.Lock()
	defer c.unhandledMu.Unlock()
	if _, ok := c.unhandled["some-string-stat"]; !ok {
		t.Errorf("unparseable statistic not recorded as unhandled: %v", c.unhandled)
	}
}

func TestUnhandledStatisticsAreCounted(t *testing.T) {
	// The names only reach the debug log, so the count has to be a metric --
	// otherwise drift after a Kea upgrade needs a restart at a higher log
	// level to discover.
	// Arrange
	srv := keaStub(t, fixture(t, "statistic-get-all.json"), fixture(t, "status-get.json"))
	defer srv.Close()

	c := newCollector(&keaClient{url: srv.URL, client: srv.Client()})
	reg := prometheus.NewPedanticRegistry()
	if err := reg.Register(c); err != nil {
		t.Fatalf("register: %v", err)
	}
	// Act
	mfs, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}

	// Assert
	var got float64
	var found bool
	for _, mf := range mfs {
		if mf.GetName() == "kea_exporter_unhandled_statistics" {
			found = true
			got = mf.GetMetric()[0].GetGauge().GetValue()
		}
	}
	if !found {
		t.Fatal("kea_exporter_unhandled_statistics was not exported")
	}
	// The real fixture contains statistics this exporter does not map; the
	// exact count is not the contract, being non-zero and observable is.
	if got == 0 {
		t.Errorf("unhandled statistics = 0, but the fixture contains unmapped keys")
	}
	c.unhandledMu.Lock()
	want := float64(len(c.unhandled))
	c.unhandledMu.Unlock()
	if got != want {
		t.Errorf("gauge = %v, collector tracked %v", got, want)
	}
}

func TestCollectDoesNotDependOnAChannelBuffer(t *testing.T) {
	// Builds the channel here rather than calling collectAll: routing through
	// the helper under test cannot demonstrate anything about the helper. The
	// property is that Collect completes against an UNBUFFERED channel, which
	// is what a registry Gather does and what the old fixed-size buffers only
	// approximated.
	// Arrange
	srv := keaStub(t, fixture(t, "statistic-get-all.json"), fixture(t, "status-get.json"))
	defer srv.Close()
	c := newCollector(&keaClient{url: srv.URL, client: srv.Client()})

	ch := make(chan prometheus.Metric)
	done := make(chan int, 1)
	go func() {
		n := 0
		for range ch {
			n++
		}
		done <- n
	}()
	// Act
	c.Collect(ch)
	close(ch)

	var n int
	select {
	case n = <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Collect did not complete against an unbuffered channel")
	}

	// Assert -- a floor, not an equality: adding metrics is expected, losing
	// them is a regression. 46 is what the real fixture produces today. It was
	// 31 before the alertable counters landed, and a floor left at the old
	// value silently tolerates losing everything added since.
	const emittedToday = 46
	if n < emittedToday {
		t.Errorf("collector emitted %d metrics, want at least %d", n, emittedToday)
	}
}

func TestBuildInfoAndScrapeDurationAreExported(t *testing.T) {
	// Arrange
	srv := keaStub(t, fixture(t, "statistic-get-all.json"), fixture(t, "status-get.json"))
	defer srv.Close()

	c := newCollector(&keaClient{url: srv.URL, client: srv.Client()})
	// Per-collector, not a package global: nothing here is shared with any
	// other test.
	c.build = buildID{version: "v1.2.3", revision: "cafebabe", goVersion: "go1.2.3"}

	reg := prometheus.NewPedanticRegistry()
	if err := reg.Register(c); err != nil {
		t.Fatalf("register: %v", err)
	}
	// Act
	mfs, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}

	// Assert
	var sawBuild, sawDuration bool
	for _, mf := range mfs {
		metrics := mf.GetMetric()
		if len(metrics) == 0 {
			t.Errorf("%s has no samples", mf.GetName())
			continue
		}
		switch mf.GetName() {
		case "kea_exporter_build_info":
			sawBuild = true
			labels := map[string]string{}
			for _, label := range metrics[0].GetLabel() {
				labels[label.GetName()] = label.GetValue()
			}
			want := map[string]string{"version": "v1.2.3", "revision": "cafebabe", "goversion": "go1.2.3"}
			for k, v := range want {
				if labels[k] != v {
					t.Errorf("build_info %s = %q, want %q", k, labels[k], v)
				}
			}
			if got := metrics[0].GetGauge().GetValue(); got != 1 {
				t.Errorf("build_info value = %v, want 1 (info-metric convention)", got)
			}
		case "kea_scrape_duration_seconds":
			sawDuration = true
			// Only that it is a real, non-negative measurement; an upper bound
			// would make this flaky on a loaded machine.
			if got := metrics[0].GetGauge().GetValue(); got < 0 {
				t.Errorf("scrape duration = %v, want >= 0", got)
			}
		}
	}
	if !sawBuild {
		t.Error("kea_exporter_build_info was not exported")
	}
	if !sawDuration {
		t.Error("kea_scrape_duration_seconds was not exported")
	}
}

func TestScrapeDurationIsEmittedEvenWhenKeaIsDown(t *testing.T) {
	// A scrape that failed or timed out is exactly the one worth timing, so
	// the measurement must not be conditional on success.
	// Arrange
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "down", http.StatusInternalServerError)
	}))
	defer srv.Close()

	c := newCollector(&keaClient{url: srv.URL, client: srv.Client()})
	// Act
	var found bool
	for _, m := range collectAll(c) {
		if strings.Contains(m.Desc().String(), "kea_scrape_duration_seconds") {
			found = true
		}
	}
	// Assert
	if !found {
		t.Error("no scrape duration emitted when every command failed")
	}
}

func TestKeaUpIsZeroWhenOnlyTheHACommandFails(t *testing.T) {
	// The mirror of TestPartialFailureIsVisible. kea_up is documented as 0 if
	// ANY command failed, but only the statistic-get-all direction was
	// covered, so dropping the haErr term from the condition went unnoticed.

	// Arrange
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if strings.Contains(string(body), "status-get") {
			http.Error(w, "boom", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(fixture(t, "statistic-get-all.json"))
	}))
	defer srv.Close()
	c := newCollector(&keaClient{url: srv.URL, client: srv.Client()})

	// Act
	got := testutil.ToFloat64(upOnly{c})

	// Assert
	if got != 0 {
		t.Errorf("kea_up = %v when status-get failed, want 0", got)
	}
}

func TestExportedMetricSetIsExactlyTheContract(t *testing.T) {
	// Metric names and label sets are this exporter's public API: renaming or
	// dropping one breaks dashboards and alerts downstream. The per-family
	// CollectAndCompare tests each name the families they check, so a metric
	// added or removed elsewhere fails none of them. This pins the whole set,
	// so a change to it has to be a deliberate edit here rather than a side
	// effect noticed after release.
	//
	// The fixture is the one carrying allocation failures, because Kea does
	// not create the per-subnet v4-allocation-fail* observations until a lease
	// request actually fails -- a healthy-server fixture would leave those
	// families out of the contract entirely.

	// Arrange
	contract := map[string][]string{
		"kea_command_up":                               {"command"},
		"kea_dhcp4_addresses_assigned":                 {"subnet"},
		"kea_dhcp4_addresses_capacity":                 {"subnet"},
		"kea_dhcp4_addresses_declined":                 {"subnet"},
		"kea_dhcp4_allocation_failures_by_cause_total": {"cause"},
		"kea_dhcp4_allocation_failures_by_scope_total": {"scope"},
		"kea_dhcp4_ha_communication_interrupted":       {},
		"kea_dhcp4_ha_enabled":                         {},
		"kea_dhcp4_ha_local_state_info":                {"mode", "role", "state"},
		"kea_dhcp4_ha_partner_in_touch":                {},
		"kea_dhcp4_ha_partner_last_contact_seconds":    {},
		"kea_dhcp4_packets_dropped_by_reason_total":    {"reason"},
		"kea_dhcp4_packets_dropped_total":              {},
		"kea_dhcp4_packets_received_total":             {"type"},
		"kea_dhcp4_packets_sent_total":                 {"type"},
		"kea_exporter_build_info":                      {"goversion", "revision", "version"},
		"kea_exporter_unhandled_statistics":            {},
		"kea_scrape_duration_seconds":                  {},
		"kea_scrape_errors_total":                      {"command"},
		"kea_up":                                       {},
	}

	srv := keaStub(t, fixture(t, "statistic-get-all-with-failures.json"), fixture(t, "status-get.json"))
	defer srv.Close()
	c := newCollector(&keaClient{url: srv.URL, client: srv.Client()})
	reg := prometheus.NewPedanticRegistry()
	if err := reg.Register(c); err != nil {
		t.Fatalf("register: %v", err)
	}

	// Act
	mfs, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	exported := map[string][]string{}
	for _, mf := range mfs {
		labels := []string{}
		if len(mf.GetMetric()) > 0 {
			for _, label := range mf.GetMetric()[0].GetLabel() {
				labels = append(labels, label.GetName())
			}
		}
		sort.Strings(labels)
		exported[mf.GetName()] = labels
	}

	// Assert
	for name, want := range contract {
		got, ok := exported[name]
		if !ok {
			t.Errorf("%s is in the contract but was not exported", name)
			continue
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%s exports labels %v, want %v", name, got, want)
		}
	}
	for name := range exported {
		if _, ok := contract[name]; !ok {
			t.Errorf("%s is exported but not in the contract; add it here and to the README metric table", name)
		}
	}
}
