package main

// Tests for logging.go: how the process reports.

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
)

func TestLogFormatSelectsTheHandler(t *testing.T) {
	// Both handlers report the same level, so a test that only checks levels
	// cannot tell them apart: json falling through to text would pass it.
	// Reading what was written is the only thing that distinguishes them.
	cases := []struct {
		name         string
		format       string
		wantJSON     bool
		wantProblems int
	}{
		{"json emits json", "json", true, 0},
		{"JSON is case-insensitive", "JSON", true, 0},
		{"text emits logfmt", "text", false, 0},
		{"TEXT is case-insensitive", "TEXT", false, 0},
		{"Json in mixed case is json", "Json", true, 0},
		{"Text in mixed case is text", "Text", false, 0},
		{"an unrecognised format falls back to text", "yaml", false, 1},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			// Arrange
			var output bytes.Buffer

			// Act
			logger, problems := newLogger(&output, "info", testCase.format)
			logger.Info("hello", "key", "value")

			// Assert
			// An unrecognised format must be reported, not silently downgraded:
			// that reporting is the whole reason newLogger returns problems.
			if len(problems) != testCase.wantProblems {
				t.Errorf("problems = %v, want %d", problems, testCase.wantProblems)
			}

			line := strings.TrimSpace(output.String())
			if line == "" {
				t.Fatal("nothing was written")
			}
			var parsed map[string]any
			isJSON := json.Unmarshal([]byte(line), &parsed) == nil
			if isJSON != testCase.wantJSON {
				t.Errorf("format %q produced JSON=%v, want %v; line was:\n  %s",
					testCase.format, isJSON, testCase.wantJSON, line)
			}
			if testCase.wantJSON && parsed["key"] != "value" {
				t.Errorf("json output lost its attributes: %s", line)
			}
			if !testCase.wantJSON && !strings.Contains(line, "key=value") {
				t.Errorf("text output is not logfmt: %s", line)
			}
		})
	}
}

func TestLogLevelThreshold(t *testing.T) {
	// The level is the lowest one logged, in either format. slog rejects
	// "warning", the syslog and Python spelling and a very likely typo.
	// Falling back to info is fine; doing it silently is not, because whoever
	// set it never learns the setting was ignored.
	formats := []string{"text", "json"}
	cases := []struct {
		level        string
		wantLowest   slog.Level
		wantProblems int
	}{
		{"debug", slog.LevelDebug, 0},
		{"DEBUG", slog.LevelDebug, 0},
		{"info", slog.LevelInfo, 0},
		{"warn", slog.LevelWarn, 0},
		{"error", slog.LevelError, 0},
		{"warning", slog.LevelInfo, 1},
		{"nonsense", slog.LevelInfo, 1},
	}

	for _, format := range formats {
		for _, testCase := range cases {
			t.Run(format+" "+testCase.level, func(t *testing.T) {
				// Arrange
				var output bytes.Buffer

				// Act
				logger, problems := newLogger(&output, testCase.level, format)

				// Assert
				if !logger.Enabled(t.Context(), testCase.wantLowest) || logger.Enabled(t.Context(), testCase.wantLowest-1) {
					t.Errorf("lowest level logged is not %v", testCase.wantLowest)
				}
				if len(problems) != testCase.wantProblems {
					t.Errorf("problems = %v, want %d", problems, testCase.wantProblems)
				}
			})
		}
	}
}
