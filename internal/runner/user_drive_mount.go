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
// THREE root values are permitted but WARNED about, the same allow-and-warn
// posture MemberMountPolicy.bootWarnings takes — and they are warned about for
// OPPOSITE reasons, which is why they do not share a sentence:
//
//   - the daemon's own $HOME is far too WIDE: every dotfile tree that home
//     holds becomes a place a drive may be authored over, and its per-person
//     subdirectories are bound into other people's sandboxes.
//   - "/" is DEAD, not wide. withinAnyRoot matches `real == root` or `real`
//     under `root + "/"`, and for the root "/" that second form is the prefix
//     "//", which no cleaned absolute path has — so a ceiling of "/" matches
//     nothing at all and refuses EVERY host_path drive. It reads like "allow
//     anywhere" and behaves like "allow nothing", so the warning has to say
//     which.
//   - A root UNDER A DENIED BIND PREFIX is dead in exactly the same way, and
//     was the silent one (F13 H2). UserDriveHostRootCheck runs
//     ValidateMountSource BEFORE it ever compares against the roots, so a
//     ceiling of /dev/shm, a share mounted under /var/run, or a relocated
//     Docker data-root under /var/lib/docker parses clean here and then refuses
//     every drive authored inside it — the operator learning from a 422 on a
//     form they believed was right rather than from the boot line that could
//     have told them. Same "matches NOTHING" wording as "/", because it is the
//     same outcome; the deny-list's own sentence is carried through so the
//     operator reads WHICH prefix bit.
//
// The deny-list runs LEXICALLY here, on the value as configured, and does not
// resolve symlinks: boot is not the place to touch a share that may not be
// mounted yet, and a root that resolves INTO a denied tree is still caught —
// by UserDriveHostRootCheck, which re-runs the same list on the real path at
// authoring and at bind time. This warning is about the value the operator can
// read back out of their own unit file.
func ParseUserDriveHostRoots(raw string) (roots []string, warnings []string, err error) {
	roots, err = parseRootList("WARDYN_USER_DRIVE_HOST_ROOTS", raw)
	if err != nil {
		return nil, nil, err
	}
	home := filepath.Clean(strings.TrimSpace(os.Getenv("HOME")))
	for _, r := range roots {
		switch derr := deniedSource(r); {
		case r == "/":
			warnings = append(warnings, fmt.Sprintf(
				"WARDYN_USER_DRIVE_HOST_ROOTS contains %q, which matches NOTHING: a root of \"/\" bounds only the literal path \"/\", "+
					"so every host_path drive under it is refused rather than allowed; point it at the mount point of the share instead", r))
		case derr != nil:
			warnings = append(warnings, fmt.Sprintf(
				"WARDYN_USER_DRIVE_HOST_ROOTS contains %q, which matches NOTHING: %s, so every host_path drive authored inside it is "+
					"refused at the write boundary and at bind time; point it at the mount point of the share instead", r, derr))
		case home != "" && home != "." && r == home:
			warnings = append(warnings, fmt.Sprintf(
				"WARDYN_USER_DRIVE_HOST_ROOTS contains %q, this daemon's own home directory — a host-path drive could then be authored over "+
					"anything in it, and its per-person subdirectories are bound into other people's sandboxes; point it at the mount point of the share instead", r))
		}
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
		// The member rule's DOTFILE deny-list, on the RESOLVED path — the same
		// segments ValidateMemberMountSource refuses (.ssh, .aws, .claude, .kube,
		// .config/gh, …). ValidateMountSource above denies whole system trees; it
		// says nothing about a credential directory inside an ordinary home, and
		// a share whose mount point is one — or a symlink that lands in one — is
		// exactly the shape THREAT-MODEL's host_path residual claims is bounded
		// by "the dotfile deny-list matches the real path". Without this the
		// claim was true of member mounts only.
		if seg := deniedMemberSegment(real); seg != "" {
			return fmt.Errorf("host_root %q resolves to %q, which is or traverses %q — a credential directory is never a drive's host root",
				hostRoot, real, seg)
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

// UserDriveMountSourceCheck is the DRIVER-side check over the path a host_path
// drive is actually BOUND from — this principal's subdirectory — as opposed to
// the host_root an admin authored, which UserDriveHostRootCheck decides about.
//
// It is that check plus ONE rule that only makes sense for a bind: the resolved
// source must be a STRICT SUBDIRECTORY of a root, never a root itself. An
// authored host_root legitimately IS a root (the ordinary shape: the ceiling
// names the share's mount point and so does the drive), so the equality arm
// cannot live inside the shared check without refusing every correct drive. But
// a MOUNT whose source resolved to the root would bind the whole share —
// everybody's home directory — into one member's sandbox, which is the single
// outcome the per-person subdirectory model exists to prevent. That can only
// arrive through a bug (a home name that resolved to "." or "", a symlink from
// a home back to its parent), and a bug is precisely what a last-thing-before-
// ContainerCreate check is for.
//
// Composed rather than restated so the driver and the API write boundary cannot
// drift on what the deny-list, the unset-roots refusal, or "inside a root" mean.
//
// IT RETURNS THE RESOLVED REAL PATH, not just nil/error, and the caller is
// expected to keep asserting about it. This function's whole job is symlink
// resolution, so handing back the answer is what stops the ONE caller that has
// to say more about it — the Docker driver, which knows whose home the bind is
// supposed to be — from resolving the same path a second time and reasoning
// about a value this one never saw. On an error the string is "": there is no
// resolved path to speak of, and a caller that ignored the error must not find
// a plausible-looking one in its place.
func UserDriveMountSourceCheck(roots []string) func(source string) (string, error) {
	within := UserDriveHostRootCheck(roots)
	return func(source string) (string, error) {
		if err := within(source); err != nil {
			return "", err
		}
		real, err := filepath.EvalSymlinks(filepath.Clean(source))
		if err != nil {
			// Unreachable in practice — `within` already resolved this path and
			// fails closed when it cannot — but a resolve that started working
			// and then stopped must not fall through to a bind.
			return "", fmt.Errorf("user drive source %q could not be resolved on this host: %w", source, err)
		}
		for _, root := range roots {
			r := filepath.Clean(root)
			if resolved, rerr := filepath.EvalSymlinks(r); rerr == nil {
				r = resolved
			}
			if real == r {
				return "", fmt.Errorf("user drive source %q resolves to %q, which IS the configured root — a drive binds one person's "+
					"subdirectory of a share, never the share itself (every other person's home is under it)", source, real)
			}
		}
		return real, nil
	}
}

// UserDriveHomeWithinItsRoot asserts that real — the symlink-RESOLVED directory
// a host_path drive is about to be bound from — is a STRICT SUBDIRECTORY of
// hostRoot, THIS drive's own root, resolved the same way.
//
// ─── WHY THE DEPLOYMENT CEILING IS NOT ENOUGH ──────────────────────────────
//
// UserDriveMountSourceCheck bounds the bind to the union of every root in
// WARDYN_USER_DRIVE_HOST_ROOTS, which is the OPERATOR's outer bound and stays
// exactly that. It cannot say which of those roots this particular drive was
// authored against, because it is not given the drive. So on a deployment with
// two share drives — ceiling `/srv/a,/srv/b`, drive A rooted at /srv/a, drive B
// at /srv/b — a home under A replaced host-side by a link to the SAME-NAMED
// home under B satisfies every check the driver had: it is inside a configured
// root, it is not a root, it traverses no denied prefix, and its base name is
// still this principal's home. And it binds drive B's tree.
//
// The per-drive root closes it, and the two bounds are kept BOTH rather than
// collapsed into one: the ceiling is the operator's (env/MDM-set, a console
// compromise cannot widen it) and the root is the row's (admin-authored, and
// therefore not allowed to be the outer bound). A drive must satisfy both.
//
// ─── FAIL CLOSED ON AN ABSENT ROOT ─────────────────────────────────────────
//
// An empty hostRoot is a REFUSAL, not a skip. types.DriveMount.HostRoot is set
// from the resolved row for every host_path drive, so "" means the mount was
// built by something that does not know about this field — an older control
// plane, a hand-written -spec for the standalone runner — and the one thing
// that must not happen then is the pre-fix behaviour silently returning.
//
// STRICT, so a source that resolved TO the root is refused here as well as by
// UserDriveMountSourceCheck: binding a share's root hands one member every
// other member's home, and a check that is only stated once is a check a
// refactor can drop.
func UserDriveHomeWithinItsRoot(hostRoot, real string) error {
	if strings.TrimSpace(hostRoot) == "" {
		return fmt.Errorf("user drive source %q carries no host_root to be contained by — a share drive's own root is what "+
			"bounds it to one drive's tree, and the deployment ceiling alone would allow another drive's", real)
	}
	root := filepath.Clean(hostRoot)
	resolved, err := filepath.EvalSymlinks(root)
	if err != nil {
		return fmt.Errorf("user drive host_root %q could not be resolved on this host (a share's root must be a directory that exists here): %w", hostRoot, err)
	}
	root = resolved
	if real == root {
		return fmt.Errorf("user drive source resolves to %q, which IS this drive's host_root — a drive binds one person's "+
			"subdirectory of a share, never the share itself (every other person's home is under it)", real)
	}
	if !withinAnyRoot(real, []string{root}) {
		return fmt.Errorf("user drive source resolves to %q, which is outside this drive's host_root %q — the deployment's "+
			"ceiling allows that tree for SOME drive, but a home replaced by a link into another drive's root would bind "+
			"that drive's directory instead of this one's", real, root)
	}
	return nil
}
