// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package runner

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/foldersync"
)

// A scan no member asked for (the library door) of a folder inside a member root: the folder, or a
// folder above it, standing as a link to a folder outside the roots that hold it is refused with the
// member refusal and nothing outside is opened, also where the link leads into another person's own
// root. A plain folder there is opened, and a folder outside every member root gets no handle.
func TestOpenSourceInMemberRoots_AFolderInAMemberRootIsOpenedAsAMembersOwn(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink semantics differ on windows")
	}
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	shared, alice, bob, outside := filepath.Join(base, "shared"), filepath.Join(base, "alice"), filepath.Join(base, "bob"), filepath.Join(base, "elsewhere")
	for dir, token := range map[string]string{
		filepath.Join(shared, "app"): "SHARED_TOKEN", filepath.Join(alice, "app"): "ALICE_TOKEN",
		filepath.Join(bob, "app"): "BOB_TOKEN", filepath.Join(outside, "app"): "OUTSIDE_TOKEN",
	} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, ".env.example"), []byte(token+"=\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for link, target := range map[string]string{
		filepath.Join(alice, "out"):    filepath.Join(outside, "app"), // the folder itself
		filepath.Join(alice, "up"):     outside,                       // a folder above it
		filepath.Join(alice, "to-bob"): filepath.Join(bob, "app"),     // into another person's root
	} {
		if err := os.Symlink(target, link); err != nil {
			t.Fatal(err)
		}
	}
	policy := UserMountPolicy{Roots: []string{shared}, RootsByPrincipal: map[string][]string{"alice": {alice}, "bob": {bob}}}

	for name, src := range map[string]string{
		"the folder is a link":           filepath.Join(alice, "out"),
		"a folder above it is a link":    filepath.Join(alice, "up", "app"),
		"a link into another one's root": filepath.Join(alice, "to-bob"),
	} {
		dir, err := policy.OpenSourceInMemberRoots(src, foldersync.OpenFromTop)
		if dir != nil {
			_ = dir.Close()
		}
		if dir != nil || !errors.Is(err, errMemberSourceRefused) || strings.Contains(err.Error(), outside) || strings.Contains(err.Error(), bob) {
			t.Errorf("%s: OpenSourceInMemberRoots = %v, %v; want no handle and the member refusal naming only the roots that hold it", name, dir, err)
		}
	}
	for src, want := range map[string]string{
		filepath.Join(alice, "app"):  "ALICE_TOKEN=\n",
		filepath.Join(shared, "app"): "SHARED_TOKEN=\n",
	} {
		dir, err := policy.OpenSourceInMemberRoots(src, foldersync.OpenFromTop)
		if err != nil {
			t.Fatalf("%s: %v, want the folder opened", src, err)
		}
		f, err := dir.Open(".env.example")
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(f)
		_ = f.Close()
		_ = dir.Close()
		if string(body) != want {
			t.Errorf("%s: read %q through the handle, want %q", src, body, want)
		}
	}
	if dir, err := policy.OpenSourceInMemberRoots(filepath.Join(outside, "app"), foldersync.OpenFromTop); dir != nil || err != nil {
		t.Errorf("a folder outside every member root = %v, %v; want no handle and no error", dir, err)
	}
	if dir, err := (UserMountPolicy{}).OpenSourceInMemberRoots(filepath.Join(alice, "out"), foldersync.OpenFromTop); dir != nil || err != nil {
		t.Errorf("no member roots set = %v, %v; want no handle and no error", dir, err)
	}
}

// One person's own root inside the shared root, the shape the per-person entry exists for: a link from
// it to another folder of the shared root is a folder that person may not mount, so the library door
// refuses it too, though the shared root holds both. So is a link to a credential folder inside the
// root. A plain folder there is opened. A root of "/" admits no member folder and makes no operator
// folder a member's.
func TestOpenSourceInMemberRoots_AFolderMustResolveInsideEveryRootThatHoldsIt(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink semantics differ on windows")
	}
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	shared, outside := filepath.Join(base, "shared"), filepath.Join(base, "elsewhere", "app")
	alice := filepath.Join(shared, "alice")
	for _, dir := range []string{filepath.Join(shared, "team"), filepath.Join(alice, "app"), filepath.Join(alice, ".ssh"), outside} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for link, target := range map[string]string{"to-team": filepath.Join(shared, "team"), "to-keys": filepath.Join(alice, ".ssh")} {
		if err := os.Symlink(target, filepath.Join(alice, link)); err != nil {
			t.Fatal(err)
		}
	}
	policy := UserMountPolicy{Roots: []string{shared}, RootsByPrincipal: map[string][]string{"alice": {alice}}}

	dir, err := policy.OpenSourceInMemberRoots(filepath.Join(alice, "to-team"), foldersync.OpenFromTop)
	if dir != nil {
		_ = dir.Close()
	}
	if dir != nil || !errors.Is(err, errMemberSourceRefused) {
		t.Errorf("a link from one person's root into the shared root around it = %v, %v; want no handle and the member refusal", dir, err)
	}
	if dir, err := policy.OpenSourceInMemberRoots(filepath.Join(alice, "to-keys"), foldersync.OpenFromTop); dir != nil || err == nil {
		t.Errorf("a link to a credential folder inside the root = %v, %v; want no handle and a refusal", dir, err)
	}
	for _, src := range []string{filepath.Join(alice, "app"), filepath.Join(shared, "team")} {
		dir, err := policy.OpenSourceInMemberRoots(src, foldersync.OpenFromTop)
		if err != nil {
			t.Fatalf("%s: %v, want the folder opened", src, err)
		}
		_ = dir.Close()
	}
	for name, top := range map[string]UserMountPolicy{
		"the shared list":  {Roots: []string{"/"}},
		"one person's own": {RootsByPrincipal: map[string][]string{"alice": {"/"}}},
	} {
		if dir, err := top.OpenSourceInMemberRoots(outside, foldersync.OpenFromTop); dir != nil || err != nil {
			t.Errorf("a root of / in %s = %v, %v; want no handle and no error", name, dir, err)
		}
	}
}

// A folder inside a member root, or a folder above it, swapped for a link to a folder outside the
// roots after its check is not opened through at the library door either.
func TestOpenSourceInMemberRoots_ASourceSwappedForALinkAfterItsCheckIsRefused(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink semantics differ on windows")
	}
	for name, swapped := range map[string]string{"the folder": "app", "a folder above it": "."} {
		t.Run(name, func(t *testing.T) {
			base, err := filepath.EvalSymlinks(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			root, outside := filepath.Join(base, "projects"), filepath.Join(base, "elsewhere")
			for _, dir := range []string{filepath.Join(root, "team", "app"), filepath.Join(outside, "app")} {
				if err := os.MkdirAll(dir, 0o755); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.WriteFile(filepath.Join(outside, "app", ".env.example"), []byte("OUTSIDE_TOKEN=\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			victim := filepath.Join(root, "team", swapped)
			old := userSourceChecked
			userSourceChecked = func() {
				if err := os.Rename(victim, victim+".was"); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(filepath.Join(outside, swapped), victim); err != nil {
					t.Fatal(err)
				}
			}
			t.Cleanup(func() { userSourceChecked = old })

			dir, err := (UserMountPolicy{Roots: []string{root}}).OpenSourceInMemberRoots(filepath.Join(root, "team", "app"), foldersync.OpenFromTop)
			if dir != nil {
				_ = dir.Close()
			}
			if dir != nil || !errors.Is(err, errMemberSourceRefused) {
				t.Errorf("OpenSourceInMemberRoots = %v, %v; want no handle and the member refusal", dir, err)
			}
		})
	}
}

// A member root set through a link, the root itself or a folder above it, holds a folder named by the
// root as it is set and as it resolves. Under either name a link that leaves the root is refused and a
// plain folder is opened.
func TestOpenSourceInMemberRoots_ARootSetThroughALinkHoldsBothNamesOfAFolder(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink semantics differ on windows")
	}
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	resolved, outside := filepath.Join(base, "real", "members"), filepath.Join(base, "elsewhere")
	for _, dir := range []string{filepath.Join(resolved, "app"), outside} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for link, target := range map[string]string{
		filepath.Join(resolved, "out"): outside,                     // leaves the root
		filepath.Join(base, "linked"):  resolved,                    // the root itself
		filepath.Join(base, "above"):   filepath.Join(base, "real"), // a folder above the root
	} {
		if err := os.Symlink(target, link); err != nil {
			t.Fatal(err)
		}
	}
	for name, set := range map[string]string{
		"the root is a link":                filepath.Join(base, "linked"),
		"a folder above the root is a link": filepath.Join(base, "above", "members"),
	} {
		policy := UserMountPolicy{Roots: []string{set}}
		for _, named := range []string{set, resolved} {
			dir, err := policy.OpenSourceInMemberRoots(filepath.Join(named, "out"), foldersync.OpenFromTop)
			if dir != nil {
				_ = dir.Close()
			}
			if dir != nil || !errors.Is(err, errMemberSourceRefused) {
				t.Errorf("%s, a link out named under %s = %v, %v; want no handle and the member refusal", name, named, dir, err)
			}
			dir, err = policy.OpenSourceInMemberRoots(filepath.Join(named, "app"), foldersync.OpenFromTop)
			if err != nil {
				t.Errorf("%s, a plain folder named under %s: %v, want the folder opened", name, named, err)
				continue
			}
			_ = dir.Close()
		}
	}
}
