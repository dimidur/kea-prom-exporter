package main

// Decoding of the HA hook's status-get payload.
//
// One reason to change: the HA hook's response shape, which Kea versions
// separately from the core statistics -- hence its own file.
//
// Why the labels are derived the way they are, and what Kea guarantees about
// them, is in docs/kea-behaviour.md.

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"

	"github.com/prometheus/client_golang/prometheus"
)

// haLabels prefixes a relationship-scoped HA metric's labels with the pair
// identifying which relationship a sample belongs to. haEnabled is the one HA
// metric that does not use it, describing the daemon rather than a
// relationship.
func haLabels(extra ...string) []string {
	out := make([]string, 0, 2+len(extra))
	out = append(out, "relationship", "peer")
	return append(out, extra...)
}

// haServer is the local half of a relationship. Kea sets server-name even in
// the modes where it omits the remote half, which is what makes it a usable
// fallback.
type haServer struct {
	Role       string `json:"role"`
	State      string `json:"state"`
	ServerName string `json:"server-name"`
}

// haRemote is the partner half. A pointer, not a value: Kea omits it entirely
// in passive-backup mode and on a backup server, and a zero-valued struct is
// indistinguishable from a partner reporting empty strings and a false
// in-touch.
type haRemote struct {
	Age                    float64 `json:"age"`
	CommunicationInterrupt bool    `json:"communication-interrupted"`
	InTouch                bool    `json:"in-touch"`
	LastState              string  `json:"last-state"`
	Role                   string  `json:"role"`
	ServerName             string  `json:"server-name"`
}

type haRelationship struct {
	HAMode    string `json:"ha-mode"`
	HAServers struct {
		Local  haServer  `json:"local"`
		Remote *haRemote `json:"remote"`
	} `json:"ha-servers"`
}

// haStatus is the trimmed status-get response we care about. The full
// response is much richer; we only decode the fields used below.
type haStatus struct {
	HighAvailability []haRelationship `json:"high-availability"`
}

// identity derives the two labels every relationship-scoped HA metric
// carries: relationship, the lower of the two server names, and peer, the
// partner's. Kea's uniqueness rule is what makes min() a stable, unique key
// that both nodes of a pair agree on -- see docs/kea-behaviour.md.
//
// With no remote half there is no pair to choose from and both fall back to
// the local name; haIdentities keeps those distinct.
func (r haRelationship) identity() (relationship, peer string) {
	local := r.HAServers.Local.ServerName
	remote := r.HAServers.Remote
	if remote == nil || remote.ServerName == "" {
		return local, local
	}
	if local == "" {
		return remote.ServerName, remote.ServerName
	}
	return min(local, remote.ServerName), remote.ServerName
}

// haIdentities derives the label pair for every relationship and guarantees
// the pairs are distinct. Kea's own uniqueness rule makes collisions
// impossible, so this is a defence against input that did not come from a
// healthy Kea: a duplicate label set fails the whole Gather, taking every
// metric with it rather than one. See docs/kea-behaviour.md.
func haIdentities(relationships []haRelationship) [][2]string {
	out := make([][2]string, len(relationships))
	seen := make(map[[2]string]bool, len(relationships))
	for i, r := range relationships {
		relationship, peer := r.identity()
		id := [2]string{relationship, peer}
		// Suffixing the relationship separates the label sets; the loop
		// covers a suffix that collides in turn.
		for n := i; seen[id]; n++ {
			id[0] = relationship + "#" + strconv.Itoa(n)
		}
		seen[id] = true
		out[i] = id
	}
	return out
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
	// An absent HA hook and a failed status-get would both show up as missing
	// HA metrics. An explicit 0 distinguishes them without reading logs.
	if len(s.HighAvailability) == 0 {
		ch <- prometheus.MustNewConstMetric(c.haEnabled, prometheus.GaugeValue, 0)
		return nil
	}
	ch <- prometheus.MustNewConstMetric(c.haEnabled, prometheus.GaugeValue, 1)

	identities := haIdentities(s.HighAvailability)
	for i, ha := range s.HighAvailability {
		relationship, peer := identities[i][0], identities[i][1]

		// ha-mode is the FIRST relationship's mode on every entry, which is
		// harmless only because Kea requires them all to match. Latent, not
		// live -- see docs/kea-behaviour.md.
		ch <- prometheus.MustNewConstMetric(
			c.haLocalState, prometheus.GaugeValue, 1,
			relationship, peer, ha.HAServers.Local.State, ha.HAServers.Local.Role, ha.HAMode)

		// No remote half means no partner exists to describe. Zeroes would
		// read as "the partner is unreachable" rather than "there is no
		// partner".
		remote := ha.HAServers.Remote
		if remote == nil {
			continue
		}

		// Kea reports age 0 when it has never reached the partner, which
		// exported unconditionally reads as "contacted 0 seconds ago" -- the
		// inverse of the truth. The gauge is emitted only when it means
		// something, alongside an explicit in-touch signal.
		inTouch := 0.0
		if remote.InTouch {
			inTouch = 1
			ch <- prometheus.MustNewConstMetric(
				c.haPartnerAge, prometheus.GaugeValue, remote.Age, relationship, peer)
		}
		ch <- prometheus.MustNewConstMetric(
			c.haPartnerInTouch, prometheus.GaugeValue, inTouch, relationship, peer)

		// Kea reports last-state as "" until the partner is first contacted,
		// and state="" is the same falsehood as the age 0 above. Gate on the
		// field this metric reads rather than on in-touch; the two are
		// equivalent. Note "unavailable" is a real state, not an absence --
		// see docs/kea-behaviour.md.
		if remote.LastState != "" {
			ch <- prometheus.MustNewConstMetric(
				c.haPartnerState, prometheus.GaugeValue, 1,
				relationship, peer, remote.LastState, remote.Role)
		}

		commVal := 0.0
		if remote.CommunicationInterrupt {
			commVal = 1
		}
		ch <- prometheus.MustNewConstMetric(
			c.haCommBroken, prometheus.GaugeValue, commVal, relationship, peer)
	}
	return nil
}
