package main

// How the process reports.
//
// One reason to change: logging policy, meaning levels and format.

import (
	"fmt"
	"io"
	"log/slog"
	"strings"
)

// logFormat is how log records are written.
type logFormat int

const (
	logFormatText logFormat = iota
	logFormatJSON
)

// parseLogFormat reads a --log-format value, in any case.
func parseLogFormat(format string) (logFormat, error) {
	if strings.EqualFold(format, "json") {
		return logFormatJSON, nil
	}
	if strings.EqualFold(format, "text") {
		return logFormatText, nil
	}
	return logFormatText, fmt.Errorf("unrecognised --log-format %q, using text; valid values are text and json", format)
}

// newLogger builds the process logger, writing to output. An unrecognised
// level or format falls back to the default rather than refusing to start,
// since a logging preference is not worth failing an exporter over, but it is
// reported, because `--log-level=warning` (the syslog/Python spelling, which
// slog rejects) otherwise silently gives you info.
func newLogger(output io.Writer, level, format string) (*slog.Logger, []error) {
	var problems []error

	var parsedLevel slog.Level
	if err := parsedLevel.UnmarshalText([]byte(level)); err != nil {
		problems = append(problems, fmt.Errorf("unrecognised --log-level %q, using info; valid values are debug, info, warn, error", level))
		parsedLevel = slog.LevelInfo
	}
	parsedFormat, err := parseLogFormat(format)
	if err != nil {
		problems = append(problems, err)
	}

	opts := &slog.HandlerOptions{Level: parsedLevel}
	if parsedFormat == logFormatJSON {
		return slog.New(slog.NewJSONHandler(output, opts)), problems
	}
	return slog.New(slog.NewTextHandler(output, opts)), problems
}
