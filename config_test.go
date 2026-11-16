package main

import (
	"errors"
	"flag"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"
)

// Tests for config.go: the flag and environment surface.

func TestEmptyEnvValueDoesNotOverrideAFlagDefault(t *testing.T) {
	// An exported-but-empty variable is how a shell wrapper says "unset", and
	// it is common in compose files. Treating it as a value would blank the
	// setting rather than leave the default.

	// Arrange
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	url := fs.String("kea-url", "http://default/", "")
	if err := fs.Parse(nil); err != nil {
		t.Fatalf("parse: %v", err)
	}

	// Act
	problems := applyEnv(fs, func(string) (string, bool) { return "", true })

	// Assert
	if len(problems) != 0 {
		t.Errorf("problems = %v, want none", problems)
	}
	if *url != "http://default/" {
		t.Errorf("kea-url = %q; an empty env value blanked the default", *url)
	}
}

func TestEnvIsConsultedInAStableOrder(t *testing.T) {
	// applyEnv sorts the flag names before walking them, because ranging a map
	// is randomised and the diagnostics would otherwise reorder between runs.
	//
	// The order is read through the injected lookup, which is called once per
	// flag in exactly the order the sort establishes.

	// Arrange
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	for name := range envForFlag {
		fs.String(name, "", "")
	}
	if err := fs.Parse(nil); err != nil {
		t.Fatalf("parse: %v", err)
	}
	var queried []string

	// Act
	applyEnv(fs, func(env string) (string, bool) {
		queried = append(queried, env)
		return "", false
	})

	// Assert
	// Env var names in the order their FLAG names sort, which is neither the
	// map literal's order nor the env names' own order.
	names := make([]string, 0, len(envForFlag))
	for name := range envForFlag {
		names = append(names, name)
	}
	slices.Sort(names)
	want := make([]string, 0, len(names))
	for _, name := range names {
		want = append(want, envForFlag[name])
	}
	if !slices.Equal(queried, want) {
		t.Errorf("env consulted in order\n got %v\nwant %v", queried, want)
	}
}

func TestApplyEnvFillsUnsetFlagsAndReportsBadValues(t *testing.T) {
	// The environment fills a flag the command line left unset, an explicit
	// flag beats it, and a bad value is reported rather than swallowed.

	// newFS builds a local FlagSet with every flag envForFlag maps, so a
	// subtest can parse arguments without disturbing flag.CommandLine. Only
	// --kea-url and --kea-timeout are read back, and --kea-timeout keeps its
	// real type, since it is the only flag whose Set can fail.
	newFS := func() (*flag.FlagSet, *string, *time.Duration) {
		fs := flag.NewFlagSet("test", flag.ContinueOnError)
		var url *string
		var timeout *time.Duration
		for name := range envForFlag {
			switch name {
			case "kea-url":
				url = fs.String(name, "http://default/", "")
			case "kea-timeout":
				timeout = fs.Duration(name, 5*time.Second, "")
			default:
				fs.String(name, "", "")
			}
		}
		return fs, url, timeout
	}
	env := func(vars map[string]string) func(string) (string, bool) {
		return func(name string) (string, bool) {
			value, ok := vars[name]
			return value, ok
		}
	}

	t.Run("env fills an unset flag", func(t *testing.T) {
		// Arrange
		fs, url, timeout := newFS()
		if err := fs.Parse(nil); err != nil {
			t.Fatalf("parse: %v", err)
		}

		// Act
		problems := applyEnv(fs, env(map[string]string{"KEA_URL": "http://from-env/", "KEA_TIMEOUT": "12s"}))

		// Assert
		if len(problems) != 0 {
			t.Errorf("problems = %v, want none", problems)
		}
		if *url != "http://from-env/" {
			t.Errorf("kea-url = %q, want the env value", *url)
		}
		if *timeout != 12*time.Second {
			t.Errorf("kea-timeout = %v, want 12s", *timeout)
		}
	})

	t.Run("an explicit flag beats the environment", func(t *testing.T) {
		// Arrange
		fs, url, _ := newFS()
		if err := fs.Parse([]string{"-kea-url", "http://from-flag/"}); err != nil {
			t.Fatalf("parse: %v", err)
		}

		// Act
		applyEnv(fs, env(map[string]string{"KEA_URL": "http://from-env/"}))

		// Assert
		if *url != "http://from-flag/" {
			t.Errorf("kea-url = %q, want the flag value", *url)
		}
	})

	t.Run("a rejected value is reported and the default kept", func(t *testing.T) {
		// Arrange
		fs, _, timeout := newFS()
		if err := fs.Parse(nil); err != nil {
			t.Fatalf("parse: %v", err)
		}

		// Act
		problems := applyEnv(fs, env(map[string]string{"KEA_TIMEOUT": "twelve", "LOG_LEVEL": ""}))

		// Assert
		if len(problems) != 1 {
			t.Fatalf("problems = %v, want exactly 1", problems)
		}
		if !strings.Contains(problems[0].Error(), "KEA_TIMEOUT") {
			t.Errorf("problem does not name the variable: %v", problems[0])
		}
		if *timeout != 5*time.Second {
			t.Errorf("kea-timeout = %v, want the default kept after a bad value", *timeout)
		}
	})
}

// rejectingValue is a flag.Value that refuses every value, including the one
// it reports holding.
type rejectingValue struct{}

func (rejectingValue) String() string   { return "held" }
func (rejectingValue) Set(string) error { return errors.New("rejected") }

func TestAFlagThatCannotBeRestoredIsReportedToo(t *testing.T) {
	// A rejected value is answered by restoring the flag to what it held. If
	// that restore fails too, the flag's value is no longer what the first
	// problem says it kept, and that must be reported rather than lost.

	// Arrange
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	fs.Var(rejectingValue{}, "kea-url", "")
	if err := fs.Parse(nil); err != nil {
		t.Fatalf("parse: %v", err)
	}

	// Act
	problems := applyEnv(fs, func(env string) (string, bool) { return "http://from-env/", env == "KEA_URL" })

	// Assert
	if len(problems) != 2 {
		t.Fatalf("problems = %v, want the rejected value and the failed restore", problems)
	}
	if !strings.Contains(problems[1].Error(), "could not restore --kea-url to held") {
		t.Errorf("second problem = %v, want it to name the flag and the value it could not restore", problems[1])
	}
}

func TestEveryFlagHasAnEnvEquivalent(t *testing.T) {
	// The container documentation tells people to configure the exporter
	// through the environment, so a flag with no envForFlag row is a setting
	// a container cannot reach.

	// Arrange
	// flag.CommandLine, not a hand-written FlagSet: walking a copy would
	// assert a property of the test's own fixture, and a flag added to
	// config.go with no envForFlag row would leave it green, which is the one
	// thing it exists to catch.
	//
	// --version is deliberately absent from envForFlag (config.go says so):
	// it exits immediately and configures nothing. That exemption is asserted
	// rather than skipped, so silently dropping another flag into it is not
	// possible.
	exempt := map[string]bool{"version": true}

	// Act
	var registered []string
	flag.CommandLine.VisitAll(func(defined *flag.Flag) {
		// The test binary injects its own -test.* flags.
		if strings.HasPrefix(defined.Name, "test.") {
			return
		}
		registered = append(registered, defined.Name)
	})

	// Assert
	if len(registered) < 5 {
		t.Fatalf("found only %d registered flags (%v); this guard would be vacuous", len(registered), registered)
	}
	for _, name := range registered {
		_, mapped := envForFlag[name]
		switch {
		case exempt[name] && mapped:
			t.Errorf("--%s is documented as having no env form but appears in envForFlag", name)
		case !exempt[name] && !mapped:
			t.Errorf("flag --%s has no entry in envForFlag", name)
		}
	}
	for name := range envForFlag {
		if !slices.Contains(registered, name) {
			t.Errorf("envForFlag maps %q, which is not a flag", name)
		}
	}
}

func TestKeaTimeoutDefaultIsTheDocumentedFiveSeconds(t *testing.T) {
	// README and --help both state 5s, and --help prints the flag's DefValue.
	// The fallback for a zero or negative timeout is the same constant, so
	// this also fails if the two stop agreeing.

	// Arrange
	const documented = "5s"

	// Act
	defined := flag.CommandLine.Lookup("kea-timeout")

	// Assert
	if defined == nil {
		t.Fatal("--kea-timeout is not registered")
	}
	if defined.DefValue != documented {
		t.Errorf("--kea-timeout default = %s, want the documented %s", defined.DefValue, documented)
	}
	if defined.DefValue != defaultKeaTimeout.String() {
		t.Errorf("--kea-timeout default = %s, but a zero timeout falls back to %v", defined.DefValue, defaultKeaTimeout)
	}
}

func TestKeaServiceDefaultPassesValidation(t *testing.T) {
	// Started with no flags, the exporter must run: a --kea-service default
	// that validateConfig refuses would stop every deployment that does not
	// set it.

	// Arrange
	defined := flag.CommandLine.Lookup("kea-service")
	if defined == nil {
		t.Fatal("--kea-service is not registered")
	}

	// Act
	err := validateConfig(keaClient{service: defined.DefValue})

	// Assert
	if err != nil {
		t.Errorf("the default --kea-service=%s is refused: %v", defined.DefValue, err)
	}
}

func TestFlagHelpStatesWhatTheCodeUses(t *testing.T) {
	// --help is where a deployment learns how many commands share the scrape
	// budget and which service is implemented, so the help must follow the
	// code.
	tests := []struct {
		flag string
		want string
	}{
		{"kea-timeout", fmt.Sprintf("budget %d of these", len(scrapeCommands))},
		{"kea-service", "Only " + supportedService + " is implemented"},
	}
	for _, tc := range tests {
		t.Run(tc.flag, func(t *testing.T) {
			// Arrange
			defined := flag.CommandLine.Lookup(tc.flag)
			if defined == nil {
				t.Fatalf("--%s is not registered", tc.flag)
			}

			// Act
			usage := defined.Usage

			// Assert
			if !strings.Contains(usage, tc.want) {
				t.Errorf("--%s help = %q, want it to say %q", tc.flag, usage, tc.want)
			}
		})
	}
}

func TestValidateConfigRefusesSettingsTheExporterCannotRunWith(t *testing.T) {
	// Settings the exporter cannot run with must stop the process at start,
	// where the error names the flag, rather than surface later as failed or
	// empty scrapes. The rows name the settings validateConfig refuses.
	tests := []struct {
		name     string
		user     string
		password string
		service  string
		wantFlag string // empty when the settings are accepted
	}{
		{"no auth", "", "", supportedService, ""},
		{"user and password", "exporter", "kea-password", supportedService, ""},
		{"user without a password", "exporter", "", supportedService, "--kea-user"},
		{"dhcp6 service", "", "", "dhcp6", "--kea-service"},
		{"empty service", "", "", "", "--kea-service"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// Arrange
			settings := keaClient{user: tc.user, password: tc.password, service: tc.service}

			// Act
			err := validateConfig(settings)

			// Assert
			switch {
			case tc.wantFlag == "" && err != nil:
				t.Errorf("validateConfig refused valid settings: %v", err)
			case tc.wantFlag != "" && err == nil:
				t.Error("validateConfig accepted the settings, want an error")
			case tc.wantFlag != "" && !strings.Contains(err.Error(), tc.wantFlag):
				t.Errorf("error %q does not name %s", err, tc.wantFlag)
			}
		})
	}
}
