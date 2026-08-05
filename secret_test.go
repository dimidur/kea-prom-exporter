package main

import (
	"os"
	"strings"
	"testing"
)

// Tests for secret.go: loading the control-socket credential.

func TestSecretTrimsOnlyTrailingNewlines(t *testing.T) {
	// TrimSpace would eat a password that legitimately ends in a space, and
	// the operator would see an authentication failure with no explanation.
	// Only the newline an editor or `echo` leaves behind may go.

	// Arrange
	dir := t.TempDir()
	cases := []struct {
		name     string
		contents string
		want     string
	}{
		{"trailing newline", "s3cret\n", "s3cret"},
		{"crlf", "s3cret\r\n", "s3cret"},
		{"no trailing newline", "s3cret", "s3cret"},
		{"a trailing space is part of the password", "s3cret \n", "s3cret "},
		{"a leading space is part of the password", " s3cret\n", " s3cret"},
		{"internal whitespace survives", "s3 cret\n", "s3 cret"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Arrange
			path := dir + "/" + strings.ReplaceAll(tc.name, " ", "-")
			if err := os.WriteFile(path, []byte(tc.contents), 0o600); err != nil {
				t.Fatalf("write: %v", err)
			}

			// Act
			got, err := loadPassword(path, "")

			// Assert
			if err != nil {
				t.Fatalf("loadPassword: %v", err)
			}
			if got != tc.want {
				t.Errorf("loadPassword(%q) = %q, want %q", tc.contents, got, tc.want)
			}
		})
	}
}

func TestLoadPassword(t *testing.T) {
	// Arrange
	dir := t.TempDir()
	file := dir + "/secret"
	// Trailing newline is what an editor or `echo` leaves behind; sending it
	// to Kea is an authentication failure with no useful diagnostic.
	if err := os.WriteFile(file, []byte("from-file\n"), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	cases := []struct {
		name    string
		file    string
		inline  string
		want    string
		wantErr bool
	}{
		{"file wins over inline", file, "inline", "from-file", false},
		{"trailing newline stripped", file, "", "from-file", false},
		{"inline when no file", "", "inline", "inline", false},
		{"neither is not an error", "", "", "", false},
		{"missing file is an error", dir + "/absent", "inline", "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Act
			got, err := loadPassword(tc.file, tc.inline)
			// Assert
			if (err != nil) != tc.wantErr {
				t.Fatalf("loadPassword(%q, %q) error = %v, wantErr %v", tc.file, tc.inline, err, tc.wantErr)
			}
			if got != tc.want {
				t.Errorf("loadPassword(%q, %q) = %q, want %q", tc.file, tc.inline, got, tc.want)
			}
		})
	}
}
