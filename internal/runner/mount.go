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
// Mounts into a sandbox are OPERATOR/ADMIN-controlled, never agent-controlled. A
// mount reaches a sandbox only via a RunPolicySpec.WorkspaceMounts, authored
// EITHER on a stored policy (admin-gated policy CRUD) OR inline on a create-run
// request by an admin / SSO-gated human operator (createRunRequest.InlinePolicy);
// internal/api dispatch copies the resolved policy's mounts into
// SandboxSpec.Mounts. The in-sandbox agent has no access to either authoring
// surface (the agent-run entrypoint never sees this request body), so a
// prompt-injected agent can never pick a host path.
//
// Even so, we DENY-LIST defense-in-depth: ValidateMount is enforced BOTH at
// policy-write / inline-validate time (validatePolicySpec, so a bad policy or
// inline spec is a 400) AND in the docker driver at sandbox-create time (so a
// bad mount fails the CreateSandbox closed). The same function backs every call
// site, so they can never drift. ValidateMount itself is UNCHANGED by the inline
// feature.
//
// Why these denials prevent a container escape / host compromise:
//   - "/" or any host-root-ish path would expose the entire host filesystem.
//   - /proc, /sys, /dev, /run, /var/run: kernel/device/runtime interfaces; a
//     writable bind of these is a direct path to host control (e.g. /dev/mem,
//     /sys/fs/cgroup release_agent, /proc/sysrq-trigger).
//   - /var/lib/docker and any docker.sock: the Docker socket/state is root-
//     equivalent on the host — mounting it lets the sandbox launch privileged
//     containers and escape trivially.
//   - /etc, /boot, /root, and uid-0's home (/root): host credentials, boot
//     config, and the root account's secrets/keys.
//   - A non-absolute or non-cleaned Source could smuggle traversal ("..") or a
//     relative path resolved against the daemon's CWD.
//   - A Target outside the allowed in-container prefixes could shadow system
//     paths (e.g. mount over /usr or /etc inside the container).

// allowedTargetPrefixes are the only in-container locations a workspace mount may
// target. Restricting the target prevents a mount from shadowing a system path
// inside the sandbox (e.g. over /usr, /bin, /etc).
var allowedTargetPrefixes = []string{"/home/agent", "/work", "/workspace"}

// deniedSourcePrefixes are host paths a bind-mount Source may neither equal nor
// live under (checked after path.Clean). Each is a path whose exposure to the
// sandbox would hand it host-level control.
var deniedSourcePrefixes = []string{
	"/proc",
	"/sys",
	"/dev",
	"/run",
	"/var/run",
	"/var/lib/docker",
	"/var/lib/containerd", // containerd state — root-equivalent, same as /var/lib/docker (M13)
	"/etc",
	"/boot",
	"/root", // uid-0's home on a standard Linux host
}

// ValidateMount enforces the host bind-mount deny-list. It returns nil for an
// allowed mount and a descriptive error (suitable for an HTTP 400 or a
// fail-closed CreateSandbox error) for any denied one. It is the single source
// of truth shared by the policy validator and the docker driver.
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
	src := m.Source
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
	// Symlink hardening (SECURITY-CRITICAL): the checks above are LEXICAL only.
	// The daemon resolves symlinks SOURCE-SIDE, so a lexically clean, un-denied
	// source that IS (or traverses) a symlink to /, /etc, or a dir holding
	// docker.sock would still be bound RW into the agent — a potential escape.
	// Resolve the real path and re-run the SAME deny-list against it, fail-closed.
	if real, err := filepath.EvalSymlinks(src); err == nil {
		if derr := deniedSource(real); derr != nil {
			return derr
		}
	} else if !os.IsNotExist(err) {
		// Any error other than "does not exist" is fail-closed.
		return fmt.Errorf("mount source %q could not be resolved: %w", src, err)
	}
	// ponytail: os.IsNotExist falls through to lexical-only on purpose. When
	// wardynd talks to a REMOTE/VM dockerd it cannot see the daemon's
	// filesystem, so a source that legitimately exists only on the daemon host
	// is unresolvable here; the deny-list is advisory for a remote daemon (a
	// known, documented limitation — the daemon still resolves it source-side).
	// Residual: even for a LOCAL daemon this validate step and the eventual
	// ContainerCreate bind are not atomic, so an actor with host write access
	// could swap the source for a symlink between them (TOCTOU). It is narrow —
	// mounts are operator-authored, not agent-chosen, and the driver binds via
	// the Mounts API without CreateMountpoint, so a source missing at create
	// fails daemon-side — but the resolve here is defense-in-depth, not a
	// race-free guarantee.

	return ValidateTarget(m.Target)
}

// ValidateTarget enforces the in-container mount/clone target shape: a
// non-empty, absolute, cleaned path under one of allowedTargetPrefixes. It is
// the target half of ValidateMount, extracted so another authoring surface
// that places something at an in-container path WITHOUT a host bind-mount
// source (e.g. a git-cloned WorkspaceRepo target) can enforce the same
// invariant without duplicating the prefix list.
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
// ValidateMount's lexical check on m.Source AND its symlink-resolved real-path
// check so the two can never drift.
func deniedSource(src string) error {
	if src == "/" {
		return fmt.Errorf("mount source %q (host root) is denied", src)
	}
	// A container-runtime socket anywhere is root-equivalent on the host: mounting
	// it lets the sandbox drive the daemon (launch privileged containers → escape).
	// Denied by BASENAME so a socket at a non-standard path (outside the denied
	// /run, /var/run prefixes) is caught too — docker, containerd, podman, cri-o
	// (M13: previously only docker.sock was named).
	switch path.Base(src) {
	case "docker.sock", "containerd.sock", "podman.sock", "crio.sock":
		return fmt.Errorf("mount source %q references a container-runtime socket; denied", src)
	}
	for _, p := range deniedSourcePrefixes {
		if src == p || strings.HasPrefix(src, p+"/") {
			return fmt.Errorf("mount source %q is under denied host path %q", src, p)
		}
	}
	return nil
}
