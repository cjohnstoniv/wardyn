// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package devicecert

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
)

const keyFile = "runner.key"

// FingerprintFromStateDir is the fingerprint of the runner key in dir: the lower-case hex SHA-256 of the
// public key, as runnerwire.Fingerprint prints it. It reads runner.key under the checks the runner makes
// when it loads its own identity: a regular file of mode 0600, the same file before and after it is
// opened, exactly one private key long, and canonical (a key whose last 32 bytes are not the public key
// its seed makes is refused, so a file cannot be made to name another runner's key). The key bytes never
// leave this function.
func FingerprintFromStateDir(dir string) (string, error) {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return "", fmt.Errorf("open the state directory: %w", err)
	}
	defer root.Close()
	before, err := root.Lstat(keyFile)
	if err != nil {
		return "", fmt.Errorf("read %s: %w", keyFile, err)
	}
	if !before.Mode().IsRegular() || before.Mode().Perm() != 0600 {
		return "", fmt.Errorf("%s must be a regular file with mode 0600", keyFile)
	}
	f, err := root.Open(keyFile)
	if err != nil {
		return "", fmt.Errorf("open %s: %w", keyFile, err)
	}
	defer f.Close()
	after, err := f.Stat()
	if err != nil {
		return "", fmt.Errorf("stat %s: %w", keyFile, err)
	}
	if !os.SameFile(before, after) || !after.Mode().IsRegular() || after.Mode().Perm() != 0600 {
		return "", fmt.Errorf("%s changed while opening it", keyFile)
	}
	b, err := io.ReadAll(io.LimitReader(f, ed25519.PrivateKeySize+1))
	if err != nil {
		return "", fmt.Errorf("read %s: %w", keyFile, err)
	}
	if len(b) != ed25519.PrivateKeySize {
		return "", errors.New("runner key is not an Ed25519 private key")
	}
	canonical := ed25519.NewKeyFromSeed(b[:ed25519.SeedSize])
	if !bytes.Equal(b, canonical) {
		return "", errors.New("runner key is not a canonical Ed25519 private key")
	}
	sum := sha256.Sum256(canonical.Public().(ed25519.PublicKey))
	return hex.EncodeToString(sum[:]), nil
}
