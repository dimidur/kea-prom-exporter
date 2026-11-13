package main

import (
	"os"
	"runtime"
	"strings"
	"testing"
)

// Tests that the suite runs on the one Go version this module uses.

func TestGoVersionMatchesGoMod(t *testing.T) {
	// go.mod's `go` line is a floor to the go command: an older local Go
	// switches up to it, but a newer one runs as itself. CI and the image use
	// exactly the line, so a pass on any other Go proves nothing about them.

	// Arrange
	data, err := os.ReadFile("go.mod")
	if err != nil {
		t.Fatalf("read go.mod: %v", err)
	}
	var want string
	for line := range strings.Lines(string(data)) {
		if fields := strings.Fields(line); len(fields) >= 2 && fields[0] == "go" {
			want = "go" + fields[1]
			break
		}
	}
	if want == "" {
		t.Fatal("go.mod has no go line")
	}

	// Act
	got := runtime.Version()

	// Assert
	if got != want {
		t.Fatalf("running on %s but go.mod pins %s; rerun with GOTOOLCHAIN=%s", got, want, want)
	}
}
