package main

import (
	"errors"
	"fmt"
	"io/fs"
	"strings"
	"testing"
)

// Tests for secret.go: loading the control-socket credential.

func TestSecretTrimsOnlyTrailingNewlines(t *testing.T) {
	// TrimSpace would eat a password that legitimately ends in a space, and
	// the result would be an authentication failure with no explanation.
	// Only the newline an editor or `echo` leaves behind may go.
	cases := []struct {
		name     string
		contents string
		want     string
	}{
		{"trailing newline", "kea-password\n", "kea-password"},
		{"crlf", "kea-password\r\n", "kea-password"},
		{"no trailing newline", "kea-password", "kea-password"},
		{"a trailing space is part of the password", "kea-password \n", "kea-password "},
		{"a leading space is part of the password", " kea-password\n", " kea-password"},
		{"internal whitespace survives", "kea password\n", "kea password"},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			// Arrange
			readFile := filesOf(map[string]string{"/run/secrets/kea": testCase.contents})

			// Act
			got, err := loadPassword(readFile, "/run/secrets/kea", "")

			// Assert
			if err != nil {
				t.Fatalf("loadPassword: %v", err)
			}
			if got != testCase.want {
				t.Errorf("loadPassword(%q) = %q, want %q", testCase.contents, got, testCase.want)
			}
		})
	}
}

func TestLoadPassword(t *testing.T) {
	// The file wins over the inline value when both are given, and a file that
	// cannot be read is an error rather than an empty password, which Kea
	// would answer with an authentication failure that names nothing. The
	// error names the file, the only clue to a secret mounted in the wrong
	// place.
	cases := []struct {
		name    string
		file    string
		inline  string
		want    string
		wantErr bool
	}{
		{"file wins over inline", "/run/secrets/kea", "inline", "from-file", false},
		{"inline when no file", "", "inline", "inline", false},
		{"neither is not an error", "", "", "", false},
		{"missing file is an error", "/run/secrets/absent", "inline", "", true},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			// Arrange
			readFile := filesOf(map[string]string{"/run/secrets/kea": "from-file\n"})
			file, inline := testCase.file, testCase.inline

			// Act
			got, err := loadPassword(readFile, file, inline)

			// Assert
			if (err != nil) != testCase.wantErr {
				t.Fatalf("loadPassword(%q, %q) error = %v, wantErr %v", file, inline, err, testCase.wantErr)
			}
			if testCase.wantErr && (!errors.Is(err, fs.ErrNotExist) || !strings.Contains(err.Error(), fmt.Sprintf("kea-password-file %q", file))) {
				t.Errorf("error = %v, want it to wrap the read error and name the flag and %s", err, file)
			}
			if got != testCase.want {
				t.Errorf("loadPassword(%q, %q) = %q, want %q", file, inline, got, testCase.want)
			}
		})
	}
}
