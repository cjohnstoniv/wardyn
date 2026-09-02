// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// The deployment's ENV CEILING over user drives whose bytes live on a host path
// (WARDYN_USER_DRIVE_HOST_ROOTS).
//
// Why a drive needs one at all, when the drive row is admin-authored: a
// host_path drive names a tree the OPERATOR mounted host-side, and Wardyn then
// binds a subdirectory of it into OTHER PEOPLE's sandboxes. That is the same
// shape member_mount.go's own doc argues about — a path typed into the product
// deciding what a sandbox can reach — and it takes the same answer: the
// security ceiling is operator/MDM-set in the environment, never a row in a
// table the product itself writes. A console compromise then widens nothing,
// because the widening it would need is not in the database.
//
// UNSET FAILS CLOSED. With no roots configured, NO host_path drive may be
// authored at all — byte-for-byte the WARDYN_MEMBER_WORKSPACE_ROOTS posture,
// and for the same reason: the safe default for "the operator has not said
// where" is "nowhere", not "anywhere".
package runner

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ParseUserDriveHostRoots parses WARDYN_USER_DRIVE_HOST_ROOTS — a CSV of
// absolute, already-cleaned host directories — into the roots a host_path drive
// may be authored inside, plus the boot WARNINGS a dangerously wide root earns.
//
// It reuses parseRootList verbatim, so a root here can no more carry a
// traversal segment than a member root can, and a malformed value REFUSES BOOT
// exactly as WARDYN_MEMBER_WORKSPACE_ROOTS does: a ceiling the operator
// mistyped must not silently become a ceiling that bounds a different tree.
//
// A root of "/" or the daemon's own $HOME is permitted but WARNED about, the
// same allow-and-warn posture MemberMountPolicy.bootWarnings takes — with such
// a root the ceiling bounds essentially nothing, which an operator may have
// chosen deliberately and should still be told.
func ParseUserDriveHostRoots(raw string) (roots []string, warnings []string, err error) {
	roots, err = parseRootList("WARDYN_USER_DRIVE_HOST_ROOTS", raw)
	if err != nil {
		return nil, nil, err
	}
	home := filepath.Clean(strings.TrimSpace(os.Getenv("HOME")))
	for _, r := range roots {
		if r != "/" && !(home != "" && home != "." && r == home) {
			continue
		}
		warnings = append(warnings, fmt.Sprintf(
			"WARDYN_USER_DRIVE_HOST_ROOTS contains %q — a host-path drive could then be authored anywhere on this host, "+
				"and its per-person subdirectories are bound into other people's sandboxes; point it at the mount point of the share instead", r))
	}
	return roots, warnings, nil
}

// UserDriveHostRootCheck returns the ceiling predicate the API write boundary
// composes with types.ValidateUserDrive: nil when hostRoot is inside one of
// roots, an error naming the ceiling otherwise. It satisfies
// types.UserDriveHostRootCheck.
//
// THE EMPTY-ROOTS ARM IS THE FEATURE, not a degenerate case: no roots means
// every host_path drive is refused, so a deployment that has not opted in
// cannot acquire one by an admin filling in a form.
//
// It resolves symlinks and matches on the REAL path (withinAnyRoot, shared with
// the member rule), and it FAILS CLOSED on a resolve error — including "does
// not exist". That last one is stricter than ValidateMountSource, which falls
// through to a lexical check so a remote dockerd's paths still validate, and
// the difference is deliberate: a member's mount source is a path they are
// looking at, while a drive's host root is a share the operator is asserting is
// mounted HERE. Authoring a drive over a path that is not there yet would defer
// the failure to somebody else's run.
func UserDriveHostRootCheck(roots []string) func(hostRoot string) error {
	return func(hostRoot string) error {
		if len(roots) == 0 {
			return fmt.Errorf("this deployment sets no WARDYN_USER_DRIVE_HOST_ROOTS, so no host_path drive may be authored " +
				"(set it to the mount point of the share, then re-save this drive)")
		}
		// The SAME host bind-mount deny-list every other authored source runs,
		// applied here rather than left to the driver: a drive whose root is
		// /etc or a directory holding a runtime socket is a row that would be
		// refused at every member's dispatch, so it is refused at authoring
		// instead. The driver still re-checks at bind time — this is the policy
		// half of the two-layer guardrail, exactly as validatePolicySpec is.
		if err := ValidateMountSource(hostRoot); err != nil {
			return err
		}
		real, err := filepath.EvalSymlinks(filepath.Clean(hostRoot))
		if err != nil {
			return fmt.Errorf("host_root %q could not be resolved on this host (a drive's host root must be a directory that exists here): %w", hostRoot, err)
		}
		if !withinAnyRoot(real, roots) {
			// The frozen wording (docs/design/user-drives-prompt.md's
			// server-composed table), naming the RESOLVED path too when it
			// differs — a symlink out of the ceiling is the case an admin
			// cannot diagnose from the path they typed.
			if real != filepath.Clean(hostRoot) {
				return fmt.Errorf("host_root %q resolves to %q and is not inside WARDYN_USER_DRIVE_HOST_ROOTS (%s) — "+
					"a drive may bind only a subdirectory of a root this deployment allows", hostRoot, real, strings.Join(roots, ", "))
			}
			return fmt.Errorf("host_root %q is not inside WARDYN_USER_DRIVE_HOST_ROOTS (%s) — "+
				"a drive may bind only a subdirectory of a root this deployment allows", hostRoot, strings.Join(roots, ", "))
		}
		return nil
	}
}
