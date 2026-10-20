// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"crypto/sha256"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/runner"
)

// orgCASource names the setting in a refusal.
const orgCASource = "--org-ca-file / WARDYN_ORG_CA_FILE "

// loadOrgCA reads the extra roots the runner trusts for its own connection to the organisation, and for
// nothing else: the bundle goes to the org client (orgHTTPClient) only, never to a sandbox's trust store
// or a run's proxy. The file must be a regular file owned by this account or root (a root-owned file is how
// a fleet delivers one) that group and others cannot write, in a directory they cannot write either, unless
// that directory is sticky and root-owned; this is a uid-and-mode hygiene rule for the trust store, and it
// adds no reach. A certificate that is expired, not yet valid or not a CA is skipped with a warning naming
// its sha256 fingerprint; the file is refused, naming it, when no usable certificate is left, and when it
// is unreadable or holds no certificates. No file is "", with no error.
func loadOrgCA(file string) (bundle string, n int, err error) {
	if file = strings.TrimSpace(file); file == "" {
		return "", 0, nil
	}
	warn := func(msg string) { slog.Warn("organisation-only CA: " + msg) }
	if bundle, n, err = readOrgCA(file, os.Geteuid(), time.Now(), warn); err != nil {
		return "", 0, fmt.Errorf("%s%s: %w", orgCASource, file, err)
	}
	return bundle, n, nil
}

// orgCAOwner is fileOwner, a seam so a test can name a file's owner without being root.
var orgCAOwner = fileOwner

// readOrgCA checks the directory holding the path as given and the one holding the resolved file, both before
// any open: a link in a directory others write can be re-pointed, whatever its target is. It then opens the
// resolved path once and takes the type, owner and mode from that descriptor, so the file checked is the file read.
func readOrgCA(path string, self int, now time.Time, warn func(string)) (string, int, error) {
	if err := checkOrgCADir(filepath.Dir(path)); err != nil {
		return "", 0, err
	}
	real, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", 0, err
	}
	if err := checkOrgCADir(filepath.Dir(real)); err != nil {
		return "", 0, err
	}
	f, err := openOrgCAFile(real)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return "", 0, err
	}
	if !st.Mode().IsRegular() {
		return "", 0, fmt.Errorf("not a regular file")
	}
	if st.Mode().Perm()&0o022 != 0 {
		return "", 0, fmt.Errorf("mode %04o lets group or others write it, so another account could add a root this runner trusts: chmod go-w it", st.Mode().Perm())
	}
	if uid, ok := orgCAOwner(st); ok && uid != 0 && uid != self {
		return "", 0, fmt.Errorf("owned by another account (uid %d): a root this runner trusts must be owned by you or by root", uid)
	}
	raw, err := io.ReadAll(io.LimitReader(f, maxCAFileRead+1))
	if err != nil {
		return "", 0, err
	}
	if len(raw) > maxCAFileRead {
		return "", 0, fmt.Errorf("the file is over %d KiB: name a file holding only your organisation's roots", maxCAFileRead>>10)
	}
	_, certs, err := runner.TrustedCABundle(raw)
	if err != nil {
		return "", 0, err
	}
	var usable []*x509.Certificate
	var skipped []string
	for _, c := range certs {
		if why := orgCACertProblem(c, now); why != "" {
			warn(fmt.Sprintf("skipped the certificate with sha256 fingerprint %x: %s", sha256.Sum256(c.Raw), why))
			skipped = append(skipped, why)
			continue
		}
		usable = append(usable, c)
	}
	if len(usable) == 0 {
		return "", 0, fmt.Errorf("no usable CA certificate in the file (%s): name a file holding your organisation's current CA roots", strings.Join(skipped, "; "))
	}
	var bundle []byte
	for _, c := range usable {
		bundle = append(bundle, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: c.Raw})...)
	}
	return string(bundle), len(usable), nil
}

// checkOrgCADir refuses a directory group or others can write, which lets another account replace the file
// beside the check. A sticky directory owned by root (/tmp) is allowed: only the file's owner can replace
// it there.
func checkOrgCADir(dir string) error {
	if abs, err := filepath.Abs(dir); err == nil {
		dir = abs
	}
	st, err := os.Stat(dir)
	if err != nil {
		return err
	}
	if st.Mode().Perm()&0o022 == 0 {
		return nil
	}
	if uid, ok := orgCAOwner(st); st.Mode()&os.ModeSticky != 0 && (!ok || uid == 0) {
		return nil
	}
	return fmt.Errorf("the directory %s (mode %04o) lets group or others write, so another account could replace the file: put it in a directory only you can write", dir, st.Mode().Perm())
}

// orgCACertProblem is why c cannot anchor trust now, or "".
func orgCACertProblem(c *x509.Certificate, now time.Time) string {
	switch {
	case !c.BasicConstraintsValid || !c.IsCA:
		return fmt.Sprintf("the certificate %q is not a CA: only a CA certificate can anchor trust", c.Subject.String())
	case now.After(c.NotAfter):
		return fmt.Sprintf("the certificate %q expired on %s", c.Subject.String(), c.NotAfter.UTC().Format(time.DateOnly))
	case now.Before(c.NotBefore):
		return fmt.Sprintf("the certificate %q is not valid before %s", c.Subject.String(), c.NotBefore.UTC().Format(time.DateOnly))
	}
	return ""
}
