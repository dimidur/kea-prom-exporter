package main

// The configuration surface: every flag and its environment equivalent.
//
// One reason to change: a knob is added, renamed, or its precedence
// changes. Reading the credential lives in secret.go, and the logger it
// configures in logging.go.

import (
	"flag"
	"fmt"
	"sort"
	"time"
)

var (
	showVersion   = flag.Bool("version", false, "Print version information and exit.")
	listenAddr    = flag.String("listen", ":9547", "Prometheus metrics listen address.")
	keaURL        = flag.String("kea-url", "http://127.0.0.1:8001/", "Kea HTTP control socket URL.")
	keaUser       = flag.String("kea-user", "", "Kea HTTP basic auth user. Empty disables auth.")
	keaPassFile   = flag.String("kea-password-file", "", "Path to a file containing the Kea HTTP basic auth password. Takes precedence over --kea-password.")
	keaPass       = flag.String("kea-password", "", "Kea HTTP basic auth password. Prefer --kea-password-file for production.")
	httpTimeout   = flag.Duration("kea-timeout", 5*time.Second, "Per-request Kea API timeout. Each control command is bounded separately, so budget two of these under Prometheus's scrape_timeout.")
	scrapeService = flag.String("kea-service", "dhcp4", "Kea service name to scrape. Only dhcp4 is implemented.")
	logLevel      = flag.String("log-level", "info", "Log level: debug, info, warn or error.")
	logFormat     = flag.String("log-format", "text", "Log format: text or json.")
)

// envForFlag maps each flag to the environment variable that can set it.
// Every flag that configures the running exporter is settable this way; the
// container documentation leans on that. Meta-flags that exit immediately,
// currently only --version, are deliberately absent.
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
// caller that already has a logger -- flag defaults are evaluated during
// package initialisation, before anything exists to report a problem to.
func applyEnv(fs *flag.FlagSet, lookup func(string) (string, bool)) []error {
	explicit := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { explicit[f.Name] = true })

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
		v, ok := lookup(env)
		if !ok || v == "" {
			continue
		}
		// Snapshot first: flag.Value implementations assign before returning a
		// parse error -- durationValue.Set does `*d = durationValue(v)`
		// unconditionally -- so a rejected value would leave the flag zeroed
		// rather than at its default. A zero --kea-timeout in particular
		// becomes an http.Client with no timeout at all.
		previous := fs.Lookup(name).Value.String()
		if err := fs.Set(name, v); err != nil {
			problems = append(problems, fmt.Errorf("%s=%q is not a valid --%s, keeping %s: %w", env, v, name, previous, err))
			if restoreErr := fs.Set(name, previous); restoreErr != nil {
				problems = append(problems, fmt.Errorf("could not restore --%s to %s: %w", name, previous, restoreErr))
			}
		}
	}
	return problems
}
