// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/cjohnstoniv/wardyn/internal/runner"
)

// maxTrustedCABundle is a sanity bound, not a delivery limit: the proxy gets its config on stdin and
// the sandbox a mounted file, so neither is bound by an environment string. A bundle this big is
// almost certainly the wrong file.
const maxTrustedCABundle = 4 << 20

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
		return "", "", 0, fmt.Errorf("%s: %d certificates (%d bytes) is over the %d-byte sanity limit; name a file holding your organisation's roots with --trusted-ca-file", source, n, len(bundle), maxTrustedCABundle)
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

// trustedCAAtStartup loads the bundle into opts and writes it to <stateDir>/trusted-ca.pem, the file
// every sandbox mounts (mode 0644: the sandbox's user reads it), or removes a stale one when none is
// configured.
func trustedCAAtStartup(ctx context.Context, opts *serveOptions, stateDir string) error {
	bundle, source, n, err := loadTrustedCA(opts.trustedCAFile, os.Getenv)
	if err != nil {
		return err
	}
	path := filepath.Join(stateDir, "trusted-ca.pem")
	if bundle == "" {
		if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		return nil
	}
	if err := os.WriteFile(path, []byte(bundle), 0o644); err != nil {
		return err
	}
	if err := os.Chmod(path, 0o644); err != nil {
		return err
	}
	opts.trustedCA, opts.trustedCAPath = bundle, path
	slog.InfoContext(ctx, "corporate CA trust configured", "source", source, "cert_count", n, "file", path)
	if len(bundle) > runner.MaxEnvTrustedCAPEM {
		slog.InfoContext(ctx, "corporate CA bundle is over 64 KiB: sandboxes get it from the mounted file only, not WARDYN_MITM_CA_PEM, so an agent image older than 0.9.3 does not trust it", "bytes", len(bundle))
	}
	return nil
}
