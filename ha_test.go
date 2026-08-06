package main

import (
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
)

// Tests for ha.go: decoding the HA hook's status-get payload.
//
// Kea symbols named below hold across the supported Kea releases; the
// supported set is in README.md.

func TestEveryHARelationshipIsExported(t *testing.T) {
	// A daemon can hold several HA relationships (hub-and-spoke), and every
	// one is exported, told apart by its labels. The two below differ in
	// every field, so exporting one twice or dropping either is visible.

	// Arrange -- the shape HAImpl::commandProcessed builds: a list of
	// {ha-servers, ha-mode} with no relationship identifier of its own.
	//
	// Two config-time constraints shape the payload:
	//
	//  - Server names are unique across ALL relationships, so the hub carries
	//    a different this-server-name in each. HAConfigParser::parseOne
	//    rejects a duplicate with "server names must be unique for different
	//    relationships".
	//  - With more than one relationship, every one must be hot-standby
	//    (HAConfigParser::validateRelationships), so neither is
	//    load-balancing.
	//
	// Roles follow the ARM too: the central server is the standby in every
	// relationship and each branch is the primary. And communication-
	// interrupted is only true where age exceeds max-response-delay, since
	// CommunicationState::isCommunicationInterrupted is exactly
	// getDurationInMillisecs() > getMaxResponseDelay(), whose default is
	// 60000ms.
	status := []byte(`[{"result":0,"arguments":{"high-availability":[
	  {"ha-mode":"hot-standby","ha-servers":{
	   "local":{"role":"standby","state":"hot-standby","server-name":"server2"},
	   "remote":{"age":2,"communication-interrupted":false,"in-touch":true,
	             "last-state":"hot-standby","role":"primary","server-name":"server1"}}},
	  {"ha-mode":"hot-standby","ha-servers":{
	   "local":{"role":"standby","state":"partner-down","server-name":"server4"},
	   "remote":{"age":94,"communication-interrupted":true,"in-touch":true,
	             "last-state":"unavailable","role":"primary","server-name":"server3"}}}]}}]`)
	srv := keaStub(t, fixture(t, "statistic-get-all.json"), status)
	defer srv.Close()
	c := newCollector(&keaClient{url: srv.URL, client: srv.Client()})

	// relationship is min(local, remote): "server1" and "server3", the remote
	// names here rather than the local ones. Deriving it from local would
	// give server2/server4, so this payload distinguishes the two rules.
	expected := `
# HELP kea_dhcp4_ha_communication_interrupted 1 if HA communication with the partner is interrupted, else 0.
# TYPE kea_dhcp4_ha_communication_interrupted gauge
kea_dhcp4_ha_communication_interrupted{peer="server1",relationship="server1"} 0
kea_dhcp4_ha_communication_interrupted{peer="server3",relationship="server3"} 1
# HELP kea_dhcp4_ha_partner_last_contact_seconds Seconds since the last successful heartbeat from the HA partner. Absent until the partner has been contacted at least once.
# TYPE kea_dhcp4_ha_partner_last_contact_seconds gauge
kea_dhcp4_ha_partner_last_contact_seconds{peer="server1",relationship="server1"} 2
kea_dhcp4_ha_partner_last_contact_seconds{peer="server3",relationship="server3"} 94
`
	// Act + Assert
	if err := testutil.CollectAndCompare(c, strings.NewReader(expected),
		"kea_dhcp4_ha_communication_interrupted",
		"kea_dhcp4_ha_partner_last_contact_seconds"); err != nil {
		t.Errorf("multi-relationship metrics: %v", err)
	}
}

func TestRelationshipLabelIsIdenticalFromBothPeers(t *testing.T) {
	// Why relationship is min(local, remote) rather than the partner's name:
	// one exporter runs on each Kea node and each sees the other as its peer,
	// so a peer-derived label differs between the two views of one
	// relationship and cannot join them. Both captures are real, taken from
	// the two live nodes of one hot-standby pair.

	// Arrange
	cases := []struct {
		name       string
		status     string
		statistics string
		wantPeer   string
	}{
		{"primary view", "status-get.json", "statistic-get-all.json", "kea-standby"},
		{"standby view", "status-get-standby.json", "statistic-get-all-standby.json", "kea-primary"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Arrange
			srv := keaStub(t, fixture(t, tc.statistics), fixture(t, tc.status))
			defer srv.Close()
			c := newCollector(&keaClient{url: srv.URL, client: srv.Client()})

			// The relationship label is the same string in both subtests;
			// only peer flips. That is the property being pinned.
			expected := `
# HELP kea_dhcp4_ha_partner_in_touch 1 once this peer has been in contact with its HA partner, else 0.
# TYPE kea_dhcp4_ha_partner_in_touch gauge
kea_dhcp4_ha_partner_in_touch{peer="` + tc.wantPeer + `",relationship="kea-primary"} 1
`
			// Act + Assert
			if err := testutil.CollectAndCompare(c, strings.NewReader(expected),
				"kea_dhcp4_ha_partner_in_touch"); err != nil {
				t.Errorf("%s: %v", tc.name, err)
			}
		})
	}
}

func TestPartnerStateAndRoleAreExported(t *testing.T) {
	// Partner state is the other half of HA visibility: local state alone
	// cannot distinguish "my partner is syncing" from "my partner is gone".

	// Arrange -- the real primary capture, whose partner reports hot-standby.
	srv := keaStub(t, fixture(t, "statistic-get-all.json"), fixture(t, "status-get.json"))
	defer srv.Close()
	c := newCollector(&keaClient{url: srv.URL, client: srv.Client()})

	expected := `
# HELP kea_dhcp4_ha_partner_state_info HA state of the partner as last reported to this peer (info-metric; value always 1). Absent until the partner has been contacted at least once, because Kea reports an empty state until then.
# TYPE kea_dhcp4_ha_partner_state_info gauge
kea_dhcp4_ha_partner_state_info{peer="kea-standby",relationship="kea-primary",role="standby",state="hot-standby"} 1
`
	// Act + Assert
	if err := testutil.CollectAndCompare(c, strings.NewReader(expected),
		"kea_dhcp4_ha_partner_state_info"); err != nil {
		t.Errorf("partner state: %v", err)
	}
}

func TestPartnerStateIsOmittedWhenKeaReportsItEmpty(t *testing.T) {
	// The CommunicationState constructor starts partner_state_ at -1 and
	// stateToString rejects it, so getReport's try/catch writes last-state:""
	// for a partner never contacted. Exporting that yields state="" -- the
	// same class of falsehood as reporting age 0 as "contacted 0 seconds
	// ago". The metric is withheld instead.

	// Arrange
	status := []byte(`[{"result":0,"arguments":{"high-availability":[{"ha-mode":"hot-standby",
	  "ha-servers":{"local":{"role":"primary","state":"waiting","server-name":"a"},
	  "remote":{"age":0,"communication-interrupted":true,"in-touch":false,
	            "last-state":"","role":"standby","server-name":"b"}}}]}}]`)
	srv := keaStub(t, fixture(t, "statistic-get-all.json"), status)
	defer srv.Close()
	c := newCollector(&keaClient{url: srv.URL, client: srv.Client()})

	// Act
	got := testutil.CollectAndCount(c, "kea_dhcp4_ha_partner_state_info")

	// Assert
	if got != 0 {
		t.Errorf("kea_dhcp4_ha_partner_state_info emitted %d series for a never-contacted partner, want none", got)
	}
}

func TestUnavailablePartnerStateIsStillExported(t *testing.T) {
	// "unavailable" is a real HA state (HA_UNAVAILABLE_ST,
	// stateToString) that Kea reports once it has decided the partner is
	// gone. It is the most alertable partner state there is, and the one most
	// at risk from a gate written slightly too broadly.
	//
	// in-touch is true here, not false: getReport derives it from
	// getPartnerState() > 0 and HA_UNAVAILABLE_ST is a large positive
	// constant. This test does not distinguish the last-state gate from an
	// in-touch gate, and no test can, because the two are equivalent for
	// every input Kea can produce -- see the gate in ha.go.

	// Arrange -- the partner-down fixture, which is the situation producing
	// this state: every path concluding the partner is gone calls
	// CommunicationState::setPartnerUnavailable, which writes exactly
	// "unavailable".
	srv := keaStub(t, fixture(t, "statistic-get-all-standby.json"), fixture(t, "status-get-partner-down.json"))
	defer srv.Close()
	c := newCollector(&keaClient{url: srv.URL, client: srv.Client()})

	expected := `
# HELP kea_dhcp4_ha_partner_state_info HA state of the partner as last reported to this peer (info-metric; value always 1). Absent until the partner has been contacted at least once, because Kea reports an empty state until then.
# TYPE kea_dhcp4_ha_partner_state_info gauge
kea_dhcp4_ha_partner_state_info{peer="kea-primary",relationship="kea-primary",role="primary",state="unavailable"} 1
`
	// Act + Assert
	if err := testutil.CollectAndCompare(c, strings.NewReader(expected),
		"kea_dhcp4_ha_partner_state_info"); err != nil {
		t.Errorf("unavailable partner state: %v", err)
	}
}

func TestBackupServerHasNoPartnerMetrics(t *testing.T) {
	// HAService::processStatusGet returns before setting the remote half in
	// passive-backup mode and on a backup server. Decoding it into a value
	// struct would make that indistinguishable from a partner reporting
	// zeroes, so the exporter would claim communication is fine with a
	// partner that does not exist. Local state is still exported, labelled
	// with the only name Kea gave.

	// Arrange
	status := []byte(`[{"result":0,"arguments":{"high-availability":[{"ha-mode":"passive-backup",
	  "ha-servers":{"local":{"role":"backup","state":"backup","server-name":"archive"}}}]}}]`)
	srv := keaStub(t, fixture(t, "statistic-get-all.json"), status)
	defer srv.Close()
	c := newCollector(&keaClient{url: srv.URL, client: srv.Client()})

	expected := `
# HELP kea_dhcp4_ha_local_state_info HA state of this peer (info-metric; value always 1; state in label).
# TYPE kea_dhcp4_ha_local_state_info gauge
kea_dhcp4_ha_local_state_info{mode="passive-backup",peer="archive",relationship="archive",role="backup",state="backup"} 1
`
	// Act + Assert -- local state present and labelled by the local name...
	if err := testutil.CollectAndCompare(c, strings.NewReader(expected),
		"kea_dhcp4_ha_local_state_info"); err != nil {
		t.Errorf("backup local state: %v", err)
	}
	// ...and every partner metric absent rather than zero.
	for _, name := range []string{
		"kea_dhcp4_ha_partner_in_touch",
		"kea_dhcp4_ha_partner_state_info",
		"kea_dhcp4_ha_communication_interrupted",
		"kea_dhcp4_ha_partner_last_contact_seconds",
	} {
		if got := testutil.CollectAndCount(c, name); got != 0 {
			t.Errorf("%s emitted %d series with no partner configured, want none", name, got)
		}
	}
}

func TestIdentityDerivesLabelsFromWhicheverNamesKeaGave(t *testing.T) {
	// identity has four arms and the collector-level tests only ever reach
	// two of them, because a healthy Kea always names both servers. The
	// remaining arms decide what happens when a name is missing, and deleting
	// either of them left the whole suite green -- so they are pinned here,
	// where each arm is one row.

	// Arrange
	named := func(local, remote string) haRelationship {
		var r haRelationship
		r.HAServers.Local.ServerName = local
		if remote != "-" {
			r.HAServers.Remote = &haRemote{ServerName: remote}
		}
		return r
	}
	cases := []struct {
		name              string
		local, remote     string // remote "-" means Kea omitted the block
		wantRel, wantPeer string
	}{
		{"local sorts first", "alpha", "zulu", "alpha", "zulu"},
		{"remote sorts first", "zulu", "alpha", "alpha", "alpha"},
		// Kea omits the remote block for a backup server or passive-backup
		// mode, so the local name is the only one there is.
		{"remote block omitted", "solo", "-", "solo", "solo"},
		// A remote block present but unnamed must behave like an omitted one.
		// Falling through instead would yield min("solo", "") == "", which
		// labels a perfectly well-named relationship with an empty string.
		{"remote present but unnamed", "solo", "", "solo", "solo"},
		// The mirror: only the partner is named. min would again pick "".
		{"local unnamed", "", "partner", "partner", "partner"},
		{"nothing named at all", "", "", "", ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Act
			gotRel, gotPeer := named(tc.local, tc.remote).identity()

			// Assert
			if gotRel != tc.wantRel || gotPeer != tc.wantPeer {
				t.Errorf("identity() = (%q, %q), want (%q, %q)",
					gotRel, gotPeer, tc.wantRel, tc.wantPeer)
			}
		})
	}
}

func TestCollidingIdentitiesDoNotDestroyTheWholeScrape(t *testing.T) {
	// Kea's own loader makes these inputs impossible -- peer names are unique
	// across a daemon's relationships or the hook does not load (see the
	// citations on identity). They are still worth defending against, because
	// a duplicate label set does not fail one metric: it fails the entire
	// Gather. A single unexpected field would take /metrics to 500 and take
	// the subnet gauges down with it, so the failure mode is total, not
	// local. Each subtest is a distinct way two relationships can arrive at
	// the same label pair.
	//
	// The subnet assertion is the point of the test: it proves an HA oddity
	// does not cost the metrics that have nothing to do with HA.

	// Arrange
	cases := []struct {
		name      string
		status    string
		want      string // expected kea_dhcp4_ha_partner_last_contact_seconds series
		wantLocal string // expected kea_dhcp4_ha_local_state_info series
	}{
		{
			// Both unnamed everywhere.
			name: "no names at all",
			status: `[{"result":0,"arguments":{"high-availability":[
			  {"ha-mode":"hot-standby","ha-servers":{"local":{"role":"primary","state":"hot-standby"},
			   "remote":{"age":1,"communication-interrupted":false,"in-touch":true}}},
			  {"ha-mode":"hot-standby","ha-servers":{"local":{"role":"primary","state":"waiting"},
			   "remote":{"age":2,"communication-interrupted":false,"in-touch":true}}}]}}]`,
			want: `kea_dhcp4_ha_partner_last_contact_seconds{peer="",relationship=""} 1
kea_dhcp4_ha_partner_last_contact_seconds{peer="",relationship="#1"} 2`,
		},
		{
			// Named locally, no remote half: both fall back to the same local
			// name. This is the shape the fallback misses if it only handles
			// the wholly-unnamed case.
			name: "same local name, remote omitted",
			status: `[{"result":0,"arguments":{"high-availability":[
			  {"ha-mode":"passive-backup","ha-servers":{"local":{"role":"backup","state":"backup","server-name":"hub"}}},
			  {"ha-mode":"passive-backup","ha-servers":{"local":{"role":"backup","state":"backup","server-name":"hub"}}}]}}]`,
			// No partner half, so no partner_last_contact series exists to
			// compare; the local-state metric carries the disambiguation.
			wantLocal: `kea_dhcp4_ha_local_state_info{mode="passive-backup",peer="hub",relationship="hub",role="backup",state="backup"} 1
kea_dhcp4_ha_local_state_info{mode="passive-backup",peer="hub",relationship="hub#1",role="backup",state="backup"} 1`,
		},
		{
			// A suffixed identity that collides in turn, which is the only
			// input where the suffix loop iterates more than once. With a
			// plain `if` instead of the loop, entries 2 and 3 both land on
			// "x#2" and Gather dies -- so this is what stops the loop being
			// decorative. Entry 2 is named "x#2" outright, which is a legal
			// Kea server name.
			name: "a suffix that collides in turn",
			status: `[{"result":0,"arguments":{"high-availability":[
			  {"ha-mode":"hot-standby","ha-servers":{"local":{"role":"primary","state":"hot-standby","server-name":"x"},
			   "remote":{"age":1,"communication-interrupted":false,"in-touch":true,"server-name":"zz"}}},
			  {"ha-mode":"hot-standby","ha-servers":{"local":{"role":"primary","state":"hot-standby","server-name":"x#2"},
			   "remote":{"age":2,"communication-interrupted":false,"in-touch":true,"server-name":"zz"}}},
			  {"ha-mode":"hot-standby","ha-servers":{"local":{"role":"primary","state":"hot-standby","server-name":"x"},
			   "remote":{"age":3,"communication-interrupted":false,"in-touch":true,"server-name":"zz"}}}]}}]`,
			want: `kea_dhcp4_ha_partner_last_contact_seconds{peer="zz",relationship="x"} 1
kea_dhcp4_ha_partner_last_contact_seconds{peer="zz",relationship="x#2"} 2
kea_dhcp4_ha_partner_last_contact_seconds{peer="zz",relationship="x#3"} 3`,
		},
		{
			// No collision here at all: this pins that the fallback is NOT
			// positional. A list-index fallback would collide head-on with a
			// server genuinely called "0", which is a name Kea accepts.
			name: "a server actually named 0",
			status: `[{"result":0,"arguments":{"high-availability":[
			  {"ha-mode":"hot-standby","ha-servers":{"local":{"role":"primary","state":"hot-standby"},
			   "remote":{"age":1,"communication-interrupted":false,"in-touch":true}}},
			  {"ha-mode":"hot-standby","ha-servers":{"local":{"role":"primary","state":"hot-standby","server-name":"0"},
			   "remote":{"age":2,"communication-interrupted":false,"in-touch":true,"server-name":"0"}}}]}}]`,
			want: `kea_dhcp4_ha_partner_last_contact_seconds{peer="",relationship=""} 1
kea_dhcp4_ha_partner_last_contact_seconds{peer="0",relationship="0"} 2`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Arrange
			srv := keaStub(t, fixture(t, "statistic-get-all.json"), []byte(tc.status))
			defer srv.Close()
			c := newCollector(&keaClient{url: srv.URL, client: srv.Client()})

			// Act + Assert -- the unrelated metrics must survive. Gather
			// fails as a whole on a duplicate, so this goes red for any
			// collision regardless of which HA metric caused it.
			subnets := `
# HELP kea_dhcp4_addresses_assigned Currently assigned IPv4 addresses in the pool. Includes declined addresses: Kea keeps them assigned so pool-utilisation stays meaningful, so do not add kea_dhcp4_addresses_declined to this.
# TYPE kea_dhcp4_addresses_assigned gauge
kea_dhcp4_addresses_assigned{subnet="1"} 23
`
			if err := testutil.CollectAndCompare(c, strings.NewReader(subnets),
				"kea_dhcp4_addresses_assigned"); err != nil {
				t.Errorf("an HA label collision took the subnet gauges with it: %v", err)
			}

			// And the HA series themselves stay distinct, with the suffix
			// landing where the comments say it does.
			if tc.want != "" {
				expected := "\n# HELP kea_dhcp4_ha_partner_last_contact_seconds Seconds since the last successful heartbeat from the HA partner. Absent until the partner has been contacted at least once.\n" +
					"# TYPE kea_dhcp4_ha_partner_last_contact_seconds gauge\n" + tc.want + "\n"
				if err := testutil.CollectAndCompare(c, strings.NewReader(expected),
					"kea_dhcp4_ha_partner_last_contact_seconds"); err != nil {
					t.Errorf("colliding identities: %v", err)
				}
			}
			if tc.wantLocal != "" {
				expected := "\n# HELP kea_dhcp4_ha_local_state_info HA state of this peer (info-metric; value always 1; state in label).\n" +
					"# TYPE kea_dhcp4_ha_local_state_info gauge\n" + tc.wantLocal + "\n"
				if err := testutil.CollectAndCompare(c, strings.NewReader(expected),
					"kea_dhcp4_ha_local_state_info"); err != nil {
					t.Errorf("colliding identities (local state): %v", err)
				}
			}
		})
	}
}

func TestStatusGetDecodeErrorFailsTheScrape(t *testing.T) {
	// A status-get whose arguments do not decode is a broken scrape, not an
	// absent HA hook. Swallowing the error reported kea_up 1 with no HA
	// metrics -- indistinguishable from the hook not being loaded.

	// Arrange
	status := []byte(`[{"result":0,"arguments":{"high-availability":"not-an-array"}}]`)
	srv := keaStub(t, fixture(t, "statistic-get-all.json"), status)
	defer srv.Close()
	c := newCollector(&keaClient{url: srv.URL, client: srv.Client()})

	// Act
	got := testutil.ToFloat64(upOnly{c})

	// Assert
	if got != 0 {
		t.Errorf("kea_up = %v when status-get could not be decoded, want 0", got)
	}
}

func TestPartnerDownIsReported(t *testing.T) {
	// kea_dhcp4_ha_communication_interrupted is the metric an operator alerts
	// on, and it was only ever asserted as 0 -- so replacing the branch with a
	// constant 0 kept the whole suite green. A partner-down pair cannot be
	// captured without breaking production DHCP, so this is the live standby
	// capture with the one boolean flipped and the local state moved to
	// partner-down, per the fixture carve-out in CONTRIBUTING.

	// Arrange
	srv := keaStub(t, fixture(t, "statistic-get-all-standby.json"), fixture(t, "status-get-partner-down.json"))
	defer srv.Close()
	c := newCollector(&keaClient{url: srv.URL, client: srv.Client()})

	expected := `
# HELP kea_dhcp4_ha_communication_interrupted 1 if HA communication with the partner is interrupted, else 0.
# TYPE kea_dhcp4_ha_communication_interrupted gauge
kea_dhcp4_ha_communication_interrupted{peer="kea-primary",relationship="kea-primary"} 1
# HELP kea_dhcp4_ha_local_state_info HA state of this peer (info-metric; value always 1; state in label).
# TYPE kea_dhcp4_ha_local_state_info gauge
kea_dhcp4_ha_local_state_info{mode="hot-standby",peer="kea-primary",relationship="kea-primary",role="standby",state="partner-down"} 1
# HELP kea_dhcp4_ha_partner_last_contact_seconds Seconds since the last successful heartbeat from the HA partner. Absent until the partner has been contacted at least once.
# TYPE kea_dhcp4_ha_partner_last_contact_seconds gauge
kea_dhcp4_ha_partner_last_contact_seconds{peer="kea-primary",relationship="kea-primary"} 73
`

	// Act + Assert
	if err := testutil.CollectAndCompare(c, strings.NewReader(expected),
		"kea_dhcp4_ha_communication_interrupted", "kea_dhcp4_ha_local_state_info",
		"kea_dhcp4_ha_partner_last_contact_seconds"); err != nil {
		t.Errorf("partner-down metrics: %v", err)
	}

	// Losing the partner is not a scrape failure: the exporter is fine and the
	// HA metrics are precisely how the operator learns the pair is degraded.
	if got := testutil.ToFloat64(upOnly{c}); got != 1 {
		t.Errorf("kea_up = %v with the partner down, want 1", got)
	}
}

func TestHAHookAbsentIsDistinguishableFromScrapeFailure(t *testing.T) {
	// No `high-availability` key means the hook is not loaded. That must be
	// an explicit 0, not silence -- otherwise it is indistinguishable from
	// status-get having failed.
	// Arrange
	status := []byte(`[{"result":0,"arguments":{"pid":1,"uptime":10}}]`)
	srv := keaStub(t, fixture(t, "statistic-get-all.json"), status)
	defer srv.Close()

	c := newCollector(&keaClient{url: srv.URL, client: srv.Client()})
	expected := `
# HELP kea_dhcp4_ha_enabled 1 when Kea reports an HA relationship, 0 when the HA hook is not loaded.
# TYPE kea_dhcp4_ha_enabled gauge
kea_dhcp4_ha_enabled 0
`
	// Act + Assert
	if err := testutil.CollectAndCompare(c, strings.NewReader(expected), "kea_dhcp4_ha_enabled"); err != nil {
		t.Errorf("ha_enabled: %v", err)
	}
	// And the scrape itself is healthy: an absent hook is not an error.
	if got := testutil.ToFloat64(upOnly{c}); got != 1 {
		t.Errorf("kea_up = %v with the HA hook absent, want 1", got)
	}
}

func TestPartnerAgeIsAbsentUntilInTouch(t *testing.T) {
	// Kea reports age 0 when it has never reached the partner. Exported
	// unconditionally that reads as "contacted 0 seconds ago" -- the inverse
	// of the truth -- so the gauge is withheld until in-touch is true.
	// Arrange
	status := []byte(`[{"result":0,"arguments":{"high-availability":[{"ha-mode":"hot-standby",
	  "ha-servers":{"local":{"role":"primary","state":"waiting","server-name":"a"},
	  "remote":{"age":0,"communication-interrupted":true,"in-touch":false,"server-name":"b"}}}]}}]`)
	srv := keaStub(t, fixture(t, "statistic-get-all.json"), status)
	defer srv.Close()

	c := newCollector(&keaClient{url: srv.URL, client: srv.Client()})
	expected := `
# HELP kea_dhcp4_ha_partner_in_touch 1 once this peer has been in contact with its HA partner, else 0.
# TYPE kea_dhcp4_ha_partner_in_touch gauge
kea_dhcp4_ha_partner_in_touch{peer="b",relationship="a"} 0
`
	// Act + Assert
	if err := testutil.CollectAndCompare(c, strings.NewReader(expected),
		"kea_dhcp4_ha_partner_in_touch", "kea_dhcp4_ha_partner_last_contact_seconds"); err != nil {
		t.Errorf("partner contact metrics: %v", err)
	}
}
