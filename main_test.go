package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
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
	reg := prometheus.NewPedanticRegistry()
	if err := reg.Register(c); err != nil {
		t.Fatalf("register: %v", err)
	}

	mfs, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}

	got := map[string]int{}
	for _, mf := range mfs {
		got[mf.GetName()] = len(mf.GetMetric())
	}

	// Every metric the README advertises must be produced from real output.
	for _, name := range []string{
		"kea_up",
		"kea_scrape_errors_total",
		"kea_dhcp4_addresses_assigned",
		"kea_dhcp4_addresses_total",
		"kea_dhcp4_packets_received_total",
		"kea_dhcp4_packets_sent_total",
		"kea_dhcp4_ha_local_state_info",
		"kea_dhcp4_ha_partner_last_contact_seconds",
		"kea_dhcp4_ha_communication_interrupted",
	} {
		if got[name] == 0 {
			t.Errorf("metric %s absent or empty; got families: %v", name, got)
		}
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
		t.Skip("fixture has no per-pool statistics; nothing to guard against")
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

func TestMostRecentValue(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want float64
		ok   bool
	}{
		{"kea sample shape", `[[42, "2026-08-02 20:04:57.000000"]]`, 42, true},
		{"newest sample wins", `[[7, "2026-08-02 20:04:57"], [3, "2026-08-02 19:00:00"]]`, 7, true},
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
