package main

// Mapping from the statistic names Kea reports onto this exporter's metrics.
//
// One reason to change: Kea renamed, added, or removed a statistic.
// The metric descriptors themselves live in collector.go, which changes for a
// different reason -- the exported contract.
//
// Why a given statistic is exported, ignored, or split across two metrics is
// in docs/kea-behaviour.md. The comments below say only what is needed to
// edit a row correctly.

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
	// Grand totals, not packet types; exporting them beside the per-type
	// series doubles sum().
	"pkt4-received": {id: statIgnored},
	"pkt4-sent":     {id: statIgnored},

	// Duplicates of the subnet[N] equivalents, which are exported instead.
	"assigned-addresses": {id: statIgnored},
	"declined-addresses": {id: statIgnored},

	// The GLOBAL keys, unlike the lease gauges above: Kea does not create the
	// per-subnet ones until the first failure. Two metrics, not one labelled
	// metric, because scope and cause are separate partitions of the same
	// events and would double-count under a single label.
	"v4-allocation-fail-shared-network": {id: statAllocFailByScope, extra: []string{"shared-network"}},
	"v4-allocation-fail-subnet":         {id: statAllocFailByScope, extra: []string{"subnet"}},
	"v4-allocation-fail-no-pools":       {id: statAllocFailByCause, extra: []string{"no-pools"}},
	"v4-allocation-fail":                {id: statAllocFailByCause, extra: []string{"attempts-exhausted"}},

	// Tracks the by-cause total rather than a class-driven subset, because
	// Kea classes every packet as ALL.
	"v4-allocation-fail-classes": {id: statIgnored},

	// The total. Kea records a reason alongside it, so the reasons below are
	// an exact partition: sum(reasons) == total.
	"pkt4-receive-drop": {id: statPacketsDropped},

	// The reasons, as their own metric rather than a label on the total: the
	// total stays correct when a Kea release adds a reason this exporter does
	// not know, and adding the two together double-counts.
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
// is the only label; rows do not use `extra` and classifySubnetStat does not
// consult it, which a test enforces.
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
// the operation. A table rather than a switch so the tests can derive the
// mapped set instead of restating it.
//
// Rows support neither `extra` nor statIgnored -- classifyPacketStat
// implements neither. Add the handling with the row that needs it.
var packetStats = map[string]statTarget{
	"-received": {id: statPacketsReceived},
	"-sent":     {id: statPacketsSent},
}

// classifyStat resolves a Kea statistic name to what it maps onto and the
// label values that accompany it -- nil for an unlabelled metric. Labels are
// a slice so the arity has one source; getting it wrong kills the process
// rather than failing a scrape (docs/kea-behaviour.md).
//
// It never returns an error: an unrecognised key is statUnmapped, which the
// caller logs once and counts.
func classifyStat(key string) (statID, []string) {
	if target, ok := exactStats[key]; ok {
		// Cloned: the table's slice is shared, and one append at a call site
		// would corrupt it process-wide.
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

	// Per-pool keys coexist with their per-subnet equivalents on 3.x and are
	// dropped to avoid double-counting. This also swallows unrecognised
	// per-pool suffixes, the one place statIgnored is broader than a
	// considered omission.
	if strings.HasPrefix(rest, "pool[") {
		return statIgnored, nil
	}
	if target, ok := subnetStats[rest]; ok {
		// An ignored statistic carries no labels; otherwise a caller could
		// read a label set for something never emitted.
		if target.id == statIgnored {
			return statIgnored, nil
		}
		// subnetID may be empty for a malformed `subnet[]` key. The label is
		// still emitted, because the descriptor has one either way.
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
			return target.id, []string{strings.TrimSuffix(op, suffix)}
		}
	}
	return statUnmapped, nil
}
