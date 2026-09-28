// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build docker

package docker

import (
	"context"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"

	"github.com/moby/moby/api/types/mount"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// Agent-container mount assembly, including the member root gate that refuses
// any MemberAuthored mount outside its canonicalized roots.

// agentMounts assembles every mount on the agent container: the recording-cast
// delivery mount, then the operator/policy workspace mounts. Any denied
// source/target FAILS CLOSED, aborting CreateSandbox and its rollback.
//
// SECURITY MODEL:
//   - Workspace mounts come ONLY from RunPolicySpec.WorkspaceMounts — the
//     create-run request has no mounts field, so a prompt-injected agent or
//     malicious requester can never choose a host mount.
//   - Deny-list defense-in-depth: re-runs the authoring surface's checks
//     (ValidateMountSource, ValidateAuthoredTarget) even on already-validated
//     policy values, fail-closed. Catches rows predating the drive's
//     reserved-target rule.
//   - DEFAULT READ-ONLY unless the policy explicitly set ReadOnly=false.
//   - MEMBER MOUNTS (memberRoots non-nil): additionally runs
//     ValidateUserMountSource against operator/MDM-set roots, but only for
//     binds the MEMBER authored — checked right before the bind is appended,
//     since a symlink benign at onboarding can be repointed before the run.
//   - USER DRIVE (spec.Drive non-nil): appended LAST by driveMount, arriving
//     as a types.DriveMount rather than a specMounts entry.
//
// rroSupported is passed in (not re-derived) so it can't disagree with the
// `docker info` call CreateSandbox already resolved.
func (d *Driver) agentMounts(ctx context.Context, spec runner.SandboxSpec, rroSupported bool) ([]mount.Mount, error) {
	specMounts, memberRoots := spec.Mounts, spec.UserMountRoots
	var mounts []mount.Mount
	if d.cfg.RecordingMount != "" {
		// Cast delivery: wardyn-rec writes the finished recording here, where
		// the control plane's FSStore reads it.
		mtype := mount.TypeVolume
		if strings.HasPrefix(d.cfg.RecordingMount, "/") {
			mtype = mount.TypeBind
			// Host-path RecordingMount is a real host bind: subject its source to
			// the workspace deny-list, fail closed. Target is a fixed Wardyn-owned
			// path, never attacker-chosen.
			if err := runner.ValidateMountSource(d.cfg.RecordingMount); err != nil {
				return nil, fmt.Errorf("docker: denied recording mount %q -> %q: %w", d.cfg.RecordingMount, RecordingMountTarget, err)
			}
		}
		mounts = append(mounts, mount.Mount{
			Type:   mtype,
			Source: d.cfg.RecordingMount,
			Target: RecordingMountTarget,
		})
	}

	// Operator/policy-controlled host bind mounts (e.g. a host repo at ~/work).
	for _, m := range specMounts {
		// ValidateMountSource + ValidateAuthoredTarget, deliberately NOT
		// ValidateMount: every mount here was AUTHORED by a human, and the
		// reserved-target rule is a property of the target namespace — a policy
		// row predating that rule could otherwise land inside the member's
		// drive. The drive's own mount never enters this loop (rides
		// SandboxSpec.Drive via driveMount instead), so this stays strict.
		if err := runner.ValidateMountSource(m.Source); err != nil {
			return nil, fmt.Errorf("docker: denied workspace mount %q -> %q: %w", m.Source, m.Target, err)
		}
		if err := runner.ValidateAuthoredTarget(m.Target); err != nil {
			return nil, fmt.Errorf("docker: denied workspace mount %q -> %q: %w", m.Source, m.Target, err)
		}
		if memberRoots != nil && m.MemberAuthored {
			if err := runner.ValidateUserMountSource(m.Source, memberRoots); err != nil {
				return nil, fmt.Errorf("docker: denied member workspace mount %q -> %q: %w", m.Source, m.Target, err)
			}
		}
		mounts = append(mounts, mount.Mount{
			Type:     mount.TypeBind,
			Source:   m.Source,
			Target:   m.Target,
			ReadOnly: m.ReadOnly, // Go zero value false == RW only when policy opted in
		})
	}

	// User drive LAST, so its host-path resolved-real-path checks run right
	// before ContainerCreate.
	driveMounts, err := d.driveMount(ctx, spec.Drive, rroSupported)
	if err != nil {
		return nil, err
	}
	return append(mounts, driveMounts...), nil
}

// driveMount converts SandboxSpec.Drive into the ZERO OR ONE container mount
// that backs it, per backend. A nil drive returns nothing and touches no
// daemon state.
//
// The two backends are different MECHANISMS, not two configurations of one:
//   - docker_volume (MANAGED): a per-person named volume, created on first use
//     and mounted as mount.TypeVolume. No host path, so the bind deny-list has
//     nothing to deny.
//   - host_path (SHARE): a bind of this person's subdirectory of an
//     operator-mounted host tree. Runs UserDriveHostRootCheck (host bind
//     deny-list plus WARDYN_USER_DRIVE_HOST_ROOTS ceiling on the
//     symlink-resolved real path, fail-closed), plus two checks the
//     deployment ceiling alone can't make: the resolved path must be inside
//     THIS drive's own host_root, and still named after this principal's home.
//
// Both checks run unconditionally for every host_path drive — no provenance
// flag gates them, since a gate whose only false state is "somebody stopped
// setting the stamp" fails open once that stamp bitrots.
//
// A Kubernetes backend is an error, never a skip: types.ValidateUserDrive
// refuses it on a Docker deployment at the write boundary, so reaching here
// means the deployment's runner target changed under a stored row.
func (d *Driver) driveMount(ctx context.Context, drive *types.DriveMount, rroSupported bool) ([]mount.Mount, error) {
	if drive == nil {
		return nil, nil
	}
	// A drive binds at the RESERVED path or not at all (equality, not prefix):
	// a DriveMount addressed to /home/agent/.claude, /home/agent, or /work
	// would mount the member's volume on top of credential staging, home, or
	// workspace. Nothing in the control plane can reach this today, but an
	// in-process caller building SandboxSpec directly has no other validating
	// boundary. Named by DriveSubject, never drive.ObjectName: this refusal
	// becomes the run's failure_hint verbatim, and on a SHARE the object name
	// is the operator's absolute share path.
	if drive.Target != runner.DriveTarget {
		return nil, fmt.Errorf("docker: denied user drive (%s) -> %q: %w (%q)",
			runner.DriveSubject(drive), drive.Target, errDriveTargetInvalid, runner.DriveTarget)
	}
	// Still checked as a path via ValidateTarget (never ValidateAuthoredTarget,
	// which refuses this path by design — the reservation exists to stop a
	// human naming it, and this mount is what it's FOR). Guards the CONSTANT:
	// if DriveTarget were ever edited somewhere no mount may land, this
	// refuses rather than binding a member's storage there.
	if err := runner.ValidateTarget(drive.Target); err != nil {
		return nil, fmt.Errorf("docker: denied user drive (%s) -> %q: %w", runner.DriveSubject(drive), drive.Target, err)
	}
	switch drive.Backend {
	case types.DriveBackendDockerVolume:
		// Target only, checked above: a named volume is Docker-managed, not a
		// host path, so there's no source to deny.
		if err := ensureDriveVolume(ctx, d.cli, drive); err != nil {
			return nil, err
		}
		return []mount.Mount{{
			Type:     mount.TypeVolume,
			Source:   drive.ObjectName,
			Target:   drive.Target,
			ReadOnly: drive.ReadOnly,
		}}, nil

	case types.DriveBackendHostPath:
		// The SHARE bind, converted here only. DriveAuthored is a driver-local
		// provenance label only (never serialized, nothing gates on it) — the
		// ceiling below runs on every host_path drive unconditionally.
		m := runner.Mount{
			Source:        drive.ObjectName, // resolver's already-derived <host_root>/<home>
			Target:        drive.Target,
			ReadOnly:      drive.ReadOnly,
			DriveAuthored: true,
		}
		// ONE call: UserDriveMountSourceCheck already runs the host bind and
		// dotfile deny-lists before resolving symlinks against the deployment's
		// roots (calling ValidateMount too would run the source half twice).
		// Uses the MOUNT check, not the authoring one: a bind's source must be
		// a STRICT SUBDIRECTORY of a root, while an authored host_root
		// legitimately IS one. Unset roots refuse every host_path drive inside
		// the check itself, not a local `len(roots) > 0` test, so this and the
		// API write boundary can't drift on what "no roots" means.
		real, err := runner.UserDriveMountSourceCheck(d.cfg.UserDriveHostRoots)(m.Source)
		if err != nil {
			return nil, fmt.Errorf("docker: denied user drive mount -> %q: %w", m.Target,
				runner.RefuseUserDriveBind(drive, m.Source, "", runner.DriveSourceRefused, err))
		}
		// AND inside THIS drive's own root, not merely some configured one:
		// the deployment ceiling can't distinguish one share's tree from
		// another's, so a home symlinked to a same-named home under a
		// different root would otherwise bind the wrong drive. The env/MDM
		// ceiling and the drive's own host_root are both load-bearing —
		// dropping either opens a real hole. An absent host_root is a
		// refusal, not a skip. Both bounds assert the symlink-resolved path
		// AT CHECK TIME; ContainerCreate resolves again itself, so a
		// host-side attacker repointing the home in between is a known TOCTOU
		// (residual #25/#35) this check narrows but does not close.
		if err := runner.UserDriveHomeWithinItsRoot(drive, real); err != nil {
			return nil, fmt.Errorf("docker: denied user drive mount -> %q: %w", m.Target, err)
		}
		// Resolved path must also still be THIS person's home (base name, not
		// full path — a share may nest homes in subdirectories): catches a
		// link that stays inside the drive's tree but lands on somebody
		// ELSE's home (`alice -> ../bob`), which the root check alone passes.
		if filepath.Base(real) != drive.HomeName {
			return nil, fmt.Errorf("docker: denied user drive mount -> %q: %w", m.Target,
				runner.RefuseUserDriveBind(drive, m.Source, real, runner.DriveHomeNameRefused, nil))
		}
		return []mount.Mount{{
			Type:        mount.TypeBind,
			Source:      m.Source,
			Target:      m.Target,
			ReadOnly:    m.ReadOnly,
			BindOptions: driveBindOptions(drive, m.ReadOnly, rroSupported),
		}}, nil

	default:
		// The SUBJECT again, not the object name: a member reading their own
		// failure hint has no use for it.
		return nil, fmt.Errorf("docker: user drive (%s) has backend %q, which this runner cannot mount "+
			"(the docker runner mounts %q and %q; a Kubernetes-backed drive belongs to a Kubernetes deployment)",
			runner.DriveSubject(drive), drive.Backend, types.DriveBackendDockerVolume, types.DriveBackendHostPath)
	}
}

// driveBindOptions decides whether the share bind asks for a recursively
// read-only mount, or carries no BindOptions at all.
//
// A bind's `ro` only reaches submounts from Linux 5.12 (mount_setattr's
// AT_RECURSIVE) — below that the kernel silently leaves submounts writable,
// exactly where a per-person home (autofs, a nested export) turns up.
// ReadOnlyForceRecursive makes the daemon ERROR instead of handing back that
// half-honoured mount.
//
// Asked only when the runtime declares the OCI `rro` option — the daemon
// REFUSES THE CREATE otherwise, and gVisor (the Wall tier's confinement
// floor) doesn't declare it, so asking unconditionally would fail every
// read-only share there. A writable bind never gets BindOptions.
//
// The loss is logged: an operator needing the recursive guarantee must run
// that drive's workloads on a runtime that declares `rro` (runc does).
func driveBindOptions(drive *types.DriveMount, readOnly, rroSupported bool) *mount.BindOptions {
	if !readOnly {
		return nil
	}
	if !rroSupported {
		slog.Warn("wardyn: user drive: this runtime does not support recursively read-only binds, so a submount under the share's home could be writable inside the sandbox",
			slog.String("drive", drive.DriveName), slog.String("home", drive.HomeName),
			slog.String("mount_option", ociMountOptionRRO))
		return nil
	}
	return &mount.BindOptions{ReadOnlyForceRecursive: true}
}
