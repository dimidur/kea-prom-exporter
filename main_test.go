package main

import (
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"reflect"
	"runtime/debug"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
)

// Tests for main.go: what the process takes from outside.

func TestOSProcessIsTheRealOne(t *testing.T) {
	// osProcess is the only place run meets the operating system and the
	// build, so each of its parts must be the real thing: logs on stdout would
	// interleave with --version, and a stand-in anywhere here would never be
	// noticed.

	// Arrange
	pointer := func(function any) uintptr { return reflect.ValueOf(function).Pointer() }
	realFunctions := map[string]any{
		"lookupEnv":      os.LookupEnv,
		"readFile":       os.ReadFile,
		"listen":         net.Listen,
		"notifyContext":  signal.NotifyContext,
		"adoptLogger":    slog.SetDefault,
		"build.readInfo": debug.ReadBuildInfo,
		"now":            time.Now,
	}
	// A second copy of each real runtime collector is refused exactly when
	// the first is registered.
	runtimeCopies := map[string]prometheus.Collector{
		"go runtime": collectors.NewGoCollector(),
		"process":    collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
	}
	// What -X main.version and -X main.revision inject at build time. The
	// test binary is built without them, so the test sets them as the linker
	// would; no test runs in parallel with it.
	builtVersion, builtRevision := version, revision
	version, revision = "1.2.3", "def456"
	t.Cleanup(func() { version, revision = builtVersion, builtRevision })

	// Act
	proc := osProcess()

	// Assert
	if proc.stdout != os.Stdout || proc.stderr != os.Stderr {
		t.Error("stdout and stderr must be the process's own, in that order")
	}
	if proc.keaTransport != http.DefaultTransport {
		t.Errorf("keaTransport = %v, want http.DefaultTransport", proc.keaTransport)
	}
	if proc.build.version != "1.2.3" || proc.build.revision != "def456" {
		t.Errorf("build version, revision = %q, %q, want the injected 1.2.3, def456", proc.build.version, proc.build.revision)
	}
	got := map[string]any{
		"lookupEnv":      proc.lookupEnv,
		"readFile":       proc.readFile,
		"listen":         proc.listen,
		"notifyContext":  proc.notifyContext,
		"adoptLogger":    proc.adoptLogger,
		"build.readInfo": proc.build.readInfo,
		"now":            proc.now,
	}
	for name, want := range realFunctions {
		if pointer(got[name]) != pointer(want) {
			t.Errorf("%s is not the real one", name)
		}
	}
	if len(proc.runtimeCollectors) != len(runtimeCopies) {
		t.Errorf("%d runtime collectors, want the %d real ones", len(proc.runtimeCollectors), len(runtimeCopies))
	}
	registered := prometheus.NewRegistry()
	for _, collector := range proc.runtimeCollectors {
		if err := registered.Register(collector); err != nil {
			t.Errorf("register runtime collector: %v", err)
		}
	}
	for name, second := range runtimeCopies {
		assertAlreadyRegistered(t, registered, name, second)
	}
}
