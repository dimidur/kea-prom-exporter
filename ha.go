package main

// The HA part of a scrape: sending status-get, decoding the HA hook's payload,
// and deciding which HA metrics it yields.
//
// One reason to change: the HA hook's response shape, which Kea versions
// separately from the core statistics, hence its own file. Which HA metrics
// are emitted, and when, follows that shape.
//
// Why the labels are derived the way they are is in docs/design.md, under "HA
// relationship labels".

import (
	"context"
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

// haRelationship is one entry of status-get's high-availability list: one
// relationship as this server sees it.
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
// that both nodes of a pair agree on (docs/design.md, "HA relationship
// labels").
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
// the pairs are distinct, against input that did not come from a healthy Kea
// (docs/design.md, "Defensive choices").
func haIdentities(relationships []haRelationship) []haIdentity {
	out := make([]haIdentity, len(relationships))
	seen := make(map[haIdentity]bool, len(relationships))
	for i, entry := range relationships {
		relationship, peer := entry.identity()
		id := haIdentity{relationship: relationship, peer: peer}
		// Suffixing the relationship separates the label sets; the loop
		// covers a suffix that collides in turn.
		for suffix := i; seen[id]; suffix++ {
			id.relationship = relationship + "#" + strconv.Itoa(suffix)
		}
		seen[id] = true
		out[i] = id
	}
	return out
}

// haIdentity is the pair of label values that identifies one relationship's
// series.
type haIdentity struct{ relationship, peer string }

// collectHA sends the HA metrics status-get reports: whether the HA hook is
// loaded, then each relationship's state. It returns the error if the command
// failed or its reply did not decode.
func (c *collector) collectHA(ctx context.Context, out chan<- prometheus.Metric) error {
	status, err := callArguments[haStatus](ctx, c.kea, cmdStatusGet)
	if err != nil {
		return err
	}
	// An absent HA hook and a failed status-get would both show up as missing
	// HA metrics. An explicit 0 distinguishes them without reading logs.
	if len(status.HighAvailability) == 0 {
		out <- prometheus.MustNewConstMetric(c.haEnabled, prometheus.GaugeValue, 0)
		return nil
	}
	out <- prometheus.MustNewConstMetric(c.haEnabled, prometheus.GaugeValue, 1)

	identities := haIdentities(status.HighAvailability)
	for i, relationshipStatus := range status.HighAvailability {
		c.emitRelationship(out, relationshipStatus, identities[i])
	}
	return nil
}

// emitRelationship sends the metrics for one HA relationship, labelled with
// identity.
func (c *collector) emitRelationship(out chan<- prometheus.Metric, status haRelationship, identity haIdentity) {
	// Kea reports the first relationship's ha-mode in every entry, which is
	// harmless only because Kea requires them all to match (docs/design.md,
	// "HA relationship labels").
	out <- prometheus.MustNewConstMetric(
		c.haLocalState, prometheus.GaugeValue, 1,
		identity.relationship, identity.peer, status.HAServers.Local.State, status.HAServers.Local.Role, status.HAMode)

	// No remote half means no partner exists to describe. Zeroes would read
	// as "the partner is unreachable" rather than "there is no partner".
	if status.HAServers.Remote != nil {
		c.emitPartner(out, status.HAServers.Remote, identity)
	}
}

// emitPartner sends the metrics describing a relationship's partner, as this
// server last heard from it, labelled with identity.
func (c *collector) emitPartner(out chan<- prometheus.Metric, remote *haRemote, identity haIdentity) {
	relationship, peer := identity.relationship, identity.peer

	// The age is emitted only once the partner is in touch, alongside an
	// explicit in-touch signal (docs/design.md, "Partner metrics before Kea
	// has a partner state").
	inTouch := 0.0
	if remote.InTouch {
		inTouch = 1
		out <- prometheus.MustNewConstMetric(
			c.haPartnerAge, prometheus.GaugeValue, remote.Age, relationship, peer)
	}
	out <- prometheus.MustNewConstMetric(
		c.haPartnerInTouch, prometheus.GaugeValue, inTouch, relationship, peer)

	// The state is emitted only once it is not empty; this gate and the one
	// on in-touch above are the same condition (docs/design.md, "Partner
	// metrics before Kea has a partner state").
	if remote.LastState != "" {
		out <- prometheus.MustNewConstMetric(
			c.haPartnerState, prometheus.GaugeValue, 1,
			relationship, peer, remote.LastState, remote.Role)
	}

	interrupted := 0.0
	if remote.CommunicationInterrupt {
		interrupted = 1
	}
	out <- prometheus.MustNewConstMetric(
		c.haCommBroken, prometheus.GaugeValue, interrupted, relationship, peer)
}
