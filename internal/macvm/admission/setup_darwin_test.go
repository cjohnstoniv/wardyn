// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build darwin

package admission

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

// A real uchg entry on a real volume: the link is refused, setup clears the flag as the file's owner and
// carries the entry. It runs on the hosted macOS job, as a plain user.
func TestSetupClearsARealUserImmutableFlag(t *testing.T) {
	r := newRig(t, bigMac())
	names := admitN(t, r, 2)
	entry := filepath.Join(r.dir, names[0])
	fstat := drift(t, r, "03777")
	if err := unix.Chflags(entry, unix.UF_IMMUTABLE); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = filepath.WalkDir(filepath.Dir(filepath.Dir(r.dir)), func(p string, _ fs.DirEntry, _ error) error {
			_ = unix.Chflags(p, 0)
			return nil
		})
	})
	if err := unix.Link(entry, entry+".probe"); !errors.Is(err, unix.EPERM) {
		t.Fatalf("premise: this volume linked a uchg file: %v", err)
	}
	if err := Setup(r.setupCfg(func(c *SetupConfig) { c.Fstat = fstat })); err != nil {
		t.Fatalf("Setup: %v", err)
	}
	var st unix.Stat_t
	if err := unix.Lstat(entry, &st); err != nil {
		t.Fatal(err)
	}
	if st.Flags&unix.UF_IMMUTABLE != 0 {
		t.Fatalf("the carried entry still has uchg: flags %#x", st.Flags)
	}
	if n := counted(t, r); n != 2 {
		t.Fatalf("a helper counts %d of 2 entries", n)
	}
}

func TestFlagConstantsAreDarwins(t *testing.T) {
	if flagUserImmutable != unix.UF_IMMUTABLE || flagSystemImmutable != unix.SF_IMMUTABLE {
		t.Fatalf("flag constants %#x/%#x, want %#x/%#x", flagUserImmutable, flagSystemImmutable, unix.UF_IMMUTABLE, unix.SF_IMMUTABLE)
	}
	p := filepath.Join(t.TempDir(), "f")
	if err := os.WriteFile(p, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := unix.Chflags(p, unix.UF_IMMUTABLE); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = unix.Chflags(p, 0) })
	var st unix.Stat_t
	if err := unix.Lstat(p, &st); err != nil {
		t.Fatal(err)
	}
	if statFlags(st)&flagUserImmutable == 0 {
		t.Fatalf("statFlags = %#x, want uchg", statFlags(st))
	}
}
