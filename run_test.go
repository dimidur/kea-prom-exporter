package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"runtime/debug"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// Tests for run.go: the process from command line to exit code.

// testRun is a process for run with nothing real behind it, and what it
// records. It listens on listener, and the shutdown signal arrives when the
// test calls deliverSignal.
type testRun struct {
	proc           process
	stdout, stderr bytes.Buffer
	listener       *fakeListener
	deliverSignal  context.CancelFunc

	listened []string
	signals  []os.Signal
	adopted  []*slog.Logger
	// releases counts the calls that released signal handling, and
	// logAtRelease is what stderr held at the first.
	releases     int
	logAtRelease string
}

// newTestRun returns a testRun whose process reads files and env from the
// given maps.
func newTestRun(files map[string]string, env map[string]string) *testRun {
	test := &testRun{listener: newFakeListener()}
	signalled, deliver := context.WithCancel(context.Background())
	test.deliverSignal = deliver
	test.proc = process{
		stdout:    &test.stdout,
		stderr:    &test.stderr,
		lookupEnv: envOf(env),
		readFile:  filesOf(files),
		listen: func(network, address string) (net.Listener, error) {
			test.listened = append(test.listened, network+" "+address)
			return test.listener, nil
		},
		notifyContext: func(_ context.Context, signals ...os.Signal) (context.Context, context.CancelFunc) {
			test.signals = signals
			return signalled, test.releaseSignals
		},
		keaTransport:      stubTransport{err: errNoKea},
		adoptLogger:       func(logger *slog.Logger) { test.adopted = append(test.adopted, logger) },
		build:             buildSource{readInfo: func() (*debug.BuildInfo, bool) { return nil, false }},
		now:               stoppedClock,
		runtimeCollectors: []prometheus.Collector{runtimeProbe()},
	}
	return test
}

// releaseSignals is the process's release of signal handling: it records the
// call and, the first time, what had been logged by then.
func (test *testRun) releaseSignals() {
	if test.releases == 0 {
		test.logAtRelease = test.stderr.String()
	}
	test.releases++
}

// assertExited fails the test unless the process exited with want, showing
// what it logged.
func (test *testRun) assertExited(t *testing.T, code, want int) {
	t.Helper()
	if code != want {
		t.Errorf("exit code = %d, want %d; stderr = %q", code, want, test.stderr.String())
	}
}

// startRun runs the process in the background as name with args, and returns
// where its exit code arrives. The shutdown signal is delivered at cleanup at
// the latest, so a run that serves when it should not fails its test rather
// than hanging the suite.
func startRun(t *testing.T, test *testRun, name string, args ...string) <-chan int {
	t.Helper()
	exited := make(chan int, 1)
	go func() { exited <- run(name, args, test.proc) }()
	t.Cleanup(test.deliverSignal)
	return exited
}

// runToExit runs the process as kea-prom-exporter with args and returns its
// exit code, failing the test if it does not exit.
func runToExit(t *testing.T, test *testRun, args ...string) int {
	t.Helper()
	return receive(t, startRun(t, test, "kea-prom-exporter", args...), "run to exit")
}

// logRecord is the part of a JSON log record the tests read.
type logRecord struct{ Level, Msg, Err string }

// jsonLogRecords decodes each line of stderr as a JSON log record, failing
// the test on a line that is not one.
func jsonLogRecords(t *testing.T, stderr string) []logRecord {
	t.Helper()
	var records []logRecord
	for _, line := range strings.Split(strings.TrimSpace(stderr), "\n") {
		var record logRecord
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatalf("log line %q is not JSON: %v", line, err)
		}
		records = append(records, record)
	}
	return records
}

// scrapeMetrics sends GET /metrics over conn and returns the body of a 200.
func scrapeMetrics(t *testing.T, conn net.Conn) string {
	t.Helper()
	if err := conn.SetDeadline(time.Now().Add(testWait)); err != nil {
		t.Fatalf("set deadline: %v", err)
	}
	req, err := http.NewRequest(http.MethodGet, "http://exporter/metrics", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Close = true
	if err := req.Write(conn); err != nil {
		t.Fatalf("write request: %v", err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(conn), req)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/metrics = %d %q, want 200", resp.StatusCode, body)
	}
	return string(body)
}

func TestExitCodesAreTheConventionalOnes(t *testing.T) {
	// Supervisors read only the exit code: Docker and systemd restart on a
	// non-zero one, and 2 is what Go's flag package and shell tools use for a
	// command line they cannot make sense of.

	// Arrange
	codes := []struct {
		name      string
		got, want int
	}{
		{"exitOK", exitOK, 0},
		{"exitFailure", exitFailure, 1},
		{"exitUsage", exitUsage, 2},
	}

	// Act + Assert
	for _, code := range codes {
		if code.got != code.want {
			t.Errorf("%s = %d, want %d", code.name, code.got, code.want)
		}
	}
}

func TestRunServesUntilASignalAndExitsCleanly(t *testing.T) {
	// The ordinary life of the process: listen where --listen says and log
	// the address actually bound, serve, and on SIGINT or SIGTERM release the
	// signals, so a second one kills the process, then shut down within a
	// grace long enough for the slowest scrape --kea-timeout allows, and exit
	// 0.

	// Arrange
	test := newTestRun(nil, nil)
	test.deliverSignal()
	wantLogged := []string{"msg=listening addr=127.0.0.1:9547", `msg="shutting down" grace=7s`}
	wantListened := []string{"tcp :9999"}
	wantSignals := []os.Signal{os.Interrupt, syscall.SIGTERM}

	// Act
	code := runToExit(t, test, "--listen=:9999", "--kea-timeout=3s")

	// Assert
	test.assertExited(t, code, exitOK)
	if !slices.Equal(test.listened, wantListened) {
		t.Errorf("listened on %v, want %v", test.listened, wantListened)
	}
	if !slices.Equal(test.signals, wantSignals) {
		t.Errorf("shutdown signals = %v, want SIGINT and SIGTERM", test.signals)
	}
	if test.releases == 0 || strings.Contains(test.logAtRelease, "shutting down") {
		t.Errorf("signal handling released %d times, first with stderr %q; want it released before shutting down",
			test.releases, test.logAtRelease)
	}
	for _, line := range wantLogged {
		if !strings.Contains(test.stderr.String(), line) {
			t.Errorf("stderr = %q, want %s", test.stderr.String(), line)
		}
	}
	if test.stdout.Len() != 0 {
		t.Errorf("stdout = %q, want nothing; logs belong on stderr", test.stdout.String())
	}
}

func TestRunServesMetricsFromTheConfiguredKea(t *testing.T) {
	// A scrape of /metrics asks the Kea that --kea-url names, through the
	// process's transport, reports the build the binary was stamped with,
	// times itself by the process's clock, and logs a failed command on the
	// process's logger. The process keeps serving until a signal arrives.

	// Arrange: every request to Kea takes one second of the process's clock.
	test := newTestRun(nil, nil)
	test.proc.build.readInfo = sentinelBuildInfo
	clock := &testClock{}
	test.proc.now = clock.now
	test.proc.keaTransport = stubTransport{err: errNoKea, observe: func(*http.Request) { clock.advance(time.Second) }}
	wantMetrics := []string{
		"kea_up 0", `revision="abc123"`, `goversion="go0.0.0-sentinel"`,
		fmt.Sprintf("kea_scrape_duration_seconds %d", len(scrapeCommands)),
		runtimeProbeSample,
	}
	wantLogged := []string{`level=ERROR msg="control command failed"`, "kea.test:8001", errNoKea.Error()}
	exited := startRun(t, test, "kea-prom-exporter", "--kea-url=http://kea.test:8001/")

	// Act
	body := scrapeMetrics(t, test.listener.dial(t))
	// A second scrape fails the test if run stopped serving after the first.
	scrapeMetrics(t, test.listener.dial(t))
	test.deliverSignal()
	code := receive(t, exited, "run to exit")

	// Assert
	test.assertExited(t, code, exitOK)
	for _, want := range wantMetrics {
		if !strings.Contains(body, want) {
			t.Errorf("/metrics = %q, want %s", body, want)
		}
	}
	for _, want := range wantLogged {
		if !strings.Contains(test.stderr.String(), want) {
			t.Errorf("stderr = %q, want %s from the configured Kea through the process's transport", test.stderr.String(), want)
		}
	}
}

func TestRunSendsKeaTheCredentialsItIsGiven(t *testing.T) {
	// With --kea-user, the password authenticates every command sent to Kea,
	// from whichever of the documented sources supplies it: the file, read
	// through the process, the inline flag, or the environment. A credential
	// lost between validation and the client is a 401 on every scrape.
	tests := []struct {
		name  string
		args  []string
		files map[string]string
		env   map[string]string
	}{
		{
			name:  "password file",
			args:  []string{"--kea-password-file=/run/secrets/kea"},
			files: map[string]string{"/run/secrets/kea": "kea-password\n"},
		},
		{
			name: "inline flag",
			args: []string{"--kea-password=kea-password"},
		},
		{
			name: "environment",
			env:  map[string]string{"KEA_PASSWORD": "kea-password"},
		},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			// Arrange
			test := newTestRun(testCase.files, testCase.env)
			var credentials []string
			test.proc.keaTransport = stubTransport{
				err: errNoKea,
				observe: func(req *http.Request) {
					user, password, _ := req.BasicAuth()
					credentials = append(credentials, user+":"+password)
				},
			}
			want := slices.Repeat([]string{"exporter:kea-password"}, len(scrapeCommands))
			exited := startRun(t, test, "kea-prom-exporter", append([]string{"--kea-user=exporter"}, testCase.args...)...)

			// Act
			scrapeMetrics(t, test.listener.dial(t))
			test.deliverSignal()
			code := receive(t, exited, "run to exit")

			// Assert
			test.assertExited(t, code, exitOK)
			if !slices.Equal(credentials, want) {
				t.Errorf("credentials sent = %q, want %q", credentials, want)
			}
		})
	}
}

func TestRunLogsInTheConfiguredFormat(t *testing.T) {
	// JSON logs are how a deployment feeds a collector that parses them, so
	// every line must be JSON, whether the flag or, as in a container, the
	// environment asks for it.
	tests := []struct {
		name string
		args []string
		env  map[string]string
	}{
		{name: "flag", args: []string{"--log-format=json"}},
		{name: "environment", env: map[string]string{"LOG_FORMAT": "json"}},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			// Arrange
			test := newTestRun(nil, testCase.env)
			test.deliverSignal()

			// Act
			code := runToExit(t, test, testCase.args...)

			// Assert
			test.assertExited(t, code, exitOK)
			if records := jsonLogRecords(t, test.stderr.String()); len(records) < 2 {
				t.Errorf("stderr = %q, want the listening and shutdown lines", test.stderr.String())
			}
		})
	}
}

func TestRunMakesTheConfiguredLoggerTheProcessWideOne(t *testing.T) {
	// net/http's client writes some errors to the log package rather than to
	// any logger it is given. Adopting the configured logger process-wide
	// routes those through it too, so they keep the configured format and
	// level instead of breaking a JSON log stream with a plain-text line.

	// Arrange
	test := newTestRun(nil, nil)
	test.deliverSignal()
	const probe = "adopted logger probe"

	// Act
	code := runToExit(t, test, "--log-format=json", "--log-level=warn")

	// Assert
	test.assertExited(t, code, exitOK)
	if len(test.adopted) != 1 {
		t.Fatalf("adopted %d loggers, want the configured one once", len(test.adopted))
	}
	test.stderr.Reset()
	test.adopted[0].Info("below the configured level")
	test.adopted[0].Warn(probe)
	records := jsonLogRecords(t, test.stderr.String())
	if len(records) != 1 || records[0].Msg != probe {
		t.Errorf("adopted logger wrote %q, want only the warning, as JSON on stderr", test.stderr.String())
	}
}

func TestRunLogsAtTheConfiguredLevel(t *testing.T) {
	// The level filters what the process says, whether the flag or, as in a
	// container, the environment sets it: at error, the info lines about
	// listening and shutting down are not written.
	tests := []struct {
		name string
		args []string
		env  map[string]string
	}{
		{name: "flag", args: []string{"--log-level=error"}},
		{name: "environment", env: map[string]string{"LOG_LEVEL": "error"}},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			// Arrange
			test := newTestRun(nil, testCase.env)
			test.deliverSignal()

			// Act
			code := runToExit(t, test, testCase.args...)

			// Assert
			test.assertExited(t, code, exitOK)
			if test.stderr.Len() != 0 {
				t.Errorf("stderr = %q, want nothing below error at --log-level=error", test.stderr.String())
			}
		})
	}
}

func TestRunAnswersHelpAndBadFlags(t *testing.T) {
	// -h is a request, not an error, so it exits 0; a command line the flag
	// package cannot parse exits 2, as Go programs conventionally do, and so
	// does a stray argument, which would otherwise silently drop every flag
	// after it. Each says what was wrong, then the usage under the name the
	// program was run as.
	tests := []struct {
		name     string
		args     []string
		want     int
		wantSaid string
	}{
		{"help", []string{"-h"}, exitOK, "Usage of"},
		{"unknown flag", []string{"--no-such-flag"}, exitUsage, "flag provided but not defined: -no-such-flag"},
		{"stray argument", []string{"--log-level=debug", "stray", "--kea-user=exporter"}, exitUsage, `unexpected argument "stray"`},
		{"a trailing stray argument", []string{"stray"}, exitUsage, `unexpected argument "stray"`},
		{"--version with a stray argument", []string{"--version", "stray"}, exitUsage, `unexpected argument "stray"`},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			// Arrange
			test := newTestRun(nil, nil)

			// Act
			code := receive(t, startRun(t, test, "/usr/local/bin/kea-prom-exporter", testCase.args...), "run to exit")

			// Assert
			test.assertExited(t, code, testCase.want)
			if !strings.Contains(test.stderr.String(), testCase.wantSaid) {
				t.Errorf("stderr = %q, want it to say %q", test.stderr.String(), testCase.wantSaid)
			}
			if !strings.Contains(test.stderr.String(), "Usage of /usr/local/bin/kea-prom-exporter:") {
				t.Errorf("stderr = %q, want the usage under the program's name", test.stderr.String())
			}
			if test.listened != nil {
				t.Error("the process listened although its command line was not one to serve")
			}
		})
	}
}

func TestRunPrintsTheVersionItWasBuiltWith(t *testing.T) {
	// --version is what a bug report quotes. It goes to stdout, where a script
	// can read it. The version and revision are the ones injected at build
	// time, which the image build relies on, and the Go version comes from
	// the build info Go stamped into the binary.

	// Arrange
	test := newTestRun(nil, nil)
	test.proc.build = buildSource{version: "1.2.3", revision: "def456", readInfo: sentinelBuildInfo}
	const want = "kea-prom-exporter 1.2.3 (revision def456, go0.0.0-sentinel)\n"

	// Act
	code := runToExit(t, test, "--version")

	// Assert
	test.assertExited(t, code, exitOK)
	if test.stdout.String() != want {
		t.Errorf("stdout = %q, want %q", test.stdout.String(), want)
	}
	if test.stderr.Len() != 0 || test.listened != nil {
		t.Errorf("--version logged %q or listened on %v; it must only print", test.stderr.String(), test.listened)
	}
}

func TestRunReportsConfigurationProblemsAndCarriesOn(t *testing.T) {
	// A bad environment value or log setting falls back to its default
	// rather than refusing to start, but each one is reported, on the
	// configured logger, so nobody is left wondering why a setting did nothing.

	// Arrange: JSON, so a warning written by any logger other than the
	// configured one would not decode.
	test := newTestRun(nil, map[string]string{"KEA_TIMEOUT": "twelve", "LOG_FORMAT": "json"})
	test.deliverSignal()
	badSettings := []string{"KEA_TIMEOUT", "--log-level"}

	// Act
	code := runToExit(t, test, "--log-level=warning")

	// Assert
	test.assertExited(t, code, exitOK)
	var problems []string
	for _, record := range jsonLogRecords(t, test.stderr.String()) {
		if record.Level == "WARN" && record.Msg == "configuration problem" {
			problems = append(problems, record.Err)
		}
	}
	if len(problems) != len(badSettings) {
		t.Errorf("configuration problems = %q, want one for each bad setting", problems)
	}
	for _, setting := range badSettings {
		if !slices.ContainsFunc(problems, func(problem string) bool { return strings.Contains(problem, setting) }) {
			t.Errorf("configuration problems = %q, want the problem with %s named", problems, setting)
		}
	}
}

func TestRunRefusesToStartWithoutWhatItNeeds(t *testing.T) {
	// A password that cannot be read, settings the exporter cannot run with,
	// an address it cannot bind and a server that fails each stop the process
	// with exit 1 and say why, the cause included, rather than serving
	// something broken. Nothing claims to be listening when the bind failed.
	// Signal handling is taken before the bind, so a SIGTERM once anything
	// can connect shuts down gracefully, and it is released however the
	// process then ends.
	tests := []struct {
		name        string
		args        []string
		adjust      func(*testRun)
		wantLog     string
		wantCause   string
		served      bool
		tookSignals bool
	}{
		{
			name:      "unreadable password file",
			args:      []string{"--kea-password-file=/run/secrets/absent"},
			wantLog:   "could not load the Kea password",
			wantCause: "/run/secrets/absent",
		},
		{
			name:      "a user without a password",
			args:      []string{"--kea-user=exporter"},
			wantLog:   "invalid configuration",
			wantCause: "--kea-user is set but no password",
		},
		{
			name: "an address already in use",
			args: []string{"--listen=:9999"},
			adjust: func(test *testRun) {
				test.proc.listen = func(string, string) (net.Listener, error) {
					return nil, errors.New("bind: address already in use")
				}
			},
			wantLog:     `could not listen" addr=:9999`,
			wantCause:   "address already in use",
			tookSignals: true,
		},
		{
			name: "a server that fails",
			adjust: func(test *testRun) {
				test.listener.acceptErr = errAcceptFailed
			},
			wantLog:     "http server failed",
			wantCause:   errAcceptFailed.Error(),
			served:      true,
			tookSignals: true,
		},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			// Arrange
			test := newTestRun(nil, nil)
			if testCase.adjust != nil {
				testCase.adjust(test)
			}

			// Act
			code := runToExit(t, test, testCase.args...)

			// Assert
			test.assertExited(t, code, exitFailure)
			if !strings.Contains(test.stderr.String(), "level=ERROR msg=\""+testCase.wantLog) {
				t.Errorf("stderr = %q, want an error saying %q", test.stderr.String(), testCase.wantLog)
			}
			if !strings.Contains(test.stderr.String(), testCase.wantCause) {
				t.Errorf("stderr = %q, want the cause %q", test.stderr.String(), testCase.wantCause)
			}
			if listening := strings.Contains(test.stderr.String(), "msg=listening"); listening != testCase.served {
				t.Errorf("listening line logged = %v, want %v; stderr = %q", listening, testCase.served, test.stderr.String())
			}
			if took := test.signals != nil; took != testCase.tookSignals {
				t.Errorf("signal handling taken = %v, want %v", took, testCase.tookSignals)
			}
			if released := test.releases > 0; released != testCase.tookSignals {
				t.Errorf("signal handling released = %v, want %v", released, testCase.tookSignals)
			}
		})
	}
}

func TestNewServiceServesOnTheListenerAndLogsToTheGivenLogger(t *testing.T) {
	// The service serves on the listener it is given, and the server's own
	// errors (failed accepts, handler panics) and the shutdown messages must
	// reach the logger that carries --log-level and --log-format; anywhere
	// else they ignore both. Its slowest scrape is two commands of
	// --kea-timeout=3s, so a shutdown waits that plus a second. The runtime
	// collectors it is given are on /metrics.

	// Arrange
	logger, logs := capturingLogger(slog.LevelError)
	listener := newFakeListener()
	settings := keaSettings{service: supportedService, timeout: 3 * time.Second}
	const wantGrace = 7 * time.Second

	// Act
	metricsService := newService(settings, listener, stubTransport{err: errNoKea}, testReport(logger), []prometheus.Collector{runtimeProbe()})

	// Assert
	served, ok := metricsService.server.(listeningServer)
	if !ok || served.listener != listener {
		t.Fatalf("the service serves %#v, want an http.Server on the given listener", metricsService.server)
	}
	if metricsService.log != logger {
		t.Error("the service does not log to the given logger")
	}
	if metricsService.grace != wantGrace {
		t.Errorf("grace = %v, want %v", metricsService.grace, wantGrace)
	}
	if metrics := serve(served.Handler, "/metrics").Body.String(); !strings.Contains(metrics, runtimeProbeSample) {
		t.Errorf("/metrics = %q, want the runtime collectors' %s", metrics, runtimeProbeSample)
	}
	served.ErrorLog.Print(serverErrorProbe)
	assertServerErrorLogged(t, logs)
}

func TestNewRegistryRegistersKeaAndRuntimeCollectors(t *testing.T) {
	// The registry is everything /metrics shows. A collector left out of it
	// is a whole family of metrics that silently stops existing. Registering
	// a second copy of each is refused exactly when the first is there, and
	// only describes the collectors, so nothing is collected.

	// Arrange
	copies := []struct {
		name      string
		collector prometheus.Collector
	}{
		{"kea", newCollector(newFakeKea(nil, nil), testReport(quietLogger))},
		{"runtime", runtimeProbe()},
	}

	// Act
	reg := newRegistry(newFakeKea(nil, nil), testReport(quietLogger), []prometheus.Collector{runtimeProbe()})

	// Assert
	for _, second := range copies {
		assertAlreadyRegistered(t, reg, second.name, second.collector)
	}
}

func TestNewRegistryLogsScrapeFailuresToTheGivenLogger(t *testing.T) {
	// The logger carries --log-level and --log-format. A failed scrape logged
	// anywhere else ignores both, or is never seen at all, and logged below
	// error it vanishes under --log-level=error. Each command's line carries
	// that command's own error, which is the only place the cause appears.

	// Arrange
	kea := newFakeKea(nil, nil)
	logger, logs := capturingLogger(slog.LevelError)
	var wantLines []string
	for _, command := range scrapeCommands {
		wantLines = append(wantLines, fmt.Sprintf(`level=ERROR msg="control command failed" command=%s err=%q`,
			command, noReply(command).Error()))
	}

	// Act
	if _, err := newRegistry(kea, testReport(logger), nil).Gather(); err != nil {
		t.Fatalf("gather: %v", err)
	}

	// Assert
	for _, line := range wantLines {
		if !strings.Contains(logs.String(), line) {
			t.Errorf("log = %q, want %s on the given logger", logs.String(), line)
		}
	}
}
