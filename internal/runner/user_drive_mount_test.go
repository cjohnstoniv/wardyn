// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package runner

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestParseUserDriveHostRoots pins the boot posture: refuse a malformed
// ceiling, warn about one so wide it bounds nothing, and treat unset as the
// closed default.
func TestParseUserDriveHostRoots(t *testing.T) {
	t.Run("unset is no roots and no error", func(t *testing.T) {
		roots, warns, err := ParseUserDriveHostRoots("")
		if err != nil || len(roots) != 0 || len(warns) != 0 {
			t.Fatalf("= %v, %v, %v; want the closed default", roots, warns, err)
		}
		// …and the closed default is what actually REFUSES, which is the half a
		// "no error" result could hide.
		if err := UserDriveHostRootCheck(roots)("/srv/homes"); err == nil {
			t.Fatal("an unset ceiling accepted a host_path drive — unset must fail CLOSED")
		}
	})

	t.Run("a malformed entry REFUSES boot", func(t *testing.T) {
		// Refuse rather than skip: a ceiling the operator mistyped must not
		// silently become a ceiling that bounds a different tree.
		for _, raw := range []string{"srv/homes", "/srv/../srv/homes", "/srv/homes/"} {
			if _, _, err := ParseUserDriveHostRoots(raw); err == nil {
				t.Errorf("%q parsed, want a boot refusal naming the var", raw)
			} else if !strings.Contains(err.Error(), "WARDYN_USER_DRIVE_HOST_ROOTS") {
				t.Errorf("err = %v, want the var named so an operator can find it", err)
			}
		}
	})

	t.Run("a root at / WARNs but is permitted", func(t *testing.T) {
		// Allow-and-warn is MemberMountPolicy.bootWarnings' own posture: the
		// operator may have chosen it deliberately, and should still be told.
		roots, warns, err := ParseUserDriveHostRoots("/,/srv/homes")
		if err != nil {
			t.Fatalf("err = %v, want permitted", err)
		}
		if len(roots) != 2 || len(warns) != 1 {
			t.Fatalf("roots = %v, warns = %v; want both roots kept and one warning", roots, warns)
		}
		if !strings.Contains(warns[0], "WARDYN_USER_DRIVE_HOST_ROOTS") {
			t.Errorf("warning = %q, want the var named", warns[0])
		}
		// AND it must say the RIGHT thing. "/" reads like "allow anywhere" and
		// behaves like "allow nothing": withinAnyRoot matches `real == root` or
		// `real` under `root + "/"`, which for "/" is the prefix "//" that no
		// cleaned absolute path has. An operator told the ceiling is "too wide"
		// would go looking for the drive it wrongly allowed instead of for the
		// drive it silently refused.
		if !strings.Contains(warns[0], "NOTHING") {
			t.Errorf("warning = %q, want it to say a root of \"/\" matches nothing — it is DEAD, not wide", warns[0])
		}
		if err := UserDriveHostRootCheck([]string{"/"})("/srv/homes"); err == nil {
			t.Error("a ceiling of \"/\" accepted a host root — the warning claims it matches nothing, so this is the claim itself")
		}
	})
}

// TestUserDriveHostRootCheck walks the ceiling itself. Every arm is a decision
// that could have gone the other way, and the counterfactual is named.
func TestUserDriveHostRootCheck(t *testing.T) {
	root := t.TempDir()
	inside := filepath.Join(root, "homes")
	if err := os.MkdirAll(inside, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	outside := t.TempDir()
	check := UserDriveHostRootCheck([]string{root})

	if err := check(inside); err != nil {
		t.Fatalf("a root INSIDE the ceiling was refused: %v", err)
	}
	if err := check(outside); err == nil {
		t.Error("a root outside the ceiling was accepted — the ceiling would bound nothing")
	}

	// FAIL CLOSED on a path that is not there. Stricter than ValidateMountSource
	// deliberately: a member's mount source is a path they are looking at, while
	// a drive's host root is a share the operator asserts is mounted HERE.
	// Accepting it would defer the failure to somebody else's run.
	if err := check(filepath.Join(root, "not-mounted-yet")); err == nil {
		t.Error("a nonexistent host root was accepted — authoring over an unmounted share must refuse now, not at a member's run")
	}

	// The bind-mount deny-list runs too, even when the ceiling is wide open:
	// /etc is inside no legitimate share.
	if err := UserDriveHostRootCheck([]string{"/"})("/etc"); err == nil {
		t.Error("/etc was accepted as a drive host root under a wide-open ceiling")
	}

	// The DOTFILE deny-list runs on the resolved path too: a share whose mount
	// point is (or traverses) a credential directory is never a drive's host
	// root. ValidateMountSource denies whole system trees and says nothing about
	// a .ssh inside an ordinary home, so without this the threat model's "the
	// dotfile deny-list matches the real path" was true of member mounts only.
	cred := filepath.Join(root, "homes", ".ssh")
	if err := os.MkdirAll(cred, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := check(cred); err == nil {
		t.Error("a host root inside .ssh was accepted — the credential deny-list must run on a drive's root too")
	} else if !strings.Contains(err.Error(), ".ssh") {
		t.Errorf("the refusal should name the offending segment, got: %v", err)
	}

	// A SYMLINK inside the ceiling pointing OUT of it is refused, which is the
	// whole reason the match is on the resolved real path rather than the
	// lexical one.
	link := filepath.Join(root, "escape")
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("symlink unsupported here: %v", err)
	}
	if err := check(link); err == nil {
		t.Error("a symlink out of the ceiling was accepted — a lexical match would have missed it")
	}
}

// TestValidateAuthoredTargetReservesTheDriveTarget pins the split between the
// two target validators, and the split is the point: an AUTHORED target may not
// be the drive's, while the drive's own mount must still pass the ordinary
// target rules. Folding the reservation into ValidateTarget would make the
// drive fail its own validation.
func TestValidateAuthoredTargetReservesTheDriveTarget(t *testing.T) {
	for _, tgt := range []string{DriveTarget, DriveTarget + "/shared", DriveTarget + "/a/b"} {
		if err := ValidateTarget(tgt); err != nil {
			t.Errorf("ValidateTarget(%q) = %v, want nil — the drive mounts there itself", tgt, err)
		}
		err := ValidateAuthoredTarget(tgt)
		if err == nil || !strings.Contains(err.Error(), "reserved") {
			t.Errorf("ValidateAuthoredTarget(%q) = %v, want a reserved-target refusal", tgt, err)
		}
	}
	// A NEIGHBOUR under the same allowed prefix is untouched: the reservation is
	// the subtree, not a string prefix over /home/agent/drive*.
	for _, tgt := range []string{"/home/agent/drives", "/home/agent/drive-report", "/home/agent/work"} {
		if err := ValidateAuthoredTarget(tgt); err != nil {
			t.Errorf("ValidateAuthoredTarget(%q) = %v, want nil", tgt, err)
		}
	}
	// And it still enforces everything ValidateTarget does.
	if err := ValidateAuthoredTarget("/usr/local"); err == nil {
		t.Error("a target outside every allowed prefix was accepted")
	}
}

// TestUserDriveMountSourceCheck is the DRIVER-side half: everything the
// authoring ceiling asserts, plus the one rule that only applies to a bind —
// the source must be a STRICT SUBDIRECTORY of a root.
//
// The split is the point. An authored host_root legitimately IS a root (the
// ordinary shape: the ceiling names the share's mount point and so does the
// drive), so folding the equality refusal into UserDriveHostRootCheck would
// refuse every correct drive. A MOUNT resolving to the root is a different
// thing entirely: it would bind the whole share — every other person's home —
// into one member's sandbox.
func TestUserDriveMountSourceCheck(t *testing.T) {
	root := t.TempDir()
	home := filepath.Join(root, "alice")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	roots := []string{root}

	real, err := UserDriveMountSourceCheck(roots)(home)
	if err != nil {
		t.Fatalf("a person's subdirectory of the root was refused: %v", err)
	}
	// The RESOLVED path comes back, not merely a nil error. The driver has one
	// more thing to say about it — is this directory still named after THIS
	// principal — and resolving it a second time there would let the driver
	// reason about a path this check never saw.
	want, err := filepath.EvalSymlinks(home)
	if err != nil {
		t.Fatalf("resolve home: %v", err)
	}
	if real != want {
		t.Errorf("resolved path = %q, want the symlink-resolved source %q", real, want)
	}
	if got, err := UserDriveMountSourceCheck(roots)(root); err == nil {
		t.Error("the ROOT itself was accepted as a bind source — that binds every other person's home into this sandbox")
	} else if got != "" {
		t.Errorf("a refused source returned the path %q — a caller that ignored the error must not find a plausible one in its place", got)
	}
	// The same path is FINE as an authored host_root, which is why the equality
	// rule cannot live in the shared check.
	if err := UserDriveHostRootCheck(roots)(root); err != nil {
		t.Errorf("the root was refused as an authored host_root, which is the ordinary shape: %v", err)
	}
	// And the composed half still refuses: unset roots, and the deny-lists.
	if _, err := UserDriveMountSourceCheck(nil)(home); err == nil {
		t.Error("unset roots accepted a bind source — the fail-closed default must survive composition")
	}
	if _, err := UserDriveMountSourceCheck([]string{"/"})("/etc/wardyn-drives"); err == nil {
		t.Error("the host bind deny-list did not run through the composed check")
	}
	// A SYMLINKED home resolves and is accepted here, and the resolved path is
	// what comes back — the cross-volume layout (`<root>/bob` -> a directory on
	// a second export the ceiling also names) is ordinary, and the sibling case
	// this enables the driver to catch is refused one layer up, where the home
	// NAME is known.
	second := t.TempDir()
	elsewhere := filepath.Join(second, "bob")
	if err := os.MkdirAll(elsewhere, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	link := filepath.Join(root, "bob")
	if err := os.Symlink(elsewhere, link); err != nil {
		t.Skipf("symlink unsupported here: %v", err)
	}
	got, err := UserDriveMountSourceCheck([]string{root, second})(link)
	if err != nil {
		t.Fatalf("a home symlinked onto a second configured root was refused: %v", err)
	}
	if wantElsewhere, _ := filepath.EvalSymlinks(elsewhere); got != wantElsewhere {
		t.Errorf("resolved path = %q, want the link's target %q", got, wantElsewhere)
	}
}
