package main

import (
	"log/slog"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
)

// Tests for ha.go: the HA part of a scrape, from the status-get payload to the
// HA metrics it yields.
//
// The hand-written payloads below follow the constraints in CONTRIBUTING.md,
// under "Tests without a Kea instance".

func TestEveryHARelationshipIsExported(t *testing.T) {
	// A daemon can hold several HA relationships (hub-and-spoke), and every
	// one is exported, told apart by its labels. The two below differ in
	// every field, so exporting one twice or dropping either is visible.

	// Arrange: a list of {ha-servers, ha-mode} with no relationship
	// identifier of its own, which is all Kea reports.
	//
	// It follows the constraints in CONTRIBUTING.md, "Tests without a Kea
	// instance": the hub carries a different server name in each
	// relationship, and both are hot-standby. Roles follow the ARM too: the central server is the standby in every
	// relationship and each branch is the primary.
	status := []byte(`[{"result":0,"arguments":{"high-availability":[
	  {"ha-mode":"hot-standby","ha-servers":{
	   "local":{"role":"standby","state":"hot-standby","server-name":"server2"},
	   "remote":{"age":2,"communication-interrupted":false,"in-touch":true,
	             "last-state":"hot-standby","role":"primary","server-name":"server1"}}},
	  {"ha-mode":"hot-standby","ha-servers":{
	   "local":{"role":"standby","state":"partner-down","server-name":"server4"},
	   "remote":{"age":94,"communication-interrupted":true,"in-touch":true,
	             "last-state":"unavailable","role":"primary","server-name":"server3"}}}]}}]`)
	kea := newFakeKea(fixture(t, "statistic-get-all.json"), status)
	collector := newCollector(kea, testReport(quietLogger))

	// relationship is min(local, remote): "server1" and "server3", the remote
	// names here rather than the local ones. Deriving it from local would
	// give server2/server4, so this payload distinguishes the two rules.
	expected := `
# HELP kea_dhcp4_ha_communication_interrupted 1 if HA communication with the partner is interrupted, else 0.
# TYPE kea_dhcp4_ha_communication_interrupted gauge
kea_dhcp4_ha_communication_interrupted{peer="server1",relationship="server1"} 0
kea_dhcp4_ha_communication_interrupted{peer="server3",relationship="server3"} 1
# HELP kea_dhcp4_ha_partner_last_contact_seconds Seconds since this peer's last successful heartbeat to the HA partner, or since the HA service started if none has succeeded. Absent until Kea has a state for the partner.
# TYPE kea_dhcp4_ha_partner_last_contact_seconds gauge
kea_dhcp4_ha_partner_last_contact_seconds{peer="server1",relationship="server1"} 2
kea_dhcp4_ha_partner_last_contact_seconds{peer="server3",relationship="server3"} 94
`

	// Act + Assert
	if err := testutil.CollectAndCompare(collector, strings.NewReader(expected),
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
	cases := []struct {
		name       string
		status     string
		statistics string
		wantPeer   string
	}{
		{"primary view", "status-get.json", "statistic-get-all.json", "kea-standby"},
		{"standby view", "status-get-standby.json", "statistic-get-all-standby.json", "kea-primary"},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			// Arrange
			kea := newFakeKea(fixture(t, testCase.statistics), fixture(t, testCase.status))
			collector := newCollector(kea, testReport(quietLogger))

			// The relationship label is the same string in both subtests;
			// only peer flips. That is the property being pinned.
			expected := `
# HELP kea_dhcp4_ha_partner_in_touch 1 once Kea has a state for the HA partner, including unavailable after a failed heartbeat, else 0.
# TYPE kea_dhcp4_ha_partner_in_touch gauge
kea_dhcp4_ha_partner_in_touch{peer="` + testCase.wantPeer + `",relationship="kea-primary"} 1
`

			// Act + Assert
			if err := testutil.CollectAndCompare(collector, strings.NewReader(expected),
				"kea_dhcp4_ha_partner_in_touch"); err != nil {
				t.Errorf("%s: %v", testCase.name, err)
			}
		})
	}
}

func TestPartnerStateAndRoleAreExported(t *testing.T) {
	// Partner state is the other half of HA visibility: local state alone
	// cannot distinguish "my partner is syncing" from "my partner is gone".

	// Arrange: the real primary capture, whose partner reports hot-standby.
	kea := healthyKea(t)
	collector := newCollector(kea, testReport(quietLogger))

	expected := `
# HELP kea_dhcp4_ha_partner_state_info HA state of the partner as this peer last recorded it (info-metric; value always 1). Absent until Kea has a state for the partner, because it reports an empty state until then.
# TYPE kea_dhcp4_ha_partner_state_info gauge
kea_dhcp4_ha_partner_state_info{peer="kea-standby",relationship="kea-primary",role="standby",state="hot-standby"} 1
`

	// Act + Assert
	if err := testutil.CollectAndCompare(collector, strings.NewReader(expected),
		"kea_dhcp4_ha_partner_state_info"); err != nil {
		t.Errorf("partner state: %v", err)
	}
}

func TestPartnerStateIsOmittedWhenKeaReportsItEmpty(t *testing.T) {
	// Kea reports last-state:"" until it has a state for the partner
	// (docs/design.md, "Partner metrics before Kea has a partner state").
	// Exporting that yields state="", so the metric is withheld instead.

	// Arrange
	status := []byte(`[{"result":0,"arguments":{"high-availability":[{"ha-mode":"hot-standby",
	  "ha-servers":{"local":{"role":"primary","state":"waiting","server-name":"a"},
	  "remote":{"age":0,"communication-interrupted":true,"in-touch":false,
	            "last-state":"","role":"standby","server-name":"b"}}}]}}]`)
	kea := newFakeKea(fixture(t, "statistic-get-all.json"), status)
	collector := newCollector(kea, testReport(quietLogger))

	// Act
	got := testutil.CollectAndCount(collector, "kea_dhcp4_ha_partner_state_info")

	// Assert
	if got != 0 {
		t.Errorf("kea_dhcp4_ha_partner_state_info emitted %d series for a never-contacted partner, want none", got)
	}
}

func TestUnavailablePartnerStateIsStillExported(t *testing.T) {
	// "unavailable" is a real HA state that Kea records after a failed
	// heartbeat, with in-touch true. It is the most alertable partner state
	// there is, and the one most at risk from a gate written slightly too
	// broadly. The last-state gate and the in-touch gate are the same
	// condition (docs/design.md, "Partner metrics before Kea has a partner
	// state"), so no test can tell them apart.

	// Arrange: the partner-down fixture, a capture of the situation that
	// produces this state.
	kea := newFakeKea(fixture(t, "statistic-get-all-standby.json"), fixture(t, "status-get-partner-down.json"))
	collector := newCollector(kea, testReport(quietLogger))

	expected := `
# HELP kea_dhcp4_ha_partner_state_info HA state of the partner as this peer last recorded it (info-metric; value always 1). Absent until Kea has a state for the partner, because it reports an empty state until then.
# TYPE kea_dhcp4_ha_partner_state_info gauge
kea_dhcp4_ha_partner_state_info{peer="kea-primary",relationship="kea-primary",role="primary",state="unavailable"} 1
`

	// Act + Assert
	if err := testutil.CollectAndCompare(collector, strings.NewReader(expected),
		"kea_dhcp4_ha_partner_state_info"); err != nil {
		t.Errorf("unavailable partner state: %v", err)
	}
}

func TestBackupServerHasNoPartnerMetrics(t *testing.T) {
	// Kea omits the remote half in passive-backup mode and on a backup server
	// (see haRemote in ha.go). Decoding it into a value struct would make that
	// indistinguishable from a partner reporting zeroes, so the exporter would
	// claim communication is fine with a partner that does not exist. Local
	// state is still exported, labelled with the only name Kea gave.

	// Arrange
	status := []byte(`[{"result":0,"arguments":{"high-availability":[{"ha-mode":"passive-backup",
	  "ha-servers":{"local":{"role":"backup","state":"backup","server-name":"archive"}}}]}}]`)
	kea := newFakeKea(fixture(t, "statistic-get-all.json"), status)
	collector := newCollector(kea, testReport(quietLogger))

	expected := `
# HELP kea_dhcp4_ha_local_state_info HA state of this peer (info-metric; value always 1; state in label).
# TYPE kea_dhcp4_ha_local_state_info gauge
kea_dhcp4_ha_local_state_info{mode="passive-backup",peer="archive",relationship="archive",role="backup",state="backup"} 1
`
	partnerMetrics := []string{
		"kea_dhcp4_ha_partner_in_touch",
		"kea_dhcp4_ha_partner_state_info",
		"kea_dhcp4_ha_communication_interrupted",
		"kea_dhcp4_ha_partner_last_contact_seconds",
	}

	// Act + Assert: local state present and labelled by the local name...
	if err := testutil.CollectAndCompare(collector, strings.NewReader(expected),
		"kea_dhcp4_ha_local_state_info"); err != nil {
		t.Errorf("backup local state: %v", err)
	}
	// ...and every partner metric absent rather than zero.
	for _, name := range partnerMetrics {
		if got := testutil.CollectAndCount(collector, name); got != 0 {
			t.Errorf("%s emitted %d series with no partner configured, want none", name, got)
		}
	}
}

func TestIdentityDerivesLabelsFromWhicheverNamesKeaGave(t *testing.T) {
	// The collector-level tests only reach the arms of identity where Kea
	// names both servers, because a healthy Kea always does. The arms for a
	// missing name are reached nowhere else, so they are pinned here, one row
	// each.
	named := func(local, remote string) haRelationship {
		var relationship haRelationship
		relationship.HAServers.Local.ServerName = local
		if remote != "-" {
			relationship.HAServers.Remote = &haRemote{ServerName: remote}
		}
		return relationship
	}
	cases := []struct {
		name              string
		local, remote     string // remote "-" means Kea omitted the block
		wantRel, wantPeer string
	}{
		{"local sorts first", "alpha", "zulu", "alpha", "zulu"},
		{"remote sorts first", "zulu", "alpha", "alpha", "alpha"},
		// No remote block, as on a backup server (see haRemote in ha.go): the
		// local name is the only one.
		{"remote block omitted", "solo", "-", "solo", "solo"},
		// A remote block present but unnamed must behave like an omitted one.
		// Falling through instead would yield min("solo", "") == "", which
		// labels a perfectly well-named relationship with an empty string.
		{"remote present but unnamed", "solo", "", "solo", "solo"},
		// The mirror: only the partner is named. min would again pick "".
		{"local unnamed", "", "partner", "partner", "partner"},
		{"nothing named at all", "", "", "", ""},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			// Arrange
			relationship := named(testCase.local, testCase.remote)

			// Act
			gotRel, gotPeer := relationship.identity()

			// Assert
			if gotRel != testCase.wantRel || gotPeer != testCase.wantPeer {
				t.Errorf("identity() = (%q, %q), want (%q, %q)",
					gotRel, gotPeer, testCase.wantRel, testCase.wantPeer)
			}
		})
	}
}

func TestCollidingIdentitiesDoNotDestroyTheWholeScrape(t *testing.T) {
	// Kea refuses a configuration that would produce these inputs, but they
	// are defended against anyway (docs/design.md, "Defensive choices"). Each
	// subtest is a distinct way two relationships can arrive at the same
	// label pair.
	//
	// The subnet assertion is the point of the test: it proves an HA oddity
	// does not cost the metrics that have nothing to do with HA.
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
			// "x#2" and Gather dies, so this is what stops the loop being
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

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			// Arrange
			kea := newFakeKea(fixture(t, "statistic-get-all.json"), []byte(testCase.status))
			collector := newCollector(kea, testReport(quietLogger))
			subnets := `
# HELP kea_dhcp4_addresses_assigned Currently assigned IPv4 addresses in the subnet. Includes declined addresses: Kea keeps them assigned so pool-utilisation stays meaningful, so do not add kea_dhcp4_addresses_declined to this.
# TYPE kea_dhcp4_addresses_assigned gauge
kea_dhcp4_addresses_assigned{subnet="1"} 23
`
			partner := "\n# HELP kea_dhcp4_ha_partner_last_contact_seconds Seconds since this peer's last successful heartbeat to the HA partner, or since the HA service started if none has succeeded. Absent until Kea has a state for the partner.\n" +
				"# TYPE kea_dhcp4_ha_partner_last_contact_seconds gauge\n" + testCase.want + "\n"
			local := "\n# HELP kea_dhcp4_ha_local_state_info HA state of this peer (info-metric; value always 1; state in label).\n" +
				"# TYPE kea_dhcp4_ha_local_state_info gauge\n" + testCase.wantLocal + "\n"

			// Act + Assert: the unrelated metrics must survive. Gather
			// fails as a whole on a duplicate, so this goes red for any
			// collision regardless of which HA metric caused it.
			if err := testutil.CollectAndCompare(collector, strings.NewReader(subnets),
				"kea_dhcp4_addresses_assigned"); err != nil {
				t.Errorf("an HA label collision took the subnet gauges with it: %v", err)
			}
			// And the HA series themselves stay distinct, with the suffix
			// landing where the comments say it does.
			if testCase.want != "" {
				if err := testutil.CollectAndCompare(collector, strings.NewReader(partner),
					"kea_dhcp4_ha_partner_last_contact_seconds"); err != nil {
					t.Errorf("colliding identities: %v", err)
				}
			}
			if testCase.wantLocal != "" {
				if err := testutil.CollectAndCompare(collector, strings.NewReader(local),
					"kea_dhcp4_ha_local_state_info"); err != nil {
					t.Errorf("colliding identities (local state): %v", err)
				}
			}
		})
	}
}

func TestStatusGetDecodeErrorFailsTheScrape(t *testing.T) {
	// A status-get whose arguments do not decode is a broken scrape, not an
	// absent HA hook. Swallowing the error would report kea_up 1 with no HA
	// metrics, indistinguishable from the hook not being loaded.

	// Arrange
	status := []byte(`[{"result":0,"arguments":{"high-availability":"not-an-array"}}]`)
	kea := newFakeKea(fixture(t, "statistic-get-all.json"), status)
	logger, logs := capturingLogger(slog.LevelInfo)
	collector := newCollector(kea, testReport(logger))

	// Act
	got := testutil.ToFloat64(upOnly{collector})

	// Assert
	if got != 0 {
		t.Errorf("kea_up = %v when status-get could not be decoded, want 0", got)
	}
	if !strings.Contains(logs.String(), "decode status-get arguments") {
		t.Errorf("log = %q, want the decode failure attributed to status-get", logs.String())
	}
}

func TestPartnerDownIsReported(t *testing.T) {
	// kea_dhcp4_ha_communication_interrupted is the metric people alert on,
	// and the other tests only ever see it at 0, so a constant 0 in its place
	// would pass them. A partner-down pair cannot be captured without
	// breaking production DHCP, so this is the live standby capture with the
	// one boolean flipped and the local state moved to partner-down, per the
	// fixture carve-out in CONTRIBUTING.

	// Arrange
	kea := newFakeKea(fixture(t, "statistic-get-all-standby.json"), fixture(t, "status-get-partner-down.json"))
	collector := newCollector(kea, testReport(quietLogger))

	expected := `
# HELP kea_dhcp4_ha_communication_interrupted 1 if HA communication with the partner is interrupted, else 0.
# TYPE kea_dhcp4_ha_communication_interrupted gauge
kea_dhcp4_ha_communication_interrupted{peer="kea-primary",relationship="kea-primary"} 1
# HELP kea_dhcp4_ha_local_state_info HA state of this peer (info-metric; value always 1; state in label).
# TYPE kea_dhcp4_ha_local_state_info gauge
kea_dhcp4_ha_local_state_info{mode="hot-standby",peer="kea-primary",relationship="kea-primary",role="standby",state="partner-down"} 1
# HELP kea_dhcp4_ha_partner_last_contact_seconds Seconds since this peer's last successful heartbeat to the HA partner, or since the HA service started if none has succeeded. Absent until Kea has a state for the partner.
# TYPE kea_dhcp4_ha_partner_last_contact_seconds gauge
kea_dhcp4_ha_partner_last_contact_seconds{peer="kea-primary",relationship="kea-primary"} 73
`

	// Act + Assert
	if err := testutil.CollectAndCompare(collector, strings.NewReader(expected),
		"kea_dhcp4_ha_communication_interrupted", "kea_dhcp4_ha_local_state_info",
		"kea_dhcp4_ha_partner_last_contact_seconds"); err != nil {
		t.Errorf("partner-down metrics: %v", err)
	}

	// Losing the partner is not a scrape failure: the exporter is fine and the
	// HA metrics are precisely how whoever runs it learns the pair is degraded.
	if got := testutil.ToFloat64(upOnly{collector}); got != 1 {
		t.Errorf("kea_up = %v with the partner down, want 1", got)
	}
}

func TestHAHookAbsentIsDistinguishableFromScrapeFailure(t *testing.T) {
	// No relationships, whether the `high-availability` key is missing or its
	// list is empty, means the hook is not loaded. That must be an explicit
	// 0, not silence, or it is indistinguishable from status-get having
	// failed.
	tests := []struct {
		name   string
		status string
	}{
		{"no high-availability key", `[{"result":0,"arguments":{"pid":1,"uptime":10}}]`},
		{"an empty high-availability list", `[{"result":0,"arguments":{"high-availability":[]}}]`},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			// Arrange
			kea := newFakeKea(fixture(t, "statistic-get-all.json"), []byte(testCase.status))
			collector := newCollector(kea, testReport(quietLogger))
			expected := `
# HELP kea_dhcp4_ha_enabled 1 when Kea reports an HA relationship, 0 when the HA hook is not loaded.
# TYPE kea_dhcp4_ha_enabled gauge
kea_dhcp4_ha_enabled 0
`

			// Act + Assert
			if err := testutil.CollectAndCompare(collector, strings.NewReader(expected), "kea_dhcp4_ha_enabled"); err != nil {
				t.Errorf("ha_enabled: %v", err)
			}
			// And the scrape itself is healthy: an absent hook is not an
			// error.
			if got := testutil.ToFloat64(upOnly{collector}); got != 1 {
				t.Errorf("kea_up = %v with the HA hook absent, want 1", got)
			}
		})
	}
}

func TestPartnerAgeIsAbsentUntilInTouch(t *testing.T) {
	// The partner age is withheld until in-touch is true (docs/design.md,
	// "Partner metrics before Kea has a partner state").

	// Arrange
	status := []byte(`[{"result":0,"arguments":{"high-availability":[{"ha-mode":"hot-standby",
	  "ha-servers":{"local":{"role":"primary","state":"waiting","server-name":"a"},
	  "remote":{"age":0,"communication-interrupted":true,"in-touch":false,"server-name":"b"}}}]}}]`)
	kea := newFakeKea(fixture(t, "statistic-get-all.json"), status)
	collector := newCollector(kea, testReport(quietLogger))
	expected := `
# HELP kea_dhcp4_ha_partner_in_touch 1 once Kea has a state for the HA partner, including unavailable after a failed heartbeat, else 0.
# TYPE kea_dhcp4_ha_partner_in_touch gauge
kea_dhcp4_ha_partner_in_touch{peer="b",relationship="a"} 0
`

	// Act + Assert
	if err := testutil.CollectAndCompare(collector, strings.NewReader(expected),
		"kea_dhcp4_ha_partner_in_touch", "kea_dhcp4_ha_partner_last_contact_seconds"); err != nil {
		t.Errorf("partner contact metrics: %v", err)
	}
}
