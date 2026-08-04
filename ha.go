package main

// Decoding of the HA hook's status-get payload.
//
// One reason to change: the HA hook's response shape, which Kea versions
// separately from the core statistics -- hence its own file.

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/prometheus/client_golang/prometheus"
)

// haStatus is the trimmed status-get response we care about. The full
// response is much richer; we only decode the fields used below.
type haStatus struct {
	HighAvailability []struct {
		HAMode    string `json:"ha-mode"`
		HAServers struct {
			Local struct {
				Role  string `json:"role"`
				State string `json:"state"`
			} `json:"local"`
			Remote struct {
				Age                    float64 `json:"age"`
				CommunicationInterrupt bool    `json:"communication-interrupted"`
				InTouch                bool    `json:"in-touch"`
			} `json:"remote"`
		} `json:"ha-servers"`
	} `json:"high-availability"`
}

func (c *collector) collectHA(ctx context.Context, ch chan<- prometheus.Metric) error {
	resp, err := c.kea.call(ctx, "status-get")
	if err != nil {
		return err
	}
	var s haStatus
	if err := json.Unmarshal(resp.Arguments, &s); err != nil {
		return fmt.Errorf("decode status-get arguments: %w", err)
	}
	// Absent HA metrics could mean the hook is not loaded, or that status-get
	// failed. This makes the first case explicit so the two are
	// distinguishable without reading logs.
	if len(s.HighAvailability) == 0 {
		ch <- prometheus.MustNewConstMetric(c.haEnabled, prometheus.GaugeValue, 0)
		return nil
	}
	ch <- prometheus.MustNewConstMetric(c.haEnabled, prometheus.GaugeValue, 1)

	if len(s.HighAvailability) > 1 {
		c.log.Warn("only the first HA relationship is exported",
			"relationships", len(s.HighAvailability))
	}
	ha := s.HighAvailability[0]
	ch <- prometheus.MustNewConstMetric(
		c.haLocalState, prometheus.GaugeValue, 1,
		ha.HAServers.Local.State, ha.HAServers.Local.Role, ha.HAMode)

	// Kea reports age 0 when it has never been in touch with the partner.
	// Exporting that unconditionally reads on a dashboard as "contacted 0
	// seconds ago" — the exact inverse of the truth — so the gauge is only
	// emitted when it means something, alongside an explicit in-touch signal.
	inTouch := 0.0
	if ha.HAServers.Remote.InTouch {
		inTouch = 1
		ch <- prometheus.MustNewConstMetric(
			c.haPartnerAge, prometheus.GaugeValue, ha.HAServers.Remote.Age)
	}
	ch <- prometheus.MustNewConstMetric(c.haPartnerInTouch, prometheus.GaugeValue, inTouch)

	commVal := 0.0
	if ha.HAServers.Remote.CommunicationInterrupt {
		commVal = 1
	}
	ch <- prometheus.MustNewConstMetric(c.haCommBroken, prometheus.GaugeValue, commVal)
	return nil
}
