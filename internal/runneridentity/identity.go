// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Package runneridentity holds the local runner key and its organisation binding.
package runneridentity

import (
	"bytes"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/federation"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
)

// Identity never serializes its private key or a person's identity.
type Identity struct {
	RunnerID     uuid.UUID          `json:"runner_id"`
	PrivateKey   ed25519.PrivateKey `json:"-"`
	Fingerprint  string             `json:"key_fingerprint"`
	OrgURLSHA256 string             `json:"org_url_sha256"`
}

// DefaultDir is shared by runnerd and the owner's local claim command.
func DefaultDir() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("runner state directory: %w", err)
	}
	return filepath.Join(dir, "wardyn", "runner"), nil
}

// Load validates the organisation before returning a key that could sign a connection.
func Load(stateDir, orgURL string) (Identity, error) {
	if err := federation.CheckOrgURL(orgURL); err != nil {
		return Identity{}, err
	}
	root, err := os.OpenRoot(stateDir)
	if err != nil {
		return Identity{}, fmt.Errorf("open runner state: %w", err)
	}
	defer root.Close()
	meta, err := privateFile(root, "runner.json", 4096)
	if err != nil {
		return Identity{}, err
	}
	var id Identity
	dec := json.NewDecoder(bytes.NewReader(meta))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&id); err != nil {
		return Identity{}, fmt.Errorf("decode runner identity: %w", err)
	}
	if dec.Decode(new(any)) != io.EOF {
		return Identity{}, errors.New("runner identity contains trailing data")
	}
	if id.RunnerID == uuid.Nil || id.OrgURLSHA256 != federation.OrgURLSHA256(orgURL) {
		return Identity{}, errors.New("runner identity is unregistered or belongs to another organisation URL")
	}
	key, err := privateFile(root, "runner.key", ed25519.PrivateKeySize)
	if err != nil {
		return Identity{}, err
	}
	if len(key) != ed25519.PrivateKeySize {
		return Identity{}, errors.New("runner key is not an Ed25519 private key")
	}
	canonical := ed25519.NewKeyFromSeed(key[:ed25519.SeedSize])
	if !bytes.Equal(key, canonical) || id.Fingerprint != runnerwire.Fingerprint(canonical.Public().(ed25519.PublicKey)) {
		return Identity{}, errors.New("runner key does not match its fingerprint")
	}
	id.PrivateKey = canonical
	return id, nil
}

func privateFile(root *os.Root, name string, limit int64) ([]byte, error) {
	before, err := root.Lstat(name)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", name, err)
	}
	if !before.Mode().IsRegular() || before.Mode().Perm() != 0600 {
		return nil, fmt.Errorf("%s must be a regular file with mode 0600", name)
	}
	f, err := root.Open(name)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", name, err)
	}
	defer f.Close()
	after, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("stat %s: %w", name, err)
	}
	if !os.SameFile(before, after) || !after.Mode().IsRegular() || after.Mode().Perm() != 0600 {
		return nil, fmt.Errorf("%s changed while opening it", name)
	}
	b, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", name, err)
	}
	if int64(len(b)) > limit {
		return nil, fmt.Errorf("%s exceeds its size limit", name)
	}
	return b, nil
}
