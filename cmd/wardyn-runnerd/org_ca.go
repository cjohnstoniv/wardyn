// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"crypto/x509"
	"fmt"
	"io"
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
// or a run's proxy. A file that is writable by group or others, owned by neither this account nor root
// (a root-owned file is how a fleet delivers one), unreadable, not certificates, expired or holding a
// certificate that is not a CA fails with its name. No file is "", with no error.
func loadOrgCA(file string) (bundle string, n int, err error) {
	if file = strings.TrimSpace(file); file == "" {
		return "", 0, nil
	}
	if bundle, n, err = readOrgCA(file, os.Geteuid(), time.Now()); err != nil {
		return "", 0, fmt.Errorf("%s%s: %w", orgCASource, file, err)
	}
	return bundle, n, nil
}

func readOrgCA(path string, self int, now time.Time) (string, int, error) {
	f, err := os.Open(path)
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
	real := path
	if r, err := filepath.EvalSymlinks(path); err == nil {
		real = r
	}
	if err := checkOrgCADir(filepath.Dir(real)); err != nil {
		return "", 0, err
	}
	if uid, ok := fileOwner(st); ok && uid != 0 && uid != self {
		return "", 0, fmt.Errorf("owned by another account (uid %d): a root this runner trusts must be owned by you or by root", uid)
	}
	raw, err := io.ReadAll(io.LimitReader(f, maxCAFileRead+1))
	if err != nil {
		return "", 0, err
	}
	if len(raw) > maxCAFileRead {
		return "", 0, fmt.Errorf("the file is over %d KiB: name a file holding only your organisation's roots", maxCAFileRead>>10)
	}
	bundle, certs, err := runner.TrustedCABundle(raw)
	if err != nil {
		return "", 0, err
	}
	for _, c := range certs {
		if err := checkOrgCACert(c, now); err != nil {
			return "", 0, err
		}
	}
	return bundle, len(certs), nil
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
	if uid, ok := fileOwner(st); st.Mode()&os.ModeSticky != 0 && (!ok || uid == 0) {
		return nil
	}
	return fmt.Errorf("the directory %s (mode %04o) lets group or others write, so another account could replace the file: put it in a directory only you can write", dir, st.Mode().Perm())
}

func checkOrgCACert(c *x509.Certificate, now time.Time) error {
	switch {
	case !c.BasicConstraintsValid || !c.IsCA:
		return fmt.Errorf("the certificate %q is not a CA: only a CA certificate can anchor trust", c.Subject.String())
	case now.After(c.NotAfter):
		return fmt.Errorf("the certificate %q expired on %s", c.Subject.String(), c.NotAfter.UTC().Format(time.DateOnly))
	}
	return nil
}
