// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/cjohnstoniv/wardyn/internal/runner"
)

// loadTrustedCA reads this runner's own corporate CA bundle, at startup only (a change needs a
// restart), from file alone (--trusted-ca-file, whose default is WARDYN_TRUSTED_CA_FILE). The
// ambient SSL_CERT_FILE and SSL_CERT_DIR are never read: a runner trusts only a bundle its operator
// named. The file goes through the daemon's certificate-only validation and reconstruction
// (runner.TrustedCABundle); an unreadable, empty or invalid one, or one over
// runner.MaxEnvTrustedCAPEM, fails startup with its name. No file is "", with no error.
//
// The bound is a delivery limit: a sandbox on a run with no TLS-inspecting proxy trusts the bundle
// only through WARDYN_MITM_CA_PEM, one environment string, so a bigger one would reach no client.
func loadTrustedCA(file string) (bundle string, n int, err error) {
	if file = strings.TrimSpace(file); file == "" {
		return "", 0, nil
	}
	source := "--trusted-ca-file / WARDYN_TRUSTED_CA_FILE " + file
	if bundle, n, err = readCAFile(file); err != nil {
		return "", 0, fmt.Errorf("%s: %w", source, err)
	}
	if len(bundle) > runner.MaxEnvTrustedCAPEM {
		return "", 0, fmt.Errorf("%s: the corporate CA bundle is %d KiB; runners take at most %d KiB: name a file holding only your organisation's roots, not a merged system bundle",
			source, (len(bundle)+1023)>>10, runner.MaxEnvTrustedCAPEM>>10)
	}
	return bundle, n, nil
}

// maxCAFileRead bounds what is read of the named file, so a path naming something huge is refused
// without reading it all. It is well above runner.MaxEnvTrustedCAPEM: the bound that matters applies
// to the rebuilt bundle, and a file with comments or a merged system bundle (a few hundred KiB)
// should still get that message.
const maxCAFileRead = 1 << 20

func readCAFile(path string) (string, int, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, maxCAFileRead+1))
	if err != nil {
		return "", 0, err
	}
	if len(raw) > maxCAFileRead {
		return "", 0, fmt.Errorf("the file is over %d KiB; runners take at most %d KiB of certificates: name a file holding only your organisation's roots, not a merged system bundle",
			maxCAFileRead>>10, runner.MaxEnvTrustedCAPEM>>10)
	}
	bundle, certs, err := runner.TrustedCABundle(raw)
	return bundle, len(certs), err
}

// trustedCAAtStartup loads the bundle into opts and writes it to <stateDir>/trusted-ca.pem, the file
// every sandbox mounts (mode 0644: the sandbox's user reads it), or empties an earlier one when none
// is configured.
func trustedCAAtStartup(ctx context.Context, opts *serveOptions, stateDir string) error {
	bundle, n, err := loadTrustedCA(opts.trustedCAFile)
	if err != nil {
		return err
	}
	path := filepath.Join(stateDir, "trusted-ca.pem")
	if bundle == "" {
		// A stopped sandbox created with a bundle still bind-mounts this file when it is revived, and
		// Docker refuses to start one whose bind source is gone: it stays, emptied, which both
		// sandbox scripts skip.
		if _, err := os.Stat(path); errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return os.WriteFile(path, nil, 0o644)
	}
	if err := os.WriteFile(path, []byte(bundle), 0o644); err != nil {
		return err
	}
	if err := os.Chmod(path, 0o644); err != nil {
		return err
	}
	opts.trustedCA, opts.trustedCAPath = bundle, path
	slog.InfoContext(ctx, "corporate CA trust configured", "file", opts.trustedCAFile, "cert_count", n, "sandbox_file", path)
	return nil
}
