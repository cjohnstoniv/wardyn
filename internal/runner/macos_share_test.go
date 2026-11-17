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

// A container bind may neither be, sit under nor contain the runner's macOS VM directory, whatever the
// spelling: a parent, an upper-case name, a link to it or to its parent, and a VM directory that is itself a
// link to another volume. A sibling stays allowed, and so does everything when there is no VM directory.
func TestCheckBindOutsideMacVMDir(t *testing.T) {
	home := realTemp(t)
	support := filepath.Join(home, "Library", "Application Support", "Wardyn")
	vm := filepath.Join(support, "macos-vm.noindex")
	mkdirs(t, filepath.Join(vm, "runs", "x"), filepath.Join(support, "other"), filepath.Join(home, "proj"))
	elsewhere := realTemp(t)
	mkdirs(t, filepath.Join(elsewhere, "vms", "runs"), filepath.Join(elsewhere, "docs"))
	linked := filepath.Join(home, "linked-vm")
	mkdirs(t, linked)
	for name, target := range map[string]string{
		filepath.Join(home, "to-vm"):      vm,
		filepath.Join(home, "to-support"): support,
		filepath.Join(home, "to-home"):    home,
		filepath.Join(linked, "vm"):       filepath.Join(elsewhere, "vms"),
	} {
		if err := os.Symlink(target, name); err != nil {
			t.Fatal(err)
		}
	}
	for _, c := range []struct {
		name, src string
		ok        bool
	}{
		{"the VM directory", vm, false},
		{"a file under it", filepath.Join(vm, "runs", "x", "disk.img"), false},
		{"its parent", support, false},
		{"the home", home, false},
		{"upper case", filepath.Join(home, "LIBRARY", "APPLICATION SUPPORT", "WARDYN", "MACOS-VM.NOINDEX"), false},
		{"a link to it", filepath.Join(home, "to-vm"), false},
		{"a link to its parent", filepath.Join(home, "to-support"), false},
		{"a link to the home", filepath.Join(home, "to-home"), false},
		{"a sibling", filepath.Join(support, "other"), true},
		{"another folder of the home", filepath.Join(home, "proj"), true},
		{"a prefix sibling", vm + "2", true},
	} {
		if err := CheckBindOutsideMacVMDir(c.src, vm); (err == nil) != c.ok {
			t.Errorf("%s (%s): err = %v, want ok %v", c.name, c.src, err, c.ok)
		}
	}
	// The VM directory is a link to another volume: the real location is held, and a sibling there is not.
	for name, src := range map[string]string{
		"the real location":    filepath.Join(elsewhere, "vms"),
		"under the real place": filepath.Join(elsewhere, "vms", "runs"),
		"the volume above it":  elsewhere,
	} {
		if err := CheckBindOutsideMacVMDir(src, filepath.Join(linked, "vm")); err == nil {
			t.Errorf("linked VM directory, %s: bind accepted", name)
		}
	}
	if err := CheckBindOutsideMacVMDir(filepath.Join(elsewhere, "docs"), filepath.Join(linked, "vm")); err != nil {
		t.Errorf("linked VM directory, a sibling on its volume: %v", err)
	}
	// No VM directory on this runner: nothing is refused for it.
	for _, vmDir := range []string{"", filepath.Join(home, "no-such", "macos-vm.noindex")} {
		if err := CheckBindOutsideMacVMDir(home, vmDir); err != nil {
			t.Errorf("VM directory %q does not exist: %v", vmDir, err)
		}
	}
}

func TestCheckMacShareSource(t *testing.T) {
	home := realTemp(t)
	roots, wroots := filepath.Join(home, "roots"), filepath.Join(home, "roots", "w")
	readOnly := filepath.Join(roots, "d")
	mkdirs(t, readOnly, wroots, filepath.Join(home, "elsewhere"))
	writable := &UserWritableCeiling{Roots: []string{wroots}}
	protected := MacProtectedPaths(home, filepath.Join(home, "vms"), filepath.Join(home, "ledger"))

	if err := CheckMacShareSource(readOnly, true, true, []string{roots}, nil, nil, protected); err != nil {
		t.Fatalf("a member folder in its roots, read-only: %v", err)
	}
	if err := CheckMacShareSource(wroots, true, false, []string{roots}, nil, writable, protected); err != nil {
		t.Fatalf("a member folder in the writable roots: %v", err)
	}
	if err := CheckMacShareSource(readOnly, true, false, []string{roots}, nil, writable, protected); err == nil {
		t.Fatal("a writable share outside the writable roots was accepted")
	}
	if err := CheckMacShareSource(filepath.Join(home, "elsewhere"), true, true, []string{roots}, nil, nil, protected); err == nil {
		t.Fatal("a member folder outside its roots was accepted")
	}
	if err := CheckMacShareSource(home, false, true, nil, nil, nil, protected); err == nil {
		t.Fatal("an operator share of the whole home was accepted")
	}
	link := filepath.Join(roots, "link")
	if err := os.Symlink(wroots, link); err != nil {
		t.Fatal(err)
	}
	if err := CheckMacShareSource(link, true, true, []string{roots}, nil, nil, protected); err == nil {
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
	err := CheckMacShareSource(d, true, false, []string{roots}, nil, writable, nil)
	if err == nil || !strings.Contains(err.Error(), "changed while it was being checked") {
		t.Fatalf("a swap and restore around the member rule: %v", err)
	}
}

// A member share on a runner is also held to the runner owner's own roots (--drive-root): none
// refuses, and a folder in the org's roots but outside the owner's is refused. No own roots (nil)
// is wardynd's host, checked as before.
func TestCheckMacShareSource_MemberHeldToOwnRoots(t *testing.T) {
	home := realTemp(t)
	roots := filepath.Join(home, "roots")
	own := filepath.Join(roots, "own")
	inOwn, outOwn := filepath.Join(own, "d"), filepath.Join(roots, "d")
	mkdirs(t, inOwn, outOwn)
	protected := MacProtectedPaths(home, filepath.Join(home, "vms"))
	writable := &UserWritableCeiling{Roots: []string{roots}}
	for _, tc := range []struct {
		name     string
		real     string
		readOnly bool
		ownRoots []string
		refused  bool
	}{
		{"inside the owner's roots", inOwn, true, []string{own}, false},
		{"inside the owner's roots, writable", inOwn, false, []string{own}, false},
		{"a runner with no own roots", inOwn, true, []string{}, true},
		{"inside the org's roots, outside the owner's", outOwn, true, []string{own}, true},
		{"writable, outside the owner's roots", outOwn, false, []string{own}, true},
		{"wardynd's host", outOwn, true, nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := CheckMacShareSource(tc.real, true, tc.readOnly, []string{roots}, tc.ownRoots, writable, protected)
			if tc.refused != (err != nil) {
				t.Fatalf("CheckMacShareSource(%s, own=%v) = %v, want refused=%v", tc.real, tc.ownRoots, err, tc.refused)
			}
		})
	}
}

// A writable member share with no writable ceiling is refused: an absent ceiling never means
// "anything goes". A read-only member share and an operator share need none.
func TestCheckMacShareSource_MemberWritableNeedsCeiling(t *testing.T) {
	home := realTemp(t)
	roots := filepath.Join(home, "roots")
	d := filepath.Join(roots, "d")
	mkdirs(t, d)
	protected := MacProtectedPaths(home, filepath.Join(home, "vms"))
	err := CheckMacShareSource(d, true, false, []string{roots}, nil, nil, protected)
	if err == nil || !strings.Contains(err.Error(), "writable") {
		t.Fatalf("a writable member share with no ceiling = %v, want the writable refusal", err)
	}
	if err := CheckMacShareSource(d, true, true, []string{roots}, nil, nil, protected); err != nil {
		t.Fatalf("a read-only member share with no ceiling = %v, want allowed", err)
	}
	if err := CheckMacShareSource(d, true, false, []string{roots}, nil, &UserWritableCeiling{Roots: []string{roots}}, protected); err != nil {
		t.Fatalf("a writable member share inside the ceiling = %v, want allowed", err)
	}
	if err := CheckMacShareSource(d, false, false, nil, nil, nil, protected); err != nil {
		t.Fatalf("a writable operator share = %v, want allowed", err)
	}
}
