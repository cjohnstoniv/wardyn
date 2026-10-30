// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package runner

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func realTemp(t *testing.T) string {
	t.Helper()
	d, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func mkdirs(t *testing.T, paths ...string) {
	t.Helper()
	for _, p := range paths {
		if err := os.MkdirAll(p, 0o700); err != nil {
			t.Fatal(err)
		}
	}
}

// A share overlaps a protected path in either direction, name by name and case-folded; a sibling
// that only shares a prefix is not an overlap.
func TestMacProtectedOverlap(t *testing.T) {
	home := realTemp(t)
	lib := filepath.Join(home, "Library")
	mkdirs(t, lib)
	protected := []string{"/System", lib}
	for _, c := range []struct {
		real string
		ok   bool
	}{
		{lib, false},
		{filepath.Join(lib, "Keychains"), false},
		{home, false},                                // contains the login's Library
		{filepath.Dir(home), false},                  // so does every ancestor
		{filepath.Join(home, "library", "x"), false}, // case alias
		{"/SYSTEM/Library", false},
		{filepath.Join(home, "Library2"), true},
		{"/Systemx", true},
		{filepath.Join(home, "proj"), true},
	} {
		if err := macProtectedOverlap(c.real, protected); (err == nil) != c.ok {
			t.Errorf("%s: err = %v, want ok %v", c.real, err, c.ok)
		}
	}
}

// A protected path that does not exist yet, behind a linked parent, compares as where it will be.
func TestMacProtectedOverlapMissingRootBehindALink(t *testing.T) {
	root := realTemp(t)
	actual := filepath.Join(root, "actual")
	mkdirs(t, actual)
	if err := os.Symlink(actual, filepath.Join(root, "alias")); err != nil {
		t.Fatal(err)
	}
	protected := []string{filepath.Join(root, "alias", "new-vms")}
	if err := macProtectedOverlap(filepath.Join(actual, "new-vms", "run"), protected); err == nil {
		t.Fatal("a share under the not-yet-made VM root was accepted")
	}
	mkdirs(t, filepath.Join(actual, "new-vms"))
	if err := macProtectedOverlap(filepath.Join(actual, "new-vms"), protected); err == nil {
		t.Fatal("the VM root made after startup was accepted")
	}
	if err := macProtectedOverlap(filepath.Join(actual, "other"), protected); err != nil {
		t.Fatalf("a sibling of the VM root: %v", err)
	}
}

func TestCheckMacShareSource(t *testing.T) {
	home := realTemp(t)
	roots, wroots := filepath.Join(home, "roots"), filepath.Join(home, "roots", "w")
	readOnly := filepath.Join(roots, "d")
	mkdirs(t, readOnly, wroots, filepath.Join(home, "elsewhere"))
	writable := &UserWritableCeiling{Roots: []string{wroots}}
	protected := MacProtectedPaths(home, filepath.Join(home, "vms"), filepath.Join(home, "ledger"))

	if err := CheckMacShareSource(readOnly, true, []string{roots}, nil, protected); err != nil {
		t.Fatalf("a member folder in its roots, read-only: %v", err)
	}
	if err := CheckMacShareSource(wroots, true, []string{roots}, writable, protected); err != nil {
		t.Fatalf("a member folder in the writable roots: %v", err)
	}
	if err := CheckMacShareSource(readOnly, true, []string{roots}, writable, protected); err == nil {
		t.Fatal("a writable share outside the writable roots was accepted")
	}
	if err := CheckMacShareSource(filepath.Join(home, "elsewhere"), true, []string{roots}, nil, protected); err == nil {
		t.Fatal("a member folder outside its roots was accepted")
	}
	if err := CheckMacShareSource(home, false, nil, nil, protected); err == nil {
		t.Fatal("an operator share of the whole home was accepted")
	}
	link := filepath.Join(roots, "link")
	if err := os.Symlink(wroots, link); err != nil {
		t.Fatal(err)
	}
	if err := CheckMacShareSource(link, true, []string{roots}, nil, protected); err == nil {
		t.Fatal("a path that is not its own resolution was accepted")
	}
}

// D, readable but not writable, is swapped for a link to the writable W while the member rule runs,
// to be restored before the last resolution: the member rule decided about W, not D, so D is refused
// there, and the restore never matters.
func TestCheckMacShareSourceRefusesASwapAndRestore(t *testing.T) {
	home := realTemp(t)
	roots, wroots := filepath.Join(home, "roots"), filepath.Join(home, "roots", "w")
	d := filepath.Join(roots, "d")
	mkdirs(t, d, wroots)
	writable := &UserWritableCeiling{Roots: []string{wroots}}
	swap := func(to string) {
		if err := os.Remove(d); err != nil {
			t.Fatal(err)
		}
		if to == "" {
			mkdirs(t, d)
		} else if err := os.Symlink(to, d); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() { macShareStep = func(string) {} })
	macShareStep = func(step string) {
		switch step {
		case "member":
			swap(wroots)
		case "protected":
			swap("")
		}
	}
	err := CheckMacShareSource(d, true, []string{roots}, writable, nil)
	if err == nil || !strings.Contains(err.Error(), "changed while it was being checked") {
		t.Fatalf("a swap and restore around the member rule: %v", err)
	}
}
