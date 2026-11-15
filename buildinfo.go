package main

// Identity of the running binary: what --version prints and what
// kea_exporter_build_info reports.
//
// One reason to change: how a build is stamped.

import (
	"fmt"
	"runtime"
	"runtime/debug"
)

// Set at build time with -X main.version / -X main.revision. Both are empty
// or "dev" for a plain `go build`, which falls back to the VCS data Go stamps
// into the build info. The image build cannot use that fallback, because
// .dockerignore keeps .git out of the build context.
var (
	version  = "dev"
	revision = ""
)

// buildID is the resolved identity of this binary, captured once rather than
// re-derived per scrape.
type buildID struct{ version, revision, goVersion string }

// versionLine is what --version prints.
func (id buildID) versionLine() string {
	return fmt.Sprintf("kea-prom-exporter %s (revision %s, %s)", id.version, id.revision, id.goVersion)
}

// readBuildInfo is a seam, not indirection for its own sake. resolveIdentity
// is the only production path to resolveFrom, and without the seam a test
// could not see it: a body of
// `resolveFrom(injectedVersion, injectedRevision, nil, false)`, which drops
// VCS stamping outright, would pass every test that only reaches resolveFrom.
var readBuildInfo = debug.ReadBuildInfo

// resolveIdentity takes the injected values as parameters rather than reading
// the package variables, so it is exercisable from a test without mutating
// global state, which is a data race the moment any test runs in parallel.
func resolveIdentity(injectedVersion, injectedRevision string) buildID {
	info, ok := readBuildInfo()
	return resolveFrom(injectedVersion, injectedRevision, info, ok)
}

// resolveFrom is the whole of the logic, with the build info passed in.
// debug.ReadBuildInfo reports whatever stamped *this* binary, and `go test`
// does not stamp VCS data into a test binary, so through resolveIdentity
// alone the -dirty suffix and the revision lookup could not be tested.
func resolveFrom(injectedVersion, injectedRevision string, info *debug.BuildInfo, ok bool) buildID {
	id := buildID{version: injectedVersion, revision: injectedRevision, goVersion: runtime.Version()}

	// Both fields get a placeholder rather than being left empty: an empty
	// label value reads as "not built yet" rather than "not recorded", and an
	// empty VERSION build-arg is reachable (a workflow_dispatch release run
	// produces no semver output).
	if id.version == "" {
		id.version = "dev"
	}

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
