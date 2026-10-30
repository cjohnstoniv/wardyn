// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package guestagent

import (
	"bytes"
	"context"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"os"
	"os/exec"
	"strings"
)

// MacSystemKeychain is a macOS guest's System keychain. Init adds the run's proxy CA to it in the
// run's clone, which is deleted when the run ends; the base it was cloned from is never changed.
const MacSystemKeychain = "/Library/Keychains/System.keychain"

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
// reached; Dir holds the certificate's file while the command reads it.
type MacKeychain struct {
	Run CmdRunner
	Dir string
}

// TrustRunCA trusts the one certificate in runCA and answers a status and a reason code. It never
// fails Init: a CA that is not trusted leaves the run going on its CA bundle alone.
func (k MacKeychain) TrustRunCA(ctx context.Context, runCA string) (status, reason string) {
	block, rest := pem.Decode([]byte(runCA))
	if block == nil || block.Type != "CERTIFICATE" || len(bytes.TrimSpace(rest)) != 0 {
		return KeychainNotTrusted, KeychainUnavailable
	}
	if _, err := x509.ParseCertificate(block.Bytes); err != nil {
		return KeychainNotTrusted, KeychainUnavailable
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
