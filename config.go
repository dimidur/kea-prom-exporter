package main

// The configuration surface: every flag, its environment equivalent, and
// which settings are refused.
//
// One reason to change: a knob is added, renamed or refused, or its
// precedence changes. Reading the credential lives in secret.go, and the
// logger it configures in logging.go.

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"maps"
	"slices"
	"time"
)

// options holds what the flags parsed to.
type options struct {
	showVersion     bool
	listen          string
	keaURL          string
	keaUser         string
	keaPasswordFile string
	keaPassword     string
	keaTimeout      time.Duration
	keaService      string
	logLevel        string
	logFormat       string
}

// newFlagSet registers every flag on a new FlagSet called name, which is the
// program name its usage message starts with, and returns the options the
// flags parse into. Errors and usage are written to output.
func newFlagSet(name string, output io.Writer) (*flag.FlagSet, *options) {
	flags := flag.NewFlagSet(name, flag.ContinueOnError)
	flags.SetOutput(output)
	opts := &options{}
	flags.BoolVar(&opts.showVersion, "version", false, "Print version information and exit.")
	flags.StringVar(&opts.listen, "listen", ":9547", "Prometheus metrics listen address.")
	flags.StringVar(&opts.keaURL, "kea-url", "http://127.0.0.1:8001/", "Kea HTTP control socket URL.")
	flags.StringVar(&opts.keaUser, "kea-user", "", "Kea HTTP basic auth user. Empty disables auth.")
	flags.StringVar(&opts.keaPasswordFile, "kea-password-file", "", "Path to a file containing the Kea HTTP basic auth password. Takes precedence over --kea-password.")
	flags.StringVar(&opts.keaPassword, "kea-password", "", "Kea HTTP basic auth password. Prefer --kea-password-file for production.")
	flags.DurationVar(&opts.keaTimeout, "kea-timeout", defaultKeaTimeout, fmt.Sprintf("Per-request Kea API timeout. Each control command is bounded separately, so budget %d of these under Prometheus's scrape_timeout.", len(scrapeCommands)))
	flags.StringVar(&opts.keaService, "kea-service", supportedService, "Kea service name to scrape. Only "+supportedService+" is implemented.")
	flags.StringVar(&opts.logLevel, "log-level", "info", "Log level: debug, info, warn or error.")
	flags.StringVar(&opts.logFormat, "log-format", "text", "Log format: text or json.")
	return flags, opts
}

// keaSettings is the Kea part of the options, with the password as loaded
// from whichever flag supplied it.
func (o *options) keaSettings(password string) keaSettings {
	return keaSettings{
		url:      o.keaURL,
		user:     o.keaUser,
		password: password,
		service:  o.keaService,
		timeout:  o.keaTimeout,
	}
}

// envForFlag maps each flag to the environment variable that can set it.
// Every flag that configures the running exporter is settable this way; the
// container documentation leans on that. Meta-flags that exit immediately,
// such as --version, are deliberately absent.
var envForFlag = map[string]string{
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

// applyEnv fills in flags the command line did not set, from the environment.
// It runs after the flags are parsed rather than seeding their defaults, so
// an explicit flag still wins, and so a rejected value is an ordinary error
// returned to a caller that can log it: a value rejected while the flags are
// registered would have nowhere to go.
func applyEnv(flags *flag.FlagSet, lookup func(string) (string, bool)) []error {
	explicit := map[string]bool{}
	flags.Visit(func(visited *flag.Flag) { explicit[visited.Name] = true })

	var problems []error
	// Sorted so the diagnostics are stable rather than map-ordered.
	for _, name := range slices.Sorted(maps.Keys(envForFlag)) {
		if explicit[name] {
			continue
		}
		env := envForFlag[name]
		value, ok := lookup(env)
		if !ok || value == "" {
			continue
		}
		// Snapshot first: flag.Value implementations assign before returning a
		// parse error (durationValue.Set does `*d = durationValue(v)`
		// unconditionally), so a rejected value would leave the flag zeroed
		// rather than at the value the problem reports it is keeping.
		previous := flags.Lookup(name).Value.String()
		if err := flags.Set(name, value); err != nil {
			problems = append(problems, fmt.Errorf("%s=%q is not a valid --%s, keeping %s: %w", env, value, name, previous, err))
			if restoreErr := flags.Set(name, previous); restoreErr != nil {
				problems = append(problems, fmt.Errorf("could not restore --%s to %s: %w", name, previous, restoreErr))
			}
		}
	}
	return problems
}

// validateConfig refuses Kea settings the exporter cannot run with. The
// password in settings is the loaded one, from whichever flag supplied it.
func validateConfig(settings keaSettings) error {
	if settings.user != "" && settings.password == "" {
		return errors.New("--kea-user is set but no password was provided; use --kea-password-file or --kea-password")
	}
	// Without this, --kea-service=dhcp6 returns a valid response whose every
	// key is unmapped: an empty metric set under a kea_dhcp4_ namespace,
	// reported as kea_up 1.
	if settings.service != supportedService {
		return fmt.Errorf("unsupported --kea-service %q; only %s is implemented", settings.service, supportedService)
	}
	return nil
}
