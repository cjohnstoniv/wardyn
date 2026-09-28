// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package runner

import (
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// Host bind-mount guardrails (SECURITY-CRITICAL).
//
// SECURITY: mounts into a sandbox are OPERATOR/ADMIN-controlled, never
// agent-controlled. A mount reaches a sandbox only via
// RunPolicySpec.WorkspaceMounts, authored on a stored policy or an admin's
// inline create-run request; the in-sandbox agent has no access to either
// authoring surface, so a prompt-injected agent can never pick a host path.
//
// Even so, DENY-LIST defense-in-depth: ValidateMount is enforced BOTH at
// policy-write/inline-validate time (a bad policy is a 400) AND in the docker
// driver at sandbox-create time (a bad mount fails CreateSandbox closed), the
// same function backing every call site so they can never drift.
//
// Why these denials prevent a container escape / host compromise:
//   - "/" or any host-root-ish path exposes the entire host filesystem.
//   - /proc, /sys, /dev, /run, /var/run: kernel/device/runtime interfaces; a
//     writable bind is a direct path to host control (/dev/mem,
//     /sys/fs/cgroup release_agent, /proc/sysrq-trigger).
//   - /var/lib/docker and any docker.sock: root-equivalent on the host —
//     mounting it lets the sandbox launch privileged containers and escape.
//   - /etc, /boot, /root: host credentials, boot config, root's secrets/keys.
//   - A non-absolute or non-cleaned Source could smuggle traversal ("..") or
//     a relative path resolved against the daemon's CWD.
//   - A Target outside the allowed prefixes could shadow a system path
//     (e.g. mount over /usr or /etc inside the container).

// allowedTargetPrefixes are the only in-container locations a workspace mount
// may target, preventing a mount from shadowing a system path inside the
// sandbox (e.g. over /usr, /bin, /etc).
var allowedTargetPrefixes = []string{"/home/agent", "/work", "/workspace"}

// DriveTarget is the RESERVED in-container path a user drive mounts at, under
// /home/agent. Reserving it stops an authored workspace mount, repo, or
// source from landing on the same path and shadowing (or being shadowed by)
// the member's persistent storage — reserved rather than first-come, since
// the two are authored by different people at different times, and a
// collision would surface as silent disappearance rather than a refusal.
const DriveTarget = "/home/agent/drive"

// ScratchTmpPath, ScratchWorkPath and ScratchCachePath are where the
// Kubernetes substrate mounts its three disk_mib-sized emptyDirs, the bytes
// that substrate's `eviction` enforcement counts against a run's disk cap.
// Live here, not private to that driver, so the run page's disk-used reading
// walks exactly the paths the kubelet meters and the two can't drift.
const (
	ScratchTmpPath   = "/tmp"
	ScratchWorkPath  = "/home/agent/work"
	ScratchCachePath = "/home/agent/.cache"
)

// ValidateAuthoredTarget is ValidateTarget PLUS the reserved-target rule: what
// every AUTHORED in-container target goes through (a policy's
// workspace_mounts/workspace_repos entry, a workspace source's target).
//
// The split names a boundary, not a stricter mood: ValidateTarget answers "is
// this a legal place in the sandbox", which the drive's own mount must still
// pass; this answers "may a HUMAN name this place", which the drive's mount
// is exactly the exception to. Folding the reserved rule into ValidateTarget
// would make the drive fail its own validation.
func ValidateAuthoredTarget(tgt string) error {
	if err := ValidateTarget(tgt); err != nil {
		return err
	}
	if targetReservedForDrive(tgt) {
		// The frozen refusal canon (docs/design/user-drives-prompt.md's
		// REFUSED_TARGET_RESERVED). Path stays PLAIN, not backticked: a mono
		// span is a display concern the console applies, not bytes baked into
		// the string — with backticks the wire bytes and the canon couldn't match.
		return fmt.Errorf("target %s is reserved for the user drive", DriveTarget)
	}
	return nil
}

// targetReservedForDrive reports whether tgt IS the reserved drive target or
// nests under it. UNDER it counts: a mount at /home/agent/drive/shared would
// be bound inside a tree the drive owns, shadowing or shadowed by it
// depending on mount order — an ambiguity a refusal is cheaper than.
func targetReservedForDrive(tgt string) bool {
	return tgt == DriveTarget || strings.HasPrefix(tgt, DriveTarget+"/")
}

// deniedSourcePrefixes are host paths a bind-mount Source may neither equal
// nor live under (checked after path.Clean). SECURITY: each is a path whose
// exposure to the sandbox would hand it host-level control.
var deniedSourcePrefixes = []string{
	"/proc",
	"/sys",
	"/dev",
	"/run",
	"/var/run",
	"/var/lib/docker",
	"/var/lib/containerd", // containerd state — root-equivalent, same as /var/lib/docker
	"/etc",
	"/boot",
	"/root", // uid-0's home on a standard Linux host
}

// ValidateMount enforces the host bind-mount deny-list: nil for an allowed
// mount, a descriptive error otherwise, single source of truth shared by the
// policy validator and the docker driver.
//
// Rules (all fail closed):
//  1. Source must be a non-empty, absolute, already-cleaned path (path.Clean
//     is idempotent on it) — no "..", no relative path.
//  2. Source must not be "/" and must not equal or be nested under any
//     deniedSourcePrefixes entry.
//  3. Source must not reference a Docker socket (any path whose base is
//     docker.sock), wherever it lives.
//  4. Target must be a non-empty, absolute, cleaned path under one of
//     allowedTargetPrefixes.
func ValidateMount(m Mount) error {
	if err := ValidateMountSource(m.Source); err != nil {
		return err
	}
	return ValidateTarget(m.Target)
}

// ValidateMountSource enforces rules 1-3 above — the SOURCE half of
// ValidateMount, extracted so a surface that only vets a reusable host path
// can run the same deny-list without inventing a placeholder target. Source
// half ONLY: any surface that actually BINDS the path must call ValidateMount.
func ValidateMountSource(src string) error {
	if src == "" {
		return fmt.Errorf("mount source is empty")
	}
	if !path.IsAbs(src) {
		return fmt.Errorf("mount source %q must be an absolute path", src)
	}
	if path.Clean(src) != src {
		// Reject uncleaned paths (trailing slash, "..", "//", "/./") so a
		// traversal segment can never slip past the prefix checks below.
		return fmt.Errorf("mount source %q must be a cleaned path (got non-canonical form)", src)
	}
	if err := deniedSource(src); err != nil {
		return err
	}
	// SECURITY-CRITICAL symlink hardening: the checks above are LEXICAL only.
	// The daemon resolves symlinks SOURCE-SIDE, so a lexically clean, un-denied
	// source that IS (or traverses) a symlink to /, /etc, or a docker.sock dir
	// would still be bound RW into the agent — a potential escape. Resolve the
	// real path and re-run the SAME deny-list against it, fail-closed.
	if real, err := filepath.EvalSymlinks(src); err == nil {
		if derr := deniedSource(real); derr != nil {
			return derr
		}
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("mount source %q could not be resolved: %w", src, err)
	}
	// os.IsNotExist falls through to lexical-only on purpose: against a
	// REMOTE/VM dockerd this can't see the daemon's filesystem, so the
	// deny-list is advisory there (a known limitation — the daemon still
	// resolves it source-side). Residual: even locally this check and the
	// eventual ContainerCreate bind aren't atomic, so a host-write-access
	// actor could swap the source for a symlink between them (TOCTOU) —
	// narrow, since mounts are operator-authored, but defense-in-depth, not a
	// race-free guarantee.
	return nil
}

// ValidateTarget enforces the in-container mount/clone target shape: a
// non-empty, absolute, cleaned path under an allowedTargetPrefixes entry.
// Target half of ValidateMount, extracted so a surface placing something at
// an in-container path WITHOUT a host bind-mount source (e.g. a git-cloned
// WorkspaceRepo target) enforces the same invariant without duplicating the list.
func ValidateTarget(tgt string) error {
	if tgt == "" {
		return fmt.Errorf("mount target is empty")
	}
	if !path.IsAbs(tgt) {
		return fmt.Errorf("mount target %q must be an absolute path", tgt)
	}
	if path.Clean(tgt) != tgt {
		return fmt.Errorf("mount target %q must be a cleaned path (got non-canonical form)", tgt)
	}
	for _, p := range allowedTargetPrefixes {
		if tgt == p || strings.HasPrefix(tgt, p+"/") {
			return nil
		}
	}
	return fmt.Errorf("mount target %q must be under an allowed prefix (%s)", tgt, strings.Join(allowedTargetPrefixes, ", "))
}

// deniedSource runs the host bind-mount source deny-list (host root, docker
// socket, denied prefixes) against an ALREADY absolute+cleaned path. Shared by
// ValidateMount's lexical check on m.Source AND its symlink-resolved
// real-path check so the two can never drift.
func deniedSource(src string) error {
	if src == "/" {
		return fmt.Errorf("mount source %q (host root) is denied", src)
	}
	// SECURITY: a container-runtime socket anywhere is root-equivalent on the
	// host (mounting it lets the sandbox drive the daemon and escape). Denied
	// by BASENAME so a socket at a non-standard path is caught too.
	switch path.Base(src) {
	case "docker.sock", "containerd.sock", "podman.sock", "crio.sock":
		return fmt.Errorf("mount source %q references a container-runtime socket; denied", src)
	}
	if p := deniedPrefixOf(src); p != "" {
		return fmt.Errorf("mount source %q is under denied host path %q", src, p)
	}
	return nil
}

// deniedPrefixOf returns the deniedSourcePrefixes entry that refuses an
// ALREADY absolute+cleaned src, or "". Lives outside deniedSource so a caller
// that must NAME the prefix (in its own vocabulary) shares the list instead
// of re-deriving it.
func deniedPrefixOf(src string) string {
	for _, p := range deniedSourcePrefixes {
		if src == p || strings.HasPrefix(src, p+"/") {
			return p
		}
	}
	return ""
}

// DeniedSourcePrefix returns the deny-list prefix that refuses src, or "" for
// every OTHER refusal reason (empty, relative, uncleaned, host root, runtime
// socket).
//
// Exists for the one surface with its own FROZEN vocabulary for this refusal
// (a user drive's host_root, UserDriveHostRootCheck): naming the prefix lets
// it explain WHICH prefix bit without keeping a second copy of the list to
// drift out of sync.
//
// Answers on the same TWO paths deniedSource runs against — lexical and
// symlink-resolved — so a root resolving INTO a denied tree names the prefix
// that caught it. A resolve error is not this function's to report: it
// answers "" and the caller's own ValidateMountSource call says why.
func DeniedSourcePrefix(src string) string {
	if p := deniedPrefixOf(src); p != "" {
		return p
	}
	if real, err := filepath.EvalSymlinks(src); err == nil {
		return deniedPrefixOf(real)
	}
	return ""
}
