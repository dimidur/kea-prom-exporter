package main

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
)

// Tests for ha.go: decoding the HA hook's status-get payload.

func TestSecondHARelationshipIsReported(t *testing.T) {
	// Kea supports several HA relationships per daemon and this exporter
	// exports only the first. Dropping the warning would make the others
	// vanish with nothing anywhere saying so.

	// Arrange
	status := []byte(`[{"result":0,"arguments":{"high-availability":[
	  {"ha-mode":"hot-standby","ha-servers":{"local":{"role":"primary","state":"hot-standby"},
	   "remote":{"age":2,"communication-interrupted":false,"in-touch":true}}},
	  {"ha-mode":"load-balancing","ha-servers":{"local":{"role":"primary","state":"load-balancing"},
	   "remote":{"age":1,"communication-interrupted":false,"in-touch":true}}}]}}]`)
	srv := keaStub(t, fixture(t, "statistic-get-all.json"), status)
	defer srv.Close()

	var buf bytes.Buffer
	c := newCollector(&keaClient{url: srv.URL, client: srv.Client()})
	c.log = slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn}))

	// Act
	collectAll(c)

	// Assert
	if !strings.Contains(buf.String(), "only the first HA relationship") {
		t.Errorf("a second HA relationship was dropped without a warning; log was:\n%s", buf.String())
	}
	if !strings.Contains(buf.String(), "relationships=2") {
		t.Errorf("the warning does not say how many were found; log was:\n%s", buf.String())
	}

	// And it must be the FIRST relationship that is exported, which is the
	// documented contract. The two differ in every field precisely so that
	// exporting the wrong one is visible: the second is load-balancing with
	// age 1.
	expected := `
# HELP kea_dhcp4_ha_local_state_info HA state of this peer (info-metric; value always 1; state in label).
# TYPE kea_dhcp4_ha_local_state_info gauge
kea_dhcp4_ha_local_state_info{mode="hot-standby",role="primary",state="hot-standby"} 1
# HELP kea_dhcp4_ha_partner_last_contact_seconds Seconds since the last successful heartbeat from the HA partner. Absent until the partner has been contacted at least once.
# TYPE kea_dhcp4_ha_partner_last_contact_seconds gauge
kea_dhcp4_ha_partner_last_contact_seconds 2
`
	if err := testutil.CollectAndCompare(c, strings.NewReader(expected),
		"kea_dhcp4_ha_local_state_info", "kea_dhcp4_ha_partner_last_contact_seconds"); err != nil {
		t.Errorf("the exported relationship is not the first one: %v", err)
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
kea_dhcp4_ha_communication_interrupted 1
# HELP kea_dhcp4_ha_local_state_info HA state of this peer (info-metric; value always 1; state in label).
# TYPE kea_dhcp4_ha_local_state_info gauge
kea_dhcp4_ha_local_state_info{mode="hot-standby",role="standby",state="partner-down"} 1
# HELP kea_dhcp4_ha_partner_last_contact_seconds Seconds since the last successful heartbeat from the HA partner. Absent until the partner has been contacted at least once.
# TYPE kea_dhcp4_ha_partner_last_contact_seconds gauge
kea_dhcp4_ha_partner_last_contact_seconds 47
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
	// Act
	expected := `
# HELP kea_dhcp4_ha_enabled 1 when Kea reports an HA relationship, 0 when the HA hook is not loaded.
# TYPE kea_dhcp4_ha_enabled gauge
kea_dhcp4_ha_enabled 0
`
	// Assert
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
	  "ha-servers":{"local":{"role":"primary","state":"waiting"},
	  "remote":{"age":0,"communication-interrupted":true,"in-touch":false}}}]}}]`)
	srv := keaStub(t, fixture(t, "statistic-get-all.json"), status)
	defer srv.Close()

	c := newCollector(&keaClient{url: srv.URL, client: srv.Client()})
	// Act
	expected := `
# HELP kea_dhcp4_ha_partner_in_touch 1 once this peer has been in contact with its HA partner, else 0.
# TYPE kea_dhcp4_ha_partner_in_touch gauge
kea_dhcp4_ha_partner_in_touch 0
`
	// Assert
	if err := testutil.CollectAndCompare(c, strings.NewReader(expected),
		"kea_dhcp4_ha_partner_in_touch", "kea_dhcp4_ha_partner_last_contact_seconds"); err != nil {
		t.Errorf("partner contact metrics: %v", err)
	}
}
