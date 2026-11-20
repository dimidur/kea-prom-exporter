package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"maps"
	"slices"
	"strings"
	"testing"
	"time"
)

// Tests for config.go: the flag and environment surface.

// parse parses args into flags, failing the test if they do not parse.
func parse(t *testing.T, flags *flag.FlagSet, args ...string) {
	t.Helper()
	if err := flags.Parse(args); err != nil {
		t.Fatalf("parse %v: %v", args, err)
	}
}

// parsedOptions is the production flag set with args parsed into it.
func parsedOptions(t *testing.T, args ...string) (*flag.FlagSet, *options) {
	t.Helper()
	flags, opts := newFlagSet("test", io.Discard)
	parse(t, flags, args...)
	return flags, opts
}

// registeredFlag returns the flag newFlagSet registers under name, failing the
// test if there is none.
func registeredFlag(t *testing.T, name string) *flag.Flag {
	t.Helper()
	flags, _ := parsedOptions(t)
	defined := flags.Lookup(name)
	if defined == nil {
		t.Fatalf("--%s is not registered", name)
	}
	return defined
}

func TestEmptyEnvValueDoesNotOverrideAFlagDefault(t *testing.T) {
	// An exported-but-empty variable is how a shell wrapper says "unset", and
	// it is common in compose files. Treating it as a value would blank the
	// setting rather than leave the default.

	// Arrange
	flags := flag.NewFlagSet("test", flag.ContinueOnError)
	url := flags.String("kea-url", "http://default/", "")
	parse(t, flags)

	// Act
	problems := applyEnv(flags, func(string) (string, bool) { return "", true })

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
	flags := flag.NewFlagSet("test", flag.ContinueOnError)
	for name := range envForFlag {
		flags.String(name, "", "")
	}
	parse(t, flags)
	var queried []string
	// Env var names in the order their flag names sort. With today's table
	// that is also the env names' own order, so this pins that the order is
	// stable, not which of the two names it follows.
	var want []string
	for _, name := range slices.Sorted(maps.Keys(envForFlag)) {
		want = append(want, envForFlag[name])
	}

	// Act
	applyEnv(flags, func(env string) (string, bool) {
		queried = append(queried, env)
		return "", false
	})

	// Assert
	if !slices.Equal(queried, want) {
		t.Errorf("env consulted in order\n got %v\nwant %v", queried, want)
	}
}

func TestApplyEnvFillsUnsetFlagsAndReportsBadValues(t *testing.T) {
	// The environment fills a flag the command line left unset, an explicit
	// flag beats it, and a bad value is reported rather than swallowed.

	// Each subtest parses its own arguments into the production flags and
	// reads back the flag it sets. --kea-timeout is the only flag settable
	// from the environment whose Set can fail.

	t.Run("env fills an unset flag", func(t *testing.T) {
		// Arrange
		flags, opts := parsedOptions(t)

		// Act
		problems := applyEnv(flags, envOf(map[string]string{"KEA_URL": "http://from-env/", "KEA_TIMEOUT": "12s"}))

		// Assert
		if len(problems) != 0 {
			t.Errorf("problems = %v, want none", problems)
		}
		if opts.keaURL != "http://from-env/" {
			t.Errorf("kea-url = %q, want the env value", opts.keaURL)
		}
		if opts.keaTimeout != 12*time.Second {
			t.Errorf("kea-timeout = %v, want 12s", opts.keaTimeout)
		}
	})

	t.Run("an explicit flag beats the environment", func(t *testing.T) {
		// Arrange
		flags, opts := parsedOptions(t, "-kea-url", "http://from-flag/")

		// Act
		applyEnv(flags, envOf(map[string]string{"KEA_URL": "http://from-env/"}))

		// Assert
		if opts.keaURL != "http://from-flag/" {
			t.Errorf("kea-url = %q, want the flag value", opts.keaURL)
		}
	})

	t.Run("an explicit flag equal to its default still beats the environment", func(t *testing.T) {
		// Arrange: --kea-user= is how a deployment turns auth off while a
		// container still exports KEA_USER.
		flags, opts := parsedOptions(t, "--kea-user=")

		// Act
		applyEnv(flags, envOf(map[string]string{"KEA_USER": "exporter"}))

		// Assert
		if opts.keaUser != "" {
			t.Errorf("kea-user = %q, want the explicit empty flag kept", opts.keaUser)
		}
	})

	t.Run("a rejected value is reported and the default kept", func(t *testing.T) {
		// Arrange: the problem names the variable, its value, the flag and
		// the value kept, which is the only place the value in effect shows.
		flags, opts := parsedOptions(t)
		const wantProblem = `KEA_TIMEOUT="twelve" is not a valid --kea-timeout, keeping 5s`

		// Act
		problems := applyEnv(flags, envOf(map[string]string{"KEA_TIMEOUT": "twelve", "LOG_LEVEL": ""}))

		// Assert
		if len(problems) != 1 {
			t.Fatalf("problems = %v, want exactly 1", problems)
		}
		if !strings.Contains(problems[0].Error(), wantProblem) {
			t.Errorf("problem = %v, want it to say %s", problems[0], wantProblem)
		}
		if opts.keaTimeout != 5*time.Second {
			t.Errorf("kea-timeout = %v, want the default kept after a bad value", opts.keaTimeout)
		}
	})
}

// rejectingValue is a flag.Value that refuses every value, including the one
// it reports holding.
type rejectingValue struct{}

// String reports the value the flag holds.
func (rejectingValue) String() string { return "held" }

// Set refuses the value.
func (rejectingValue) Set(string) error { return errors.New("rejected") }

func TestAFlagThatCannotBeRestoredIsReportedToo(t *testing.T) {
	// A rejected value is answered by restoring the flag to what it held. If
	// that restore fails too, the flag's value is no longer what the first
	// problem says it kept, and that must be reported rather than lost.

	// Arrange
	flags := flag.NewFlagSet("test", flag.ContinueOnError)
	flags.Var(rejectingValue{}, "kea-url", "")
	parse(t, flags)

	// Act
	problems := applyEnv(flags, envOf(map[string]string{"KEA_URL": "http://from-env/"}))

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
	// newFlagSet, not a hand-written FlagSet: walking a copy would assert a
	// property of the test's own fixture, and a flag added to config.go with
	// no envForFlag row would leave it green, which is the one thing it exists
	// to catch.
	//
	// --version is deliberately absent from envForFlag (config.go says so):
	// it exits immediately and configures nothing. That exemption is asserted
	// rather than skipped, so silently dropping another flag into it is not
	// possible.
	exempt := map[string]bool{"version": true}
	flags, _ := parsedOptions(t)

	// Act
	var registered []string
	flags.VisitAll(func(defined *flag.Flag) {
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

func TestFlagDefaultsAreTheDocumentedOnes(t *testing.T) {
	// README's flag table states every default, --help prints each flag's
	// DefValue, and the image exposes the default listen port, so a default
	// changed here silently contradicts all three.
	tests := []struct {
		flag string
		want string
	}{
		{"listen", ":9547"},
		{"kea-url", "http://127.0.0.1:8001/"},
		{"kea-user", ""},
		{"kea-password-file", ""},
		{"kea-password", ""},
		{"kea-timeout", "5s"},
		{"kea-service", "dhcp4"},
		{"log-level", "info"},
		{"log-format", "text"},
	}
	for _, testCase := range tests {
		t.Run(testCase.flag, func(t *testing.T) {
			// Arrange
			documented := testCase.want

			// Act
			defined := registeredFlag(t, testCase.flag)

			// Assert
			if defined.DefValue != documented {
				t.Errorf("--%s default = %q, want the documented %q", testCase.flag, defined.DefValue, documented)
			}
		})
	}
}

func TestEnvNamesAreTheDocumentedOnes(t *testing.T) {
	// README's flag table and its container example tell people which
	// variables to set, so a renamed variable is a setting every deployment
	// that uses it silently loses.

	// Arrange
	documented := map[string]string{
		"listen":            "LISTEN",
		"kea-url":           "KEA_URL",
		"kea-user":          "KEA_USER",
		"kea-password-file": "KEA_PASSWORD_FILE",
		"kea-password":      "KEA_PASSWORD",
		"kea-timeout":       "KEA_TIMEOUT",
		"kea-service":       "KEA_SERVICE",
		"log-level":         "LOG_LEVEL",
		"log-format":        "LOG_FORMAT",
	}

	// Act
	mapped := maps.Clone(envForFlag)

	// Assert
	if !maps.Equal(mapped, documented) {
		t.Errorf("envForFlag = %v, want the documented %v", mapped, documented)
	}
}

func TestTimeoutFallbackIsTheDocumentedDefault(t *testing.T) {
	// A zero or negative --kea-timeout falls back to defaultKeaTimeout, which
	// README states is the same 5s as the flag's default.

	// Arrange
	const documented = "5s"

	// Act
	fallback := defaultKeaTimeout.String()

	// Assert
	if fallback != documented {
		t.Errorf("a zero timeout falls back to %s, want the documented %s", fallback, documented)
	}
}

func TestKeaServiceDefaultPassesValidation(t *testing.T) {
	// Started with no flags, the exporter must run: a --kea-service default
	// that validateConfig refuses would stop every deployment that does not
	// set it.

	// Arrange
	defined := registeredFlag(t, "kea-service")

	// Act
	err := validateConfig(keaSettings{service: defined.DefValue})

	// Assert
	if err != nil {
		t.Errorf("the default --kea-service=%s is refused: %v", defined.DefValue, err)
	}
}

func TestKeaSettingsCarryWhatTheFlagsSay(t *testing.T) {
	// The Kea client is built from these settings alone, so a flag lost on
	// the way is one the command line accepts and the exporter then ignores.

	// Arrange
	_, opts := parsedOptions(t, "--kea-url=http://kea.test:8001/", "--kea-user=exporter", "--kea-service=dhcp6", "--kea-timeout=3s")
	want := keaSettings{
		url:      "http://kea.test:8001/",
		user:     "exporter",
		password: "kea-password",
		service:  "dhcp6",
		timeout:  3 * time.Second,
	}

	// Act
	settings := opts.keaSettings("kea-password")

	// Assert
	if settings != want {
		t.Errorf("settings = %+v, want %+v", settings, want)
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
	for _, testCase := range tests {
		t.Run(testCase.flag, func(t *testing.T) {
			// Arrange
			defined := registeredFlag(t, testCase.flag)

			// Act
			usage := defined.Usage

			// Assert
			if !strings.Contains(usage, testCase.want) {
				t.Errorf("--%s help = %q, want it to say %q", testCase.flag, usage, testCase.want)
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
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			// Arrange
			settings := keaSettings{user: testCase.user, password: testCase.password, service: testCase.service}

			// Act
			err := validateConfig(settings)

			// Assert
			switch {
			case testCase.wantFlag == "" && err != nil:
				t.Errorf("validateConfig refused valid settings: %v", err)
			case testCase.wantFlag != "" && err == nil:
				t.Error("validateConfig accepted the settings, want an error")
			case testCase.wantFlag != "" && !strings.Contains(err.Error(), testCase.wantFlag):
				t.Errorf("error %q does not name %s", err, testCase.wantFlag)
			}
		})
	}
}
