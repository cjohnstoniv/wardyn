// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/cjohnstoniv/wardyn/internal/runner"
)

// maxTrustedCABundle bounds the runner's bundle. A sandbox receives it in one environment variable
// (WARDYN_MITM_CA_PEM, after the run's own CA and the organisation's roots), and Linux refuses to
// start a process with any single environment string over 128 KiB, so a whole system bundle would
// fail every run instead of this one start.
const maxTrustedCABundle = 64 << 10

// loadTrustedCA reads this runner's own corporate CA bundle, at startup only (a change needs a
// restart). The first source set wins and only it is read: file (--trusted-ca-file, whose default
// is WARDYN_TRUSTED_CA_FILE), else SSL_CERT_FILE, else SSL_CERT_DIR, a directory whose files are
// read in lexical order. Every file goes through the daemon's certificate-only validation and
// reconstruction (runner.TrustedCABundle), and an unreadable, empty or invalid one fails startup
// with its name. None set is "", with no error.
func loadTrustedCA(file string, getenv func(string) string) (bundle, source string, n int, err error) {
	if file = strings.TrimSpace(file); file != "" {
		source = "--trusted-ca-file / WARDYN_TRUSTED_CA_FILE " + file
	} else if file = strings.TrimSpace(getenv("SSL_CERT_FILE")); file != "" {
		source = "SSL_CERT_FILE " + file
	}
	switch dir := strings.TrimSpace(getenv("SSL_CERT_DIR")); {
	case source != "":
		if bundle, n, err = readCAFile(file); err != nil {
			return "", "", 0, fmt.Errorf("%s: %w", source, err)
		}
	case dir != "":
		source = "SSL_CERT_DIR " + dir
		entries, err := os.ReadDir(dir)
		if err != nil {
			return "", "", 0, fmt.Errorf("%s: %w", source, err)
		}
		for _, e := range entries {
			path := filepath.Join(dir, e.Name())
			if st, err := os.Stat(path); err == nil && st.IsDir() {
				continue
			}
			b, c, err := readCAFile(path)
			if err != nil {
				return "", "", 0, fmt.Errorf("SSL_CERT_DIR file %s: %w", path, err)
			}
			bundle, n = bundle+b, n+c
		}
		if n == 0 {
			return "", "", 0, fmt.Errorf("%s: no certificate files", source)
		}
	default:
		return "", "", 0, nil
	}
	if len(bundle) > maxTrustedCABundle {
		return "", "", 0, fmt.Errorf("%s: %d certificates (%d bytes) is over the %d-byte limit a sandbox's environment can carry; name a file holding only your organisation's roots with --trusted-ca-file", source, n, len(bundle), maxTrustedCABundle)
	}
	return bundle, source, n, nil
}

func readCAFile(path string) (string, int, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", 0, err
	}
	bundle, certs, err := runner.TrustedCABundle(raw)
	return bundle, len(certs), err
}
