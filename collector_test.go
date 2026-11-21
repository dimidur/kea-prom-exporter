package main

import (
	"encoding/json"
	"log/slog"
	"reflect"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	dto "github.com/prometheus/client_model/go"
)

// Tests for collector.go. The helpers they share with the rest of the suite
// are in helpers_test.go.

func TestEveryDescriptorIsInDescribe(t *testing.T) {
	// Describe sends descriptors.all, so a descriptor field filled without
	// newDescriptors' add would be collected but never described.

	// Arrange
	descType := reflect.TypeFor[*prometheus.Desc]()

	// Act
	set := newDescriptors()

	// Assert
	listed := map[uintptr]int{}
	for _, desc := range set.all {
		listed[reflect.ValueOf(desc).Pointer()]++
	}
	value := reflect.ValueOf(set)
	timesListed := map[string]int{}
	for i := range value.NumField() {
		if field := value.Field(i); field.Type() == descType {
			timesListed[value.Type().Field(i).Name] = listed[field.Pointer()]
		}
	}
	if len(timesListed) == 0 || len(set.all) != len(timesListed) {
		t.Errorf("all holds %d descriptors for %d descriptor fields", len(set.all), len(timesListed))
	}
	for name, count := range timesListed {
		if count != 1 {
			t.Errorf("descriptor field %s appears %d times in all, want once", name, count)
		}
	}
}

func TestCollectAgainstRealKeaOutput(t *testing.T) {
	// The values the exporter derives from a real Kea capture, for the subnet
	// and HA families. Exact values, not "the family is non-empty": a
	// presence check passes for a mislabelled aggregate, such as pkt4-sent
	// emitted as type="sent" beside the real types, which doubles any sum().

	// Arrange
	kea := healthyKea(t)
	collector := newCollector(kea, testReport(quietLogger))
	subnetExpected := `
# HELP kea_dhcp4_addresses_assigned Currently assigned IPv4 addresses in the subnet. Includes declined addresses: Kea keeps them assigned so pool-utilisation stays meaningful, so do not add kea_dhcp4_addresses_declined to this.
# TYPE kea_dhcp4_addresses_assigned gauge
kea_dhcp4_addresses_assigned{subnet="1"} 23
# HELP kea_dhcp4_addresses_capacity Pool size: total IPv4 addresses in the subnet's pools.
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
# HELP kea_dhcp4_ha_partner_in_touch 1 once Kea has a state for the HA partner, including unavailable after a failed heartbeat, else 0.
# TYPE kea_dhcp4_ha_partner_in_touch gauge
kea_dhcp4_ha_partner_in_touch{peer="kea-standby",relationship="kea-primary"} 1
# HELP kea_dhcp4_ha_partner_last_contact_seconds Seconds since this peer's last successful heartbeat to the HA partner, or since the HA service started if none has succeeded. Absent until Kea has a state for the partner.
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
	collector := newCollector(kea, testReport(quietLogger))

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
	collector := newCollector(kea, testReport(quietLogger))

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
	for _, testCase := range tests {
		t.Run(testCase.timeout.String(), func(t *testing.T) {
			// Arrange
			timeout := testCase.timeout

			// Act
			got := maxScrapeDuration(timeout)

			// Assert
			if got != testCase.want {
				t.Errorf("maxScrapeDuration(%v) = %v, want %v", timeout, got, testCase.want)
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
	want := time.Duration(len(original)+1) * time.Second

	// Act
	got := maxScrapeDuration(time.Second)

	// Assert
	if got != want {
		t.Errorf("maxScrapeDuration(1s) with one more command = %v, want %v", got, want)
	}
}

func TestPacketTypesDoNotIncludeTheAggregate(t *testing.T) {
	// pkt4-sent is the total of the per-type series (docs/design.md, "Totals of
	// other series are not exported"), so emitting it as type="sent" would
	// double sum(). This capture sent only offers and ACKs, so there the
	// per-type series sum to exactly the total.

	// Arrange
	aggregate, kind := newestSample(fixtureStatistics[json.RawMessage](t, "statistic-get-all.json")["pkt4-sent"])
	if kind != sampleNumber {
		t.Fatal("fixture has no pkt4-sent aggregate to guard against")
	}
	kea := healthyKea(t)
	collector := newCollector(kea, testReport(quietLogger))

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
		t.Errorf("per-type packets sum to %v, Kea's aggregate is %v; on this capture they agree", sum, aggregate)
	}
}

func TestMetricNamesSatisfyPromlint(t *testing.T) {
	// Catches convention breaches automatically, such as a gauge carrying the
	// _total suffix, which is reserved for counters.

	// Arrange
	kea := healthyKea(t)
	collector := newCollector(kea, testReport(quietLogger))

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

func TestScrapeErrorCountSurvivesASuccess(t *testing.T) {
	// kea_scrape_errors_total is a counter: a successful scrape after failed
	// ones must keep the count, because a drop reads to Prometheus as a
	// counter reset and corrupts rate() and increase().

	// Arrange: a command that has failed three times before.
	collector := newCollector(nil, testReport(quietLogger))
	var failures atomic.Uint64
	failures.Store(3)
	emitted := make(chan prometheus.Metric, 2)

	// Act
	collector.emitCommandHealth(emitted, cmdStatusGet, nil, &failures)

	// Assert
	var up, errorCount dto.Metric
	if err := (<-emitted).Write(&up); err != nil {
		t.Fatalf("write kea_command_up: %v", err)
	}
	if err := (<-emitted).Write(&errorCount); err != nil {
		t.Fatalf("write kea_scrape_errors_total: %v", err)
	}
	if got := up.GetGauge().GetValue(); got != 1 {
		t.Errorf("kea_command_up = %v after a success, want 1", got)
	}
	if got := errorCount.GetCounter().GetValue(); got != 3 {
		t.Errorf("kea_scrape_errors_total = %v after a success, want the 3 earlier failures kept", got)
	}
}

func TestPartialFailureIsVisible(t *testing.T) {
	// When one command fails and the rest succeed, kea_up must be 0, since
	// the alternative is reporting healthy while part of the metric set is
	// missing. The per-command series and the log must blame the command that
	// failed, not the one that worked.
	tests := []struct {
		failing, working   string
		statistics, status string // fixtures; empty fails the command
	}{
		{cmdStatisticGetAll, cmdStatusGet, "", "status-get.json"},
		{cmdStatusGet, cmdStatisticGetAll, "statistic-get-all.json", ""},
	}
	for _, testCase := range tests {
		t.Run(testCase.failing+" fails", func(t *testing.T) {
			// Arrange
			kea := newFakeKea(fixtureOrNone(t, testCase.statistics), fixtureOrNone(t, testCase.status))
			logger, logs := capturingLogger(slog.LevelInfo)
			collector := newCollector(kea, testReport(logger))
			want := map[string]float64{
				"kea_up": 0,
				"kea_command_up{" + testCase.failing + "}":          0,
				"kea_command_up{" + testCase.working + "}":          1,
				"kea_scrape_errors_total{" + testCase.failing + "}": 1,
				"kea_scrape_errors_total{" + testCase.working + "}": 0,
			}

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
			for name, value := range want {
				if actual, ok := got[name]; !ok || actual != value {
					t.Errorf("%s = %v (present %v), want %v", name, actual, ok, value)
				}
			}
			if logged := logs.String(); !strings.Contains(logged, "command="+testCase.failing) || strings.Contains(logged, "command="+testCase.working) {
				t.Errorf("log = %q, want the failure logged against %s only", logged, testCase.failing)
			}
		})
	}
}

func TestScrapeSucceedsAgainstRealOutput(t *testing.T) {
	// kea_up is the metric alerts are built on; against a healthy Kea it
	// must say 1.

	// Arrange
	kea := healthyKea(t)
	collector := newCollector(kea, testReport(quietLogger))

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
	collector := newCollector(kea, testReport(quietLogger))

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
	collector := newCollector(kea, testReport(logger))

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
	// The per-pool keys must be skipped (docs/design.md, "Totals of other
	// series are not exported").

	// Arrange
	statistics := fixtureStatistics[json.RawMessage](t, "statistic-get-all.json")
	var perPool int
	for key := range statistics {
		if strings.Contains(key, ".pool[") {
			perPool++
		}
	}
	if perPool == 0 {
		t.Fatal("fixture lost its per-pool statistics; this guard is now vacuous")
	}
	// One assigned-addresses series per subnet, never per pool.
	subnets := map[string]struct{}{}
	for key := range statistics {
		if strings.HasPrefix(key, "subnet[") && strings.HasSuffix(key, "].assigned-addresses") {
			subnets[key[:strings.Index(key, "]")]] = struct{}{}
		}
	}
	wantSeries := len(subnets)
	kea := healthyKea(t)
	collector := newCollector(kea, testReport(quietLogger))

	// Act
	families := gather(t, collector)

	// Assert
	series := -1
	for _, family := range families {
		if family.GetName() == "kea_dhcp4_addresses_assigned" {
			series = len(family.GetMetric())
		}
	}
	if series != wantSeries {
		t.Errorf("kea_dhcp4_addresses_assigned has %d series (-1: absent), want %d, one per subnet with per-pool keys excluded",
			series, wantSeries)
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
	collector := newCollector(kea, testReport(quietLogger))
	const scrapes = 8

	// Act
	var running sync.WaitGroup
	for range scrapes {
		running.Go(func() { collectAll(collector) })
	}
	running.Wait()

	// Assert
	if got := collector.haErrorCount.Load(); got != scrapes {
		t.Errorf("haErrorCount = %d after %d failing scrapes, want %d", got, scrapes, scrapes)
	}
}

func TestAMappedStatisticWithNoValueIsLeftOutNotUnhandled(t *testing.T) {
	// A mapped statistic with no value to report (docs/design.md, "Statistics
	// the exporter cannot export") is left out: exporting it anyway would
	// publish a confident 0. Its readable sibling still is exported. Its
	// mapping is complete, so counting it as unhandled would send the reader
	// looking for a gap that does not exist.
	tests := []struct {
		name, samples string
	}{
		{"empty sample list", `[]`},
		{"null value", `[[null,"2026-08-02 20:04:57"]]`},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			// Arrange
			stats := []byte(`[{"result":0,"arguments":{
			  "subnet[1].assigned-addresses":` + testCase.samples + `,
			  "subnet[1].total-addresses":[[256,"2026-08-02 20:04:57"]]}}]`)
			kea := newFakeKea(stats, fixture(t, "status-get.json"))
			logger, logs := capturingLogger(slog.LevelDebug)
			collector := newCollector(kea, testReport(logger))
			wantSeries := map[string]int{"kea_dhcp4_addresses_assigned": 0, "kea_dhcp4_addresses_capacity": 1}

			// Act
			families := gather(t, collector)

			// Assert
			series := map[string]int{}
			for _, family := range families {
				series[family.GetName()] = len(family.GetMetric())
			}
			for name, want := range wantSeries {
				if series[name] != want {
					t.Errorf("%s has %d series, want %d", name, series[name], want)
				}
			}
			if got := sumOf(families, "kea_exporter_unhandled_statistics"); got != 0 {
				t.Errorf("kea_exporter_unhandled_statistics = %v, want 0 for a mapped statistic with no value", got)
			}
			if !strings.Contains(logs.String(), `msg="statistic has no value; not exported this scrape" statistic=subnet[1].assigned-addresses`) {
				t.Errorf("log = %q, want the missing value named at debug", logs.String())
			}
		})
	}
}

func TestOnlyAMappedStatisticHasItsValueRead(t *testing.T) {
	// A mapped statistic Kea now reports as something other than a number is
	// drift worth counting. An ignored statistic is not exported whatever it
	// holds, so its value type is no reason to report it.

	// Arrange
	stats := []byte(`[{"result":0,"arguments":{
	  "subnet[1].assigned-addresses":[["text","2026-08-02 20:04:57"]],
	  "pkt4-received":[["text","2026-08-02 20:04:57"]]}}]`)
	kea := newFakeKea(stats, fixture(t, "status-get.json"))
	logger, logs := capturingLogger(slog.LevelDebug)
	collector := newCollector(kea, testReport(logger))
	wantUnhandled := []string{"subnet[1].assigned-addresses"}

	// Act
	unhandled := sumFamily(t, collector, "kea_exporter_unhandled_statistics")

	// Assert
	if unhandled != float64(len(wantUnhandled)) {
		t.Errorf("kea_exporter_unhandled_statistics = %v, want %d", unhandled, len(wantUnhandled))
	}
	if logged := loggedUnhandled(logs.String()); !slices.Equal(logged, wantUnhandled) {
		t.Errorf("logged unhandled statistics %v, want %v", logged, wantUnhandled)
	}
}

func TestAMalformedSubnetKeyDoesNotFailTheScrape(t *testing.T) {
	// A key that only resembles subnet[N].<name> is unhandled, not read as
	// one, which would duplicate the real key's series and fail the scrape
	// (docs/design.md, "Defensive choices").

	// Arrange
	stats := []byte(`[{"result":0,"arguments":{
	  "subnet[1].assigned-addresses":[[23,"2026-08-02 20:04:57"]],
	  "subnet[1]_assigned-addresses":[[23,"2026-08-02 20:04:57"]]}}]`)
	kea := newFakeKea(stats, fixture(t, "status-get.json"))
	collector := newCollector(kea, testReport(quietLogger))

	// Act
	unhandled := sumFamily(t, collector, "kea_exporter_unhandled_statistics")

	// Assert
	if unhandled != 1 {
		t.Errorf("kea_exporter_unhandled_statistics = %v, want 1 for the malformed key", unhandled)
	}
}

func TestUnhandledSetNotesEachKeyOnce(t *testing.T) {
	// Each unhandled statistic is logged once, on the scrape that first
	// reports it, and kea_exporter_unhandled_statistics counts distinct keys,
	// not sightings. The zero value is an empty set.

	// Arrange
	var set unhandledSet
	keys := []string{"reclaimed-leases", "reclaimed-leases", "v4-lease-reuses"}
	wantNew := []bool{true, false, true}
	const wantCount = 2

	// Act
	var gotNew []bool
	for _, key := range keys {
		gotNew = append(gotNew, set.note(key))
	}

	// Assert
	if !slices.Equal(gotNew, wantNew) {
		t.Errorf("note reported new = %v, want %v", gotNew, wantNew)
	}
	if got := set.count(); got != wantCount {
		t.Errorf("count = %d, want %d distinct keys", got, wantCount)
	}
}

func TestUnhandledStatisticsAreCounted(t *testing.T) {
	// The names only reach the debug log, so the count has to be a metric:
	// otherwise drift after a Kea upgrade needs a restart at a higher log
	// level to discover.

	// Arrange
	kea := healthyKea(t)
	logger, logs := capturingLogger(slog.LevelDebug)
	collector := newCollector(kea, testReport(logger))

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
	if logged := float64(len(loggedUnhandled(logs.String()))); got != logged {
		t.Errorf("gauge = %v, but %v unhandled statistics were logged", got, logged)
	}
}

func TestCollectDoesNotDependOnAChannelBuffer(t *testing.T) {
	// Collect must complete against an UNBUFFERED channel, which is what a
	// registry Gather gives it. This builds the channel itself rather than
	// calling collectAll: routing through the helper under test cannot
	// demonstrate anything about the helper.

	// Arrange
	kea := healthyKea(t)
	collector := newCollector(kea, testReport(quietLogger))
	// A floor, not an equality: adding metrics is expected, losing them is a
	// regression. emittedToday is what the real fixture produces today; raise
	// it when metrics are added, since a floor below today's count silently
	// tolerates losing them.
	const emittedToday = 46
	metrics := make(chan prometheus.Metric)
	done := make(chan int, 1)
	go func() {
		count := 0
		for range metrics {
			count++
		}
		done <- count
	}()

	// Act
	collector.Collect(metrics)
	close(metrics)
	count := receive(t, done, "Collect to complete against an unbuffered channel")

	// Assert
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
	build := buildID{version: "v1.2.3", revision: "cafebabe", goVersion: "go1.2.3"}
	collector := newCollector(kea, selfReport{log: quietLogger, build: build, now: stoppedClock})
	wantBuildLabels := map[string]string{"version": "v1.2.3", "revision": "cafebabe", "goversion": "go1.2.3"}

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
			for name, value := range wantBuildLabels {
				if labels[name] != value {
					t.Errorf("build_info %s = %q, want %q", name, labels[name], value)
				}
			}
			if got := metrics[0].GetGauge().GetValue(); got != 1 {
				t.Errorf("build_info value = %v, want 1 (info-metric convention)", got)
			}
		case "kea_scrape_duration_seconds":
			sawDuration = true
		}
	}
	if !sawBuild {
		t.Error("kea_exporter_build_info was not exported")
	}
	if !sawDuration {
		t.Error("kea_scrape_duration_seconds was not exported")
	}
}

func TestScrapeDurationSpansEveryCommand(t *testing.T) {
	// kea_scrape_duration_seconds says whether the exporter or Kea is the
	// slow part, so it runs from before the first command to after the last.
	// A scrape that failed or timed out is exactly the one worth timing, so
	// the measurement must not be conditional on success.
	tests := []struct {
		name               string
		statistics, status string // fixtures; empty fails the command
	}{
		{"Kea up", "statistic-get-all.json", "status-get.json"},
		{"Kea down", "", ""},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			// Arrange: every command takes one second of a clock that moves
			// only then.
			clock := &testClock{}
			kea := newFakeKea(fixtureOrNone(t, testCase.statistics), fixtureOrNone(t, testCase.status))
			kea.onCall = func(string) { clock.advance(time.Second) }
			collector := newCollector(kea, selfReport{log: quietLogger, build: testBuild, now: clock.now})
			want := float64(len(scrapeCommands))

			// Act
			got := sumFamily(t, collector, "kea_scrape_duration_seconds")

			// Assert
			if got != want {
				t.Errorf("kea_scrape_duration_seconds = %v, want %v, one second per command", got, want)
			}
		})
	}
}

func TestExportedMetricSetIsExactlyTheContract(t *testing.T) {
	// Metric names and label sets are this exporter's public API: renaming or
	// dropping one breaks dashboards and alerts downstream. The per-family
	// CollectAndCompare tests each name the families they check, so a metric
	// added or removed elsewhere fails none of them. This pins the whole set,
	// so a change to it has to be a deliberate edit here rather than a side
	// effect noticed after release.

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
	kea := healthyKea(t)
	collector := newCollector(kea, testReport(quietLogger))

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
		slices.Sort(labels)
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
