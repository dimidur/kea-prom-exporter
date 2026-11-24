package main

import (
	"encoding/json"
	"os"
	"regexp"
	"testing"
)

// Tests that the README names the Kea the compatibility check runs against.

// keaVersionIn is the version the patterns read from content, failing the
// test unless they match exactly once.
func keaVersionIn(t *testing.T, name string, content []byte, patterns []*regexp.Regexp) string {
	t.Helper()
	var versions []string
	for _, pattern := range patterns {
		for _, match := range pattern.FindAllSubmatch(content, -1) {
			versions = append(versions, string(match[pattern.SubexpIndex("currentValue")]))
		}
	}
	if len(versions) != 1 {
		t.Fatalf("Renovate reads %d Kea versions in %s, want 1: %q", len(versions), name, versions)
	}
	return versions[0]
}

func TestREADMENamesTheTestedKea(t *testing.T) {
	// README's tested version has to be the one the check runs, and both have
	// to stay readable by the Renovate manager that moves them together. A
	// hand edit to one alone, or one that manager cannot read, would leave
	// README claiming a version nothing checks.

	// Arrange
	raw, err := os.ReadFile(".github/renovate.json")
	if err != nil {
		t.Fatalf("read renovate.json: %v", err)
	}
	var config struct {
		CustomManagers []struct {
			DepNameTemplate string   `json:"depNameTemplate"`
			MatchStrings    []string `json:"matchStrings"`
		} `json:"customManagers"`
	}
	if err := json.Unmarshal(raw, &config); err != nil {
		t.Fatalf("parse renovate.json: %v", err)
	}
	var patterns []*regexp.Regexp
	for _, manager := range config.CustomManagers {
		if manager.DepNameTemplate != "jonasal/kea-dhcp4" {
			continue
		}
		for _, source := range manager.MatchStrings {
			pattern, err := regexp.Compile(source)
			if err != nil {
				t.Fatalf("Renovate pattern %q: %v", source, err)
			}
			patterns = append(patterns, pattern)
		}
	}
	if len(patterns) == 0 {
		t.Fatal("renovate.json has no custom manager for jonasal/kea-dhcp4")
	}
	workflow, err := os.ReadFile(".github/workflows/compat.yml")
	if err != nil {
		t.Fatalf("read compat.yml: %v", err)
	}
	readme, err := os.ReadFile("README.md")
	if err != nil {
		t.Fatalf("read README.md: %v", err)
	}

	// Act
	pinned := keaVersionIn(t, "compat.yml", workflow, patterns)
	named := keaVersionIn(t, "README.md", readme, patterns)

	// Assert
	if named != pinned {
		t.Errorf("README names Kea %s as tested; compat.yml pins %s", named, pinned)
	}
}
