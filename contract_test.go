package main

import (
	"maps"
	"regexp"
	"slices"
	"testing"

	dto "github.com/prometheus/client_model/go"
)

// The exporter's contract with whoever reads its metrics and with Kea: the
// metric set it exports, and the Kea statistics it knows it does not map. The
// unit tests and the compatibility check in compat_test.go both hold the
// exporter to these.

// metricContract is every metric family the exporter exports, with its label
// names sorted: the exporter's public API.
var metricContract = map[string][]string{
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

// unmappedStatistics are the statistics a stock Kea reports that the exporter
// does not map yet, with the subnet index written as [N]. A statistic in
// neither this list nor the mapping tables is one a Kea release added.
var unmappedStatistics = []string{
	"cumulative-assigned-addresses",
	"reclaimed-declined-addresses",
	"reclaimed-leases",
	"subnet[N].cumulative-assigned-addresses",
	"subnet[N].reclaimed-declined-addresses",
	"subnet[N].reclaimed-leases",
	"subnet[N].v4-lease-reuses",
	"subnet[N].v4-reservation-conflicts",
	"v4-lease-reuses",
	"v4-reservation-conflicts",
}

// statisticIndex matches the index in a statistic name such as subnet[1].
var statisticIndex = regexp.MustCompile(`\[[0-9]+\]`)

// unmappedForm is key as unmappedStatistics writes it, with [N] for each
// index.
func unmappedForm(key string) string {
	return statisticIndex.ReplaceAllString(key, "[N]")
}

// withoutRelationshipMetrics is contract without the families labelled by HA
// relationship, which a Kea with no HA hook does not report.
func withoutRelationshipMetrics(contract map[string][]string) map[string][]string {
	kept := maps.Clone(contract)
	maps.DeleteFunc(kept, func(_ string, labels []string) bool {
		return slices.Contains(labels, "relationship")
	})
	return kept
}

// assertExportsExactly fails the test unless families are exactly the
// families in want, each with exactly its label names.
func assertExportsExactly(t *testing.T, families []*dto.MetricFamily, want map[string][]string) {
	t.Helper()
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
	for name, wantLabels := range want {
		got, ok := exported[name]
		if !ok {
			t.Errorf("%s was expected but not exported", name)
			continue
		}
		if !slices.Equal(got, wantLabels) {
			t.Errorf("%s exports labels %v, want %v", name, got, wantLabels)
		}
	}
	for name := range exported {
		if _, ok := want[name]; !ok {
			t.Errorf("%s is exported but not expected; a new metric belongs in metricContract and the README metric table", name)
		}
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
	kea := healthyKea(t)
	collector := newCollector(kea, testReport(quietLogger))

	// Act
	families := gather(t, collector)

	// Assert
	assertExportsExactly(t, families, metricContract)
}

func TestKeaWithoutHAExportsTheContractWithoutRelationshipMetrics(t *testing.T) {
	// The compatibility check runs a Kea with no HA hook and expects exactly
	// the contract without the families labelled by HA relationship. Pinned
	// here against the live capture, so that expectation is checked on every
	// pull request, and a helper that dropped too many families fails.

	// Arrange
	noHA := []byte(`[{"result":0,"arguments":{"pid":1,"uptime":10}}]`)
	collector := newCollector(newFakeKea(fixture(t, "statistic-get-all.json"), noHA), testReport(quietLogger))

	// Act
	families := gather(t, collector)

	// Assert
	assertExportsExactly(t, families, withoutRelationshipMetrics(metricContract))
}
