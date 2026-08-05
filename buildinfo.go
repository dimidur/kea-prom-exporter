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

// readBuildInfo is a seam, not indirection for its own sake. Extracting
// resolveFrom made the logic testable but left this adapter as the only
// production path and invisible to a test: replacing its body with
// `resolveFrom(ver, rev, nil, false)` -- which deletes VCS stamping outright --
// kept the whole suite green. Swapping the function is what closes that.
var readBuildInfo = debug.ReadBuildInfo

// resolveIdentity takes the injected values as parameters rather than reading
// the package variables, so it is exercisable from a test without mutating
// global state -- which is a data race the moment any test runs in parallel.
func resolveIdentity(ver, rev string) buildID {
	info, ok := readBuildInfo()
	return resolveFrom(ver, rev, info, ok)
}

// resolveFrom is the whole of the logic, with the build info passed in.
// debug.ReadBuildInfo reports whatever stamped *this* binary, and `go test`
// does not stamp VCS data into a test binary -- so a test calling
// resolveIdentity could only skip, which is how the -dirty suffix and the
// revision lookup were both deletable with the suite still green.
func resolveFrom(ver, rev string, info *debug.BuildInfo, ok bool) buildID {
	id := buildID{version: ver, revision: rev, goVersion: runtime.Version()}

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
