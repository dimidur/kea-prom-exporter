package main

// Loading the Kea control-socket credential.
//
// One reason to change: how the secret is supplied (a file, an env var,
// and in future perhaps a systemd credential or a secrets manager).
// Deliberately separate from config.go, which changes when a flag is
// added, a different trigger entirely.

import (
	"fmt"
	"strings"
)

// loadPassword returns the password from file when one is named, read with
// readFile, and inline otherwise.
func loadPassword(readFile func(string) ([]byte, error), file, inline string) (string, error) {
	if file != "" {
		contents, err := readFile(file)
		if err != nil {
			return "", fmt.Errorf("read kea-password-file %q: %w", file, err)
		}
		return strings.TrimRight(string(contents), "\n\r"), nil
	}
	return inline, nil
}
