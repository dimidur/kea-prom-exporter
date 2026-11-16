package main

import (
	"log/slog"
	"reflect"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

// Tests for collector.go. The helpers they share with the rest of the suite
// are in helpers_test.go.

func TestCollectAgainstRealKeaOutput(t *testing.T) {
	// The values the exporter derives from a real Kea capture, for the subnet
	// and HA families. Exact values, not "the family is non-empty": a
	// presence check passes for a mislabelled aggregate, such as pkt4-sent
	// emitted as type="sent" beside the real types, which doubles any sum().

	// Arrange
	kea := healthyKea(t)
	collector := newCollector(kea, quietLogger)
	subnetExpected := `
# HELP kea_dhcp4_addresses_assigned Currently assigned IPv4 addresses in the pool. Includes declined addresses: Kea keeps them assigned so pool-utilisation stays meaningful, so do not add kea_dhcp4_addresses_declined to this.
# TYPE kea_dhcp4_addresses_assigned gauge
kea_dhcp4_addresses_assigned{subnet="1"} 23
# HELP kea_dhcp4_addresses_capacity Pool size: total IPv4 addresses available in the subnet.
# TYPE kea_dhcp4_addresses_capacity gauge
kea_dhcp4_addresses_capacity{subnet="1"} 121
`
	haExpected := `
# HELP kea_dhcp4_ha_communication_interrupted 1 if HA communication with the partner is interrupted, else 0.
# TYPE kea_dhcp4_ha_communication_interrupted gauge
kea_dhcp4_ha_communication_interrupted{peer="kea-standby",relationship="kea-primary"} 0
# HELP kea_dhcp4_ha_enabled 1 when Kea reports an HA relationship, 0 when the HA hook is not loaded.
# TYPE kea_dhcp4_ha_enabled gauge
kea_dhcp4_ha_enabled 1
# HELP kea_dhcp4_ha_local_state_info HA state of this peer (info-metric; value always 1; state in label).
# TYPE kea_dhcp4_ha_local_state_info gauge
kea_dhcp4_ha_local_state_info{mode="hot-standby",peer="kea-standby",relationship="kea-primary",role="primary",state="hot-standby"} 1
# HELP kea_dhcp4_ha_partner_in_touch 1 once this peer has been in contact with its HA partner, else 0.
# TYPE kea_dhcp4_ha_partner_in_touch gauge
kea_dhcp4_ha_partner_in_touch{peer="kea-standby",relationship="kea-primary"} 1
# HELP kea_dhcp4_ha_partner_last_contact_seconds Seconds since the last successful heartbeat from the HA partner. Absent until the partner has been contacted at least once.
# TYPE kea_dhcp4_ha_partner_last_contact_seconds gauge
kea_dhcp4_ha_partner_last_contact_seconds{peer="kea-standby",relationship="kea-primary"} 2
`

	// Act + Assert
	if err := testutil.CollectAndCompare(collector, strings.NewReader(subnetExpected),
		"kea_dhcp4_addresses_assigned", "kea_dhcp4_addresses_capacity"); err != nil {
		t.Errorf("subnet metrics: %v", err)
	}
	if err := testutil.CollectAndCompare(collector, strings.NewReader(haExpected),
		"kea_dhcp4_ha_local_state_info", "kea_dhcp4_ha_communication_interrupted",
		"kea_dhcp4_ha_enabled", "kea_dhcp4_ha_partner_in_touch",
		"kea_dhcp4_ha_partner_last_contact_seconds"); err != nil {
		t.Errorf("HA metrics: %v", err)
	}
}

func TestCollectSendsExactlyScrapeCommands(t *testing.T) {
	// maxScrapeDuration, and through it the shutdown grace, counts the
	// commands in scrapeCommands. A command Collect sends without being listed
	// there would let a shutdown cut off a scrape still in flight.

	// Arrange
	kea := healthyKea(t)
	collector := newCollector(kea, quietLogger)

	// Act
	collectAll(collector)

	// Assert
	var sent []string
	for _, call := range kea.recorded() {
		sent = append(sent, call.command)
	}
	if !slices.Equal(sent, scrapeCommands) {
		t.Errorf("Collect sent %v, want exactly scrapeCommands %v", sent, scrapeCommands)
	}
}

func TestCollectLeavesEachCommandItsOwnDeadline(t *testing.T) {
	// keaCaller's contract is that each call sets its own deadline and the
	// caller passes a context without one. A deadline Collect set would be
	// shared by every command, so a slow first command would eat into the
	// time of the ones after it. This is the caller's half of that contract.

	// Arrange
	kea := healthyKea(t)
	collector := newCollector(kea, quietLogger)

	// Act
	collectAll(collector)

	// Assert
	calls := kea.recorded()
	if len(calls) == 0 {
		t.Fatal("Collect sent no commands")
	}
	for _, call := range calls {
		if call.hadDeadline {
			t.Errorf("%s reached the client with a deadline already on its context", call.command)
		}
		if call.ctxErr != nil {
			t.Errorf("%s reached the client with its context already done: %v", call.command, call.ctxErr)
		}
	}
}

func TestMaxScrapeDurationCoversEveryCommandAtItsDeadline(t *testing.T) {
	// The shutdown grace is built on this bound. A scrape whose commands each
	// run to their deadline must fit inside it, including when the timeout
	// falls back because it was not set.
	tests := []struct {
		timeout time.Duration
		want    time.Duration
	}{
		{0, 10 * time.Second}, // every command at the 5s fallback
		{time.Second, 2 * time.Second},
		{5 * time.Second, 10 * time.Second},
	}
	for _, tc := range tests {
		t.Run(tc.timeout.String(), func(t *testing.T) {
			// Arrange
			timeout := tc.timeout

			// Act
			got := maxScrapeDuration(timeout)

			// Assert
			if got != tc.want {
				t.Errorf("maxScrapeDuration(%v) = %v, want %v", timeout, got, tc.want)
			}
		})
	}
}

func TestMaxScrapeDurationFollowsScrapeCommands(t *testing.T) {
	// A command added to scrapeCommands lengthens every scrape, so the bound,
	// and the shutdown grace built on it, must grow with the list.

	// Arrange
	original := scrapeCommands
	t.Cleanup(func() { scrapeCommands = original })
	scrapeCommands = append(slices.Clone(original), "extra-command")

	// Act
	got := maxScrapeDuration(time.Second)

	// Assert
	if want := time.Duration(len(original)+1) * time.Second; got != want {
		t.Errorf("maxScrapeDuration(1s) with one more command = %v, want %v", got, want)
	}
}

func TestPacketTypesDoNotIncludeTheAggregate(t *testing.T) {
	// Kea reports pkt4-sent as the grand total alongside pkt4-offer-sent and
	// pkt4-ack-sent. Emitting it as type="sent" makes sum() return double.

	// Arrange
	aggregate, ok := mostRecentValue(fixtureArguments(t, "statistic-get-all.json")["pkt4-sent"])
	if !ok {
		t.Fatal("fixture has no pkt4-sent aggregate to guard against")
	}
	kea := healthyKea(t)
	collector := newCollector(kea, quietLogger)

	// Act
	families := gather(t, collector)

	// Assert
	var sum float64
	for _, family := range families {
		if family.GetName() != "kea_dhcp4_packets_sent_total" {
			continue
		}
		for _, metric := range family.GetMetric() {
			for _, label := range metric.GetLabel() {
				if label.GetName() == "type" && (label.GetValue() == "sent" || label.GetValue() == "received") {
					t.Errorf("aggregate exported as a packet type: type=%q", label.GetValue())
				}
			}
			sum += metric.GetCounter().GetValue()
		}
	}
	if sum != aggregate {
		t.Errorf("per-type packets sum to %v, Kea's aggregate is %v; they must agree", sum, aggregate)
	}
}

func TestMetricNamesSatisfyPromlint(t *testing.T) {
	// Catches convention breaches automatically, such as a gauge carrying the
	// _total suffix, which is reserved for counters.

	// Arrange
	kea := healthyKea(t)
	collector := newCollector(kea, quietLogger)

	// Act
	problems, err := testutil.CollectAndLint(collector)
	if err != nil {
		t.Fatalf("lint: %v", err)
	}

	// Assert
	for _, problem := range problems {
		t.Errorf("promlint: %s: %s", problem.Metric, problem.Text)
	}
}

func TestPartialFailureIsVisible(t *testing.T) {
	// When one command fails and the rest succeed, kea_up must be 0, since
	// the alternative is reporting healthy while part of the metric set is
	// missing. The per-command series and the log must blame the command that
	// failed, not the one that worked.
	statistics := fixture(t, "statistic-get-all.json")
	status := fixture(t, "status-get.json")
	tests := []struct {
		failing, working   string
		statistics, status []byte
	}{
		{cmdStatisticGetAll, cmdStatusGet, nil, status},
		{cmdStatusGet, cmdStatisticGetAll, statistics, nil},
	}
	for _, tc := range tests {
		t.Run(tc.failing+" fails", func(t *testing.T) {
			// Arrange
			kea := newFakeKea(tc.statistics, tc.status)
			logger, logs := capturingLogger(slog.LevelInfo)
			collector := newCollector(kea, logger)

			// Act
			families := gather(t, collector)

			// Assert
			got := map[string]float64{}
			for _, family := range families {
				for _, metric := range family.GetMetric() {
					name := family.GetName()
					for _, label := range metric.GetLabel() {
						if label.GetName() == "command" {
							name += "{" + label.GetValue() + "}"
						}
					}
					if gauge := metric.GetGauge(); gauge != nil {
						got[name] = gauge.GetValue()
					}
					if counter := metric.GetCounter(); counter != nil {
						got[name] = counter.GetValue()
					}
				}
			}
			want := map[string]float64{
				"kea_up":                                      0,
				"kea_command_up{" + tc.failing + "}":          0,
				"kea_command_up{" + tc.working + "}":          1,
				"kea_scrape_errors_total{" + tc.failing + "}": 1,
				"kea_scrape_errors_total{" + tc.working + "}": 0,
			}
			for name, value := range want {
				if actual, ok := got[name]; !ok || actual != value {
					t.Errorf("%s = %v (present %v), want %v", name, actual, ok, value)
				}
			}
			if log := logs.String(); !strings.Contains(log, "command="+tc.failing) || strings.Contains(log, "command="+tc.working) {
				t.Errorf("log = %q, want the failure logged against %s only", log, tc.failing)
			}
		})
	}
}

func TestScrapeSucceedsAgainstRealOutput(t *testing.T) {
	// kea_up is the metric alerts are built on; against a healthy Kea it
	// must say 1.

	// Arrange
	kea := healthyKea(t)
	collector := newCollector(kea, quietLogger)

	// Act + Assert
	if got := testutil.ToFloat64(upOnly{collector}); got != 1 {
		t.Errorf("kea_up = %v, want 1 when every command succeeds", got)
	}
}

func TestUnreachableKeaReportsDownRatherThanFailing(t *testing.T) {
	// A dead control socket must produce kea_up 0, not a panic and not an
	// empty scrape: "down" has to be observable.

	// Arrange
	kea := newFakeKea(nil, nil)
	collector := newCollector(kea, quietLogger)

	// Act + Assert
	if got := testutil.ToFloat64(upOnly{collector}); got != 0 {
		t.Errorf("kea_up = %v, want 0 when the control socket errors", got)
	}
}

func TestUndecodableStatisticsFailTheScrape(t *testing.T) {
	// statistic-get-all arguments that do not decode are a broken scrape, not
	// an empty statistic set: kea_up 1 with no lease metrics would hide it,
	// and the log must say whose reply was unusable.

	// Arrange
	statistics := []byte(`[{"result":0,"arguments":["not-an-object"]}]`)
	kea := newFakeKea(statistics, fixture(t, "status-get.json"))
	logger, logs := capturingLogger(slog.LevelInfo)
	collector := newCollector(kea, logger)

	// Act
	got := testutil.ToFloat64(upOnly{collector})

	// Assert
	if got != 0 {
		t.Errorf("kea_up = %v when statistic-get-all could not be decoded, want 0", got)
	}
	if !strings.Contains(logs.String(), "decode statistic-get-all arguments") {
		t.Errorf("log = %q, want the decode failure attributed to statistic-get-all", logs.String())
	}
}

func TestPerPoolStatisticsAreNotDoubleCounted(t *testing.T) {
	// Kea 3.x reports both subnet[N].assigned-addresses and
	// subnet[N].pool[M].assigned-addresses. Counting both would inflate
	// utilisation; the per-pool keys must be skipped.

	// Arrange
	statistics := fixtureArguments(t, "statistic-get-all.json")
	var perPool int
	for key := range statistics {
		if strings.Contains(key, ".pool[") {
			perPool++
		}
	}
	if perPool == 0 {
		t.Fatal("fixture lost its per-pool statistics; this guard is now vacuous")
	}
	kea := healthyKea(t)
	collector := newCollector(kea, quietLogger)

	// Act
	families := gather(t, collector)

	// Assert
	// One assigned-addresses series per subnet, never per pool.
	subnets := map[string]struct{}{}
	for key := range statistics {
		if strings.HasPrefix(key, "subnet[") && strings.HasSuffix(key, "].assigned-addresses") {
			subnets[key[:strings.Index(key, "]")]] = struct{}{}
		}
	}
	for _, family := range families {
		if family.GetName() != "kea_dhcp4_addresses_assigned" {
			continue
		}
		if len(family.GetMetric()) != len(subnets) {
			t.Errorf("kea_dhcp4_addresses_assigned has %d series, want %d (one per subnet, per-pool keys excluded)",
				len(family.GetMetric()), len(subnets))
		}
	}
}

func TestConcurrentScrapesAreRaceFree(t *testing.T) {
	// Prometheus may call Collect concurrently when scrapes overlap, so
	// per-collector state must be safe under that. Real statistics make every
	// scrape record the unmapped keys, and a failing status-get makes every
	// scrape count an error, so an unsynchronised write to either is what
	// `go test -race` flags here.

	// Arrange
	kea := newFakeKea(fixture(t, "statistic-get-all.json"), nil)
	collector := newCollector(kea, quietLogger)
	const scrapes = 8

	// Act
	var wg sync.WaitGroup
	for range scrapes {
		wg.Add(1)
		go func() {
			defer wg.Done()
			collectAll(collector)
		}()
	}
	wg.Wait()

	// Assert
	if got := collector.haErrorCount.Load(); got != scrapes {
		t.Errorf("haErrorCount = %d after %d failing scrapes, want %d", got, scrapes, scrapes)
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
	kea := newFakeKea(stats, fixture(t, "status-get.json"))
	logger, logs := capturingLogger(slog.LevelDebug)
	collector := newCollector(kea, logger)

	// Act
	collectAll(collector)

	// Assert
	if unhandled := loggedUnhandled(logs.String()); !slices.Equal(unhandled, []string{"some-string-stat"}) {
		t.Errorf("logged unhandled statistics %v, want exactly some-string-stat once", unhandled)
	}
}

func TestUnhandledStatisticsAreCounted(t *testing.T) {
	// The names only reach the debug log, so the count has to be a metric:
	// otherwise drift after a Kea upgrade needs a restart at a higher log
	// level to discover.

	// Arrange
	kea := healthyKea(t)
	logger, logs := capturingLogger(slog.LevelDebug)
	collector := newCollector(kea, logger)

	// Act
	families := gather(t, collector)

	// Assert
	var got float64
	var found bool
	for _, family := range families {
		if family.GetName() == "kea_exporter_unhandled_statistics" && len(family.GetMetric()) > 0 {
			found = true
			got = family.GetMetric()[0].GetGauge().GetValue()
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
	if want := float64(len(loggedUnhandled(logs.String()))); got != want {
		t.Errorf("gauge = %v, but %v unhandled statistics were logged", got, want)
	}
}

func TestCollectDoesNotDependOnAChannelBuffer(t *testing.T) {
	// Collect must complete against an UNBUFFERED channel, which is what a
	// registry Gather gives it. This builds the channel itself rather than
	// calling collectAll: routing through the helper under test cannot
	// demonstrate anything about the helper.

	// Arrange
	kea := healthyKea(t)
	collector := newCollector(kea, quietLogger)
	ch := make(chan prometheus.Metric)
	done := make(chan int, 1)
	go func() {
		count := 0
		for range ch {
			count++
		}
		done <- count
	}()

	// Act
	collector.Collect(ch)
	close(ch)
	count := receive(t, done, "Collect to complete against an unbuffered channel")

	// Assert
	// A floor, not an equality: adding metrics is expected, losing them is a
	// regression. emittedToday is what the real fixture produces today; raise
	// it when metrics are added, since a floor below today's count silently
	// tolerates losing them.
	const emittedToday = 46
	if count < emittedToday {
		t.Errorf("collector emitted %d metrics, want at least %d", count, emittedToday)
	}
}

func TestBuildInfoAndScrapeDurationAreExported(t *testing.T) {
	// build_info answers the first question in any bug report, and the scrape
	// duration says whether the exporter or Kea is the slow part. Both must
	// be on every scrape, build_info carrying the identity it was given.

	// Arrange
	kea := healthyKea(t)
	collector := newCollector(kea, quietLogger)
	// Per-collector, not a package global: nothing here is shared with any
	// other test.
	collector.build = buildID{version: "v1.2.3", revision: "cafebabe", goVersion: "go1.2.3"}

	// Act
	families := gather(t, collector)

	// Assert
	var sawBuild, sawDuration bool
	for _, family := range families {
		metrics := family.GetMetric()
		if len(metrics) == 0 {
			t.Errorf("%s has no samples", family.GetName())
			continue
		}
		switch family.GetName() {
		case "kea_exporter_build_info":
			sawBuild = true
			labels := map[string]string{}
			for _, label := range metrics[0].GetLabel() {
				labels[label.GetName()] = label.GetValue()
			}
			want := map[string]string{"version": "v1.2.3", "revision": "cafebabe", "goversion": "go1.2.3"}
			for name, value := range want {
				if labels[name] != value {
					t.Errorf("build_info %s = %q, want %q", name, labels[name], value)
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
	kea := newFakeKea(nil, nil)
	collector := newCollector(kea, quietLogger)

	// Act
	metrics := collectAll(collector)

	// Assert
	var found bool
	for _, metric := range metrics {
		if strings.Contains(metric.Desc().String(), "kea_scrape_duration_seconds") {
			found = true
		}
	}
	if !found {
		t.Error("no scrape duration emitted when every command failed")
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
	// request actually fails: a healthy-server fixture would leave those
	// families out of the contract entirely.

	// Arrange
	contract := map[string][]string{
		"kea_command_up":                               {"command"},
		"kea_dhcp4_addresses_assigned":                 {"subnet"},
		"kea_dhcp4_addresses_capacity":                 {"subnet"},
		"kea_dhcp4_addresses_declined":                 {"subnet"},
		"kea_dhcp4_allocation_failures_by_cause_total": {"cause"},
		"kea_dhcp4_allocation_failures_by_scope_total": {"scope"},
		"kea_dhcp4_ha_communication_interrupted":       {"peer", "relationship"},
		"kea_dhcp4_ha_enabled":                         {},
		"kea_dhcp4_ha_local_state_info":                {"mode", "peer", "relationship", "role", "state"},
		"kea_dhcp4_ha_partner_in_touch":                {"peer", "relationship"},
		"kea_dhcp4_ha_partner_last_contact_seconds":    {"peer", "relationship"},
		"kea_dhcp4_ha_partner_state_info":              {"peer", "relationship", "role", "state"},
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
	kea := newFakeKea(fixture(t, "statistic-get-all-with-failures.json"), fixture(t, "status-get.json"))
	collector := newCollector(kea, quietLogger)

	// Act
	families := gather(t, collector)

	// Assert
	exported := map[string][]string{}
	for _, family := range families {
		labels := []string{}
		if len(family.GetMetric()) > 0 {
			for _, label := range family.GetMetric()[0].GetLabel() {
				labels = append(labels, label.GetName())
			}
		}
		sort.Strings(labels)
		exported[family.GetName()] = labels
	}
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
