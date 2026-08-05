package main

import (
	"flag"
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

func TestApplyEnvFillsUnsetFlagsOnly(t *testing.T) {
	// Arrange -- a local FlagSet mirroring the real one, so a subtest can
	// parse arguments without disturbing flag.CommandLine.
	// An explicit flag must beat the environment, every flag must be settable
	// from it, and a bad value must be reported rather than swallowed.
	newFS := func() (*flag.FlagSet, *string, *time.Duration) {
		fs := flag.NewFlagSet("test", flag.ContinueOnError)
		url := fs.String("kea-url", "http://default/", "")
		timeout := fs.Duration("kea-timeout", 5*time.Second, "")
		fs.String("listen", ":9547", "")
		fs.String("kea-user", "", "")
		fs.String("kea-password-file", "", "")
		fs.String("kea-password", "", "")
		fs.String("kea-service", "dhcp4", "")
		fs.String("log-level", "info", "")
		fs.String("log-format", "text", "")
		return fs, url, timeout
	}
	env := func(m map[string]string) func(string) (string, bool) {
		return func(k string) (string, bool) { v, ok := m[k]; return v, ok }
	}

	t.Run("env fills an unset flag", func(t *testing.T) {
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
		// Arrange -- only --kea-timeout is typed, so it is the only flag whose
		// Set can fail. applyEnv collects problems into a slice rather than
		// keeping the last one, but with a single failable flag that
		// difference is not observable; naming this "every bad value" would
		// claim coverage the flag set cannot provide.
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

	t.Run("every real flag has an env equivalent", func(t *testing.T) {
		// Arrange -- flag.CommandLine, not the local FlagSet above. Walking a
		// hand-written copy made this assert a property of the test's own
		// fixture: adding a flag to config.go with no envForFlag row left it
		// green, which is the one thing it exists to catch.
		//
		// --version is deliberately absent from envForFlag (config.go says so):
		// it exits immediately and configures nothing. That exemption is
		// asserted rather than skipped, so silently dropping another flag into
		// it is not possible.
		exempt := map[string]bool{"version": true}

		// Act
		var real []string
		flag.CommandLine.VisitAll(func(f *flag.Flag) {
			// The test binary injects its own -test.* flags.
			if strings.HasPrefix(f.Name, "test.") {
				return
			}
			real = append(real, f.Name)
		})

		// Assert
		if len(real) < 5 {
			t.Fatalf("found only %d real flags (%v); this guard would be vacuous", len(real), real)
		}
		for _, name := range real {
			_, mapped := envForFlag[name]
			switch {
			case exempt[name] && mapped:
				t.Errorf("--%s is documented as having no env form but appears in envForFlag", name)
			case !exempt[name] && !mapped:
				t.Errorf("flag --%s has no entry in envForFlag", name)
			}
		}
		for name := range envForFlag {
			if !slices.Contains(real, name) {
				t.Errorf("envForFlag maps %q, which is not a flag", name)
			}
		}
	})
}
