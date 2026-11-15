// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build linux || darwin

package foldersync

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// A failed copy someone rewrote in place to its own length, through its name, is the same file with the same size
// but not the bytes the copy wrote: it is never removed. Where the volume shows it as the copy it is moved aside and
// kept, and the refusal names the aside; where it does not, it stays at the name and the refusal names that.
// Catches a failed copy removed on its file number and size alone.
func TestPass_AFailedCopyRewrittenInPlaceToItsLengthIsKept(t *testing.T) {
	sy, root, name := copyCase(t, false)
	sync := fsyncFile
	onSync(t, func(f *os.File) error {
		if filepath.Base(f.Name()) != name {
			return sync(f)
		}
		g, err := os.OpenFile(filepath.Join(root, name), os.O_WRONLY, 0)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := g.WriteAt([]byte("edited in place!"), 0); err != nil { // the copy's own 16 bytes, rewritten
			t.Fatal(err)
		}
		if err := g.Close(); err != nil {
			t.Fatal(err)
		}
		return syscall.EIO
	}, nil)
	reason := refusedFor(pass(t, sy), name)
	temps, _ := filepath.Glob(filepath.Join(root, ".wardyn-sync-*"))
	atName, nameErr := os.ReadFile(filepath.Join(root, name))
	switch {
	case len(temps) == 1:
		if got := getSyncFile(t, temps[0]); got != "edited in place!" || nameErr == nil ||
			!strings.Contains(reason, "it is kept as") || !strings.Contains(reason, filepath.Base(temps[0])) {
			t.Errorf("aside %q, laptop %s %q (%v), refusal %q: want the rewritten copy kept at the aside the refusal names", got, name, atName, nameErr, reason)
		}
	case len(temps) == 0:
		if nameErr != nil || string(atName) != "edited in place!" || !strings.Contains(reason, "its partial copy is left at "+name) {
			t.Errorf("laptop %s %q (%v), refusal %q: want the rewritten copy kept at its name and the refusal naming it", name, atName, nameErr, reason)
		}
	default:
		t.Errorf("temps %v: want at most one", temps)
	}
}

// A failed copy of an empty file is never removed: its size proves nothing, and a filesystem may give every empty
// file the same number, so an empty save at the name would pass for it. It stays at the name and the refusal names
// it. Catches an empty failed copy removed on its number and size alone.
func TestPass_AFailedEmptyCopyIsNamedNotRemoved(t *testing.T) {
	sy, box, root := runnerSyncer(t, true)
	putSyncFile(t, filepath.Join(box, "new.txt"), "", 0o644)
	onLink(t, syscall.EXDEV)
	failCopySync(t, "new.txt")
	reason := refusedFor(pass(t, sy), "new.txt")
	temps, _ := filepath.Glob(filepath.Join(root, ".wardyn-sync-*"))
	if got := getSyncFile(t, filepath.Join(root, "new.txt")); got != "" || len(temps) != 0 ||
		!strings.Contains(reason, "its partial copy is left at new.txt") {
		t.Errorf("laptop new.txt = %q, temps %v, refusal %q: want the empty copy kept at its name, no temp and the refusal naming it", got, temps, reason)
	}
}
