package main

import (
	"runtime"
	"runtime/debug"
	"testing"
)

// Tests for buildinfo.go: what --version and kea_exporter_build_info report.

func TestResolveFromBuildInfo(t *testing.T) {
	// go test does not stamp VCS data into the test binary, so a test that
	// calls debug.ReadBuildInfo can only skip -- which is why deleting the
	// -dirty suffix and the vcs.revision lookup both left the suite green.
	// Passing the build info in makes both reachable.

	// Arrange
	buildInfo := func(settings map[string]string) *debug.BuildInfo {
		info := &debug.BuildInfo{GoVersion: "go1.99.0"}
		for key, value := range settings {
			info.Settings = append(info.Settings, debug.BuildSetting{Key: key, Value: value})
		}
		return info
	}

	cases := []struct {
		name          string
		ver, rev      string
		info          *debug.BuildInfo
		ok            bool
		wantVersion   string
		wantRevision  string
		wantGoVersion string
	}{
		{
			name: "clean tree takes the stamped revision",
			ver:  "dev", info: buildInfo(map[string]string{"vcs.revision": "abc123", "vcs.modified": "false"}), ok: true,
			wantVersion: "dev", wantGoVersion: "go1.99.0", wantRevision: "abc123",
		},
		{
			// The suffix is the whole point: a dirty build is not the commit
			// it names, and reporting the clean SHA hides that.
			name: "dirty tree is marked",
			ver:  "dev", info: buildInfo(map[string]string{"vcs.revision": "abc123", "vcs.modified": "true"}), ok: true,
			wantVersion: "dev", wantGoVersion: "go1.99.0", wantRevision: "abc123-dirty",
		},
		{
			name: "an injected revision wins and is not second-guessed",
			ver:  "v1.2.3", rev: "deadbeef",
			info: buildInfo(map[string]string{"vcs.revision": "abc123", "vcs.modified": "true"}), ok: true,
			wantVersion: "v1.2.3", wantGoVersion: "go1.99.0", wantRevision: "deadbeef",
		},
		{
			name: "no build info falls back to the placeholder",
			ver:  "v1.2.3", ok: false,
			wantVersion: "v1.2.3", wantGoVersion: runtime.Version(), wantRevision: "unknown",
		},
		{
			// ok true but no GoVersion: without the `info.GoVersion != ""`
			// guard this emits an empty goversion label.
			name: "build info with no Go version falls back to runtime",
			ver:  "dev", info: &debug.BuildInfo{}, ok: true,
			wantVersion: "dev", wantGoVersion: runtime.Version(), wantRevision: "unknown",
		},
		{
			name: "build info without VCS settings",
			ver:  "dev", info: buildInfo(nil), ok: true,
			wantVersion: "dev", wantGoVersion: "go1.99.0", wantRevision: "unknown",
		},
		{
			// Reachable: a workflow_dispatch release run produces no semver.
			name: "an empty version becomes the placeholder",
			ver:  "", rev: "abc123", info: buildInfo(nil), ok: true,
			wantVersion: "dev", wantGoVersion: "go1.99.0", wantRevision: "abc123",
		},
		{
			// Dirty with no revision must not produce a bare "-dirty".
			name: "dirty without a revision stays unknown",
			ver:  "dev", info: buildInfo(map[string]string{"vcs.modified": "true"}), ok: true,
			wantVersion: "dev", wantGoVersion: "go1.99.0", wantRevision: "unknown",
		},
		{
			// info present but ok false: this is what distinguishes the
			// `ok &&` guard from an `info != nil &&` one.
			name: "build info present but not ok is ignored",
			ver:  "dev", info: buildInfo(map[string]string{"vcs.revision": "abc123"}), ok: false,
			wantVersion: "dev", wantGoVersion: runtime.Version(), wantRevision: "unknown",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Act
			id := resolveFrom(tc.ver, tc.rev, tc.info, tc.ok)

			// Assert
			if id.version != tc.wantVersion {
				t.Errorf("version = %q, want %q", id.version, tc.wantVersion)
			}
			if id.revision != tc.wantRevision {
				t.Errorf("revision = %q, want %q", id.revision, tc.wantRevision)
			}
			// Asserted exactly, not merely non-empty: in-process
			// debug.ReadBuildInfo().GoVersion equals runtime.Version(), so a
			// test routing through the real build info cannot tell the
			// difference and deleting the override survived.
			if id.goVersion != tc.wantGoVersion {
				t.Errorf("goVersion = %q, want %q", id.goVersion, tc.wantGoVersion)
			}
		})
	}
}

func TestResolveIdentityReadsTheRealBuildInfo(t *testing.T) {
	// resolveFrom being testable is not enough: resolveIdentity is the only
	// production path, and replacing its body with
	// `resolveFrom(ver, rev, nil, false)` -- which deletes VCS stamping
	// entirely -- kept the whole suite green. Swapping the seam is what makes
	// that visible.

	// Arrange
	original := readBuildInfo
	t.Cleanup(func() { readBuildInfo = original })
	readBuildInfo = func() (*debug.BuildInfo, bool) {
		return &debug.BuildInfo{
			GoVersion: "go0.0.0-sentinel",
			Settings: []debug.BuildSetting{
				{Key: "vcs.revision", Value: "seam"},
				{Key: "vcs.modified", Value: "true"},
			},
		}, true
	}

	// Act
	id := resolveIdentity("dev", "")

	// Assert -- both the revision and the Go version must come from what the
	// seam returned, proving resolveIdentity actually consults it.
	if id.revision != "seam-dirty" {
		t.Errorf("revision = %q, want seam-dirty; resolveIdentity is not reading the build info", id.revision)
	}
	if id.goVersion != "go0.0.0-sentinel" {
		t.Errorf("goVersion = %q, want the sentinel; resolveIdentity is not reading the build info", id.goVersion)
	}
}
