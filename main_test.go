package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

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
		body := make([]byte, r.ContentLength)
		if _, err := r.Body.Read(body); err != nil && err.Error() != "EOF" {
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
	srv := keaStub(t, fixture(t, "statistic-get-all.json"), fixture(t, "status-get.json"))
	defer srv.Close()

	c := newCollector(&keaClient{url: srv.URL, client: srv.Client()})

	// Exact values, not "the family is non-empty". The previous version of
	// this test asserted only presence, which let a mislabelled aggregate
	// counter through (pkt4-sent emitted as type="sent" beside the real
	// types, doubling any sum()).
	expected := `
# HELP kea_dhcp4_addresses_assigned Currently assigned IPv4 addresses in the pool.
# TYPE kea_dhcp4_addresses_assigned gauge
kea_dhcp4_addresses_assigned{subnet="1"} 23
# HELP kea_dhcp4_addresses_capacity Pool size: total IPv4 addresses available in the subnet.
# TYPE kea_dhcp4_addresses_capacity gauge
kea_dhcp4_addresses_capacity{subnet="1"} 121
`
	if err := testutil.CollectAndCompare(c, strings.NewReader(expected),
		"kea_dhcp4_addresses_assigned", "kea_dhcp4_addresses_capacity"); err != nil {
		t.Errorf("subnet metrics: %v", err)
	}

	haExpected := `
# HELP kea_dhcp4_ha_communication_interrupted 1 if HA communication with the partner is interrupted, else 0.
# TYPE kea_dhcp4_ha_communication_interrupted gauge
kea_dhcp4_ha_communication_interrupted 0
# HELP kea_dhcp4_ha_local_state_info HA state of this peer (info-metric; value always 1; state in label).
# TYPE kea_dhcp4_ha_local_state_info gauge
kea_dhcp4_ha_local_state_info{role="primary",state="hot-standby"} 1
`
	if err := testutil.CollectAndCompare(c, strings.NewReader(haExpected),
		"kea_dhcp4_ha_local_state_info", "kea_dhcp4_ha_communication_interrupted"); err != nil {
		t.Errorf("HA metrics: %v", err)
	}
}

func TestPacketTypesDoNotIncludeTheAggregate(t *testing.T) {
	// Kea reports pkt4-sent as the grand total alongside pkt4-offer-sent and
	// pkt4-ack-sent. Emitting it as type="sent" makes sum() return double.
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
	mfs, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}

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
	srv := keaStub(t, fixture(t, "statistic-get-all.json"), fixture(t, "status-get.json"))
	defer srv.Close()
	c := newCollector(&keaClient{url: srv.URL, client: srv.Client()})
	problems, err := testutil.CollectAndLint(c)
	if err != nil {
		t.Fatalf("lint: %v", err)
	}
	for _, p := range problems {
		t.Errorf("promlint: %s: %s", p.Metric, p.Text)
	}
}

func TestPartialFailureIsVisible(t *testing.T) {
	// statistic-get-all fails, status-get succeeds. kea_up must be 0 -- the
	// alternative is reporting healthy while every lease metric is missing --
	// and the per-command series must say which half broke.
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
	srv := keaStub(t, fixture(t, "statistic-get-all.json"), fixture(t, "status-get.json"))
	defer srv.Close()

	c := newCollector(&keaClient{url: srv.URL, client: srv.Client()})
	if got := testutil.ToFloat64(upOnly{c}); got != 1 {
		t.Errorf("kea_up = %v, want 1 when both commands succeed", got)
	}
}

// upOnly narrows the collector to kea_up so ToFloat64 sees a single metric.
type upOnly struct{ c *collector }

func (u upOnly) Describe(ch chan<- *prometheus.Desc) { ch <- u.c.up }
func (u upOnly) Collect(ch chan<- prometheus.Metric) {
	tmp := make(chan prometheus.Metric, 256)
	u.c.Collect(tmp)
	close(tmp)
	for m := range tmp {
		if strings.Contains(m.Desc().String(), `"kea_up"`) {
			ch <- m
		}
	}
}

func TestUnreachableKeaReportsDownRatherThanFailing(t *testing.T) {
	// A dead control socket must produce kea_up 0, not a panic and not an
	// empty scrape: "down" has to be observable.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "nope", http.StatusInternalServerError)
	}))
	defer srv.Close()

	c := newCollector(&keaClient{url: srv.URL, client: srv.Client()})
	if got := testutil.ToFloat64(upOnly{c}); got != 0 {
		t.Errorf("kea_up = %v, want 0 when the control socket errors", got)
	}
}

func TestPerPoolStatisticsAreNotDoubleCounted(t *testing.T) {
	// Kea 3.x reports both subnet[N].assigned-addresses and
	// subnet[N].pool[M].assigned-addresses. Counting both would inflate
	// utilisation; the per-pool keys must be skipped.
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
	mfs, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}

	// One assigned-addresses series per subnet, never per pool.
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
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "down", http.StatusInternalServerError)
	}))
	defer srv.Close()

	c := newCollector(&keaClient{url: srv.URL, client: srv.Client()})
	const scrapes = 8
	var wg sync.WaitGroup
	for range scrapes {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ch := make(chan prometheus.Metric, 64)
			c.Collect(ch)
		}()
	}
	wg.Wait()

	if got := c.statErrorCount.Load(); got != scrapes {
		t.Errorf("statErrorCount = %d after %d failing scrapes, want %d", got, scrapes, scrapes)
	}
}

func TestMostRecentValue(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want float64
		ok   bool
	}{
		{"kea sample shape", `[[42, "2026-08-02 20:04:57.000000"]]`, 42, true},
		{"first sample is taken (Kea reports newest first)", `[[7, "2026-08-02 20:04:57"], [3, "2026-08-02 19:00:00"]]`, 7, true},
		{"float value", `[[1.5, "2026-08-02 20:04:57"]]`, 1.5, true},
		{"empty list", `[]`, 0, false},
		{"not a list", `{"nope": 1}`, 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := mostRecentValue(json.RawMessage(tc.in))
			if ok != tc.ok || (ok && got != tc.want) {
				t.Errorf("mostRecentValue(%s) = (%v, %v), want (%v, %v)", tc.in, got, ok, tc.want, tc.ok)
			}
		})
	}
}
