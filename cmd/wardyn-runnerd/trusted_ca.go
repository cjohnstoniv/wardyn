// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

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
	bundle, certs, err := readCAFile(file)
	if err != nil {
		return "", 0, fmt.Errorf("%s: %w", source, err)
	}
	n = len(certs)
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

func readCAFile(path string) (string, []*x509.Certificate, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", nil, err
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, maxCAFileRead+1))
	if err != nil {
		return "", nil, err
	}
	if len(raw) > maxCAFileRead {
		return "", nil, fmt.Errorf("the file is over %d KiB; runners take at most %d KiB of certificates: name a file holding only your organisation's roots, not a merged system bundle",
			maxCAFileRead>>10, runner.MaxEnvTrustedCAPEM>>10)
	}
	return runner.TrustedCABundle(raw)
}

// trustedCAFileCheck is what a trusted-CA file that loads still deserves a look for: a certificate that
// has expired, is not valid yet or is not a certificate authority, and a file that another account can
// change. These are warnings only: the file is used exactly as before, because refusing such a file
// would stop runners that work today. certs is the parsed bundle, nil when the file does not load (its
// refusal is loadTrustedCA's).
func trustedCAFileCheck(file string, now time.Time) (certs []*x509.Certificate, warnings []string) {
	if file = strings.TrimSpace(file); file == "" {
		return nil, nil
	}
	if _, certs, _ = readCAFile(file); certs == nil {
		return nil, nil
	}
	for _, c := range certs {
		name := certName(c)
		if now.After(c.NotAfter) {
			warnings = append(warnings, fmt.Sprintf("the trusted CA file %s holds a certificate for %s that expired on %s.", file, name, c.NotAfter.UTC().Format(time.DateOnly)))
		} else if now.Before(c.NotBefore) {
			warnings = append(warnings, fmt.Sprintf("the trusted CA file %s holds a certificate for %s that is not valid until %s.", file, name, c.NotBefore.UTC().Format(time.DateOnly)))
		}
		if !c.IsCA {
			warnings = append(warnings, fmt.Sprintf("the trusted CA file %s holds a certificate for %s that is not marked as a certificate authority, so it cannot vouch for a server.", file, name))
		}
	}
	if fi, err := os.Stat(file); err == nil {
		if perm := fi.Mode().Perm(); perm&0o022 != 0 {
			warnings = append(warnings, fmt.Sprintf("the trusted CA file %s can be changed by other accounts (mode %04o): run chmod go-w on it.", file, perm))
		}
		if uid, ok := fileOwner(fi); ok {
			if w := caOwnerWarning(file, uid, os.Getuid()); w != "" {
				warnings = append(warnings, w)
			}
		}
	}
	// A sticky directory (such as /tmp) lets no other account rename or remove the file.
	if dir := filepath.Dir(file); dir != "" {
		if fi, err := os.Stat(dir); err == nil && fi.Mode().Perm()&0o022 != 0 && fi.Mode()&fs.ModeSticky == 0 {
			warnings = append(warnings, fmt.Sprintf("the directory %s holding the trusted CA file can be changed by other accounts (mode %04o): run chmod go-w on it.", dir, fi.Mode().Perm()))
		}
	}
	for i := range warnings {
		warnings[i] += " The runner still uses the file as it is."
	}
	return certs, warnings
}

// caOwnerWarning names a file owned by neither root nor the account the runner runs as (self): that
// account can change what the runner trusts.
func caOwnerWarning(file string, uid, self int) string {
	if uid == 0 || uid == self {
		return ""
	}
	return fmt.Sprintf("the trusted CA file %s belongs to another account (uid %d), not to root or to the account this runner runs as.", file, uid)
}

// certName is a certificate's common name, or its whole subject when it has none.
func certName(c *x509.Certificate) string {
	if c.Subject.CommonName != "" {
		return fmt.Sprintf("%q", c.Subject.CommonName)
	}
	return fmt.Sprintf("%q", c.Subject.String())
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
	certs, warnings := trustedCAFileCheck(opts.trustedCAFile, time.Now())
	for _, c := range certs {
		slog.InfoContext(ctx, "corporate CA certificate", "subject", c.Subject.String(), "issuer", c.Issuer.String(), "not_after", c.NotAfter.UTC().Format(time.RFC3339))
	}
	for _, w := range warnings {
		slog.WarnContext(ctx, w, "file", opts.trustedCAFile)
	}
	return nil
}
