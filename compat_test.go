//go:build compat

package main

// The compatibility check: the exporter against a running Kea. How to run it
// is in CONTRIBUTING.md, "Tests against a real Kea"; why it exists and when CI
// runs it is in docs/design.md, "Compatibility is checked against a real Kea".
// The Kea under test is the one .github/compat/kea.sh starts.

import (
	"log/slog"
	"net/http"
	"os"
	"slices"
	"testing"
	"time"
)

// compatSettings are the settings for the Kea under test, from the
// environment.
func compatSettings(t *testing.T) keaSettings {
	t.Helper()
	setting := func(name string) string {
		value := os.Getenv(name)
		if value == "" {
			t.Fatalf("%s is not set; the compat tests need a running Kea", name)
		}
		return value
	}
	return keaSettings{
		url:      setting("KEA_COMPAT_URL"),
		user:     setting("KEA_COMPAT_USER"),
		password: setting("KEA_COMPAT_PASSWORD"),
		service:  supportedService,
		timeout:  5 * time.Second,
	}
}

func TestCompat(t *testing.T) {
	// One scrape of the Kea under test, held to each property the exporter
	// depends on. Each subtest is one property; its name is what a failure
	// reports.

	// Arrange
	logger, logs := capturingLogger(slog.LevelDebug)
	collector := newCollector(newKeaClient(compatSettings(t), http.DefaultTransport), testReport(logger))

	// Act
	families := gather(t, collector)

	// Assert
	t.Run("kea_up", func(t *testing.T) {
		if got := sumOf(families, "kea_up"); got != 1 {
			t.Errorf("kea_up = %v, want 1: a control command failed against this Kea", got)
		}
	})
	t.Run("metric_set", func(t *testing.T) {
		assertExportsExactly(t, families, withoutRelationshipMetrics(metricContract))
	})
	t.Run("drop_reasons_sum_to_the_total", func(t *testing.T) {
		total := sumOf(families, "kea_dhcp4_packets_dropped_total")
		reasons := sumOf(families, "kea_dhcp4_packets_dropped_by_reason_total")
		if total == 0 {
			t.Fatal("Kea reports no dropped packets, so this property was not exercised; did the malformed packet reach it?")
		}
		if reasons != total {
			t.Errorf("drop reasons sum to %v but the total is %v", reasons, total)
		}
	})
	t.Run("every_statistic_is_known", func(t *testing.T) {
		if sumOf(families, "kea_up") != 1 {
			t.Fatal("a control command failed, so Kea's statistics may not have been read; this property was not exercised")
		}
		unhandled := loggedUnhandled(logs.String())
		var unknown []string
		for _, key := range unhandled {
			if !slices.Contains(unmappedStatistics, unmappedForm(key)) {
				unknown = append(unknown, key)
			}
		}
		slices.Sort(unknown)
		if len(unknown) > 0 {
			t.Errorf("statistics the exporter cannot export and does not list in unmappedStatistics: %v", unknown)
		}
		// The check above reads the log, so it would pass with nothing logged.
		// The gauge counts the same statistics; agreeing with it shows the log
		// missed none.
		if got := sumOf(families, "kea_exporter_unhandled_statistics"); got != float64(len(unhandled)) {
			t.Errorf("kea_exporter_unhandled_statistics = %v, but %d statistics were logged as unhandled", got, len(unhandled))
		}
	})
}
