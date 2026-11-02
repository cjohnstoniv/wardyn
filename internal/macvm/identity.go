// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package macvm

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// IdentityStrategy picks a run's machine identifier. Both are built and kept
// until a real Mac shows which ones boot and give two concurrent guests
// distinct identities:
//
//   - travel: the base's identifier is copied with its disk and aux storage;
//   - fresh: each run gets a new identifier.
//
// The per-slot-base fallback (one base per licence slot, each with the
// identifier of its own install) is travel with the run's base_dir set to its
// ledger slot's base (Layout.BaseDir(slot)).
type IdentityStrategy string

const (
	IdentityTravel IdentityStrategy = "travel"
	IdentityFresh  IdentityStrategy = "fresh"
)

// Valid reports whether s is one of the strategies.
func (s IdentityStrategy) Valid() bool { return s == IdentityTravel || s == IdentityFresh }

// HostCapFor is the host cap (for the admission ledger) a strategy allows.
// fresh configures every VM with its own identifier, so 2. travel configures
// two concurrent VMs from one base with one identifier, so 1 until a real Mac
// proves the framework replaces it; the helper's read-back comes from its own
// configuration object and cannot show that. The per-slot-base fallback
// (travel from slotBases ready bases, each its own install) allows one VM per
// base, up to 2.
func HostCapFor(s IdentityStrategy, slotBases int) int {
	if s == IdentityFresh {
		return 2
	}
	return min(max(slotBases, 1), 2)
}

// Identity is the identifier a clone was configured with, recorded so the
// helper's status can report exactly what each run's VM was given.
type Identity struct {
	Strategy          IdentityStrategy
	MachineIdentifier []byte
}

// Encoded is the identifier as the status reports it (standard base64 of its
// opaque data representation).
func (id Identity) Encoded() string { return base64.StdEncoding.EncodeToString(id.MachineIdentifier) }

// IDHash is the hex SHA-256 of an identifier's data representation, the form
// the helper logs and reports for every VM it starts.
func IDHash(id []byte) string {
	h := sha256.Sum256(id)
	return hex.EncodeToString(h[:])
}

// CloneRun makes a run's disk and aux storage as copy-on-write clones of the
// base's, and gives it a machine identifier by strategy. The base is only
// ever read. A run dir that already holds a disk is refused.
//
// It holds the base's lock shared while it reads, so a tools update's swap
// (which holds it exclusively) is never half seen.
func CloneRun(e Engine, baseDir, runDir string, s IdentityStrategy) (Identity, error) {
	unlock, err := lockFile(context.Background(), baseLockPath(baseDir), false)
	if err != nil {
		return Identity{}, err
	}
	defer unlock()
	return cloneRun(e, baseDir, runDir, s)
}

// cloneReadyBase reads the base's manifest and clones the base under one hold
// of its lock, so both come from the same generation.
func cloneReadyBase(e Engine, baseDir, runDir string, s IdentityStrategy) (Manifest, Identity, error) {
	unlock, err := lockFile(context.Background(), baseLockPath(baseDir), false)
	if err != nil {
		return Manifest{}, Identity{}, err
	}
	defer unlock()
	m, err := ReadyBase(baseDir)
	if err != nil {
		return m, Identity{}, err
	}
	id, err := cloneRun(e, baseDir, runDir, s)
	return m, id, err
}

// cloneRun is CloneRun for a caller that already holds the base's lock.
func cloneRun(e Engine, baseDir, runDir string, s IdentityStrategy) (Identity, error) {
	if !s.Valid() {
		return Identity{}, fmt.Errorf("macvm: unknown identity strategy %q", s)
	}
	// No key, no clone: nothing could pair with its guest.
	if _, err := readPairKey(baseDir); err != nil {
		return Identity{}, err
	}
	if err := mkdirPrivate(runDir); err != nil {
		return Identity{}, err
	}
	if err := copyPairKey(baseDir, runDir); err != nil {
		return Identity{}, err
	}
	for _, f := range []string{DiskFile, AuxFile} {
		dst := filepath.Join(runDir, f)
		if _, err := os.Lstat(dst); err == nil {
			return Identity{}, fmt.Errorf("macvm: %s already exists; a run dir is cloned once", dst)
		}
		if err := cloneFile(filepath.Join(baseDir, f), dst); err != nil {
			return Identity{}, fmt.Errorf("macvm: clone %s: %w", f, err)
		}
	}
	var id []byte
	switch s {
	case IdentityTravel:
		b, err := os.ReadFile(filepath.Join(baseDir, MachineIDFile))
		if err != nil {
			return Identity{}, fmt.Errorf("macvm: base machine identifier: %w", err)
		}
		id = b
	case IdentityFresh:
		b, err := e.NewMachineIdentifier()
		if err != nil {
			return Identity{}, fmt.Errorf("macvm: new machine identifier: %w", err)
		}
		id = b
	}
	if len(id) == 0 {
		return Identity{}, errors.New("macvm: empty machine identifier")
	}
	if err := os.WriteFile(filepath.Join(runDir, MachineIDFile), id, 0o600); err != nil {
		return Identity{}, err
	}
	return Identity{Strategy: s, MachineIdentifier: id}, nil
}
