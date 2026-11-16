package main

import (
	"log/slog"
	"strings"
	"testing"
)

// Tests for main.go: the registry the process serves.

func TestNewExporterServesKeaAndRuntimeMetrics(t *testing.T) {
	// The registry is everything /metrics shows. A collector left out of it
	// is a whole family of metrics that silently stops existing.

	// Arrange
	kea := healthyKea(t)

	// Act
	families, err := newExporter(kea, quietLogger).Gather()

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

func TestNewExporterLogsScrapeFailuresToTheGivenLogger(t *testing.T) {
	// The logger carries --log-level and --log-format. A failed scrape logged
	// anywhere else ignores both, or is never seen at all, and logged below
	// error it vanishes under --log-level=error.

	// Arrange
	kea := newFakeKea(nil, nil)
	logger, logs := capturingLogger(slog.LevelError)

	// Act
	if _, err := newExporter(kea, logger).Gather(); err != nil {
		t.Fatalf("gather: %v", err)
	}

	// Assert
	for _, command := range scrapeCommands {
		line := `level=ERROR msg="control command failed" command=` + command
		if !strings.Contains(logs.String(), line) {
			t.Errorf("log = %q, want %s failing at error on the given logger", logs.String(), command)
		}
	}
}
