// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package macvm

import (
	"crypto/rand"
	"errors"
	"os"
	"path/filepath"

	"github.com/cjohnstoniv/wardyn/internal/vm/guestagent"
)

// errNoPairKey: the base (or the clone a boot runs) holds no good pairing key, so nothing can pair with
// its guest. Only a new prepare writes one into both the guest and the base.
var errNoPairKey = errors.New("macvm: this macOS base has no pairing key; prepare it again")

// newPairKey is a fresh pairing key.
func newPairKey() []byte {
	key := make([]byte, guestagent.PairKeyLen)
	_, _ = rand.Read(key) // crypto/rand.Read does not return an error: it crashes the program instead
	return key
}

// readPairKey reads the pairing key in dir: a regular file (not a link), mode 0600, exactly
// guestagent.PairKeyLen bytes. Anything else is no key. Every boot reads its key this way.
func readPairKey(dir string) ([]byte, error) {
	p := filepath.Join(dir, PairKeyFile)
	fi, err := os.Lstat(p)
	if err != nil || !fi.Mode().IsRegular() || fi.Mode().Perm() != 0o600 || fi.Size() != guestagent.PairKeyLen {
		return nil, errNoPairKey
	}
	key, err := os.ReadFile(p)
	if err != nil || len(key) != guestagent.PairKeyLen {
		return nil, errNoPairKey
	}
	return key, nil
}

// writePairKey writes key into dir at 0600, through a temp file and a rename.
func writePairKey(dir string, key []byte) error {
	tmp, err := os.CreateTemp(dir, "."+PairKeyFile+".*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(key); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), filepath.Join(dir, PairKeyFile))
}

// copyPairKey gives dst (a run's or a staging copy's clone of src) src's key, the one its guest disk holds.
func copyPairKey(src, dst string) error {
	key, err := readPairKey(src)
	if err != nil {
		return err
	}
	defer clear(key)
	return writePairKey(dst, key)
}
