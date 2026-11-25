// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package devicecert

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
)

func writeKey(t *testing.T, dir string, b []byte, mode os.FileMode) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "runner.key"), b, mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(dir, "runner.key"), mode); err != nil {
		t.Fatal(err)
	}
}

func TestFingerprintFromStateDir(t *testing.T) {
	t.Run("reads the runner key", testFingerprintReadsTheRunnerKey)
	t.Run("refuses what the runner would refuse", testFingerprintRefuses)
}

func testFingerprintReadsTheRunnerKey(t *testing.T) {
	pub, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	writeKey(t, dir, key, 0600)
	sum := sha256.Sum256(pub)
	got, err := FingerprintFromStateDir(dir)
	if err != nil || got != hex.EncodeToString(sum[:]) {
		t.Fatalf("fingerprint = %q, %v; want %x", got, err, sum)
	}
}

func testFingerprintRefuses(t *testing.T) {
	victim, _, _ := ed25519.GenerateKey(rand.Reader)
	_, own, _ := ed25519.GenerateKey(rand.Reader)
	forged := append(append([]byte{}, own[:ed25519.SeedSize]...), victim...)
	_, good, _ := ed25519.GenerateKey(rand.Reader)
	for name, c := range map[string]struct {
		key  []byte
		mode os.FileMode
	}{
		"a 64-byte file whose tail is not its seed's public key": {forged, 0600},
		"a short file":          {good[:32], 0600},
		"a long file":           {append(append([]byte{}, good...), 0), 0600},
		"a group-readable file": {good, 0640},
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			writeKey(t, dir, c.key, c.mode)
			if fp, err := FingerprintFromStateDir(dir); err == nil {
				t.Fatalf("fingerprint = %q, want a refusal", fp)
			}
		})
	}
	t.Run("no key", func(t *testing.T) {
		if _, err := FingerprintFromStateDir(t.TempDir()); err == nil {
			t.Fatal("want a refusal")
		}
	})
	t.Run("a link", func(t *testing.T) {
		dir := t.TempDir()
		real := filepath.Join(t.TempDir(), "k")
		if err := os.WriteFile(real, good, 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(real, filepath.Join(dir, "runner.key")); err != nil {
			t.Fatal(err)
		}
		if _, err := FingerprintFromStateDir(dir); err == nil {
			t.Fatal("want a refusal")
		}
	})
}
