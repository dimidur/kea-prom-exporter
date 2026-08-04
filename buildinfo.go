package main

// Identity of the running binary: what --version prints and what
// kea_exporter_build_info reports.
//
// One reason to change: how a build is stamped.

import (
	"runtime"
	"runtime/debug"
)

// Set at build time with -X main.version / -X main.revision. Both are empty
// or "dev" for a plain `go build`, which falls back to the VCS data Go stamps
// into the build info -- the image build cannot use that fallback, because
// .dockerignore keeps .git out of the build context.
var (
	version  = "dev"
	revision = ""
)

// buildID is the resolved identity of this binary, captured once rather than
// re-derived per scrape.
type buildID struct{ version, revision, goVersion string }

// resolveIdentity takes the injected values as parameters rather than reading
// the package variables, so it is exercisable from a test without mutating
// global state -- which is a data race the moment any test runs in parallel.
func resolveIdentity(ver, rev string) buildID {
	id := buildID{version: ver, revision: rev, goVersion: runtime.Version()}

	// Both fields get a placeholder rather than being left empty: an empty
	// label value reads as "not built yet" rather than "not recorded", and an
	// empty VERSION build-arg is reachable (a workflow_dispatch release run
	// produces no semver output).
	if id.version == "" {
		id.version = "dev"
	}

	info, ok := debug.ReadBuildInfo()
	if ok && info.GoVersion != "" {
		id.goVersion = info.GoVersion
	}
	if id.revision != "" {
		// An injected revision is taken at face value. The image build has no
		// .git, so the -dirty suffix below is unavailable on that path.
		return id
	}

	id.revision = "unknown"
	if !ok {
		return id
	}
	var dirty bool
	for _, setting := range info.Settings {
		switch setting.Key {
		case "vcs.revision":
			id.revision = setting.Value
		case "vcs.modified":
			dirty = setting.Value == "true"
		}
	}
	if dirty && id.revision != "unknown" {
		// A dirty build is not the commit it claims to be, and silently
		// reporting the clean SHA is how "but that fix is deployed" happens.
		id.revision += "-dirty"
	}
	return id
}

func buildIdentity() buildID { return resolveIdentity(version, revision) }
