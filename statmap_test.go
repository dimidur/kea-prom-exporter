package main

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"maps"
	"reflect"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

// Tests for statmap.go: the statistic-name mapping.
//
// Kea symbols named below hold across the supported Kea releases; the
// supported set is in README.md.

// classifyStat is a pure function of the key, so the mapping can be pinned
// exhaustively without constructing a collector or a fake Kea, which a branch
// chain inside an emitter cannot be.

func TestClassifyStat(t *testing.T) {
	// Arrange
	cases := []struct {
		name       string
		key        string
		wantID     statID
		wantLabels []string
	}{
		{"subnet assigned", "subnet[1].assigned-addresses", statAddressesAssigned, []string{"1"}},
		{"subnet capacity", "subnet[1].total-addresses", statAddressesCapacity, []string{"1"}},
		{"multi-digit subnet id", "subnet[42].assigned-addresses", statAddressesAssigned, []string{"42"}},

		// Per-pool keys coexist with per-subnet ones on Kea 3.x; counting both
		// would inflate utilisation.
		{"per-pool is ignored", "subnet[1].pool[0].assigned-addresses", statIgnored, nil},

		// Kea's grand totals equal the sum of the per-type series, so
		// exporting them as a type doubled any sum().
		{"received aggregate is ignored", "pkt4-received", statIgnored, nil},
		{"sent aggregate is ignored", "pkt4-sent", statIgnored, nil},

		{"packet received", "pkt4-discover-received", statPacketsReceived, []string{"discover"}},
		{"packet sent", "pkt4-ack-sent", statPacketsSent, []string{"ack"}},
		// A multi-word op keeps its internal hyphens; only the direction
		// suffix is stripped.
		{"multi-hyphen op", "pkt4-lease-query-received", statPacketsReceived, []string{"lease-query"}},

		{"unknown global", "some-future-statistic", statUnmapped, nil},
		{"unknown subnet suffix", "subnet[1].some-future-thing", statUnmapped, nil},
		{"unknown pkt4 shape", "pkt4-some-future-counter", statUnmapped, nil},
		{"empty key", "", statUnmapped, nil},

		// Malformed shapes must be reported, not skipped: they mean Kea
		// emitted something this parser does not understand.
		{"subnet with no closing bracket", "subnet[1.assigned-addresses", statUnmapped, nil},
		{"subnet truncated after bracket", "subnet[1]", statUnmapped, nil},

		// An empty subnet id still yields one label, because the descriptor
		// has one either way -- passing zero would panic in Gather.
		{"empty subnet id", "subnet[].assigned-addresses", statAddressesAssigned, []string{""}},

		// Kea increments the global and the subnet[N] form of these for the
		// same event, so the global is a sum across subnets. Exporting both
		// would double any sum(); the per-subnet form is kept.
		{"global assigned is the subnet sum", "assigned-addresses", statIgnored, nil},
		{"global declined is the subnet sum", "declined-addresses", statIgnored, nil},

		// Two partitions of one event, hence two metrics: scope and cause each
		// sum to the real total, and classes overlaps both.
		// Taken from the global keys, because Kea does not create the
		// per-subnet ones until the first failure -- a healthy server would
		// otherwise export no series at all.
		{"alloc fail scope shared-network", "v4-allocation-fail-shared-network", statAllocFailByScope, []string{"shared-network"}},
		{"alloc fail scope subnet", "v4-allocation-fail-subnet", statAllocFailByScope, []string{"subnet"}},
		{"alloc fail cause no-pools", "v4-allocation-fail-no-pools", statAllocFailByCause, []string{"no-pools"}},
		{"alloc fail cause exhausted", "v4-allocation-fail", statAllocFailByCause, []string{"attempts-exhausted"}},
		{"per-subnet alloc fail duplicates the global", "subnet[1].v4-allocation-fail", statIgnored, nil},

		// Kea adds the ALL class to every packet, so this counts essentially
		// every failure rather than a classification-driven subset.
		{"alloc fail classes is not exported", "v4-allocation-fail-classes", statIgnored, nil},

		{"declined per subnet", "subnet[1].declined-addresses", statAddressesDeclined, []string{"1"}},

		// The reasons are an exact partition of the total: Kea records the
		// reason, then its caller bumps the drop.
		{"drop total", "pkt4-receive-drop", statPacketsDropped, nil},
		{"drop reason queue-full", "pkt4-queue-full", statDropReason, []string{"queue-full"}},
		{"rfc-violation is a drop reason too", "pkt4-rfc-violation", statDropReason, []string{"rfc-violation"}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Act
			id, labels := classifyStat(tc.key)

			// Assert
			if id != tc.wantID {
				t.Errorf("classifyStat(%q) id = %v, want %v", tc.key, id, tc.wantID)
			}
			if !reflect.DeepEqual(labels, tc.wantLabels) {
				t.Errorf("classifyStat(%q) labels = %#v, want %#v", tc.key, labels, tc.wantLabels)
			}
		})
	}
}

// mappedIDs derives the set of statIDs the classifier can actually return,
// from the tables themselves. Restating it by hand is what made an earlier
// version of the descriptor test inert: both halves had to be edited
// together, which is precisely the coupling the test exists to catch.
func mappedIDs() map[statID]struct{} {
	ids := map[statID]struct{}{}
	for _, table := range []map[string]statTarget{exactStats, subnetStats, packetStats} {
		for _, target := range table {
			if target.id == statUnmapped || target.id == statIgnored {
				continue
			}
			ids[target.id] = struct{}{}
		}
	}
	return ids
}

func TestEveryMappedStatIDHasADescriptor(t *testing.T) {
	// A table row pointing at an ID with no descriptor makes the statistic
	// silently unmapped, which reads as Kea drift rather than as the exporter
	// bug it is.

	// Arrange
	c := newCollector(&keaClient{})

	// Act
	metrics := c.stats

	// Assert
	for id := range mappedIDs() {
		m, ok := metrics[id]
		if !ok {
			t.Errorf("statID %v is reachable from a table but has no descriptor", id)
			continue
		}
		if m.desc == nil {
			t.Errorf("statID %v has a nil descriptor", id)
		}
	}
	for id := range metrics {
		if _, ok := mappedIDs()[id]; !ok {
			t.Errorf("statID %v has a descriptor but no table row can produce it", id)
		}
	}
}

func TestLabelArityMatchesTheDescriptor(t *testing.T) {
	// MustNewConstMetric panics when the label count disagrees with the
	// descriptor, and Registry.Gather runs collectors without recovering it --
	// so this disagreement kills the process on a scrape rather than
	// producing a bad metric. Checked here so a statistic absent from the
	// fixture is covered too.

	// Arrange
	c := newCollector(&keaClient{})
	probes := map[statID]string{
		statAddressesAssigned: "subnet[1].assigned-addresses",
		statAddressesCapacity: "subnet[1].total-addresses",
		statPacketsReceived:   "pkt4-discover-received",
		statPacketsSent:       "pkt4-ack-sent",
		statAddressesDeclined: "subnet[1].declined-addresses",
		statPacketsDropped:    "pkt4-receive-drop",
		statDropReason:        "pkt4-queue-full",
		statAllocFailByScope:  "v4-allocation-fail-subnet",
		statAllocFailByCause:  "v4-allocation-fail",
	}

	for id, m := range c.stats {
		key, ok := probes[id]
		if !ok {
			t.Errorf("statID %v has no probe key; add one so its arity is checked", id)
			continue
		}

		// Act
		gotID, labels := classifyStat(key)

		// Assert
		if gotID != id {
			t.Errorf("probe %q classifies as %v, want %v", key, gotID, id)
			continue
		}
		// Ask client_golang directly rather than parsing Desc.String():
		// NewConstMetric returns exactly the error MustNewConstMetric panics
		// on, so this is the real failure mode rather than a rendering of it.
		if _, err := prometheus.NewConstMetric(m.desc, m.typ, 0, labels...); err != nil {
			t.Errorf("%q yields labels %#v that its descriptor rejects: %v", key, labels, err)
		}
	}
}

func TestTablesAndClassifierAgree(t *testing.T) {
	// The tables are the documented extension point, so a row the classifier
	// cannot reach would be quietly inert.

	// Arrange -- the tables themselves; each subtest pins one of them.
	t.Run("exact names", func(t *testing.T) {
		// Arrange -- written out rather than read back from exactStats, which
		// would make the assertion a restatement of the code under test.
		want := map[string][]string{
			"pkt4-receive-drop":                 nil,
			"pkt4-queue-full":                   {"queue-full"},
			"pkt4-rfc-violation":                {"rfc-violation"},
			"pkt4-not-for-us":                   {"not-for-us"},
			"pkt4-duplicate":                    {"duplicate"},
			"pkt4-limit-exceeded":               {"limit-exceeded"},
			"v4-allocation-fail":                {"attempts-exhausted"},
			"v4-allocation-fail-no-pools":       {"no-pools"},
			"v4-allocation-fail-shared-network": {"shared-network"},
			"v4-allocation-fail-subnet":         {"subnet"},
			"pkt4-parse-failed":                 {"parse-failed"},
			"pkt4-processing-failed":            {"processing-failed"},
			"pkt4-service-disabled":             {"service-disabled"},
			"pkt4-admin-filtered":               {"admin-filtered"},
		}

		for key, wantLabels := range want {
			// Act
			_, labels := classifyStat(key)

			// Assert
			if !reflect.DeepEqual(labels, wantLabels) {
				t.Errorf("classifyStat(%q) labels = %#v, want %#v", key, labels, wantLabels)
			}
		}

		// Every row must still be reachable, whatever its labels.
		for key, target := range exactStats {
			if got, _ := classifyStat(key); got != target.id {
				t.Errorf("exactStats[%q] = %v but classifyStat returns %v", key, target.id, got)
			}
		}

		// And every labelled row must be pinned above. Without this, a new row
		// can be added with no expectation and its label is then free to be
		// wrong -- which is how three drop reasons went unpinned before.
		for key, target := range exactStats {
			if len(target.extra) == 0 {
				continue
			}
			if _, pinned := want[key]; !pinned {
				t.Errorf("exactStats[%q] carries labels %#v but nothing above pins them", key, target.extra)
			}
		}
	})

	t.Run("returned labels do not alias the tables", func(t *testing.T) {
		// Arrange -- checking for corruption via append cannot fail: every
		// `extra` is a composite literal with len == cap, and append on a full
		// slice always reallocates. Compare the backing arrays instead, which
		// goes red the moment the clone is removed.
		const key = "pkt4-queue-full"
		target := exactStats[key]

		// Act
		_, labels := classifyStat(key)

		// Assert
		if len(labels) == 0 || len(target.extra) == 0 {
			t.Fatalf("expected %q to carry a label", key)
		}
		if &labels[0] == &target.extra[0] {
			t.Error("classifyStat returned the package table's own backing array")
		}
	})

	t.Run("subnet suffixes carry the subnet id", func(t *testing.T) {
		for suffix, want := range subnetStats {
			// Arrange
			key := "subnet[7]." + suffix

			// Act
			got, labels := classifyStat(key)

			// Assert
			if got != want.id {
				t.Errorf("subnetStats[%q] = %v but classifyStat(%q) returns %v", suffix, want.id, key, got)
			}
			if want.id == statIgnored {
				if labels != nil {
					t.Errorf("classifyStat(%q) is ignored but carries labels %#v", key, labels)
				}
				continue
			}
			// Exactly one label, the subnet id: these rows carry no extras,
			// so a looser check would let a stray label through.
			if !reflect.DeepEqual(labels, []string{"7"}) {
				t.Errorf("classifyStat(%q) labels = %#v, want exactly the subnet id", key, labels)
			}
		}
	})

	t.Run("packet suffixes carry the operation", func(t *testing.T) {
		for suffix, want := range packetStats {
			// Arrange -- the operation is the only label these carry.
			key := "pkt4-discover" + suffix

			// Act
			got, labels := classifyStat(key)

			// Assert
			if got != want.id {
				t.Errorf("packetStats[%q] = %v but classifyStat(%q) returns %v", suffix, want.id, key, got)
			}
			if !reflect.DeepEqual(labels, []string{"discover"}) {
				t.Errorf("classifyStat(%q) labels = %#v, want exactly the operation", key, labels)
			}
		}
	})
}

func TestAllocationFailureMetricsAgainstAFailingServer(t *testing.T) {
	// The captured fixture cannot cover this: Kea only creates the
	// allocation-failure statistics once a failure has happened, so a healthy
	// capture has them at zero and the per-subnet ones absent entirely. This
	// fixture is derived from the real capture and constructed so both
	// partitions sum to the same real total (10), which is the property the
	// two-metric split exists to preserve.

	// Arrange
	srv := keaStub(t, fixture(t, "statistic-get-all-with-failures.json"), fixture(t, "status-get.json"))
	defer srv.Close()
	c := newCollector(&keaClient{url: srv.URL, client: srv.Client()})

	// Arrange -- the expected exposition. The Act is the Gather inside
	// CollectAndCompare below, which is where an arity mismatch would surface
	// the way it does in production: a panic in an unrecovered goroutine.
	expected := `
# HELP kea_dhcp4_allocation_failures_by_cause_total Server-wide address allocation failures by cause. A second true partition of the SAME failures: no-pools and attempts-exhausted also sum to the total, so do not add this to kea_dhcp4_allocation_failures_by_scope_total.
# TYPE kea_dhcp4_allocation_failures_by_cause_total counter
kea_dhcp4_allocation_failures_by_cause_total{cause="attempts-exhausted"} 7
kea_dhcp4_allocation_failures_by_cause_total{cause="no-pools"} 3
# HELP kea_dhcp4_allocation_failures_by_scope_total Server-wide address allocation failures by where the client was looked up. A true partition: shared-network and subnet sum to the total.
# TYPE kea_dhcp4_allocation_failures_by_scope_total counter
kea_dhcp4_allocation_failures_by_scope_total{scope="shared-network"} 4
kea_dhcp4_allocation_failures_by_scope_total{scope="subnet"} 6
`

	// Act + Assert
	if err := testutil.CollectAndCompare(c, strings.NewReader(expected),
		"kea_dhcp4_allocation_failures_by_scope_total",
		"kea_dhcp4_allocation_failures_by_cause_total"); err != nil {
		t.Errorf("allocation failure metrics: %v", err)
	}

	// The two partitions summing equal is what makes them two metrics, and the
	// golden above already pins all four values -- 4+6 == 3+7 follows from it.
	// A separate sum assertion here detects nothing the golden does not
	// (verified by deleting it and re-running the mutations), so it is stated
	// in the fixture and in the README rather than restated as a test that
	// cannot fail.

	// Declined addresses are non-zero here, so this pins the value and not
	// merely the presence of the family.
	declined := `
# HELP kea_dhcp4_addresses_declined Addresses currently withdrawn from the pool by a DHCPDECLINE.
# TYPE kea_dhcp4_addresses_declined gauge
kea_dhcp4_addresses_declined{subnet="1"} 2
`
	if err := testutil.CollectAndCompare(c, strings.NewReader(declined), "kea_dhcp4_addresses_declined"); err != nil {
		t.Errorf("declined addresses: %v", err)
	}
}

func TestFixtureCarriesThePerSubnetAllocationFailKeys(t *testing.T) {
	// The derived fixture exists for the subnet[N].v4-allocation-fail* keys --
	// the ones Kea does not create until a failure happens. They are
	// deliberately ignored rather than exported, so nothing in the output
	// would notice if they vanished from the file; the fixture's own content
	// has to be checked, as the per-pool guard in collector_test.go does.

	// Arrange
	var raw []struct {
		Arguments map[string]json.RawMessage `json:"arguments"`
	}
	if err := json.Unmarshal(fixture(t, "statistic-get-all-with-failures.json"), &raw); err != nil {
		t.Fatalf("decode fixture: %v", err)
	}

	// Act
	var perSubnetFailures int
	for key := range raw[0].Arguments {
		if strings.HasPrefix(key, "subnet[") && strings.Contains(key, "v4-allocation-fail") {
			perSubnetFailures++
		}
	}

	// Assert
	if perSubnetFailures != 5 {
		t.Errorf("fixture has %d subnet[N].v4-allocation-fail* keys, want 5; without them it covers nothing the real capture does not",
			perSubnetFailures)
	}
}

func TestIgnoredStatisticsAreNotReportedAsUnmapped(t *testing.T) {
	// statIgnored and statUnmapped mean different things to an operator: one
	// is a considered omission, the other is drift in Kea. A regression that
	// reclassified the ignored keys would show up here and nowhere else.

	// Arrange
	srv := keaStub(t, fixture(t, "statistic-get-all-with-failures.json"), fixture(t, "status-get.json"))
	defer srv.Close()
	c := newCollector(&keaClient{url: srv.URL, client: srv.Client()})

	// Act
	unhandled := sumFamily(t, c, "kea_exporter_unhandled_statistics")

	// Assert -- five families the exporter does not map yet, each in a global
	// and a subnet[N] form. The key set rather than the count: a count stays
	// green when one key is reclassified and another appears.
	want := []string{
		"cumulative-assigned-addresses", "reclaimed-declined-addresses",
		"reclaimed-leases", "v4-lease-reuses", "v4-reservation-conflicts",
		"subnet[1].cumulative-assigned-addresses", "subnet[1].reclaimed-declined-addresses",
		"subnet[1].reclaimed-leases", "subnet[1].v4-lease-reuses",
		"subnet[1].v4-reservation-conflicts",
	}
	sort.Strings(want)
	c.unhandledMu.Lock()
	got := slices.Sorted(maps.Keys(c.unhandled))
	c.unhandledMu.Unlock()
	if !reflect.DeepEqual(got, want) {
		t.Errorf("unmapped set is\n  %v\nwant\n  %v", got, want)
	}
	if unhandled != float64(len(want)) {
		t.Errorf("kea_exporter_unhandled_statistics = %v but %d keys are tracked", unhandled, len(want))
	}
}

// TestTablesWithoutExtraSupportDoNotUseIt guards the class, not the instance:
// classifySubnetStat and classifyPacketStat deliberately ignore `extra`, so a
// row that sets one would have its label silently dropped.
func TestTablesWithoutExtraSupportDoNotUseIt(t *testing.T) {
	// Arrange
	tables := map[string]map[string]statTarget{
		"subnetStats": subnetStats,
		"packetStats": packetStats,
	}

	// Act + Assert
	for name, table := range tables {
		for key, target := range table {
			if len(target.extra) != 0 {
				t.Errorf("%s[%q] sets extra=%#v, but its classifier does not consult it -- the label would be dropped silently. Add the handling with the row that needs it.",
					name, key, target.extra)
			}
		}
	}
}

// sumFamily totals every sample in one metric family.
func sumFamily(t *testing.T, c prometheus.Collector, name string) float64 {
	t.Helper()
	reg := prometheus.NewPedanticRegistry()
	if err := reg.Register(c); err != nil {
		t.Fatalf("register: %v", err)
	}
	mfs, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	var total float64
	for _, mf := range mfs {
		if mf.GetName() != name {
			continue
		}
		for _, m := range mf.GetMetric() {
			total += m.GetCounter().GetValue() + m.GetGauge().GetValue()
		}
	}
	return total
}

func TestDropReasonsSumToTheDropTotal(t *testing.T) {
	// Arrange -- both fixtures, so the invariant is checked against Kea's own
	// capture and not only against the derived one.
	// Act + Assert -- both live in the helper, run once per fixture.
	for _, fixtureName := range []string{"statistic-get-all.json", "statistic-get-all-with-failures.json"} {
		t.Run(fixtureName, func(t *testing.T) { assertDropReasonsSumToTotal(t, fixtureName) })
	}
}

func assertDropReasonsSumToTotal(t *testing.T, fixtureName string) {
	t.Helper()
	// Kea records a reason and bumps the drop alongside it, as in
	// Dhcpv4Srv::processPacket, so the two must agree exactly. Several sites
	// bump the total and each records a reason at the same time. If they
	// diverge, either Kea changed or a reason is mapped onto the wrong metric.

	// Arrange -- both fixtures: the real capture is the evidence about Kea,
	// the derived one exercises reasons the capture leaves at zero.
	srv := keaStub(t, fixture(t, fixtureName), fixture(t, "status-get.json"))
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

	var total, reasons float64
	for _, mf := range mfs {
		switch mf.GetName() {
		case "kea_dhcp4_packets_dropped_total":
			total = mf.GetMetric()[0].GetCounter().GetValue()
		case "kea_dhcp4_packets_dropped_by_reason_total":
			for _, m := range mf.GetMetric() {
				reasons += m.GetCounter().GetValue()
			}
		}
	}

	// Assert
	if total == 0 {
		t.Fatal("fixture reports no drops; this guard would be vacuous")
	}
	if reasons != total {
		t.Errorf("drop reasons sum to %v but the total is %v; they must agree", reasons, total)
	}
}

func TestDropMetricsAgainstRealKeaOutput(t *testing.T) {
	// The evidence about Kea has to come from Kea. The real capture carries a
	// live instance of the invariant -- pkt4-service-disabled 4 alongside
	// pkt4-receive-drop 4 -- and every one of the nine drop reasons, because
	// Kea initialises every name in its dhcp4_statistics set to 0 at startup.
	// Pinning the whole family here is what catches a reason mapped to the
	// wrong metric or spelled wrong, which a fixture built to satisfy a sum
	// cannot.

	// Arrange
	srv := keaStub(t, fixture(t, "statistic-get-all.json"), fixture(t, "status-get.json"))
	defer srv.Close()
	c := newCollector(&keaClient{url: srv.URL, client: srv.Client()})

	expected := `
# HELP kea_dhcp4_packets_dropped_by_reason_total Inbound DHCPv4 packets dropped, by reason. These sum to kea_dhcp4_packets_dropped_total, so use one or the other -- adding them together double-counts.
# TYPE kea_dhcp4_packets_dropped_by_reason_total counter
kea_dhcp4_packets_dropped_by_reason_total{reason="admin-filtered"} 0
kea_dhcp4_packets_dropped_by_reason_total{reason="duplicate"} 0
kea_dhcp4_packets_dropped_by_reason_total{reason="limit-exceeded"} 0
kea_dhcp4_packets_dropped_by_reason_total{reason="not-for-us"} 0
kea_dhcp4_packets_dropped_by_reason_total{reason="parse-failed"} 0
kea_dhcp4_packets_dropped_by_reason_total{reason="processing-failed"} 0
kea_dhcp4_packets_dropped_by_reason_total{reason="queue-full"} 0
kea_dhcp4_packets_dropped_by_reason_total{reason="rfc-violation"} 0
kea_dhcp4_packets_dropped_by_reason_total{reason="service-disabled"} 4
# HELP kea_dhcp4_packets_dropped_total Total inbound DHCPv4 packets dropped. Equal to the sum of kea_dhcp4_packets_dropped_by_reason_total, and stays correct when a Kea release adds a reason this exporter does not know.
# TYPE kea_dhcp4_packets_dropped_total counter
kea_dhcp4_packets_dropped_total 4
`

	// Act + Assert
	if err := testutil.CollectAndCompare(c, strings.NewReader(expected),
		"kea_dhcp4_packets_dropped_by_reason_total", "kea_dhcp4_packets_dropped_total"); err != nil {
		t.Errorf("drop metrics: %v", err)
	}
}

func TestIgnoredStatisticsDoNotLogAnExporterBug(t *testing.T) {
	// Deleting emitStat's statIgnored fast-path changes no metric, because
	// ignored statistics have no descriptor and fall through to the
	// "exporter bug" branch. The scrape output is identical; the only
	// difference is one ERROR per ignored statistic per scrape, forever.

	// Arrange
	var buf bytes.Buffer
	srv := keaStub(t, fixture(t, "statistic-get-all-with-failures.json"), fixture(t, "status-get.json"))
	defer srv.Close()
	c := newCollector(&keaClient{url: srv.URL, client: srv.Client()})
	c.log = slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelError}))

	// Act
	collectAll(c)

	// Assert
	if strings.Contains(buf.String(), "exporter bug") {
		t.Errorf("a clean scrape logged an exporter bug; ignored statistics are reaching the no-descriptor branch:\n%s", buf.String())
	}
	if buf.Len() != 0 {
		t.Errorf("a clean scrape logged at error level:\n%s", buf.String())
	}
}

func TestFixturesKeepKeasSampleShape(t *testing.T) {
	// Kea records one sample per increment, capped at 20, newest first. The
	// derived fixture was hand-checked against that rule once; this keeps it
	// true through the next edit, which is the point at which a fixture
	// quietly stops resembling the daemon.

	// Arrange -- the four exceptions are gauges, inherited unmodified from the
	// real capture, where they also break the rule.
	gauges := map[string]bool{
		"subnet[1].pool[0].assigned-addresses": true,
		"subnet[1].pool[0].total-addresses":    true,
		"subnet[1].total-addresses":            true,
		"declined-addresses":                   true,
	}

	for _, name := range []string{"statistic-get-all.json", "statistic-get-all-with-failures.json"} {
		t.Run(name, func(t *testing.T) {
			var raw []struct {
				Arguments map[string][][]json.RawMessage `json:"arguments"`
			}
			if err := json.Unmarshal(fixture(t, name), &raw); err != nil {
				t.Fatalf("decode fixture: %v", err)
			}

			// Act + Assert
			for key, samples := range raw[0].Arguments {
				if len(samples) == 0 {
					t.Errorf("%s: no samples", key)
					continue
				}
				var previous time.Time
				for i, sample := range samples {
					var stamp string
					if err := json.Unmarshal(sample[1], &stamp); err != nil {
						t.Errorf("%s sample %d: timestamp is not a string: %v", key, i, err)
						continue
					}
					at, err := time.Parse("2006-01-02 15:04:05.999999", stamp)
					if err != nil {
						t.Errorf("%s sample %d: unparseable timestamp %q", key, i, stamp)
						continue
					}
					if i > 0 && !at.Before(previous) {
						t.Errorf("%s sample %d: %v is not older than the previous sample %v; Kea reports newest first",
							key, i, at, previous)
					}
					previous = at
				}

				if gauges[key] {
					continue
				}
				var value int
				if err := json.Unmarshal(samples[0][0], &value); err != nil || value < 0 {
					continue // string, null, or float statistic
				}
				if want := min(value+1, 20); len(samples) != want {
					t.Errorf("%s has %d samples for value %d, want %d; Kea records one sample per increment capped at 20",
						key, len(samples), value, want)
				}
			}
		})
	}
}

func TestAllocationFailureSeriesExistOnAHealthyServer(t *testing.T) {
	// This is the property that forced the globals to be used instead of the
	// per-subnet keys. Kea does not create subnet[N].v4-allocation-fail* until
	// something fails, so exporting only those left a healthy server with no
	// series at all -- and an alert cannot tell "no failures" from "exporter
	// broken" when the series is simply absent.
	//
	// The real capture is a healthy server, so it is the right evidence.

	// Arrange
	srv := keaStub(t, fixture(t, "statistic-get-all.json"), fixture(t, "status-get.json"))
	defer srv.Close()
	c := newCollector(&keaClient{url: srv.URL, client: srv.Client()})

	expected := `
# HELP kea_dhcp4_allocation_failures_by_cause_total Server-wide address allocation failures by cause. A second true partition of the SAME failures: no-pools and attempts-exhausted also sum to the total, so do not add this to kea_dhcp4_allocation_failures_by_scope_total.
# TYPE kea_dhcp4_allocation_failures_by_cause_total counter
kea_dhcp4_allocation_failures_by_cause_total{cause="attempts-exhausted"} 0
kea_dhcp4_allocation_failures_by_cause_total{cause="no-pools"} 0
# HELP kea_dhcp4_allocation_failures_by_scope_total Server-wide address allocation failures by where the client was looked up. A true partition: shared-network and subnet sum to the total.
# TYPE kea_dhcp4_allocation_failures_by_scope_total counter
kea_dhcp4_allocation_failures_by_scope_total{scope="shared-network"} 0
kea_dhcp4_allocation_failures_by_scope_total{scope="subnet"} 0
`

	// Act + Assert -- present and zero, not absent.
	if err := testutil.CollectAndCompare(c, strings.NewReader(expected),
		"kea_dhcp4_allocation_failures_by_scope_total",
		"kea_dhcp4_allocation_failures_by_cause_total"); err != nil {
		t.Errorf("allocation failures on a healthy server: %v", err)
	}
}

func TestUnmappedStatisticsAreLoggedOncePerKey(t *testing.T) {
	// noteUnhandled dedups so an unmapped statistic costs one line for the
	// process lifetime, not one per scrape. Removing the dedup changes no
	// metric -- the map write is idempotent -- so only the log shows it.

	// Arrange
	var buf bytes.Buffer
	srv := keaStub(t, fixture(t, "statistic-get-all.json"), fixture(t, "status-get.json"))
	defer srv.Close()
	c := newCollector(&keaClient{url: srv.URL, client: srv.Client()})
	c.log = slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	// Act -- three scrapes over the same statistics.
	collectAll(c)
	collectAll(c)
	collectAll(c)

	// Assert
	lines := strings.Count(buf.String(), "unhandled statistic")
	c.unhandledMu.Lock()
	distinct := len(c.unhandled)
	c.unhandledMu.Unlock()
	if lines != distinct {
		t.Errorf("logged %d unhandled-statistic lines for %d distinct keys over three scrapes; it must be one per key, not one per scrape",
			lines, distinct)
	}
}

func TestUnmappedStatisticsLogAtDebugNotHigher(t *testing.T) {
	// A stock Kea reports ten statistics this exporter does not map. At info
	// they would bury anything that matters on every start.

	// Arrange
	var buf bytes.Buffer
	srv := keaStub(t, fixture(t, "statistic-get-all.json"), fixture(t, "status-get.json"))
	defer srv.Close()
	c := newCollector(&keaClient{url: srv.URL, client: srv.Client()})
	c.log = slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}))

	// Act
	collectAll(c)

	// Assert
	if strings.Contains(buf.String(), "unhandled statistic") {
		t.Errorf("unmapped statistics logged at info or higher:\n%s", buf.String())
	}
}

func TestMissingDescriptorIsAnExporterBugNotKeaDrift(t *testing.T) {
	// If classifyStat names an ID with no descriptor, that is a bug here, not
	// a statistic Kea added. Reporting it through
	// kea_exporter_unhandled_statistics would send the reader to look at the
	// wrong system, so the branch logs and deliberately does not count.

	// Arrange
	srv := keaStub(t, fixture(t, "statistic-get-all.json"), fixture(t, "status-get.json"))
	defer srv.Close()
	var buf bytes.Buffer
	c := newCollector(&keaClient{url: srv.URL, client: srv.Client()})
	c.log = slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelError}))
	before := sumFamily(t, c, "kea_exporter_unhandled_statistics")

	// Act -- remove a descriptor the classifier still routes to.
	delete(c.stats, statPacketsReceived)
	collectAll(c)

	// Assert
	if !strings.Contains(buf.String(), "exporter bug") {
		t.Errorf("a statistic with no descriptor was not reported as an exporter bug:\n%s", buf.String())
	}
	if after := sumFamily(t, c, "kea_exporter_unhandled_statistics"); after != before {
		t.Errorf("unhandled statistics moved %v -> %v; an exporter bug must not be reported as Kea drift", before, after)
	}
}

func TestDropReasonsPartitionTheDropTotalOnRealCaptures(t *testing.T) {
	// Kea records a drop reason and then its caller bumps pkt4-receive-drop,
	// so the reasons sum to the total. Verified on all three real captures,
	// including a hot-standby standby whose HA hook drops every packet.
	//
	// No relation is asserted between pkt4-received and the typed counters.
	// The gap between them is not the set of drops occurring before typing,
	// and no fixed "pre-typing" set exists: HAImpl::buffer4Receive drops
	// not-for-us in the buffer4_receive callout, which Dhcpv4Srv::processPacket
	// runs before it types the packet, while Dhcpv4Srv::acceptServerId bumps
	// the same statistic after typing. The live standby capture shows the
	// consequence -- 98836 pkt4-not-for-us with every typed counter at zero.

	// Arrange
	reasons := []string{
		"pkt4-queue-full", "pkt4-parse-failed", "pkt4-processing-failed",
		"pkt4-service-disabled", "pkt4-admin-filtered", "pkt4-limit-exceeded",
		"pkt4-rfc-violation", "pkt4-not-for-us", "pkt4-duplicate",
	}

	for _, name := range []string{
		"statistic-get-all.json",
		"statistic-get-all-with-failures.json",
		"statistic-get-all-standby.json",
	} {
		t.Run(name, func(t *testing.T) {
			// Arrange
			var raw []struct {
				Arguments map[string][][]json.RawMessage `json:"arguments"`
			}
			if err := json.Unmarshal(fixture(t, name), &raw); err != nil {
				t.Fatalf("decode fixture: %v", err)
			}
			value := func(key string) int {
				samples, ok := raw[0].Arguments[key]
				if !ok || len(samples) == 0 {
					return 0
				}
				var v int
				if err := json.Unmarshal(samples[0][0], &v); err != nil {
					t.Fatalf("%s: %v", key, err)
				}
				return v
			}

			// Act
			var summed int
			for _, reason := range reasons {
				summed += value(reason)
			}
			var typed int
			for key := range raw[0].Arguments {
				if key != "pkt4-received" && strings.HasPrefix(key, "pkt4-") && strings.HasSuffix(key, "-received") {
					typed += value(key)
				}
			}

			// Assert
			if total := value("pkt4-receive-drop"); summed != total {
				t.Errorf("drop reasons sum to %d, pkt4-receive-drop is %d; they must agree", summed, total)
			}
			if received := value("pkt4-received"); typed > received {
				t.Errorf("typed counters sum to %d but only %d packets were received", typed, received)
			}
		})
	}
}

func TestStandbyPeerFromALiveCapture(t *testing.T) {
	// Captured from the standby of a live hot-standby pair. Every packet it
	// receives is dropped by the HA hook as not-for-us, its scopes list is
	// empty, and remote.age is 0 while in-touch is true -- which is what a
	// healthy just-contacted partner looks like, not a never-contacted one.
	// None of that appears in the primary's capture, and the age-0-but-in-touch
	// case is exactly what the in-touch gate was built for.

	// Arrange
	srv := keaStub(t, fixture(t, "statistic-get-all-standby.json"), fixture(t, "status-get-standby.json"))
	defer srv.Close()
	c := newCollector(&keaClient{url: srv.URL, client: srv.Client()})

	expected := `
# HELP kea_dhcp4_ha_enabled 1 when Kea reports an HA relationship, 0 when the HA hook is not loaded.
# TYPE kea_dhcp4_ha_enabled gauge
kea_dhcp4_ha_enabled 1
# HELP kea_dhcp4_ha_local_state_info HA state of this peer (info-metric; value always 1; state in label).
# TYPE kea_dhcp4_ha_local_state_info gauge
kea_dhcp4_ha_local_state_info{mode="hot-standby",peer="kea-primary",relationship="kea-primary",role="standby",state="hot-standby"} 1
# HELP kea_dhcp4_ha_partner_in_touch 1 once this peer has been in contact with its HA partner, else 0.
# TYPE kea_dhcp4_ha_partner_in_touch gauge
kea_dhcp4_ha_partner_in_touch{peer="kea-primary",relationship="kea-primary"} 1
# HELP kea_dhcp4_ha_partner_last_contact_seconds Seconds since the last successful heartbeat from the HA partner. Absent until the partner has been contacted at least once.
# TYPE kea_dhcp4_ha_partner_last_contact_seconds gauge
kea_dhcp4_ha_partner_last_contact_seconds{peer="kea-primary",relationship="kea-primary"} 0
`

	// Act + Assert -- age 0 with in-touch true must be exported, not withheld:
	// the gate is on in-touch, not on the value being non-zero.
	if err := testutil.CollectAndCompare(c, strings.NewReader(expected),
		"kea_dhcp4_ha_enabled", "kea_dhcp4_ha_local_state_info",
		"kea_dhcp4_ha_partner_in_touch", "kea_dhcp4_ha_partner_last_contact_seconds"); err != nil {
		t.Errorf("standby HA metrics: %v", err)
	}

	drops := `
# HELP kea_dhcp4_packets_dropped_by_reason_total Inbound DHCPv4 packets dropped, by reason. These sum to kea_dhcp4_packets_dropped_total, so use one or the other -- adding them together double-counts.
# TYPE kea_dhcp4_packets_dropped_by_reason_total counter
kea_dhcp4_packets_dropped_by_reason_total{reason="admin-filtered"} 0
kea_dhcp4_packets_dropped_by_reason_total{reason="duplicate"} 0
kea_dhcp4_packets_dropped_by_reason_total{reason="limit-exceeded"} 0
kea_dhcp4_packets_dropped_by_reason_total{reason="not-for-us"} 98836
kea_dhcp4_packets_dropped_by_reason_total{reason="parse-failed"} 0
kea_dhcp4_packets_dropped_by_reason_total{reason="processing-failed"} 0
kea_dhcp4_packets_dropped_by_reason_total{reason="queue-full"} 0
kea_dhcp4_packets_dropped_by_reason_total{reason="rfc-violation"} 0
kea_dhcp4_packets_dropped_by_reason_total{reason="service-disabled"} 0
# HELP kea_dhcp4_packets_dropped_total Total inbound DHCPv4 packets dropped. Equal to the sum of kea_dhcp4_packets_dropped_by_reason_total, and stays correct when a Kea release adds a reason this exporter does not know.
# TYPE kea_dhcp4_packets_dropped_total counter
kea_dhcp4_packets_dropped_total 98836
`
	if err := testutil.CollectAndCompare(c, strings.NewReader(drops),
		"kea_dhcp4_packets_dropped_by_reason_total", "kea_dhcp4_packets_dropped_total"); err != nil {
		t.Errorf("standby drop metrics: %v", err)
	}

	// The scrape is healthy: a standby dropping everything is doing its job.
	if got := testutil.ToFloat64(upOnly{c}); got != 1 {
		t.Errorf("kea_up = %v scraping a healthy standby, want 1", got)
	}
}
