package main

// Mapping from the statistic names Kea reports onto this exporter's metrics.
//
// One reason to change: Kea renamed, added, or removed a statistic.
// The metric descriptors themselves live in collector.go, which changes for a
// different reason -- the exported contract.

import (
	"slices"
	"strings"
)

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
	statAddressesDeclined
	statPacketsReceived
	statPacketsSent
	statPacketsDropped
	statDropReason
	statAllocFailByScope
	statAllocFailByCause
)

// statTarget is a table entry: which metric a statistic feeds, plus any label
// values the key itself does not supply.
//
// Only exactStats uses `extra`; the other two tables derive their single
// label from the key. It exists because several Kea statistics differ only in
// their name and belong on one metric under different labels --
// v4-allocation-fail-no-pools is the "no-pools" cause of the same counter
// that v4-allocation-fail is the "attempts-exhausted" cause of.
type statTarget struct {
	id    statID
	extra []string
}

// exactStats covers statistics Kea reports under a fixed name.
//
// Adding a statistic is not only a row here. It also needs a statID above, a
// descriptor field and NewDesc in collector.go, a line in Describe, and a
// statMetrics row. The tables remove the branching, not the wiring.
var exactStats = map[string]statTarget{
	// Kea's grand totals, not a packet type: pkt4-sent equals
	// pkt4-offer-sent + pkt4-ack-sent exactly. Exporting them beside the
	// per-type series made sum(kea_dhcp4_packets_sent_total) return double.
	"pkt4-received": {id: statIgnored},
	"pkt4-sent":     {id: statIgnored},

	// Server-wide lease counters. Kea increments these and their
	// subnet[N] equivalents for the same event, so the global one is the sum
	// across subnets; exporting both would double any sum(). The per-subnet
	// form is exported instead, being strictly more informative.
	"assigned-addresses": {id: statIgnored},
	"declined-addresses": {id: statIgnored},

	// Allocation failures are taken from the GLOBAL keys, unlike the lease
	// gauges above. Kea pre-creates the per-subnet lease observations at
	// startup, but not the per-subnet allocation-fail ones: those spring into
	// existence on the first failure. Exporting only the subnet form would
	// mean a healthy server exports no series at all, so an alert could not
	// tell "no failures" from "exporter broken". The globals are always
	// present and are the cross-subnet sum.
	//
	// Kea counts ONE failure in two independent branches
	// (alloc_engine.cc:5203-5264 at Kea-3.2.0): shared-network XOR subnet,
	// then no-pools XOR attempts-exhausted. Scope and cause are therefore two
	// different partitions of the same events, which is why they are two
	// metrics -- under one `reason` label, sum() would report double.
	"v4-allocation-fail-shared-network": {id: statAllocFailByScope, extra: []string{"shared-network"}},
	"v4-allocation-fail-subnet":         {id: statAllocFailByScope, extra: []string{"subnet"}},
	"v4-allocation-fail-no-pools":       {id: statAllocFailByCause, extra: []string{"no-pools"}},
	"v4-allocation-fail":                {id: statAllocFailByCause, extra: []string{"attempts-exhausted"}},

	// Not exported. Kea adds the ALL class to every packet
	// (dhcp4_srv.cc:642 at Kea-3.2.0), so `classes` is never empty by the
	// time allocation runs, and this counter tracks the by-cause total
	// rather than isolating a classification-driven subset.
	"v4-allocation-fail-classes": {id: statIgnored},

	// Total inbound packets dropped. Kea bumps this in the caller once a
	// reason has been recorded (dhcp4_srv.cc:1496-1499 at Kea-3.2.0,
	// "Specific drop cause stat was increased by accept* methods"), so the
	// reasons below are an exact partition of it: sum(reasons) == total.
	"pkt4-receive-drop": {id: statPacketsDropped},

	// The reasons. A separate metric from the total rather than a label on it,
	// because the total stays meaningful when a Kea release adds a reason this
	// exporter does not know about yet -- and because adding the two together
	// would double-count.
	"pkt4-queue-full":        {id: statDropReason, extra: []string{"queue-full"}},
	"pkt4-parse-failed":      {id: statDropReason, extra: []string{"parse-failed"}},
	"pkt4-processing-failed": {id: statDropReason, extra: []string{"processing-failed"}},
	"pkt4-service-disabled":  {id: statDropReason, extra: []string{"service-disabled"}},
	"pkt4-admin-filtered":    {id: statDropReason, extra: []string{"admin-filtered"}},
	"pkt4-limit-exceeded":    {id: statDropReason, extra: []string{"limit-exceeded"}},
	"pkt4-rfc-violation":     {id: statDropReason, extra: []string{"rfc-violation"}},
	"pkt4-not-for-us":        {id: statDropReason, extra: []string{"not-for-us"}},
	"pkt4-duplicate":         {id: statDropReason, extra: []string{"duplicate"}},
}

// subnetStats covers the suffix of a `subnet[N].<suffix>` key. The subnet ID
// is the only label; rows here do not use `extra`, and classifySubnetStat
// does not consult it -- a test enforces that, because a silently dropped
// label is worse than a missing feature.
//
// Allocation failures are handled on the global keys instead; see exactStats.
var subnetStats = map[string]statTarget{
	"assigned-addresses": {id: statAddressesAssigned},
	"total-addresses":    {id: statAddressesCapacity},
	"declined-addresses": {id: statAddressesDeclined},

	// Per-subnet allocation failures duplicate the globals above, which are
	// the ones exported; see the note there for why.
	"v4-allocation-fail-shared-network": {id: statIgnored},
	"v4-allocation-fail-subnet":         {id: statIgnored},
	"v4-allocation-fail-no-pools":       {id: statIgnored},
	"v4-allocation-fail":                {id: statIgnored},
	"v4-allocation-fail-classes":        {id: statIgnored},
}

// packetStats covers the suffix of a `pkt4-<op>-<suffix>` key; the label is
// the operation. A table rather than a switch so that every mapped statID is
// reachable from the tables, which is what lets the tests derive the mapped
// set instead of restating it.
//
// Rows here support neither `extra` nor statIgnored: classifyPacketStat
// implements neither, because nothing needs them and an untested branch is
// worse than a missing one. Add the handling with the row that needs it.
var packetStats = map[string]statTarget{
	"-received": {id: statPacketsReceived},
	"-sent":     {id: statPacketsSent},
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
	if target, ok := exactStats[key]; ok {
		// Cloned rather than returned directly: target.extra is the package-level
		// table's backing array, and one `labels = append(labels, ...)` at a
		// call site would corrupt it for the rest of the process.
		return target.id, slices.Clone(target.extra)
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
	if target, ok := subnetStats[rest]; ok {
		// Labels accompany a metric, so an ignored statistic carries none --
		// otherwise a caller could read a label set for something that is
		// never emitted.
		if target.id == statIgnored {
			return statIgnored, nil
		}
		// subnetID may be empty for a malformed `subnet[]` key. The label is
		// still emitted, because the descriptor has one either way.
		//
		// target.extra is deliberately not consulted: no row here carries one,
		// so honouring it would be an untestable branch. Only exactStats needs
		// per-key label values.
		return target.id, []string{subnetID}
	}
	return statUnmapped, nil
}

// classifyPacketStat handles `pkt4-<op>-received` and `pkt4-<op>-sent`; the
// label is the operation, e.g. "discover" or "lease-query".
func classifyPacketStat(key string) (statID, []string) {
	op := strings.TrimPrefix(key, "pkt4-")
	for suffix, target := range packetStats {
		if strings.HasSuffix(op, suffix) {
			// As in classifySubnetStat, target.extra is not consulted: the
			// operation is the only label and no row here carries more.
			return target.id, []string{strings.TrimSuffix(op, suffix)}
		}
	}
	return statUnmapped, nil
}
