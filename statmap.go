package main

// Mapping from the statistic names Kea reports onto this exporter's metrics.
//
// One reason to change: Kea renamed, added, or removed a statistic.
// The metric descriptors themselves live in collector.go, which changes for a
// different reason -- the exported contract.

import "strings"

// statID names what a statistic maps onto. Deliberately not a
// *prometheus.Desc: classification is then a pure function of the key, so it
// is testable without constructing a collector, and the descriptors stay in
// one place rather than being referenced from two.
type statID int

const (
	// statUnmapped is the default: a statistic this exporter has no mapping
	// for. Logged once and counted, never silently dropped.
	statUnmapped statID = iota
	// statIgnored is a statistic deliberately not exported. Distinct from
	// unmapped so a considered omission is not reported as a gap.
	statIgnored

	statAddressesAssigned
	statAddressesCapacity
	statPacketsReceived
	statPacketsSent
)

// exactStats covers statistics Kea reports under a fixed name.
//
// Adding a statistic is not only a row here. It also needs a statID below, a
// descriptor field and NewDesc in collector.go, a line in Describe, and a
// statMetrics row. The tables remove the branching, not the wiring.
var exactStats = map[string]statID{
	// Kea's grand totals, not a packet type: pkt4-sent equals
	// pkt4-offer-sent + pkt4-ack-sent exactly. Exporting them beside the
	// per-type series made sum(kea_dhcp4_packets_sent_total) return double.
	"pkt4-received": statIgnored,
	"pkt4-sent":     statIgnored,
}

// subnetStats covers the suffix of a `subnet[N].<suffix>` key; the label is
// the subnet ID.
var subnetStats = map[string]statID{
	"assigned-addresses": statAddressesAssigned,
	"total-addresses":    statAddressesCapacity,
}

// packetStats covers the suffix of a `pkt4-<op>-<suffix>` key; the label is
// the operation. A table rather than a switch so that every mapped statID is
// reachable from the tables, which is what lets the tests derive the mapped
// set instead of restating it.
var packetStats = map[string]statID{
	"-received": statPacketsReceived,
	"-sent":     statPacketsSent,
}

// classifyStat resolves a Kea statistic name to what it maps onto and the
// label values that accompany it -- nil for an unlabelled metric. Returning
// the labels as a slice rather than a string plus a "has a label" flag keeps
// the arity in one place: a mismatch between the descriptor and the number of
// values passed panics inside MustNewConstMetric, and Registry.Gather runs
// collectors without recovering, so that panic kills the process on a scrape.
//
// It never returns an error: an unrecognised key is statUnmapped, which the
// caller logs once and counts.
func classifyStat(key string) (statID, []string) {
	if id, ok := exactStats[key]; ok {
		return id, nil
	}
	if strings.HasPrefix(key, "subnet[") {
		return classifySubnetStat(key)
	}
	if strings.HasPrefix(key, "pkt4-") {
		return classifyPacketStat(key)
	}
	return statUnmapped, nil
}

// classifySubnetStat handles `subnet[N].<suffix>`, and recognises then drops
// the `subnet[N].pool[M].<suffix>` form Kea 3.x reports alongside it.
func classifySubnetStat(key string) (statID, []string) {
	end := strings.Index(key, "]")
	// A malformed key is unmapped rather than silently skipped: it means Kea
	// emitted a shape this parser does not understand, which is exactly the
	// drift the unmapped path exists to surface.
	if end < 0 || end+2 > len(key) {
		return statUnmapped, nil
	}
	subnetID := key[len("subnet["):end]
	rest := key[end+2:]

	// Per-pool statistics coexist with their per-subnet equivalents on 3.x.
	// Counting both would inflate reported utilisation, so the pool-scoped
	// ones are dropped deliberately. Note this swallows unrecognised per-pool
	// suffixes too, which is the one place statIgnored is broader than a
	// considered omission.
	if strings.HasPrefix(rest, "pool[") {
		return statIgnored, nil
	}
	if id, ok := subnetStats[rest]; ok {
		// subnetID may be empty for a malformed `subnet[]` key. The label is
		// still emitted, because the descriptor has one either way.
		return id, []string{subnetID}
	}
	return statUnmapped, nil
}

// classifyPacketStat handles `pkt4-<op>-received` and `pkt4-<op>-sent`; the
// label is the operation, e.g. "discover" or "lease-query".
func classifyPacketStat(key string) (statID, []string) {
	op := strings.TrimPrefix(key, "pkt4-")
	for suffix, id := range packetStats {
		if strings.HasSuffix(op, suffix) {
			return id, []string{strings.TrimSuffix(op, suffix)}
		}
	}
	return statUnmapped, nil
}
