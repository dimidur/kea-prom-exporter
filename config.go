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
	"sort"
)

var (
	showVersion   = flag.Bool("version", false, "Print version information and exit.")
	listenAddr    = flag.String("listen", ":9547", "Prometheus metrics listen address.")
	keaURL        = flag.String("kea-url", "http://127.0.0.1:8001/", "Kea HTTP control socket URL.")
	keaUser       = flag.String("kea-user", "", "Kea HTTP basic auth user. Empty disables auth.")
	keaPassFile   = flag.String("kea-password-file", "", "Path to a file containing the Kea HTTP basic auth password. Takes precedence over --kea-password.")
	keaPass       = flag.String("kea-password", "", "Kea HTTP basic auth password. Prefer --kea-password-file for production.")
	httpTimeout   = flag.Duration("kea-timeout", defaultKeaTimeout, fmt.Sprintf("Per-request Kea API timeout. Each control command is bounded separately, so budget %d of these under Prometheus's scrape_timeout.", len(scrapeCommands)))
	scrapeService = flag.String("kea-service", supportedService, "Kea service name to scrape. Only "+supportedService+" is implemented.")
	logLevel      = flag.String("log-level", "info", "Log level: debug, info, warn or error.")
	logFormat     = flag.String("log-format", "text", "Log format: text or json.")
)

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
// It runs after flag.Parse rather than seeding flag defaults, so an explicit
// flag still wins, and so a rejected value is an ordinary error returned to a
// caller that already has a logger: flag defaults are evaluated during
// package initialisation, before anything exists to report a problem to.
func applyEnv(fs *flag.FlagSet, lookup func(string) (string, bool)) []error {
	explicit := map[string]bool{}
	fs.Visit(func(visited *flag.Flag) { explicit[visited.Name] = true })

	var problems []error
	// Sorted so the diagnostics are stable rather than map-ordered.
	names := make([]string, 0, len(envForFlag))
	for name := range envForFlag {
		names = append(names, name)
	}
	sort.Strings(names)

	for _, name := range names {
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
		previous := fs.Lookup(name).Value.String()
		if err := fs.Set(name, value); err != nil {
			problems = append(problems, fmt.Errorf("%s=%q is not a valid --%s, keeping %s: %w", env, value, name, previous, err))
			if restoreErr := fs.Set(name, previous); restoreErr != nil {
				problems = append(problems, fmt.Errorf("could not restore --%s to %s: %w", name, previous, restoreErr))
			}
		}
	}
	return problems
}

// validateConfig refuses Kea settings the exporter cannot run with. The
// password in settings is the loaded one, from whichever flag supplied it.
func validateConfig(settings keaClient) error {
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
