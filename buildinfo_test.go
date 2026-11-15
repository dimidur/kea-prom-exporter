package main

import (
	"runtime"
	"runtime/debug"
	"testing"
)

// Tests for buildinfo.go: what --version and kea_exporter_build_info report.

func TestResolveFromBuildInfo(t *testing.T) {
	// --version and kea_exporter_build_info are how a deployment tells which
	// commit it runs, so every shape of build info a binary can carry must
	// resolve to the right identity.

	// buildInfo builds the stamped settings a real build would carry.
	buildInfo := func(settings map[string]string) *debug.BuildInfo {
		info := &debug.BuildInfo{GoVersion: "go1.99.0"}
		for key, value := range settings {
			info.Settings = append(info.Settings, debug.BuildSetting{Key: key, Value: value})
		}
		return info
	}

	cases := []struct {
		name             string
		injectedVersion  string
		injectedRevision string
		info             *debug.BuildInfo
		ok               bool
		wantVersion      string
		wantRevision     string
		wantGoVersion    string
	}{
		{
			name:            "clean tree takes the stamped revision",
			injectedVersion: "dev", info: buildInfo(map[string]string{"vcs.revision": "abc123", "vcs.modified": "false"}), ok: true,
			wantVersion: "dev", wantGoVersion: "go1.99.0", wantRevision: "abc123",
		},
		{
			name:            "dirty tree is marked",
			injectedVersion: "dev", info: buildInfo(map[string]string{"vcs.revision": "abc123", "vcs.modified": "true"}), ok: true,
			wantVersion: "dev", wantGoVersion: "go1.99.0", wantRevision: "abc123-dirty",
		},
		{
			name:            "an injected revision wins and is not second-guessed",
			injectedVersion: "v1.2.3", injectedRevision: "deadbeef",
			info: buildInfo(map[string]string{"vcs.revision": "abc123", "vcs.modified": "true"}), ok: true,
			wantVersion: "v1.2.3", wantGoVersion: "go1.99.0", wantRevision: "deadbeef",
		},
		{
			name:            "no build info falls back to the placeholder",
			injectedVersion: "v1.2.3", ok: false,
			wantVersion: "v1.2.3", wantGoVersion: runtime.Version(), wantRevision: "unknown",
		},
		{
			// ok true but no GoVersion: without the `info.GoVersion != ""`
			// guard this emits an empty goversion label.
			name:            "build info with no Go version falls back to runtime",
			injectedVersion: "dev", info: &debug.BuildInfo{}, ok: true,
			wantVersion: "dev", wantGoVersion: runtime.Version(), wantRevision: "unknown",
		},
		{
			name:            "build info without VCS settings",
			injectedVersion: "dev", info: buildInfo(nil), ok: true,
			wantVersion: "dev", wantGoVersion: "go1.99.0", wantRevision: "unknown",
		},
		{
			name:            "an empty version becomes the placeholder",
			injectedVersion: "", injectedRevision: "abc123", info: buildInfo(nil), ok: true,
			wantVersion: "dev", wantGoVersion: "go1.99.0", wantRevision: "abc123",
		},
		{
			// Dirty with no revision must not produce a bare "-dirty".
			name:            "dirty without a revision stays unknown",
			injectedVersion: "dev", info: buildInfo(map[string]string{"vcs.modified": "true"}), ok: true,
			wantVersion: "dev", wantGoVersion: "go1.99.0", wantRevision: "unknown",
		},
		{
			// info present but ok false: this is what distinguishes the
			// `ok &&` guard from an `info != nil &&` one.
			name:            "build info present but not ok is ignored",
			injectedVersion: "dev", info: buildInfo(map[string]string{"vcs.revision": "abc123"}), ok: false,
			wantVersion: "dev", wantGoVersion: runtime.Version(), wantRevision: "unknown",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Arrange
			info, ok := tc.info, tc.ok

			// Act
			id := resolveFrom(tc.injectedVersion, tc.injectedRevision, info, ok)

			// Assert
			if id.version != tc.wantVersion {
				t.Errorf("version = %q, want %q", id.version, tc.wantVersion)
			}
			if id.revision != tc.wantRevision {
				t.Errorf("revision = %q, want %q", id.revision, tc.wantRevision)
			}
			// Asserted exactly, not merely non-empty: in-process
			// debug.ReadBuildInfo().GoVersion equals runtime.Version(), so a
			// test routing through the real build info could not tell
			// whether the override is there at all.
			if id.goVersion != tc.wantGoVersion {
				t.Errorf("goVersion = %q, want %q", id.goVersion, tc.wantGoVersion)
			}
		})
	}
}

func TestResolveIdentityReadsTheRealBuildInfo(t *testing.T) {
	// resolveIdentity must take the revision and Go version from the build
	// info, which only this test sees through the readBuildInfo seam.

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

	// Assert
	// Both the revision and the Go version must come from what the seam
	// returned, proving resolveIdentity actually consults it.
	if id.revision != "seam-dirty" {
		t.Errorf("revision = %q, want seam-dirty; resolveIdentity is not reading the build info", id.revision)
	}
	if id.goVersion != "go0.0.0-sentinel" {
		t.Errorf("goVersion = %q, want the sentinel; resolveIdentity is not reading the build info", id.goVersion)
	}
}

func TestVersionLineNamesVersionRevisionAndGoVersion(t *testing.T) {
	// --version is what a bug report quotes, so it must carry every part of
	// the identity in a fixed shape.

	// Arrange
	id := buildID{version: "v1.2.3", revision: "cafebabe", goVersion: "go1.2.3"}

	// Act
	got := id.versionLine()

	// Assert
	if want := "kea-prom-exporter v1.2.3 (revision cafebabe, go1.2.3)"; got != want {
		t.Errorf("versionLine() = %q, want %q", got, want)
	}
}
