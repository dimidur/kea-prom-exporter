package main

import (
	"fmt"
	"os"
	"strings"
)

// Loading the Kea control-socket credential.
//
// One reason to change: how the secret is supplied (a file, an env var,
// and in future perhaps a systemd credential or a secrets manager).
// Deliberately separate from config.go, which changes when a flag is
// added -- a different trigger entirely.

func loadPassword(file, inline string) (string, error) {
	if file != "" {
		// #nosec G304 -- the path is supplied by the operator via
		// --kea-password-file / KEA_PASSWORD_FILE. Reading an operator-named
		// file is the feature; there is no untrusted input on this path.
		b, err := os.ReadFile(file)
		if err != nil {
			return "", fmt.Errorf("read kea-password-file %q: %w", file, err)
		}
		return strings.TrimRight(string(b), "\n\r"), nil
	}
	return inline, nil
}
