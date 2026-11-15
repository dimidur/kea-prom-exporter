package main

import (
	"testing"
	"time"
)

// Tests for main.go: the registry the process serves.

func TestNewExporterServesKeaAndRuntimeMetrics(t *testing.T) {
	// The registry is everything /metrics shows. A collector left out of it
	// is a whole family of metrics that silently stops existing.

	// Arrange
	stub := keaStub(t, fixture(t, "statistic-get-all.json"), fixture(t, "status-get.json"))
	t.Cleanup(stub.Close)
	client := newKeaClient(keaClient{url: stub.URL, service: supportedService, timeout: 5 * time.Second})

	// Act
	families, err := newExporter(client).Gather()

	// Assert
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	up := -1.0
	names := map[string]bool{}
	for _, family := range families {
		names[family.GetName()] = true
		if family.GetName() == "kea_up" && len(family.GetMetric()) > 0 {
			up = family.GetMetric()[0].GetGauge().GetValue()
		}
	}
	if up != 1 {
		t.Errorf("kea_up = %v, want 1 against a healthy Kea", up)
	}
	for _, name := range []string{"go_goroutines", "process_start_time_seconds"} {
		if !names[name] {
			t.Errorf("%s is missing; the runtime and process collectors must be registered", name)
		}
	}
}
