package main

import (
	"reflect"
	"strings"
	"testing"
)

// classifyStat is a pure function of the key, so the mapping can be pinned
// exhaustively without constructing a collector or a fake Kea. That is the
// point of separating it: the old form was a branch chain inside an emitter,
// reachable only through a full scrape.

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

		{"unknown global", "v4-allocation-fail", statUnmapped, nil},
		{"unknown subnet suffix", "subnet[1].declined-addresses", statUnmapped, nil},
		{"unknown pkt4 shape", "pkt4-parse-failed", statUnmapped, nil},
		{"empty key", "", statUnmapped, nil},

		// Malformed shapes must be reported, not skipped: they mean Kea
		// emitted something this parser does not understand.
		{"subnet with no closing bracket", "subnet[1.assigned-addresses", statUnmapped, nil},
		{"subnet truncated after bracket", "subnet[1]", statUnmapped, nil},

		// An empty subnet id still yields one label, because the descriptor
		// has one either way -- passing zero would panic in Gather.
		{"empty subnet id", "subnet[].assigned-addresses", statAddressesAssigned, []string{""}},
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
	for _, table := range []map[string]statID{exactStats, subnetStats, packetStats} {
		for _, id := range table {
			if id == statUnmapped || id == statIgnored {
				continue
			}
			ids[id] = struct{}{}
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
	metrics := c.statMetrics()

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
	}

	for id, m := range c.statMetrics() {
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
		// Desc.String() renders "variableLabels: {subnet}" or "{}".
		wantLabels := 1
		if strings.Contains(m.desc.String(), "variableLabels: {}") {
			wantLabels = 0
		}
		if len(labels) != wantLabels {
			t.Errorf("%q yields %d labels but its descriptor takes %d: %s",
				key, len(labels), wantLabels, m.desc.String())
		}
	}
}

func TestTablesAndClassifierAgree(t *testing.T) {
	// The tables are the documented extension point, so a row the classifier
	// cannot reach would be quietly inert.

	t.Run("exact names", func(t *testing.T) {
		for key, want := range exactStats {
			// Act
			got, _ := classifyStat(key)

			// Assert
			if got != want {
				t.Errorf("exactStats[%q] = %v but classifyStat returns %v", key, want, got)
			}
		}
	})

	t.Run("subnet suffixes carry the subnet id", func(t *testing.T) {
		for suffix, want := range subnetStats {
			// Arrange
			key := "subnet[7]." + suffix

			// Act
			got, labels := classifyStat(key)

			// Assert
			if got != want {
				t.Errorf("subnetStats[%q] = %v but classifyStat(%q) returns %v", suffix, want, key, got)
			}
			if len(labels) != 1 || labels[0] != "7" {
				t.Errorf("classifyStat(%q) labels = %#v, want the subnet id", key, labels)
			}
		}
	})

	t.Run("packet suffixes carry the operation", func(t *testing.T) {
		for suffix, want := range packetStats {
			// Arrange
			key := "pkt4-discover" + suffix

			// Act
			got, labels := classifyStat(key)

			// Assert
			if got != want {
				t.Errorf("packetStats[%q] = %v but classifyStat(%q) returns %v", suffix, want, key, got)
			}
			if len(labels) != 1 || labels[0] != "discover" {
				t.Errorf("classifyStat(%q) labels = %#v, want the operation", key, labels)
			}
		}
	})
}
