// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package runner

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/types"
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

	// A root under a DENIED BIND PREFIX is dead exactly as "/" is, and used to
	// be the silent one (F13 H2): UserDriveHostRootCheck runs ValidateMountSource
	// before it ever compares against the roots, so /dev/shm, a share mounted
	// under /var/run, or a relocated Docker data-root under /var/lib/docker
	// parses clean and then refuses every drive authored inside it. The
	// operator's first signal was a 422 on a form they believed was right.
	t.Run("a root under a denied bind prefix WARNs that it matches nothing", func(t *testing.T) {
		for _, dead := range []string{"/dev/shm", "/var/run/shares", "/var/lib/docker/homes"} {
			roots, warns, err := ParseUserDriveHostRoots(dead + ",/srv/homes")
			if err != nil {
				t.Fatalf("%q: err = %v, want permitted-and-warned, the bootWarnings posture", dead, err)
			}
			if len(roots) != 2 || len(warns) != 1 {
				t.Fatalf("%q: roots = %v, warns = %v; want both roots kept and one warning", dead, roots, warns)
			}
			if !strings.Contains(warns[0], "WARDYN_USER_DRIVE_HOST_ROOTS") || !strings.Contains(warns[0], dead) {
				t.Errorf("%q: warning = %q, want the var and the root named", dead, warns[0])
			}
			// The SAME word "/" earns, because it is the same outcome: an
			// operator told a ceiling is "too wide" goes looking for the drive it
			// wrongly allowed instead of for the drive it silently refused.
			if !strings.Contains(warns[0], "NOTHING") {
				t.Errorf("%q: warning = %q, want it to say the root matches nothing", dead, warns[0])
			}
			// And the claim the warning makes IS the behaviour: a drive at that
			// root, and one under it, are both refused by the ceiling itself.
			for _, hostRoot := range []string{dead, dead + "/team"} {
				if err := UserDriveHostRootCheck([]string{dead})(hostRoot); err == nil {
					t.Errorf("%q: the ceiling accepted host_root %q — the warning claims it matches nothing", dead, hostRoot)
				}
			}
		}
	})

	// The control: an ordinary root earns no warning at all, so the arm above is
	// a statement about denied prefixes rather than about every root.
	t.Run("an ordinary root is silent", func(t *testing.T) {
		roots, warns, err := ParseUserDriveHostRoots("/srv/homes,/mnt/nas")
		if err != nil || len(roots) != 2 || len(warns) != 0 {
			t.Fatalf("= %v, %v, %v; want two roots and no warning", roots, warns, err)
		}
	})

	// The CSV's own shape, which an operator types by hand into a unit file.
	// Surrounding whitespace and a trailing separator are the two things that
	// survive an edit, and neither may become a root — nor may either REFUSE
	// boot, which is the wrong answer in the other direction: a mistyped
	// ceiling must refuse, but " /srv/homes , /mnt/nas " is not mistyped, it is
	// formatted, and a deployment that will not start over a space is one an
	// operator disables the feature to get past.
	t.Run("the CSV is trimmed and an empty entry is dropped", func(t *testing.T) {
		for _, tc := range []struct {
			raw  string
			want int
		}{
			{"/srv/homes,", 1},
			{" /srv/homes , /mnt/nas ", 2},
		} {
			roots, warns, err := ParseUserDriveHostRoots(tc.raw)
			if err != nil || len(roots) != tc.want {
				t.Errorf("ParseUserDriveHostRoots(%q) = %v, %v; want %d root(s)", tc.raw, roots, err, tc.want)
			}
			if len(warns) != 0 {
				t.Errorf("ParseUserDriveHostRoots(%q) warned %v — trimming is not a mistake to report", tc.raw, warns)
			}
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

	// THE SPELLING TABLE. withinAnyRoot resolves BOTH sides, so a ceiling and a
	// drive row may name one tree by two different paths — which is the ordinary
	// operator shape rather than an edge case: /srv/homes is a symlink to the
	// mount point on plenty of hosts, and an admin fills the form in from
	// whichever spelling their runbook uses. Resolving only the drive's side
	// would refuse a correct row for a reason no error message could explain.
	//
	// The last three rows are the other direction — the ways a path that LOOKS
	// in-ceiling is not, and each is a decision parseRootList or withinAnyRoot
	// makes rather than a property of the filesystem.
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("resolve tempdir: %v", err)
	}
	real := filepath.Join(base, "nas", "homes")
	if err := os.MkdirAll(filepath.Join(real, "alice"), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	sibling := real + "-other"
	if err := os.MkdirAll(sibling, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	linked := filepath.Join(base, "srv-homes")
	if err := os.Symlink(real, linked); err != nil {
		t.Skipf("symlink unsupported here: %v", err)
	}
	for _, tc := range []struct {
		name     string
		roots    []string
		hostRoot string
		ok       bool
	}{
		{"a symlinked root entry accepts the link spelling", []string{linked}, linked, true},
		{"a symlinked root entry accepts the real spelling", []string{linked}, real, true},
		{"a real root entry accepts the symlinked spelling", []string{real}, linked, true},
		// Separator-anchored, or "/srv/homes-other" would be inside "/srv/homes"
		// and every neighbouring export on the same parent would be authorable.
		{"a prefix-string sibling is outside", []string{real}, sibling, false},
		// parseRootList's cleanliness rule applies to the DRIVE's side too: a
		// trailing slash is the same tree under a different string, and letting
		// one through would mean two rows naming one directory that no UNIQUE
		// constraint or nesting check could see were the same.
		{"an uncleaned trailing slash is refused", []string{real}, real + "/", false},
		// Relative resolves against the DAEMON's working directory, which is not
		// a thing an admin filling in a form can reason about.
		{"a relative path is refused", []string{real}, "nas/homes", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := UserDriveHostRootCheck(tc.roots)(tc.hostRoot); (err == nil) != tc.ok {
				t.Errorf("UserDriveHostRootCheck(%v)(%q) = %v, want ok=%v", tc.roots, tc.hostRoot, err, tc.ok)
			}
		})
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
	// A SYMLINKED home resolves and is accepted HERE, and the resolved path is
	// what comes back. That is this check's whole scope: the deployment's
	// ceiling, which is a statement about the operator's roots and nothing
	// else. The two things it cannot say are decided one layer up, on the
	// resolved path it hands back — whether the target is inside THIS drive's
	// own host_root (UserDriveHomeWithinItsRoot, which refuses exactly the link
	// below when the drive was authored against `root` alone) and whether the
	// directory still carries this principal's home name.
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

	// THE SAME SPELLING TABLE AS THE AUTHORING CHECK, plus the rule only a BIND
	// has: the root is resolved before the comparison, so a ceiling naming the
	// link and a source naming the real tree (or the other way round) agree —
	// and the strict-subdirectory rule survives that resolution, which is the
	// row that matters. `<link>` and `<real>` are ONE directory, so a source
	// that spells it either way is the share itself and binds every person's
	// home; a lexical equality check would have caught only one of the two.
	base, berr := filepath.EvalSymlinks(t.TempDir())
	if berr != nil {
		t.Fatalf("resolve tempdir: %v", berr)
	}
	shared := filepath.Join(base, "nas", "homes")
	if err := os.MkdirAll(filepath.Join(shared, "alice"), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	nextDoor := shared + "-other"
	if err := os.MkdirAll(nextDoor, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	sharedLink := filepath.Join(base, "srv-homes")
	if err := os.Symlink(shared, sharedLink); err != nil {
		t.Skipf("symlink unsupported here: %v", err)
	}
	for _, tc := range []struct {
		name     string
		roots    []string
		source   string
		ok       bool
		wantReal string
	}{
		{"a home under a symlinked root", []string{sharedLink}, filepath.Join(sharedLink, "alice"), true, filepath.Join(shared, "alice")},
		{"the link itself IS the root", []string{sharedLink}, sharedLink, false, ""},
		{"the real root reached through the link", []string{shared}, sharedLink, false, ""},
		{"a sibling directory of the root", []string{shared}, nextDoor, false, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resolved, err := UserDriveMountSourceCheck(tc.roots)(tc.source)
			if (err == nil) != tc.ok {
				t.Fatalf("UserDriveMountSourceCheck(%v)(%q) = %q, %v; want ok=%v", tc.roots, tc.source, resolved, err, tc.ok)
			}
			if resolved != tc.wantReal {
				t.Errorf("resolved = %q, want %q", resolved, tc.wantReal)
			}
		})
	}

	// THE DENY LIST RUNS ON THE RESOLVED PATH, so listing a denied tree as a
	// root buys nothing: ValidateMountSource resolves the source and re-runs
	// deniedSource on what it found, which is why ParseUserDriveHostRoots warns
	// that such a root matches NOTHING rather than treating it as an escape
	// hatch. Without this the deny-list would be a statement about the string an
	// admin typed, and a host-side symlink would be the way past it.
	//
	// /dev/shm because /dev is deny-listed and /dev/shm is world-writable, so
	// the arm needs no privilege; it skips where the tmpfs is absent.
	t.Run("a symlink into a denied tree is refused even when that tree is a configured root", func(t *testing.T) {
		if st, err := os.Stat("/dev/shm"); err != nil || !st.IsDir() {
			t.Skip("/dev/shm not available")
		}
		shm, err := os.MkdirTemp("/dev/shm", "wardyn-drive-deny-")
		if err != nil {
			t.Skipf("/dev/shm not writable: %v", err)
		}
		t.Cleanup(func() { _ = os.RemoveAll(shm) })
		if err := os.MkdirAll(filepath.Join(shm, "alice"), 0o700); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		ordinary, err := filepath.EvalSymlinks(t.TempDir())
		if err != nil {
			t.Fatalf("resolve tempdir: %v", err)
		}
		escaping := filepath.Join(ordinary, "alice")
		if err := os.Symlink(filepath.Join(shm, "alice"), escaping); err != nil {
			t.Skipf("symlink unsupported here: %v", err)
		}
		resolved, err := UserDriveMountSourceCheck([]string{ordinary, "/dev/shm"})(escaping)
		if err == nil {
			t.Fatalf("a bind source resolving to %q — under a denied prefix — was accepted", resolved)
		}
		if !strings.Contains(err.Error(), "denied host path") {
			t.Errorf("refusal = %v, want the deny-list's wording on the RESOLVED path", err)
		}
		// The control: with the denied tree NOT listed, the same link is refused
		// for the ordinary reason (it leaves every root), so the row above is a
		// statement about the deny list rather than about the ceiling.
		if _, err := UserDriveMountSourceCheck([]string{ordinary})(escaping); err == nil {
			t.Error("a symlink out of the only configured root was accepted")
		}
	})
}

// TestUserDriveHomeWithinItsRoot walks the PER-DRIVE bound — the one the
// deployment ceiling cannot make, because with two share drives inside one
// ceiling both trees are equally allowed and the ceiling cannot tell which
// drive this bind belongs to.
func TestUserDriveHomeWithinItsRoot(t *testing.T) {
	// The check takes the MOUNT now, not two paths: its refusal names the drive
	// and the directory and never a path, so it needs the names.
	within := func(hostRoot, real string) error {
		return UserDriveHomeWithinItsRoot(&types.DriveMount{
			Backend: types.DriveBackendHostPath, ObjectName: real,
			HostRoot: hostRoot, DriveName: "nas", HomeName: "alice",
		}, real)
	}
	base := t.TempDir()
	rootA := filepath.Join(base, "a")
	rootB := filepath.Join(base, "b")
	for _, dir := range []string{filepath.Join(rootA, "alice", "docs"), filepath.Join(rootB, "alice")} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
	}

	// The ordinary shape, and one nested deeper: a strict subdirectory at any
	// depth is inside this drive's tree.
	for _, real := range []string{filepath.Join(rootA, "alice"), filepath.Join(rootA, "alice", "docs")} {
		if err := within(rootA, real); err != nil {
			t.Errorf("a home inside its own drive's root was refused: %v", err)
		}
	}
	// The whole point: rootB is a perfectly legal tree for SOME drive, and the
	// deployment ceiling says so. It is not this drive's.
	crossDrive := within(rootA, filepath.Join(rootB, "alice"))
	if crossDrive == nil {
		t.Fatal("a home resolving into ANOTHER drive's root was accepted — that binds the other drive's directory")
	}
	// AND THE REFUSAL IS MEMBER-SAFE. Every driver refusal on this path becomes
	// the run's failure_hint, read by the run's CREATOR — so the message names
	// the drive and the directory (driveVolumeAdoptable's shape) and neither the
	// drive's root nor the resolved real path. The operator reads those from the
	// log line beside it.
	for _, leak := range []string{rootA, rootB, filepath.Join(rootB, "alice")} {
		if strings.Contains(crossDrive.Error(), leak) {
			t.Errorf("refusal = %q leaks the host path %q to the run's creator", crossDrive, leak)
		}
	}
	for _, want := range []string{`drive "nas"`, `directory "alice"`} {
		if !strings.Contains(crossDrive.Error(), want) {
			t.Errorf("refusal = %q, want it to name %s", crossDrive, want)
		}
	}
	// A mount that cannot name its drive names the DIRECTORY alone — never the
	// object, which on a share is the absolute path.
	anon := UserDriveHomeWithinItsRoot(&types.DriveMount{
		Backend: types.DriveBackendHostPath, ObjectName: filepath.Join(rootB, "alice"),
		HostRoot: rootA, HomeName: "alice",
	}, filepath.Join(rootB, "alice"))
	if anon == nil || !strings.Contains(anon.Error(), `directory "alice"`) || strings.Contains(anon.Error(), rootB) {
		t.Errorf("nameless-drive refusal = %v, want the directory alone and no path", anon)
	}
	// A nil mount is a refusal too, not a nil-deref.
	if err := UserDriveHomeWithinItsRoot(nil, filepath.Join(rootA, "alice")); err == nil {
		t.Error("a nil drive was accepted — an absent mount cannot be contained by anything")
	}
	// STRICT: the root itself would bind every other person's home, and the rule
	// is stated here as well as in UserDriveMountSourceCheck so a refactor
	// cannot drop the only copy.
	if err := within(rootA, rootA); err == nil {
		t.Error("the drive's own root was accepted as a bind source — a drive binds one person's subdirectory, never the share")
	}
	// Separator-anchored, so a sibling tree whose name merely starts with the
	// root's is outside it.
	if err := within(rootA, rootA+"2"); err == nil {
		t.Errorf("%q was treated as inside %q — the prefix match must be separator-anchored", rootA+"2", rootA)
	}
	// FAIL CLOSED on an absent root: "" means the mount was built by something
	// that does not carry the field, and falling through would be the pre-fix
	// behaviour reappearing where nobody would look for it.
	for _, missing := range []string{"", "   "} {
		if err := within(missing, filepath.Join(rootA, "alice")); err == nil {
			t.Errorf("an empty host_root (%q) was accepted — an absent per-drive bound must refuse, never skip", missing)
		}
	}
	// And on a root that cannot be resolved on this host, for the reason
	// UserDriveHostRootCheck fails closed on the same thing: a bound that cannot
	// be evaluated is not a bound.
	if err := within(filepath.Join(base, "no-such-root"), filepath.Join(rootA, "alice")); err == nil {
		t.Error("an unresolvable host_root was accepted")
	}
	// A root reached through a SYMLINK still bounds: both sides are resolved, so
	// an operator whose share is mounted behind a link is not refused for it.
	linkedRoot := filepath.Join(base, "a-link")
	if err := os.Symlink(rootA, linkedRoot); err != nil {
		t.Skipf("symlink unsupported here: %v", err)
	}
	realHome, err := filepath.EvalSymlinks(filepath.Join(rootA, "alice"))
	if err != nil {
		t.Fatalf("resolve home: %v", err)
	}
	if err := within(linkedRoot, realHome); err != nil {
		t.Errorf("a host_root reached through a symlink refused its own home: %v", err)
	}
}
