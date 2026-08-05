package main

// Tests for logging.go: how the process reports, and how it reports its way out.

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
)

func TestLogFormatSelectsTheHandler(t *testing.T) {
	// Both handlers report the same level, so a test that only checks levels
	// cannot tell them apart -- deleting the json branch's return, making json
	// fall through to text, survived the whole suite. Reading what was written
	// is the only thing that distinguishes them.

	// Arrange
	cases := []struct {
		name         string
		format       string
		wantJSON     bool
		wantProblems int
	}{
		{"json emits json", "json", true, 0},
		{"JSON is case-insensitive", "JSON", true, 0},
		{"text emits logfmt", "text", false, 0},
		{"an unrecognised format falls back to text", "yaml", false, 1},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Arrange
			var buf bytes.Buffer
			logger, problems := newLoggerTo(&buf, "info", tc.format)

			// Act
			logger.Info("hello", "key", "value")

			// Assert
			// An unrecognised format must be reported, not silently downgraded:
			// that reporting is the whole reason newLogger returns problems.
			if len(problems) != tc.wantProblems {
				t.Errorf("problems = %v, want %d", problems, tc.wantProblems)
			}

			line := strings.TrimSpace(buf.String())
			if line == "" {
				t.Fatal("nothing was written")
			}
			var parsed map[string]any
			isJSON := json.Unmarshal([]byte(line), &parsed) == nil
			if isJSON != tc.wantJSON {
				t.Errorf("format %q produced JSON=%v, want %v; line was:\n  %s",
					tc.format, isJSON, tc.wantJSON, line)
			}
			if tc.wantJSON && parsed["key"] != "value" {
				t.Errorf("json output lost its attributes: %s", line)
			}
			if !tc.wantJSON && !strings.Contains(line, "key=value") {
				t.Errorf("text output is not logfmt: %s", line)
			}
		})
	}
}

func TestLogLevelThreshold(t *testing.T) {
	// slog rejects "warning" -- the syslog and Python spelling, and a very
	// likely operator typo. Falling back to info is fine; doing it silently is
	// not, because the operator never learns the setting was ignored.

	// Arrange
	cases := []struct {
		level        string
		wantDebug    bool
		wantProblems int
	}{
		{"debug", true, 0},
		{"DEBUG", true, 0},
		{"info", false, 0},
		{"warning", false, 1},
		{"nonsense", false, 1},
	}

	for _, tc := range cases {
		t.Run(tc.level, func(t *testing.T) {
			// Arrange
			var buf bytes.Buffer

			// Act
			logger, problems := newLoggerTo(&buf, tc.level, "text")

			// Assert
			if got := logger.Enabled(t.Context(), slog.LevelDebug); got != tc.wantDebug {
				t.Errorf("debug enabled = %v, want %v", got, tc.wantDebug)
			}
			if len(problems) != tc.wantProblems {
				t.Errorf("problems = %v, want %d", problems, tc.wantProblems)
			}
		})
	}
}
