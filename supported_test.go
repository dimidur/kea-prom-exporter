package main

import (
	"encoding/json"
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// Tests that the README names the Kea the compatibility check runs against.

// keaBadge matches a shields.io badge in README whose URL mentions Kea.
var keaBadge = regexp.MustCompile(`img\.shields\.io/badge/[^)\s]*Kea[^)\s]*`)

// versionsPerPattern is, for each pattern, the versions it reads from content.
func versionsPerPattern(content []byte, patterns []*regexp.Regexp) [][]string {
	versions := make([][]string, len(patterns))
	for i, pattern := range patterns {
		for _, match := range pattern.FindAllSubmatch(content, -1) {
			versions[i] = append(versions[i], string(match[pattern.SubexpIndex("currentValue")]))
		}
	}
	return versions
}

func TestREADMENamesTheTestedKea(t *testing.T) {
	// Every place README names the tested version has to name the one the
	// check runs, and has to stay readable by the Renovate manager that moves
	// them together. A hand edit to one alone, or one that manager cannot
	// read, would leave README claiming a version nothing checks. The pattern
	// that reads compat.yml is the pin; every other pattern has to read
	// README, and so does some pattern for every Kea badge README shows.

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
				t.Fatalf("Renovate pattern `%s`: %v", source, err)
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
	inWorkflow := versionsPerPattern(workflow, patterns)
	inREADME := versionsPerPattern(readme, patterns)

	// Assert
	pinPattern, pinned := -1, ""
	var reading []string
	for i, versions := range inWorkflow {
		if len(versions) > 0 {
			reading = append(reading, "`"+patterns[i].String()+"`")
			pinPattern, pinned = i, versions[0]
		}
	}
	if len(reading) != 1 || len(inWorkflow[pinPattern]) != 1 {
		if len(reading) == 0 {
			reading = []string{"none"}
		}
		t.Fatalf("Renovate has to read exactly one Kea version in compat.yml; patterns reading it: %s",
			strings.Join(reading, ", "))
	}
	if len(patterns) < 2 {
		t.Fatal("renovate.json has no Kea pattern for README, so Renovate never moves its tested version")
	}
	for i, versions := range inREADME {
		if i == pinPattern {
			continue
		}
		stale := slices.ContainsFunc(versions, func(version string) bool { return version != pinned })
		if len(versions) == 0 || stale {
			t.Errorf("README, where Renovate reads `%s`: names %q, want only %q, the version compat.yml pins",
				patterns[i], versions, pinned)
		}
	}
	for _, badge := range keaBadge.FindAllString(string(readme), -1) {
		read := slices.ContainsFunc(patterns, func(pattern *regexp.Regexp) bool { return pattern.MatchString(badge) })
		if !read {
			t.Errorf("README shows a Kea badge no Renovate pattern reads, so it would go stale: %s", badge)
		}
	}
}
