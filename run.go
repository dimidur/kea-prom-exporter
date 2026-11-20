package main

// The life of the process, from its command line to its exit code, and the
// exporter it assembles to serve.
//
// One reason to change: the order the process starts up in, what it assembles,
// or the exit code an outcome maps to.

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"syscall"

	"github.com/prometheus/client_golang/prometheus"
)

// Exit codes. exitUsage is what the flag package uses for a command line it
// cannot parse.
const (
	exitOK      = 0
	exitFailure = 1
	exitUsage   = 2
)

// shutdownSignals are the signals that start a graceful shutdown.
var shutdownSignals = []os.Signal{os.Interrupt, syscall.SIGTERM}

// run is the exporter, from the command line to its exit code. name is the
// program name the usage message starts with, and args the arguments after it.
func run(name string, args []string, proc process) int {
	flags, opts := newFlagSet(name, proc.stderr)
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return exitOK
		}
		return exitUsage
	}
	// Anything after the flags is a mistake, and the flag package stops
	// parsing at the first one, silently dropping every flag after it.
	if flags.NArg() > 0 {
		fmt.Fprintf(proc.stderr, "unexpected argument %q\n", flags.Arg(0))
		flags.Usage()
		return exitUsage
	}

	build := proc.build.resolve()
	if opts.showVersion {
		fmt.Fprintln(proc.stdout, build.versionLine())
		return exitOK
	}
	envProblems := applyEnv(flags, proc.lookupEnv)

	logger, logProblems := newLogger(proc.stderr, opts.logLevel, opts.logFormat)
	proc.adoptLogger(logger)
	// Deferred until the logger exists, and every problem is reported rather
	// than only the last one.
	for _, err := range append(envProblems, logProblems...) {
		logger.Warn("configuration problem", "err", err)
	}
	return runExporter(opts, proc, logger, build)
}

// runExporter serves the exporter that opts describe until a shutdown signal,
// and returns the exit code.
func runExporter(opts *options, proc process, logger *slog.Logger, build buildID) int {
	password, err := loadPassword(proc.readFile, opts.keaPasswordFile, opts.keaPassword)
	if err != nil {
		logger.Error("could not load the Kea password", "err", err)
		return exitFailure
	}
	settings := opts.keaSettings(password)
	if err := validateConfig(settings); err != nil {
		logger.Error("invalid configuration", "err", err)
		return exitFailure
	}

	// Signals are taken before the port is bound, so a SIGTERM that arrives
	// once anything can connect starts a graceful shutdown.
	ctx, stop := proc.notifyContext(context.Background(), shutdownSignals...)
	defer stop()
	listener, err := proc.listen("tcp", opts.listen)
	if err != nil {
		logger.Error("could not listen", "addr", opts.listen, "err", err)
		return exitFailure
	}
	logger.Info("listening", "addr", listener.Addr().String())
	report := selfReport{log: logger, build: build, now: proc.now}
	metricsService := newService(settings, listener, proc.keaTransport, report, proc.runtimeCollectors)

	if err := metricsService.serveUntilSignal(ctx, stop); err != nil {
		logger.Error("http server failed", "err", err)
		return exitFailure
	}
	return exitOK
}

// newService assembles the exporter that settings describe, serving on
// listener, reaching Kea through transport, reporting on itself through
// report, and exporting runtimeCollectors alongside.
func newService(settings keaSettings, listener net.Listener, transport http.RoundTripper, report selfReport, runtimeCollectors []prometheus.Collector) service {
	registry := newRegistry(newKeaClient(settings, transport), report, runtimeCollectors)
	return serviceFor(registry, listener, report.log, maxScrapeDuration(settings.timeout))
}

// newRegistry returns the registry that /metrics serves: the Kea collector,
// reporting on itself through report, and runtimeCollectors.
func newRegistry(kea keaCaller, report selfReport, runtimeCollectors []prometheus.Collector) *prometheus.Registry {
	reg := prometheus.NewRegistry()
	reg.MustRegister(newCollector(kea, report))
	reg.MustRegister(runtimeCollectors...)
	return reg
}
