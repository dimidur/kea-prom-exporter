package main

// The Prometheus collector: the metric descriptors and the scrape that
// fills them.
//
// One reason to change: what a scrape produces, meaning the metrics it exports
// (added, renamed or relabelled) and the Kea commands it sends to get them.
// Which Kea statistic feeds which metric is statmap.go.
//
// Kea symbols named below hold across the supported Kea releases; the
// supported set is in README.md.

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// supportedService is the only Kea service whose statistics are mapped.
const supportedService = "dhcp4"

// The control commands a scrape sends. Each name is also the `command` label
// of kea_command_up and kea_scrape_errors_total.
const (
	cmdStatisticGetAll = "statistic-get-all"
	cmdStatusGet       = "status-get"
)

// scrapeCommands are the control commands one Collect sends, in order and one
// after the other.
var scrapeCommands = []string{cmdStatisticGetAll, cmdStatusGet}

// maxScrapeDuration is the longest a legitimate scrape can take: every command
// in scrapeCommands running to its own deadline in turn.
func maxScrapeDuration(keaTimeout time.Duration) time.Duration {
	return time.Duration(len(scrapeCommands)) * effectiveTimeout(keaTimeout)
}

// keaCaller is what the collector needs from Kea: one control command and
// its reply. Each call bounds itself with a deadline of its own, and callers
// pass a context without one, so a slow command cannot eat into the time of
// the next; maxScrapeDuration, and the shutdown grace built on it, rely on
// that. *keaClient is the implementation.
type keaCaller interface {
	call(ctx context.Context, command string) (*keaResponse, error)
}

// collector implements prometheus.Collector. Each /metrics scrape triggers one
// round of scrapeCommands. It emits ConstMetrics inline rather than
// maintaining a pre-registered gauge cache, which avoids label-set drift
// surprises when Kea adds new statistic dimensions between releases.
type collector struct {
	descriptors
	selfReport

	kea keaCaller

	// Cumulative scrape failures per command. Atomic and per-collector, not
	// package globals: Prometheus may call Collect concurrently for
	// overlapping scrapes, and a plain counter increment there is a data race.
	statErrorCount atomic.Uint64
	haErrorCount   atomic.Uint64

	// Stat keys without a mapping, each logged once, which keeps scrape noise
	// low while still surfacing drift.
	unhandled unhandledSet

	// Built once per collector, not per statistic per scrape: it never
	// changes.
	stats map[statID]statMetric
}

// unhandledSet is the statistic keys noted as unhandled, each once. Collect
// may run concurrently, so the set is locked, and its size is kept beside it
// so that reading it takes no lock. The zero value is an empty set.
type unhandledSet struct {
	mu   sync.Mutex
	keys map[string]struct{}
	size atomic.Int64
}

// note records key and reports whether it was not already in the set.
func (s *unhandledSet) note(key string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, seen := s.keys[key]; seen {
		return false
	}
	if s.keys == nil {
		s.keys = map[string]struct{}{}
	}
	s.keys[key] = struct{}{}
	s.size.Add(1)
	return true
}

// count is how many distinct keys have been noted.
func (s *unhandledSet) count() int64 { return s.size.Load() }

// selfReport is how a collector reports on its own work: where command
// failures, statistic drift and exporter bugs are logged, the build identity
// it exports, and the clock its scrape duration is measured by.
type selfReport struct {
	log   *slog.Logger
	build buildID
	now   func() time.Time
}

// descriptors are the metrics a collector exports. all is what Describe sends;
// newDescriptors' add lists each descriptor there as it builds it, and a test
// holds every field to that.
type descriptors struct {
	all []*prometheus.Desc

	// Scrape health.
	up           *prometheus.Desc
	commandUp    *prometheus.Desc
	scrapeErrors *prometheus.Desc

	// dhcp4 lease metrics, subnet-scoped.
	addressesAssigned *prometheus.Desc
	addressesCapacity *prometheus.Desc
	addressesDeclined *prometheus.Desc

	// dhcp4 packet counters.
	pkt4Received    *prometheus.Desc
	pkt4Sent        *prometheus.Desc
	pkt4Dropped     *prometheus.Desc
	pkt4DropReasons *prometheus.Desc

	// Allocation failures, split the way Kea actually counts them.
	allocFailScope *prometheus.Desc
	allocFailCause *prometheus.Desc

	// HA state.
	haEnabled        *prometheus.Desc
	haLocalState     *prometheus.Desc
	haPartnerState   *prometheus.Desc
	haPartnerAge     *prometheus.Desc
	haPartnerInTouch *prometheus.Desc
	haCommBroken     *prometheus.Desc

	// Identity of the running binary, and how long a scrape took. Both are
	// standard for an exporter: the first is the first question asked in a bug
	// report, the second is what tells you the exporter rather than Kea is the
	// slow part.
	buildInfo     *prometheus.Desc
	scrapeSeconds *prometheus.Desc

	// Count of statistics Kea reports that this exporter has no mapping for.
	// The names are only logged, and only at debug, so a gauge makes drift
	// after a Kea upgrade alertable without restarting at a higher log level.
	unhandledStats *prometheus.Desc
}

// newDescriptors builds every descriptor the collector exports, with its name,
// help text and labels.
func newDescriptors() descriptors {
	const namespace = "kea_" + supportedService
	var set descriptors
	add := func(name, help string, labels []string) *prometheus.Desc {
		desc := prometheus.NewDesc(name, help, labels, nil)
		set.all = append(set.all, desc)
		return desc
	}

	set.up = add("kea_up", "1 if the last scrape of the Kea control socket succeeded, else 0.", nil)
	set.commandUp = add("kea_command_up", "1 if this Kea control command succeeded on the last scrape, else 0.",
		[]string{"command"})
	set.scrapeErrors = add("kea_scrape_errors_total", "Cumulative scrape errors since exporter start, by command.",
		[]string{"command"})

	// Dhcpv4Srv::declineLease leaves a declined lease counted in
	// assigned-addresses so that pool utilisation stays meaningful, which is
	// why assigned/capacity is already correct and assigned+declined
	// over-counts. The HELP text carries the consequence; the citation stays
	// here rather than in it, since HELP ships on every scrape.
	set.addressesAssigned = add(namespace+"_addresses_assigned",
		"Currently assigned IPv4 addresses in the pool. Includes declined addresses: Kea keeps them assigned so pool-utilisation stays meaningful, so do not add "+namespace+"_addresses_declined to this.",
		[]string{"subnet"})
	set.addressesCapacity = add(namespace+"_addresses_capacity",
		"Pool size: total IPv4 addresses available in the subnet.", []string{"subnet"})
	set.addressesDeclined = add(namespace+"_addresses_declined",
		"Addresses currently withdrawn from the pool by a DHCPDECLINE.", []string{"subnet"})

	set.pkt4Received = add(namespace+"_packets_received_total", "Total DHCPv4 packets received by type.",
		[]string{"type"})
	set.pkt4Sent = add(namespace+"_packets_sent_total", "Total DHCPv4 packets sent by type.", []string{"type"})
	set.pkt4Dropped = add(namespace+"_packets_dropped_total",
		"Total inbound DHCPv4 packets dropped. Equal to the sum of "+namespace+"_packets_dropped_by_reason_total, and stays correct when a Kea release adds a reason this exporter does not know.",
		nil)
	set.pkt4DropReasons = add(namespace+"_packets_dropped_by_reason_total",
		"Inbound DHCPv4 packets dropped, by reason. These sum to "+namespace+"_packets_dropped_total, so use one or the other; adding them together double-counts.",
		[]string{"reason"})

	set.allocFailScope = add(namespace+"_allocation_failures_by_scope_total",
		"Server-wide address allocation failures by where the client was looked up. A true partition: shared-network and subnet sum to the total.",
		[]string{"scope"})
	set.allocFailCause = add(namespace+"_allocation_failures_by_cause_total",
		"Server-wide address allocation failures by cause. A second true partition of the SAME failures: no-pools and attempts-exhausted also sum to the total, so do not add this to "+namespace+"_allocation_failures_by_scope_total.",
		[]string{"cause"})

	// ha_enabled describes the daemon, not any one relationship: it answers
	// "is the HA hook loaded", which is still meaningful when there are no
	// relationships to name. It is deliberately the only unlabelled HA metric.
	set.haEnabled = add(namespace+"_ha_enabled",
		"1 when Kea reports an HA relationship, 0 when the HA hook is not loaded.", nil)
	set.haLocalState = add(namespace+"_ha_local_state_info",
		"HA state of this peer (info-metric; value always 1; state in label).", haLabels("state", "role", "mode"))
	set.haPartnerState = add(namespace+"_ha_partner_state_info",
		"HA state of the partner as last reported to this peer (info-metric; value always 1). Absent until the partner has been contacted at least once, because Kea reports an empty state until then.",
		haLabels("state", "role"))
	set.haPartnerAge = add(namespace+"_ha_partner_last_contact_seconds",
		"Seconds since the last successful heartbeat from the HA partner. Absent until the partner has been contacted at least once.",
		haLabels())
	set.haPartnerInTouch = add(namespace+"_ha_partner_in_touch",
		"1 once this peer has been in contact with its HA partner, else 0.", haLabels())
	set.haCommBroken = add(namespace+"_ha_communication_interrupted",
		"1 if HA communication with the partner is interrupted, else 0.", haLabels())

	set.buildInfo = add("kea_exporter_build_info",
		"Build identity of the running exporter (info-metric; value always 1).",
		[]string{"version", "revision", "goversion"})
	set.scrapeSeconds = add("kea_scrape_duration_seconds",
		"Duration of the exporter's collection for this scrape, including the Kea round trips.", nil)

	set.unhandledStats = add("kea_exporter_unhandled_statistics",
		"Number of distinct Kea statistics seen that this exporter has no mapping for.", nil)
	return set
}

// newCollector returns a collector that scrapes kea and reports on its own
// work through report.
func newCollector(kea keaCaller, report selfReport) *collector {
	set := newDescriptors()
	return &collector{
		descriptors: set,
		selfReport:  report,
		kea:         kea,
		stats:       set.statMetrics(),
	}
}

// Describe sends every descriptor the collector exports.
func (c *collector) Describe(out chan<- *prometheus.Desc) {
	for _, desc := range c.all {
		out <- desc
	}
}

// Collect runs one round of scrapeCommands against Kea and sends what it
// yields, followed by the exporter's own health and identity metrics.
func (c *collector) Collect(out chan<- prometheus.Metric) {
	// No deadline here: keaCaller bounds each command itself.
	ctx := context.Background()
	start := c.now()

	statErr := c.collectStats(ctx, out)
	haErr := c.collectHA(ctx, out)

	if statErr != nil {
		c.log.Error("control command failed", "command", cmdStatisticGetAll, "err", statErr)
	}
	if haErr != nil {
		c.log.Error("control command failed", "command", cmdStatusGet, "err", haErr)
	}

	// kea_up is 0 if ANY command failed. Reporting 1 while part of the metric
	// set is silently missing is worse than reporting down: an alert on
	// kea_up == 0 would never see a partly broken scrape.
	upValue := 1.0
	if statErr != nil || haErr != nil {
		upValue = 0
	}
	out <- prometheus.MustNewConstMetric(c.up, prometheus.GaugeValue, upValue)

	// Per-command health and error counters, so which command failed is
	// visible without reading logs.
	c.emitCommandHealth(out, cmdStatisticGetAll, statErr, &c.statErrorCount)
	c.emitCommandHealth(out, cmdStatusGet, haErr, &c.haErrorCount)

	unhandled := float64(c.unhandled.count())
	out <- prometheus.MustNewConstMetric(c.unhandledStats, prometheus.GaugeValue, unhandled)

	out <- prometheus.MustNewConstMetric(c.buildInfo, prometheus.GaugeValue, 1,
		c.build.version, c.build.revision, c.build.goVersion)

	// Emitted last so it covers everything above it, including the failure
	// paths: a scrape that timed out is exactly the one worth timing.
	out <- prometheus.MustNewConstMetric(c.scrapeSeconds, prometheus.GaugeValue, c.now().Sub(start).Seconds())
}

// emitCommandHealth sends kea_command_up and kea_scrape_errors_total for
// command, counting err, if any, in counter.
func (c *collector) emitCommandHealth(
	out chan<- prometheus.Metric, command string, err error, counter *atomic.Uint64,
) {
	value := 1.0
	count := counter.Load()
	if err != nil {
		value = 0
		count = counter.Add(1)
	}
	out <- prometheus.MustNewConstMetric(c.commandUp, prometheus.GaugeValue, value, command)
	out <- prometheus.MustNewConstMetric(c.scrapeErrors, prometheus.CounterValue, float64(count), command)
}

// callArguments sends command to kea and decodes the arguments of its reply
// as Arguments.
func callArguments[Arguments any](ctx context.Context, kea keaCaller, command string) (Arguments, error) {
	var arguments Arguments
	resp, err := kea.call(ctx, command)
	if err != nil {
		return arguments, err
	}
	if err := json.Unmarshal(resp.Arguments, &arguments); err != nil {
		return arguments, fmt.Errorf("decode %s arguments: %w", command, err)
	}
	return arguments, nil
}

// collectStats sends a metric for each statistic statistic-get-all reports,
// and returns the error if the command failed or its reply did not decode.
func (c *collector) collectStats(ctx context.Context, out chan<- prometheus.Metric) error {
	// Kea returns: {"<stat-name>": [[value, "timestamp"], ...]}.
	// Numeric values may decode as float64 or as JSON number.
	stats, err := callArguments[map[string]json.RawMessage](ctx, c.kea, cmdStatisticGetAll)
	if err != nil {
		return err
	}
	for key, raw := range stats {
		value, ok := mostRecentValue(raw)
		if !ok {
			// Kea also reports string- and duration-typed statistics. Route
			// them through the same once-only log as unmapped keys: dropped
			// silently, they would leave an absent metric with nothing
			// anywhere explaining why.
			c.noteUnhandled(key)
			continue
		}
		c.emitStat(out, key, value)
	}
	return nil
}

// statMetric binds a statID to what the exporter exports for it. Kept beside
// the descriptors rather than in statmap.go: which descriptor a statistic
// feeds is part of the metric contract, while the key that selects it is not.
type statMetric struct {
	desc *prometheus.Desc
	typ  prometheus.ValueType
}

// statMetrics is the metric each mapped statID is exported as.
func (d descriptors) statMetrics() map[statID]statMetric {
	return map[statID]statMetric{
		statAddressesAssigned: {d.addressesAssigned, prometheus.GaugeValue},
		statAddressesCapacity: {d.addressesCapacity, prometheus.GaugeValue},
		statPacketsReceived:   {d.pkt4Received, prometheus.CounterValue},
		statPacketsSent:       {d.pkt4Sent, prometheus.CounterValue},
		statAddressesDeclined: {d.addressesDeclined, prometheus.GaugeValue},
		statPacketsDropped:    {d.pkt4Dropped, prometheus.CounterValue},
		statDropReason:        {d.pkt4DropReasons, prometheus.CounterValue},
		statAllocFailByScope:  {d.allocFailScope, prometheus.CounterValue},
		statAllocFailByCause:  {d.allocFailCause, prometheus.CounterValue},
	}
}

// statOutcome is what one statistic becomes. statMissingDescriptor is the zero
// value, so a resolvedStat nobody filled in is logged as an exporter bug
// rather than emitted.
type statOutcome int

const (
	statMissingDescriptor statOutcome = iota // mapped to an ID with no descriptor
	statEmit                                 // a metric, from metric and labels
	statSkip                                 // deliberately not exported
	statRecordUnhandled                      // Kea reports something not mapped
)

// resolvedStat is resolveStat's answer for one key.
type resolvedStat struct {
	outcome statOutcome
	id      statID
	metric  statMetric
	labels  []string
}

// resolveStat decides what the statistic key becomes, given the descriptor for
// each statID. What a key means is classifyStat's decision; this pairs it with
// its descriptor.
func resolveStat(stats map[statID]statMetric, key string) resolvedStat {
	id, labels := classifyStat(key)
	switch id {
	case statIgnored:
		return resolvedStat{outcome: statSkip, id: id}
	case statUnmapped:
		return resolvedStat{outcome: statRecordUnhandled, id: id}
	}
	metric, ok := stats[id]
	if !ok {
		return resolvedStat{outcome: statMissingDescriptor, id: id}
	}
	return resolvedStat{outcome: statEmit, id: id, metric: metric, labels: labels}
}

// emitStat translates one statistic into at most one metric.
func (c *collector) emitStat(out chan<- prometheus.Metric, key string, value float64) {
	c.emitResolved(out, key, value, resolveStat(c.stats, key))
}

// emitResolved carries out what resolveStat decided for key.
func (c *collector) emitResolved(out chan<- prometheus.Metric, key string, value float64, resolved resolvedStat) {
	switch resolved.outcome {
	case statSkip:
		// Deliberately not exported: classifyStat ignores the key.
		return
	case statRecordUnhandled:
		c.noteUnhandled(key)
	case statMissingDescriptor:
		// classifyStat named an ID with no descriptor behind it. That is a bug
		// in this package, not drift in Kea, so it must not be counted as an
		// unhandled statistic: kea_exporter_unhandled_statistics means
		// "Kea reports something we do not map", and mislabelling this as that
		// sends the reader looking at the wrong system.
		c.log.Error("statistic classified to an ID with no descriptor; this is an exporter bug",
			"statistic", key, "id", int(resolved.id))
	case statEmit:
		out <- prometheus.MustNewConstMetric(resolved.metric.desc, resolved.metric.typ, value, resolved.labels...)
	}
}

// noteUnhandled records a statistic the exporter does not map, logging it the
// first time it is seen.
func (c *collector) noteUnhandled(key string) {
	if !c.unhandled.note(key) {
		return
	}
	// Debug, not info: a stock Kea reports statistics this exporter does
	// not map, and printing all of them on the first scrape buries anything
	// that matters.
	c.log.Debug("unhandled statistic; extend the collector to map it", "statistic", key)
}
