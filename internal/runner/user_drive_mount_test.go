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
		// operator may have chosen it deliberately, and should still be told the
		// ceiling now bounds essentially nothing.
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
