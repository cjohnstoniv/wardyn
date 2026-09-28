// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// The deployment's ENV ceiling over user drives whose bytes live on a host path
// (WARDYN_USER_DRIVE_HOST_ROOTS): operator/MDM-set, never a row the product
// itself writes, so a console compromise cannot widen it. Unset fails closed —
// no roots configured means no host_path drive may be authored at all.
package runner

import (
	"fmt"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// ParseUserDriveHostRoots parses WARDYN_USER_DRIVE_HOST_ROOTS — a CSV of
// absolute, already-cleaned host directories — into the roots a host_path drive
// may be authored inside, plus boot WARNINGS for a dangerously wide root. It
// reuses parseRootList, so a malformed value REFUSES BOOT like
// WARDYN_USER_WORKSPACE_ROOTS does.
//
// Three values are allowed but warned about: the daemon's own $HOME (too WIDE —
// every dotfile tree becomes authorable, and per-person subdirectories get
// bound into other sandboxes); "/" (DEAD, not wide — withinAnyRoot never
// matches it, so it refuses every drive); and a root under a denied bind prefix
// (dead the same way, and previously silent — UserDriveHostRootCheck runs
// ValidateMountSource before comparing against roots).
//
// The deny-list runs LEXICALLY on the configured value, not the resolved path:
// boot must not touch a share that may not be mounted yet. UserDriveHostRootCheck
// re-runs the same list on the real path at authoring and bind time.
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

// MountCeilingOverlapWarnings returns boot WARNINGS when the two operator-set
// mount ceilings (WARDYN_USER_WORKSPACE_ROOTS and WARDYN_USER_DRIVE_HOST_ROOTS)
// name overlapping trees. They are one level apart by design — a drive's
// host_root is a share's mount point and Wardyn binds only one person's
// subdirectory of it, while a workspace root may be bound WHOLE and writable —
// so overlap lets a member onboard a drive's share as a workspace and bind every
// person's home through a surface that never consults drive allocation.
//
// A WARNING, not a refusal: an operator may have deliberately opened a tree to
// both, and refusing at boot would take a running deployment down on upgrade.
// Comparison is LEXICAL, on configured values, and covers the shared list plus
// every per-principal override (which replaces rather than extends it).
func MountCeilingOverlapWarnings(member UserMountPolicy, driveRoots []string) []string {
	var out []string
	seen := map[string]bool{}
	for _, m := range userCeilingRoots(member) {
		for _, d := range driveRoots {
			d = filepath.Clean(d)
			var msg string
			switch {
			case m == d:
				msg = fmt.Sprintf("WARDYN_USER_WORKSPACE_ROOTS and WARDYN_USER_DRIVE_HOST_ROOTS both name %q: a member can onboard "+
					"that directory as a workspace and bind the WHOLE share, every other person's home included, without a drive "+
					"allocation. Point the drive ceiling at the share and the member ceiling somewhere else", m)
			case strings.HasPrefix(d, m+string(filepath.Separator)):
				msg = fmt.Sprintf("WARDYN_USER_WORKSPACE_ROOTS contains %q, which holds the WARDYN_USER_DRIVE_HOST_ROOTS entry %q: a member can onboard "+
					"that share as a workspace and bind it whole, every other person's home included, without a drive allocation. "+
					"Point the member ceiling at a tree that does not contain the share", m, d)
			case strings.HasPrefix(m, d+string(filepath.Separator)):
				msg = fmt.Sprintf("WARDYN_USER_WORKSPACE_ROOTS contains %q, which is INSIDE the WARDYN_USER_DRIVE_HOST_ROOTS entry %q: member workspaces "+
					"would be authored inside a share whose directories Wardyn hands out one person at a time. Point the member "+
					"ceiling outside the share", m, d)
			default:
				continue
			}
			if !seen[msg] {
				seen[msg] = true
				out = append(out, msg)
			}
		}
	}
	return out
}

// userCeilingRoots is every distinct root that bounds SOME member's mounts:
// the shared list plus each per-principal override, which replaces rather than
// extends it and is therefore its own ceiling.
func userCeilingRoots(member UserMountPolicy) []string {
	var out []string
	seen := map[string]bool{}
	add := func(roots []string) {
		for _, r := range roots {
			c := filepath.Clean(r)
			if !seen[c] {
				seen[c] = true
				out = append(out, c)
			}
		}
	}
	add(member.Roots)
	// Sorted so boot warnings are stable across restarts (map order isn't).
	for _, p := range slices.Sorted(maps.Keys(member.RootsByPrincipal)) {
		add(member.RootsByPrincipal[p])
	}
	return out
}

// UserDriveHostRootCheck returns the ceiling predicate the API write boundary
// composes with types.ValidateUserDrive: nil when hostRoot is inside one of
// roots, an error naming the ceiling otherwise. Satisfies
// types.UserDriveHostRootCheck.
//
// Empty roots refuses EVERY host_path drive by design — a deployment that has
// not opted in cannot acquire one via an admin form.
//
// Resolves symlinks and matches on the REAL path, failing closed on any
// resolve error (including "does not exist") — stricter than
// ValidateMountSource, since a drive's host root asserts a share is mounted
// HERE, not merely a path being looked at.
func UserDriveHostRootCheck(roots []string) func(hostRoot string) error {
	return func(hostRoot string) error {
		if len(roots) == 0 {
			return fmt.Errorf("this deployment sets no WARDYN_USER_DRIVE_HOST_ROOTS, so no host_path drive may be authored " +
				"(set it to the mount point of the share, then re-save this drive)")
		}
		// The same host bind-mount deny-list every authored source runs (policy
		// half of the two-layer guardrail; the driver re-checks at bind time).
		// Rewritten to name host_root, since the drive editor has no "mount
		// source" field — the frozen wording is pinned in
		// docs/design/user-drives-prompt.md §7.1.
		if err := ValidateMountSource(hostRoot); err != nil {
			if p := DeniedSourcePrefix(hostRoot); p != "" {
				return fmt.Errorf("host_root %q is under a denied prefix (%s) — the same deny list every host bind obeys", hostRoot, p)
			}
			return err
		}
		real, err := filepath.EvalSymlinks(filepath.Clean(hostRoot))
		if err != nil {
			return fmt.Errorf("host_root %q could not be resolved on this host (a drive's host root must be a directory that exists here): %w", hostRoot, err)
		}
		// The member rule's dotfile deny-list, on the RESOLVED path (.ssh, .aws,
		// .claude, .kube, .config/gh, …) — ValidateMountSource above denies whole
		// system trees but says nothing about a credential dir inside an
		// ordinary home.
		if seg := deniedUserSegment(real); seg != "" {
			return fmt.Errorf("host_root %q resolves to %q, which is or traverses %q — a credential directory is never a drive's host root",
				hostRoot, real, seg)
		}
		if !withinAnyRoot(real, roots) {
			// Frozen wording (docs/design/user-drives-prompt.md); names the
			// resolved path too when it differs from what the admin typed.
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
// drive is actually bound from (this principal's subdirectory), as opposed to
// the authored host_root UserDriveHostRootCheck decides about.
//
// Adds one rule beyond the shared check: the resolved source must be a STRICT
// subdirectory of a root, never the root itself — a source that resolved TO
// the root would bind the whole share into one sandbox, which only a bug (a
// home name resolving to "." or a symlink back to its parent) could produce.
//
// Returns the resolved real path (not just error) so the one caller that needs
// it — the Docker driver — doesn't re-resolve the same path. On error the path
// is always "".
func UserDriveMountSourceCheck(roots []string) func(source string) (string, error) {
	within := UserDriveHostRootCheck(roots)
	return func(source string) (string, error) {
		if err := within(source); err != nil {
			return "", err
		}
		real, err := filepath.EvalSymlinks(filepath.Clean(source))
		if err != nil {
			// Unreachable in practice (`within` already resolved this path),
			// kept as a fail-closed guard against a resolve that stops working
			// mid-flight.
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

// UserDriveHomeWithinItsRoot asserts that real — the symlink-resolved directory
// a host_path drive is about to be bound from — is a STRICT SUBDIRECTORY of
// hostRoot, THIS drive's own root, resolved the same way.
//
// The deployment ceiling (UserDriveMountSourceCheck) only bounds the bind to
// the union of every configured root, not to which root THIS drive was
// authored against — so on a deployment with two share drives, a home replaced
// host-side by a link to the same-named home under a DIFFERENT drive's root
// would pass every other check and bind the wrong drive's tree. The per-drive
// root closes that gap; a drive must satisfy both bounds.
//
// An empty hostRoot is a REFUSAL, not a skip: types.DriveMount.HostRoot is set
// from the resolved row for every host_path drive, so "" means the mount was
// built by something that doesn't know this field (an older control plane, or
// a caller that assembled a SandboxSpec directly) — never the standalone
// runner's -spec JSON, which cannot produce a drive at all
// (TestLoadSpec_CannotProduceADrive).
//
// STRICT: a source resolved TO the root is refused here too, since a check
// stated only once is a check a refactor can drop.
//
// The returned error names the drive and directory only (DriveSubject); the
// host_root, real path, and resolve error go to slog for the operator.
func UserDriveHomeWithinItsRoot(drive *types.DriveMount, real string) error {
	if drive == nil {
		return fmt.Errorf("user drive: this mount carries no drive to be contained by")
	}
	who := DriveSubject(drive)
	if strings.TrimSpace(drive.HostRoot) == "" {
		slog.Warn("wardyn: user drive: a share mount carried no host_root, so it could not be bounded to its own drive's tree",
			slog.String("drive", drive.DriveName), slog.String("home", drive.HomeName), slog.String("real_path", real))
		return fmt.Errorf("user drive: %s carries no host_root to be contained by — a share drive's own root is what "+
			"bounds it to one drive's tree, and the deployment ceiling alone would allow another drive's", who)
	}
	root := filepath.Clean(drive.HostRoot)
	resolved, err := filepath.EvalSymlinks(root)
	if err != nil {
		slog.Warn("wardyn: user drive: this drive's host_root could not be resolved on this host",
			slog.String("drive", drive.DriveName), slog.String("home", drive.HomeName),
			slog.String("host_root", drive.HostRoot), slog.String("err", err.Error()))
		return fmt.Errorf("user drive: %s cannot be bound — this drive's host_root is not a directory that exists on this "+
			"host, so there is nothing to contain it", who)
	}
	root = resolved
	if real == root {
		slog.Warn("wardyn: user drive: the bind source resolved to this drive's own host_root",
			slog.String("drive", drive.DriveName), slog.String("home", drive.HomeName),
			slog.String("host_root", root), slog.String("real_path", real))
		return fmt.Errorf("user drive: %s resolves to this drive's host_root itself — a drive binds one person's "+
			"subdirectory of a share, never the share itself (every other person's home is under it)", who)
	}
	if !withinAnyRoot(real, []string{root}) {
		slog.Warn("wardyn: user drive: the bind source resolved outside this drive's own host_root",
			slog.String("drive", drive.DriveName), slog.String("home", drive.HomeName),
			slog.String("host_root", root), slog.String("real_path", real))
		return fmt.Errorf("user drive: %s resolves outside this drive's own host_root — the deployment's ceiling allows "+
			"that tree for SOME drive, but a home replaced by a link into another drive's root would bind that drive's "+
			"directory instead of this one's", who)
	}
	return nil
}

// DriveSubject names a mount the way a member-facing refusal may: which drive,
// whose directory, and NO PATH. A mount carrying no drive name (older control
// plane, or a directly-built spec) names the directory alone — never the
// object, which on a share IS the operator's absolute path.
//
// Exported so the driver's own refusals (RefuseUserDriveBind, docker's drive
// arm) compose the same sentence rather than drift on what may be disclosed.
func DriveSubject(drive *types.DriveMount) string {
	switch {
	case drive == nil:
		return "this drive"
	case drive.DriveName == "":
		return fmt.Sprintf("directory %q", drive.HomeName)
	default:
		return fmt.Sprintf("drive %q, directory %q", drive.DriveName, drive.HomeName)
	}
}

// The MEMBER halves of the two bind refusals the Docker driver makes about a
// share's source. Constants rather than literals at the call sites so the
// audience rule is checkable in one place: neither may name a path, and
// RefuseUserDriveBind's own test asserts they do not.
const (
	// DriveSourceRefused covers every way the source check can refuse the
	// resolved path; which reason applies is an operator-filesystem fact, so
	// it goes to the log, not here.
	DriveSourceRefused = "cannot be bound on this host: the directory this deployment resolved for it is not one a drive may bind here — " +
		"wardynd's log names the rule that refused it, and an operator can fix it"
	// DriveHomeNameRefused is the sibling-symlink case; the path it would name
	// is another principal's home directory.
	DriveHomeNameRefused = "resolves to a directory named after somebody else — a home replaced by a link to a sibling " +
		"would bind another person's directory"
)

// RefuseUserDriveBind is the ONE place a DRIVER-side refusal about a share's
// bind source becomes an error, and it splits the audience in two (same shape
// as k8s.refuseForbiddenDriveClaim).
//
// The OPERATOR gets the paths in a log line (source, resolved real path, and
// the underlying check's sentence). The MEMBER gets only drive, directory, and
// `reason` — `reason` carries no path, since it becomes the run's
// failure_hint verbatim and is read by whoever launched the run.
//
// Field NAMES are kept (host_root, source) — a field name is not a value.
func RefuseUserDriveBind(drive *types.DriveMount, source, real, reason string, cause error) error {
	attrs := []any{slog.String("source", source)}
	if drive != nil {
		attrs = append(attrs, slog.String("drive", drive.DriveName), slog.String("home", drive.HomeName),
			slog.String("host_root", drive.HostRoot))
	}
	if real != "" {
		attrs = append(attrs, slog.String("real_path", real))
	}
	if cause != nil {
		attrs = append(attrs, slog.String("err", cause.Error()))
	}
	slog.Warn("wardyn: user drive: this share mount was refused at bind time", attrs...)
	return fmt.Errorf("user drive: %s %s", DriveSubject(drive), reason)
}
