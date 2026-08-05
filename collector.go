package main

// The Prometheus collector: the metric descriptors and the scrape that
// fills them.
//
// One reason to change: the exported metric contract -- a metric added,
// renamed, or relabelled. Which Kea statistic feeds which metric is
// statmap.go.

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

// collector implements prometheus.Collector. Each /metrics scrape
// triggers a single statistic-get-all + status-get round trip — we
// emit ConstMetrics inline rather than maintaining a pre-registered
// gauge cache, which avoids label-set drift surprises when Kea adds new
// statistic dimensions between releases.
type collector struct {
	kea   *keaClient
	build buildID

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
	// The names are only logged, and only at debug -- a gauge makes drift
	// after a Kea upgrade alertable without restarting at a higher log level.
	unhandledStats *prometheus.Desc

	// Cumulative scrape failures per command. Atomic and per-collector, not
	// package globals: Prometheus may call Collect concurrently for
	// overlapping scrapes, and a plain counter increment there is a data race.
	statErrorCount atomic.Uint64
	haErrorCount   atomic.Uint64

	// Track stat keys we don't have a mapping for. Logged exactly
	// once each — keeps scrape noise low while still surfacing drift.
	unhandledMu sync.Mutex
	unhandled   map[string]struct{}

	// Injected rather than taken from slog.Default() at each call site, so a
	// test can assert on what the collector logs without mutating a global.
	log *slog.Logger

	// Built once. It was rebuilt per statistic per scrape, which is ~33 map
	// constructions a scrape for a value that never changes.
	stats map[statID]statMetric
}

func newCollector(k *keaClient) *collector {
	const ns = "kea_dhcp4"
	c := &collector{
		kea:       k,
		build:     buildIdentity(),
		unhandled: make(map[string]struct{}),
		log:       slog.Default(),

		up: prometheus.NewDesc(
			"kea_up", "1 if the last scrape of the Kea control socket succeeded, else 0.",
			nil, nil),
		commandUp: prometheus.NewDesc(
			"kea_command_up", "1 if this Kea control command succeeded on the last scrape, else 0.",
			[]string{"command"}, nil),
		scrapeErrors: prometheus.NewDesc(
			"kea_scrape_errors_total", "Cumulative scrape errors since exporter start, by command.",
			[]string{"command"}, nil),

		addressesAssigned: prometheus.NewDesc(
			ns+"_addresses_assigned", "Currently assigned IPv4 addresses in the pool. Includes declined addresses (dhcp4_srv.cc:4455-4458 at Kea-3.2.0): Kea keeps them assigned so pool-utilisation stays meaningful, so do not add "+ns+"_addresses_declined to this.",
			[]string{"subnet"}, nil),
		addressesCapacity: prometheus.NewDesc(
			ns+"_addresses_capacity", "Pool size: total IPv4 addresses available in the subnet.",
			[]string{"subnet"}, nil),
		addressesDeclined: prometheus.NewDesc(
			ns+"_addresses_declined", "Addresses currently withdrawn from the pool by a DHCPDECLINE.",
			[]string{"subnet"}, nil),

		pkt4Received: prometheus.NewDesc(
			ns+"_packets_received_total", "Total DHCPv4 packets received by type.",
			[]string{"type"}, nil),
		pkt4Sent: prometheus.NewDesc(
			ns+"_packets_sent_total", "Total DHCPv4 packets sent by type.",
			[]string{"type"}, nil),
		pkt4Dropped: prometheus.NewDesc(
			ns+"_packets_dropped_total",
			"Total inbound DHCPv4 packets dropped. Equal to the sum of "+ns+"_packets_dropped_by_reason_total, and stays correct when a Kea release adds a reason this exporter does not know.",
			nil, nil),
		pkt4DropReasons: prometheus.NewDesc(
			ns+"_packets_dropped_by_reason_total",
			"Inbound DHCPv4 packets dropped, by reason. These sum to "+ns+"_packets_dropped_total, so use one or the other -- adding them together double-counts.",
			[]string{"reason"}, nil),

		allocFailScope: prometheus.NewDesc(
			ns+"_allocation_failures_by_scope_total",
			"Server-wide address allocation failures by where the client was looked up. A true partition: shared-network and subnet sum to the total.",
			[]string{"scope"}, nil),
		allocFailCause: prometheus.NewDesc(
			ns+"_allocation_failures_by_cause_total",
			"Server-wide address allocation failures by cause. A second true partition of the SAME failures: no-pools and attempts-exhausted also sum to the total, so do not add this to "+ns+"_allocation_failures_by_scope_total.",
			[]string{"cause"}, nil),

		haEnabled: prometheus.NewDesc(
			ns+"_ha_enabled", "1 when Kea reports an HA relationship, 0 when the HA hook is not loaded.",
			nil, nil),
		haLocalState: prometheus.NewDesc(
			ns+"_ha_local_state_info", "HA state of this peer (info-metric; value always 1; state in label).",
			[]string{"state", "role", "mode"}, nil),
		haPartnerAge: prometheus.NewDesc(
			ns+"_ha_partner_last_contact_seconds",
			"Seconds since the last successful heartbeat from the HA partner. Absent until the partner has been contacted at least once.",
			nil, nil),
		haPartnerInTouch: prometheus.NewDesc(
			ns+"_ha_partner_in_touch", "1 once this peer has been in contact with its HA partner, else 0.",
			nil, nil),
		haCommBroken: prometheus.NewDesc(
			ns+"_ha_communication_interrupted", "1 if HA communication with the partner is interrupted, else 0.",
			nil, nil),

		buildInfo: prometheus.NewDesc(
			"kea_exporter_build_info",
			"Build identity of the running exporter (info-metric; value always 1).",
			[]string{"version", "revision", "goversion"}, nil),
		scrapeSeconds: prometheus.NewDesc(
			"kea_scrape_duration_seconds",
			"Duration of the exporter's collection for this scrape, including the Kea round trips.",
			nil, nil),

		unhandledStats: prometheus.NewDesc(
			"kea_exporter_unhandled_statistics",
			"Number of distinct Kea statistics seen that this exporter has no mapping for.",
			nil, nil),
	}
	c.stats = c.buildStatMetrics()
	return c
}

func (c *collector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.up
	ch <- c.commandUp
	ch <- c.scrapeErrors
	ch <- c.addressesAssigned
	ch <- c.addressesCapacity
	ch <- c.addressesDeclined
	ch <- c.pkt4Received
	ch <- c.pkt4Sent
	ch <- c.pkt4Dropped
	ch <- c.pkt4DropReasons
	ch <- c.allocFailScope
	ch <- c.allocFailCause
	ch <- c.haEnabled
	ch <- c.haLocalState
	ch <- c.haPartnerAge
	ch <- c.haPartnerInTouch
	ch <- c.haCommBroken
	ch <- c.buildInfo
	ch <- c.scrapeSeconds
	ch <- c.unhandledStats
}

func (c *collector) Collect(ch chan<- prometheus.Metric) {
	// No shared deadline here: keaClient.call bounds each request on its own,
	// so a slow first command cannot starve the second.
	ctx := context.Background()
	start := time.Now()

	statErr := c.collectStats(ctx, ch)
	haErr := c.collectHA(ctx, ch)

	if statErr != nil {
		c.log.Error("control command failed", "command", "statistic-get-all", "err", statErr)
	}
	if haErr != nil {
		c.log.Error("control command failed", "command", "status-get", "err", haErr)
	}

	// kea_up is 0 if ANY command failed. Reporting 1 while half the metric
	// set is silently missing is worse than reporting down: an operator
	// alerting on kea_up == 0 would never see a half-broken scrape.
	upValue := 1.0
	if statErr != nil || haErr != nil {
		upValue = 0
	}
	ch <- prometheus.MustNewConstMetric(c.up, prometheus.GaugeValue, upValue)

	// Per-command health and error counters, so the two failure modes are
	// distinguishable without reading logs.
	c.emitCommandHealth(ch, "statistic-get-all", statErr, &c.statErrorCount)
	c.emitCommandHealth(ch, "status-get", haErr, &c.haErrorCount)

	c.unhandledMu.Lock()
	unhandled := float64(len(c.unhandled))
	c.unhandledMu.Unlock()
	ch <- prometheus.MustNewConstMetric(c.unhandledStats, prometheus.GaugeValue, unhandled)

	ch <- prometheus.MustNewConstMetric(c.buildInfo, prometheus.GaugeValue, 1,
		c.build.version, c.build.revision, c.build.goVersion)

	// Emitted last so it covers everything above it, including the failure
	// paths -- a scrape that timed out is exactly the one worth timing.
	ch <- prometheus.MustNewConstMetric(c.scrapeSeconds, prometheus.GaugeValue, time.Since(start).Seconds())
}

func (c *collector) emitCommandHealth(
	ch chan<- prometheus.Metric, command string, err error, counter *atomic.Uint64,
) {
	value := 1.0
	count := counter.Load()
	if err != nil {
		value = 0
		count = counter.Add(1)
	}
	ch <- prometheus.MustNewConstMetric(c.commandUp, prometheus.GaugeValue, value, command)
	ch <- prometheus.MustNewConstMetric(c.scrapeErrors, prometheus.CounterValue, float64(count), command)
}

func (c *collector) collectStats(ctx context.Context, ch chan<- prometheus.Metric) error {
	resp, err := c.kea.call(ctx, "statistic-get-all")
	if err != nil {
		return err
	}
	// Kea returns: {"<stat-name>": [[value, "timestamp"], ...]}.
	// Numeric values may decode as float64 or as JSON number.
	var stats map[string]json.RawMessage
	if err := json.Unmarshal(resp.Arguments, &stats); err != nil {
		return fmt.Errorf("decode statistic-get-all arguments: %w", err)
	}
	for key, raw := range stats {
		val, ok := mostRecentValue(raw)
		if !ok {
			// Kea also reports string- and duration-typed statistics. Route
			// them through the same once-only log as unmapped keys: dropping
			// them silently left an absent metric with nothing anywhere
			// explaining why.
			c.noteUnhandled(key)
			continue
		}
		c.emitStat(ch, key, val)
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

func (c *collector) buildStatMetrics() map[statID]statMetric {
	return map[statID]statMetric{
		statAddressesAssigned: {c.addressesAssigned, prometheus.GaugeValue},
		statAddressesCapacity: {c.addressesCapacity, prometheus.GaugeValue},
		statPacketsReceived:   {c.pkt4Received, prometheus.CounterValue},
		statPacketsSent:       {c.pkt4Sent, prometheus.CounterValue},
		statAddressesDeclined: {c.addressesDeclined, prometheus.GaugeValue},
		statPacketsDropped:    {c.pkt4Dropped, prometheus.CounterValue},
		statDropReason:        {c.pkt4DropReasons, prometheus.CounterValue},
		statAllocFailByScope:  {c.allocFailScope, prometheus.CounterValue},
		statAllocFailByCause:  {c.allocFailCause, prometheus.CounterValue},
	}
}

// emitStat translates one statistic into at most one metric. What a key means
// is classifyStat's decision; this only carries it out.
func (c *collector) emitStat(ch chan<- prometheus.Metric, key string, value float64) {
	id, labels := classifyStat(key)
	switch id {
	case statIgnored:
		return
	case statUnmapped:
		c.noteUnhandled(key)
		return
	}

	m, ok := c.stats[id]
	if !ok {
		// classifyStat named an ID with no descriptor behind it. That is a bug
		// in this package, not drift in Kea, so it must not be counted as an
		// unhandled statistic -- kea_exporter_unhandled_statistics means
		// "Kea reports something we do not map", and mislabelling this as that
		// sends the reader looking at the wrong system.
		c.log.Error("statistic classified to an ID with no descriptor; this is an exporter bug",
			"statistic", key, "id", int(id))
		return
	}
	ch <- prometheus.MustNewConstMetric(m.desc, m.typ, value, labels...)
}

func (c *collector) noteUnhandled(key string) {
	c.unhandledMu.Lock()
	defer c.unhandledMu.Unlock()
	if _, seen := c.unhandled[key]; seen {
		return
	}
	c.unhandled[key] = struct{}{}
	// Debug, not info: a stock Kea reports statistics this exporter does
	// not map, and printing all of them on the first scrape buries anything
	// that matters.
	c.log.Debug("unhandled statistic; extend the collector to map it", "statistic", key)
}
