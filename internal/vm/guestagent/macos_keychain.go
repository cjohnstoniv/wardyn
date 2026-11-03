// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package guestagent

import (
	"bytes"
	"context"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
)

// MacSystemKeychain is a macOS guest's System keychain. Init adds the run's proxy CA to it; a clone
// can outlive its run (a kept machine), so each trusted CA is recorded first (MacRunCARecordDir) and
// the scrub removes it (UntrustRecorded). The base a run is cloned from is never changed.
const MacSystemKeychain = "/Library/Keychains/System.keychain"

// MacRunCARecordDir is the root-only folder that holds each run CA TrustRunCA trusted, as
// <SHA-256>.pem, until the scrub has removed it from the System keychain.
const MacRunCARecordDir = "/var/db/wardyn/run-cas"

// macSecurity is macOS's keychain command.
const macSecurity = "/usr/bin/security"

// The keychain trust codes InitResult carries. They are the only thing the guest says about it:
// the command's output stays in the guest. The host reads them as runner.KeychainTrust.
const (
	KeychainTrusted    = "trusted"
	KeychainNotTrusted = "not_trusted"
	// KeychainRefused: macOS refused the trust change (it asked for an authorization no one was
	// there to give).
	KeychainRefused = "refused"
	// KeychainFailed: the security command exited with an error for another reason.
	KeychainFailed = "failed"
	// KeychainUnavailable: the command could not run, or the run CA was not exactly one certificate.
	KeychainUnavailable = "unavailable"
)

// KeychainTrustArgs is the one command that trusts the run CA: as a root (trustRoot) in the admin
// trust settings (-d), in the System keychain, for the SSL policy only (-p ssl), never every policy.
// See security(1), add-trusted-cert.
func KeychainTrustArgs(certFile string) []string {
	return []string{"add-trusted-cert", "-d", "-r", "trustRoot", "-p", "ssl", "-k", MacSystemKeychain, certFile}
}

// MacKeychain trusts a run's proxy CA in the guest's System keychain. Run is how security is
// reached; Dir holds the certificate's file while the command reads it. RecordDir is
// MacRunCARecordDir in a guest; "" records nothing (tests only).
type MacKeychain struct {
	Run       CmdRunner
	Dir       string
	RecordDir string
}

// TrustRunCA trusts the one certificate in runCA and answers a status and a reason code. It never
// fails Init: a CA that is not trusted leaves the run going on its CA bundle alone. The certificate
// is recorded before security runs, and a CA that cannot be recorded is not trusted, so no trust is
// ever left that the scrub does not know of.
func (k MacKeychain) TrustRunCA(ctx context.Context, runCA string) (status, reason string) {
	block, rest := pem.Decode([]byte(runCA))
	if block == nil || block.Type != "CERTIFICATE" || len(bytes.TrimSpace(rest)) != 0 {
		return KeychainNotTrusted, KeychainUnavailable
	}
	if _, err := x509.ParseCertificate(block.Bytes); err != nil {
		return KeychainNotTrusted, KeychainUnavailable
	}
	if k.RecordDir != "" {
		if err := k.record(block); err != nil {
			return KeychainNotTrusted, KeychainUnavailable
		}
	}
	f, err := os.CreateTemp(k.Dir, "wardyn-run-ca-*.pem")
	if err != nil {
		return KeychainNotTrusted, KeychainUnavailable
	}
	defer os.Remove(f.Name())
	_, err = f.Write(pem.EncodeToMemory(block))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return KeychainNotTrusted, KeychainUnavailable
	}
	_, err = k.Run.Run(ctx, macSecurity, KeychainTrustArgs(f.Name())...)
	return keychainCode(err)
}

// keychainCode maps how security ended to a code. Its output decides only which code; it is never
// passed on.
func keychainCode(err error) (status, reason string) {
	var exit *exec.ExitError
	switch {
	case err == nil:
		return KeychainTrusted, ""
	case !errors.As(err, &exit):
		return KeychainNotTrusted, KeychainUnavailable
	}
	msg := strings.ToLower(err.Error())
	if strings.Contains(msg, "authorization") || strings.Contains(msg, "user interaction") {
		return KeychainNotTrusted, KeychainRefused
	}
	return KeychainNotTrusted, KeychainFailed
}

// record writes block as <SHA-256>.pem in RecordDir, root's alone.
func (k MacKeychain) record(block *pem.Block) error {
	if err := os.MkdirAll(k.RecordDir, 0o700); err != nil {
		return err
	}
	sum := sha256.Sum256(block.Bytes)
	return os.WriteFile(filepath.Join(k.RecordDir, hex.EncodeToString(sum[:])+".pem"), pem.EncodeToMemory(block), 0o600)
}

var caRecordName = regexp.MustCompile(`^[0-9a-f]{64}\.pem$`)

// Untrusted is what UntrustRecorded did, by SHA-256 fingerprint: Removed are gone from the System
// keychain (or were already), Refused are those macOS would not remove without a person, Failed the
// rest. A refused or failed CA keeps its record, so the next scrub tries it again.
type Untrusted struct {
	Removed, Refused, Failed []string
}

// UntrustRecorded removes every recorded run CA's trust and certificate, and drops the record of each
// one that is gone. TrustRunCA trusts in the admin trust domain (-d), which delete-certificate -t does
// not reach (-t is the user domain), so the admin trust goes first, by the recorded file
// (remove-trusted-cert -d); then the certificate, by SHA-1. A step macOS refuses stops that CA there.
// err is a record folder that could not be read.
func (k MacKeychain) UntrustRecorded(ctx context.Context) (Untrusted, error) {
	var u Untrusted
	if k.RecordDir == "" {
		return u, nil
	}
	entries, err := os.ReadDir(k.RecordDir)
	if errors.Is(err, os.ErrNotExist) {
		return u, nil
	}
	if err != nil {
		return u, err
	}
	for _, e := range entries {
		if !e.Type().IsRegular() || !caRecordName.MatchString(e.Name()) {
			continue
		}
		path := filepath.Join(k.RecordDir, e.Name())
		fp := strings.TrimSuffix(e.Name(), ".pem")
		b, err := os.ReadFile(path)
		block, _ := pem.Decode(b)
		if err != nil || block == nil || block.Type != "CERTIFICATE" {
			u.Failed = append(u.Failed, fp)
			continue
		}
		s1 := sha1.Sum(block.Bytes)
		err = k.untrust(ctx, path, strings.ToUpper(hex.EncodeToString(s1[:])))
		if err == nil {
			err = os.Remove(path)
		}
		switch _, reason := keychainCode(err); {
		case err == nil:
			u.Removed = append(u.Removed, fp)
		case reason == KeychainRefused:
			u.Refused = append(u.Refused, fp)
		default:
			u.Failed = append(u.Failed, fp)
		}
	}
	return u, nil
}

// untrust runs RemoveTrustArgs then DeleteCertArgs; one that finds nothing to remove has nothing to do.
func (k MacKeychain) untrust(ctx context.Context, certFile, sha1Hex string) error {
	for _, args := range [][]string{RemoveTrustArgs(certFile), DeleteCertArgs(sha1Hex)} {
		if _, err := k.Run.Run(ctx, macSecurity, args...); err != nil && !certNotFound(err) {
			return err
		}
	}
	return nil
}

// RemoveTrustArgs removes a certificate's trust settings from the admin trust domain, where
// KeychainTrustArgs put them. See security(1), remove-trusted-cert.
func RemoveTrustArgs(certFile string) []string {
	return []string{"remove-trusted-cert", "-d", certFile}
}

// DeleteCertArgs deletes the certificate with SHA-1 sha1Hex from the System keychain, with its user
// trust settings. See security(1), delete-certificate.
func DeleteCertArgs(sha1Hex string) []string {
	return []string{"delete-certificate", "-t", "-Z", sha1Hex, MacSystemKeychain}
}

// certNotFound is security's exit for a certificate no keychain holds: the CA is already gone.
func certNotFound(err error) bool {
	var exit *exec.ExitError
	return errors.As(err, &exit) && strings.Contains(strings.ToLower(err.Error()), "could not be found")
}
