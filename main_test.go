package main

import (
	"context"
	"encoding/json"
	"flag"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

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
			collectAll(c)
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

func TestNullSampleIsSkippedRatherThanReportedAsZero(t *testing.T) {
	// Kea emits `[[null, "<ts>"]]` for a statistic it has no value for yet.
	// json.Unmarshal of null into a float64 succeeds and leaves 0, so without
	// an explicit guard the exporter reports a confident zero.
	if got, ok := mostRecentValue(json.RawMessage(`[[null, "2026-08-02 20:04:57"]]`)); ok {
		t.Errorf("mostRecentValue(null sample) = (%v, true), want ok=false", got)
	}
}

func TestCommandIsValidJSONWhateverTheServiceName(t *testing.T) {
	// The service name reaches the wire from a flag. Built with fmt.Sprintf it
	// was possible to inject a quote and change the command actually sent.
	var got keaCommand
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(body, &got); err != nil {
			t.Errorf("request body is not valid JSON: %v (%s)", err, body)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[{"result":0,"arguments":{}}]`))
	}))
	defer srv.Close()

	k := &keaClient{url: srv.URL, service: `dhcp4","injected":"`, client: srv.Client()}
	if _, err := k.call(t.Context(), "status-get"); err != nil {
		t.Fatalf("call: %v", err)
	}
	if got.Command != "status-get" {
		t.Errorf("command = %q, want status-get", got.Command)
	}
	if len(got.Service) != 1 || got.Service[0] != `dhcp4","injected":"` {
		t.Errorf("service = %q, want the literal flag value carried as one element", got.Service)
	}
}

func TestHTTPErrorBodyIsReportedAndBounded(t *testing.T) {
	// Kea explains 401/403 in the body. "HTTP 401" alone is the least useful
	// possible message for the most common misconfiguration -- but an
	// unbounded body would let a proxy's HTML error page into the logs.
	long := strings.Repeat("x", errBodyLimit*4)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, long, http.StatusUnauthorized)
	}))
	defer srv.Close()

	k := &keaClient{url: srv.URL, client: srv.Client()}
	_, err := k.call(t.Context(), "status-get")
	if err == nil {
		t.Fatal("call succeeded against a 401")
	}
	if !strings.Contains(err.Error(), "HTTP 401") {
		t.Errorf("error does not name the status: %v", err)
	}
	if !strings.Contains(err.Error(), "xxxx") {
		t.Errorf("error does not quote the response body: %v", err)
	}
	if n := strings.Count(err.Error(), "x"); n > errBodyLimit {
		t.Errorf("error quotes %d body bytes, want at most %d", n, errBodyLimit)
	}
}

func TestNon200SuccessStatusIsAccepted(t *testing.T) {
	// Kea itself answers 200, but a reverse proxy in front of the control
	// socket may legitimately answer 204/206 or similar. Rejecting anything
	// but exactly 200 turned a working deployment into kea_up 0.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`[{"result":0,"arguments":{}}]`))
	}))
	defer srv.Close()

	k := &keaClient{url: srv.URL, client: srv.Client()}
	if _, err := k.call(t.Context(), "status-get"); err != nil {
		t.Errorf("call on HTTP 202: %v", err)
	}
}

func TestKeaResultErrorIsSurfaced(t *testing.T) {
	// A non-zero `result` is HTTP 200 with a failure inside. Kea returns 1 for
	// an error and 2 for an unsupported command; both must fail the scrape.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[{"result":2,"text":"'status-get' command not supported"}]`))
	}))
	defer srv.Close()

	k := &keaClient{url: srv.URL, client: srv.Client()}
	_, err := k.call(t.Context(), "status-get")
	if err == nil {
		t.Fatal("call succeeded against result=2")
	}
	if !strings.Contains(err.Error(), "result=2") || !strings.Contains(err.Error(), "not supported") {
		t.Errorf("error drops Kea's own explanation: %v", err)
	}
}

func TestHAHookAbsentIsDistinguishableFromScrapeFailure(t *testing.T) {
	// No `high-availability` key means the hook is not loaded. That must be
	// an explicit 0, not silence -- otherwise it is indistinguishable from
	// status-get having failed.
	status := []byte(`[{"result":0,"arguments":{"pid":1,"uptime":10}}]`)
	srv := keaStub(t, fixture(t, "statistic-get-all.json"), status)
	defer srv.Close()

	c := newCollector(&keaClient{url: srv.URL, client: srv.Client()})
	expected := `
# HELP kea_dhcp4_ha_enabled 1 when Kea reports an HA relationship, 0 when the HA hook is not loaded.
# TYPE kea_dhcp4_ha_enabled gauge
kea_dhcp4_ha_enabled 0
`
	if err := testutil.CollectAndCompare(c, strings.NewReader(expected), "kea_dhcp4_ha_enabled"); err != nil {
		t.Errorf("ha_enabled: %v", err)
	}
	// And the scrape itself is healthy: an absent hook is not an error.
	if got := testutil.ToFloat64(upOnly{c}); got != 1 {
		t.Errorf("kea_up = %v with the HA hook absent, want 1", got)
	}
}

func TestPartnerAgeIsAbsentUntilInTouch(t *testing.T) {
	// Kea reports age 0 when it has never reached the partner. Exported
	// unconditionally that reads as "contacted 0 seconds ago" -- the inverse
	// of the truth -- so the gauge is withheld until in-touch is true.
	status := []byte(`[{"result":0,"arguments":{"high-availability":[{"ha-mode":"hot-standby",
	  "ha-servers":{"local":{"role":"primary","state":"waiting"},
	  "remote":{"age":0,"communication-interrupted":true,"in-touch":false}}}]}}]`)
	srv := keaStub(t, fixture(t, "statistic-get-all.json"), status)
	defer srv.Close()

	c := newCollector(&keaClient{url: srv.URL, client: srv.Client()})
	expected := `
# HELP kea_dhcp4_ha_partner_in_touch 1 once this peer has been in contact with its HA partner, else 0.
# TYPE kea_dhcp4_ha_partner_in_touch gauge
kea_dhcp4_ha_partner_in_touch 0
`
	if err := testutil.CollectAndCompare(c, strings.NewReader(expected),
		"kea_dhcp4_ha_partner_in_touch", "kea_dhcp4_ha_partner_last_contact_seconds"); err != nil {
		t.Errorf("partner contact metrics: %v", err)
	}
}

func TestBasicAuthIsSentOnlyWhenAUserIsConfigured(t *testing.T) {
	cases := []struct {
		name     string
		user     string
		password string
		wantAuth bool
	}{
		{"credentials configured", "kea", "s3cret", true},
		{"no user means no auth header", "", "s3cret", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				user, password, ok := r.BasicAuth()
				if ok != tc.wantAuth {
					t.Errorf("basic auth present = %v, want %v", ok, tc.wantAuth)
				}
				if ok && (user != tc.user || password != tc.password) {
					t.Errorf("credentials = %q/%q, want %q/%q", user, password, tc.user, tc.password)
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`[{"result":0,"arguments":{}}]`))
			}))
			defer srv.Close()

			k := &keaClient{url: srv.URL, user: tc.user, password: tc.password, client: srv.Client()}
			if _, err := k.call(t.Context(), "status-get"); err != nil {
				t.Fatalf("call: %v", err)
			}
		})
	}
}

func TestPerRequestTimeoutBoundsEachCallSeparately(t *testing.T) {
	// One deadline shared across the scrape let a slow first command consume
	// the budget and made the second fail as if it were at fault. Each call
	// now carries its own.
	// The first handler must outlive the first call's deadline, then be
	// released explicitly. Waiting on r.Context() instead would deadlock:
	// net/http only starts watching for a client disconnect once the request
	// body has been consumed, and this handler never reads it.
	release := make(chan struct{})
	var calls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			select {
			case <-release:
			case <-r.Context().Done():
			}
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[{"result":0,"arguments":{}}]`))
	}))
	// Registered after Close so it runs before it: the handler has to be let
	// go before Close will stop waiting on it.
	defer srv.Close()
	defer close(release)

	k := &keaClient{url: srv.URL, timeout: 50 * time.Millisecond, client: srv.Client()}
	ctx := t.Context()
	if _, err := k.call(ctx, "statistic-get-all"); err == nil {
		t.Fatal("first call succeeded, expected it to time out")
	}
	if _, err := k.call(ctx, "status-get"); err != nil {
		t.Errorf("second call inherited the first call's exhausted deadline: %v", err)
	}
}

func TestLoadPassword(t *testing.T) {
	dir := t.TempDir()
	file := dir + "/secret"
	// Trailing newline is what an editor or `echo` leaves behind; sending it
	// to Kea is an authentication failure with no useful diagnostic.
	if err := os.WriteFile(file, []byte("from-file\n"), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	cases := []struct {
		name    string
		file    string
		inline  string
		want    string
		wantErr bool
	}{
		{"file wins over inline", file, "inline", "from-file", false},
		{"trailing newline stripped", file, "", "from-file", false},
		{"inline when no file", "", "inline", "inline", false},
		{"neither is not an error", "", "", "", false},
		{"missing file is an error", dir + "/absent", "inline", "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := loadPassword(tc.file, tc.inline)
			if (err != nil) != tc.wantErr {
				t.Fatalf("loadPassword(%q, %q) error = %v, wantErr %v", tc.file, tc.inline, err, tc.wantErr)
			}
			if got != tc.want {
				t.Errorf("loadPassword(%q, %q) = %q, want %q", tc.file, tc.inline, got, tc.want)
			}
		})
	}
}

func TestUnparseableStatisticIsLoggedOnceNotDroppedSilently(t *testing.T) {
	// Kea also reports string- and duration-typed statistics. They cannot
	// become a float, but an absent metric with nothing explaining why is a
	// worse outcome than a one-line log.
	stats := []byte(`[{"result":0,"arguments":{
	  "pkt4-ack-sent":[[1,"2026-08-02 20:04:57"]],
	  "some-string-stat":[["not-a-number","2026-08-02 20:04:57"]]}}]`)
	srv := keaStub(t, stats, fixture(t, "status-get.json"))
	defer srv.Close()

	c := newCollector(&keaClient{url: srv.URL, client: srv.Client()})
	collectAll(c)

	c.unhandledMu.Lock()
	defer c.unhandledMu.Unlock()
	if _, ok := c.unhandled["some-string-stat"]; !ok {
		t.Errorf("unparseable statistic not recorded as unhandled: %v", c.unhandled)
	}
}

func TestNewLoggerReportsRatherThanSilentlyIgnoring(t *testing.T) {
	// slog rejects "warning" -- the syslog and Python spelling, and a very
	// likely operator typo. Falling back to info is fine; doing it silently is
	// not, because the operator then never learns their setting was ignored.
	cases := []struct {
		level, format string
		wantDebug     bool
		wantProblems  int
	}{
		{"debug", "text", true, 0},
		{"DEBUG", "json", true, 0},
		{"info", "text", false, 0},
		{"warning", "text", false, 1},
		{"info", "yaml", false, 1},
		{"nope", "nope", false, 2},
	}
	for _, tc := range cases {
		t.Run(tc.level+"/"+tc.format, func(t *testing.T) {
			l, problems := newLogger(tc.level, tc.format)
			if l == nil {
				t.Fatal("newLogger returned nil")
			}
			if got := l.Enabled(t.Context(), slog.LevelDebug); got != tc.wantDebug {
				t.Errorf("debug enabled = %v, want %v", got, tc.wantDebug)
			}
			if len(problems) != tc.wantProblems {
				t.Errorf("problems = %v, want %d", problems, tc.wantProblems)
			}
		})
	}
}

func TestApplyEnvFillsUnsetFlagsOnly(t *testing.T) {
	// An explicit flag must beat the environment, every flag must be settable
	// from it, and a bad value must be reported rather than swallowed.
	newFS := func() (*flag.FlagSet, *string, *time.Duration) {
		fs := flag.NewFlagSet("test", flag.ContinueOnError)
		url := fs.String("kea-url", "http://default/", "")
		timeout := fs.Duration("kea-timeout", 5*time.Second, "")
		fs.String("listen", ":9547", "")
		fs.String("kea-user", "", "")
		fs.String("kea-password-file", "", "")
		fs.String("kea-password", "", "")
		fs.String("kea-service", "dhcp4", "")
		fs.String("log-level", "info", "")
		fs.String("log-format", "text", "")
		return fs, url, timeout
	}
	env := func(m map[string]string) func(string) (string, bool) {
		return func(k string) (string, bool) { v, ok := m[k]; return v, ok }
	}

	t.Run("env fills an unset flag", func(t *testing.T) {
		fs, url, timeout := newFS()
		if err := fs.Parse(nil); err != nil {
			t.Fatalf("parse: %v", err)
		}
		problems := applyEnv(fs, env(map[string]string{"KEA_URL": "http://from-env/", "KEA_TIMEOUT": "12s"}))
		if len(problems) != 0 {
			t.Errorf("problems = %v, want none", problems)
		}
		if *url != "http://from-env/" {
			t.Errorf("kea-url = %q, want the env value", *url)
		}
		if *timeout != 12*time.Second {
			t.Errorf("kea-timeout = %v, want 12s", *timeout)
		}
	})

	t.Run("an explicit flag beats the environment", func(t *testing.T) {
		fs, url, _ := newFS()
		if err := fs.Parse([]string{"-kea-url", "http://from-flag/"}); err != nil {
			t.Fatalf("parse: %v", err)
		}
		applyEnv(fs, env(map[string]string{"KEA_URL": "http://from-env/"}))
		if *url != "http://from-flag/" {
			t.Errorf("kea-url = %q, want the flag value", *url)
		}
	})

	t.Run("every bad value is reported, not just the last", func(t *testing.T) {
		fs, _, timeout := newFS()
		if err := fs.Parse(nil); err != nil {
			t.Fatalf("parse: %v", err)
		}
		problems := applyEnv(fs, env(map[string]string{"KEA_TIMEOUT": "twelve", "LOG_LEVEL": ""}))
		if len(problems) != 1 {
			t.Fatalf("problems = %v, want exactly 1", problems)
		}
		if !strings.Contains(problems[0].Error(), "KEA_TIMEOUT") {
			t.Errorf("problem does not name the variable: %v", problems[0])
		}
		if *timeout != 5*time.Second {
			t.Errorf("kea-timeout = %v, want the default kept after a bad value", *timeout)
		}
	})

	t.Run("every flag has an env equivalent", func(t *testing.T) {
		fs, _, _ := newFS()
		fs.VisitAll(func(f *flag.Flag) {
			if _, ok := envForFlag[f.Name]; !ok {
				t.Errorf("flag --%s has no entry in envForFlag", f.Name)
			}
		})
	})
}

func TestServeUntilSignalDrainsInFlightRequests(t *testing.T) {
	// The point of the grace period: a request already being served must
	// finish, and shutting down must not be reported as a failure.
	started := make(chan struct{})
	mux := http.NewServeMux()
	mux.HandleFunc("/slow", func(w http.ResponseWriter, _ *http.Request) {
		close(started)
		time.Sleep(150 * time.Millisecond)
		_, _ = w.Write([]byte("done"))
	})
	srv := &http.Server{Addr: "127.0.0.1:0", Handler: mux, ReadHeaderTimeout: time.Second}

	// Bind first so the test knows the port, then hand the server the address.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	srv.Addr = addr

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

	<-started
	cancel() // signal arrives mid-request

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
	mux := http.NewServeMux()
	release := make(chan struct{})
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
	<-started
	cancel()

	if err := <-done; err != nil {
		t.Errorf("serveUntilSignal returned %v, want nil when only the grace expired", err)
	}
	close(release)
}

func TestServeUntilSignalReportsABindFailure(t *testing.T) {
	// A port already in use must surface as an error, not as a "listening"
	// line followed by silence.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()

	srv := &http.Server{Addr: ln.Addr().String(), ReadHeaderTimeout: time.Second}
	if err := serveUntilSignal(t.Context(), srv, time.Second, nil); err == nil {
		t.Error("serveUntilSignal returned nil for an address already in use")
	}
}

func TestUnhandledStatisticsAreCounted(t *testing.T) {
	// The names only reach the debug log, so the count has to be a metric --
	// otherwise drift after a Kea upgrade needs a restart at a higher log
	// level to discover.
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

func TestEmptyResponseArrayIsAnError(t *testing.T) {
	// Kea wraps every response in an array with one entry per service the
	// command reached. A zero-length list therefore carries no result at all
	// and must not be read as "succeeded with nothing to report".
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[]`))
	}))
	defer srv.Close()

	k := &keaClient{url: srv.URL, client: srv.Client()}
	_, err := k.call(t.Context(), "status-get")
	if err == nil {
		t.Fatal("call succeeded against an empty response array")
	}
	if !strings.Contains(err.Error(), "empty response array") {
		t.Errorf("error does not explain the empty array: %v", err)
	}
}

func TestCollectDoesNotDependOnAChannelBuffer(t *testing.T) {
	// Builds the channel here rather than calling collectAll: routing through
	// the helper under test cannot demonstrate anything about the helper. The
	// property is that Collect completes against an UNBUFFERED channel, which
	// is what a registry Gather does and what the old fixed-size buffers only
	// approximated.
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
	c.Collect(ch)
	close(ch)

	var n int
	select {
	case n = <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Collect did not complete against an unbuffered channel")
	}

	// A floor, not an equality: adding metrics is expected, losing them is a
	// regression. 29 is what the real fixture produces today.
	const emittedToday = 29
	if n < emittedToday {
		t.Errorf("collector emitted %d metrics, want at least %d", n, emittedToday)
	}
}
