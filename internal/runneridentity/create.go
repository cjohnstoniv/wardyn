// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package runneridentity

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"os"
)

// Generate persists a new key before it can be submitted for registration.
// Existing keys are never overwritten, including identities a server revoked.
func Generate(stateDir string) (ed25519.PrivateKey, error) {
	if err := os.MkdirAll(stateDir, 0700); err != nil {
		return nil, err
	}
	info, err := os.Lstat(stateDir)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() || info.Mode().Perm()&0077 != 0 {
		return nil, errors.New("runner state must be a private directory with mode 0700")
	}
	root, err := os.OpenRoot(stateDir)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	if err := writeNewPrivate(root, "runner.key", key); err != nil {
		return nil, err
	}
	return key, nil
}

// Save records the server's registration while preserving the generated key.
func Save(stateDir string, id Identity) error {
	root, err := os.OpenRoot(stateDir)
	if err != nil {
		return err
	}
	defer root.Close()
	key, err := privateFile(root, "runner.key", ed25519.PrivateKeySize)
	if err != nil {
		return err
	}
	if !ed25519.PrivateKey(key).Equal(id.PrivateKey) {
		return errors.New("runner key changed during registration")
	}
	b, err := json.Marshal(id)
	if err != nil {
		return err
	}
	return writeNewPrivate(root, "runner.json", b)
}

func writeNewPrivate(root *os.Root, name string, data []byte) error {
	f, err := root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return fmt.Errorf("create %s (existing identities are never overwritten): %w", name, err)
	}
	defer f.Close()
	if _, err := f.Write(data); err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		return err
	}
	return f.Close()
}
