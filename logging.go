package main

// How the process reports, and how it reports its way out.
//
// One reason to change: logging policy -- levels, format, or what a fatal
// condition looks like on the way out.

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
)

// logSink is a seam, not indirection for its own sake. Extracting newLoggerTo
// made the level and format logic testable but left newLogger as the only
// production path and invisible to a test: replacing its body with io.Discard
// kept the whole suite green, so nothing pinned that logs reach stderr at all.
var logSink io.Writer = os.Stderr

// newLogger builds the process logger. An unrecognised level or format falls
// back to the default rather than refusing to start -- a logging preference is
// not worth failing an exporter over -- but it is reported, because
// `--log-level=warning` (the syslog/Python spelling, which slog rejects)
// otherwise silently gives you info.
func newLogger(level, format string) (*slog.Logger, []error) {
	return newLoggerTo(logSink, level, format)
}

// newLoggerTo takes the sink so a test can read what was actually written.
// Without it the json branch was unobservable: both handlers report the same
// level, so deleting the json return changed nothing any assertion could see.
func newLoggerTo(w io.Writer, level, format string) (*slog.Logger, []error) {
	var problems []error

	var lv slog.Level
	if err := lv.UnmarshalText([]byte(level)); err != nil {
		problems = append(problems, fmt.Errorf("unrecognised --log-level %q, using info; valid values are debug, info, warn, error", level))
		lv = slog.LevelInfo
	}

	opts := &slog.HandlerOptions{Level: lv}
	switch {
	case strings.EqualFold(format, "json"):
		return slog.New(slog.NewJSONHandler(w, opts)), problems
	case strings.EqualFold(format, "text"):
	default:
		problems = append(problems, fmt.Errorf("unrecognised --log-format %q, using text; valid values are text and json", format))
	}
	return slog.New(slog.NewTextHandler(w, opts)), problems
}

// fatal logs at error level and exits non-zero. slog has no Fatal, and
// log.Fatal would write outside the configured handler.
func fatal(msg string, args ...any) {
	slog.Error(msg, args...)
	os.Exit(1)
}
