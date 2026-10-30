// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package guestagent

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"os"
	"os/exec"
	"slices"
	"strings"
	"testing"
	"time"
)

func testCAPEM(t *testing.T, cn string) string {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: cn}, IsCA: true,
		BasicConstraintsValid: true, NotBefore: time.Now(), NotAfter: time.Now().Add(time.Hour)}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
}

// exitError is a real *exec.ExitError, as security's non-zero exit gives one.
func exitError(t *testing.T, stderr string) error {
	t.Helper()
	err := exec.Command("false").Run()
	var ee *exec.ExitError
	if !errors.As(err, &ee) {
		t.Fatalf("false did not exit non-zero: %v", err)
	}
	return fmt.Errorf("%s: %w: %s", macSecurity, err, stderr)
}

// The run CA is trusted with exactly one command: security add-trusted-cert in the admin trust
// settings, as a root, for SSL only, in the System keychain, from a file that holds exactly that one
// certificate and is gone afterwards.
func TestTrustRunCAArgvAndTheOneCertificate(t *testing.T) {
	ca := testCAPEM(t, "run proxy CA")
	dir := t.TempDir()
	var file, held string
	r := &fakeRunner{out: func(argv []string) ([]byte, error) {
		file = argv[len(argv)-1]
		b, _ := os.ReadFile(file)
		held = string(b)
		return nil, nil
	}}
	status, reason := MacKeychain{Run: r, Dir: dir}.TrustRunCA(context.Background(), ca)
	if status != KeychainTrusted || reason != "" {
		t.Fatalf("TrustRunCA = %q %q", status, reason)
	}
	want := []string{"/usr/bin/security", "add-trusted-cert", "-d", "-r", "trustRoot", "-p", "ssl", "-k", "/Library/Keychains/System.keychain", file}
	if len(r.calls) != 1 || !slices.Equal(r.calls[0], want) {
		t.Fatalf("calls = %q, want one %q", r.calls, want)
	}
	if held != ca {
		t.Errorf("the certificate file held %q, want the run CA alone", held)
	}
	if !strings.HasPrefix(file, dir+"/") {
		t.Errorf("the certificate file %s is not in the agent's directory", file)
	}
	if _, err := os.Stat(file); !os.IsNotExist(err) {
		t.Errorf("the certificate file is left behind: %v", err)
	}
	// Stage 2's tools mode, which changes the base, cannot run it.
	if err := ToolsExecAllowed(want, "/usr/local/bin"); err == nil {
		t.Error("tools mode allows the keychain trust, so a base could be changed")
	}
}

// Anything but exactly one certificate is refused before security runs.
func TestTrustRunCARefusesAnythingButOneCertificate(t *testing.T) {
	one := testCAPEM(t, "a")
	for name, in := range map[string]string{
		"empty":         "",
		"two":           one + testCAPEM(t, "b"),
		"not pem":       "hello",
		"a key":         "-----BEGIN PRIVATE KEY-----\nAAAA\n-----END PRIVATE KEY-----\n",
		"not der":       string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: []byte("x")})),
		"trailing junk": one + "junk",
	} {
		r := &fakeRunner{}
		status, reason := MacKeychain{Run: r, Dir: t.TempDir()}.TrustRunCA(context.Background(), in)
		if status != KeychainNotTrusted || reason != KeychainUnavailable || len(r.calls) != 0 {
			t.Errorf("%s: TrustRunCA = %q %q after %d calls", name, status, reason, len(r.calls))
		}
	}
}

// How security ends maps to a fixed code; its output never comes back.
func TestTrustRunCAMapsTheExitToACode(t *testing.T) {
	ca := testCAPEM(t, "c")
	for _, tc := range []struct {
		name   string
		err    error
		reason string
	}{
		{"authorization denied", exitError(t, "SecTrustSettingsSetTrustSettings: The authorization was denied since no user interaction was possible."), KeychainRefused},
		{"other exit", exitError(t, "SecCertificateAddToKeychain: some other error"), KeychainFailed},
		{"did not run", errors.New("fork/exec /usr/bin/security: no such file or directory"), KeychainUnavailable},
	} {
		r := &fakeRunner{out: func([]string) ([]byte, error) { return nil, tc.err }}
		status, reason := MacKeychain{Run: r, Dir: t.TempDir()}.TrustRunCA(context.Background(), ca)
		if status != KeychainNotTrusted || reason != tc.reason {
			t.Errorf("%s: TrustRunCA = %q %q, want %q", tc.name, status, reason, tc.reason)
		}
	}
}
