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

// Agent-container mount assembly, including the member root gate that
// refuses any MemberAuthored mount outside its canonicalized roots. Carved out
// of driver.go by seam (file-size gate).

// agentMounts assembles every mount attached to the agent container: the
// recording-cast delivery mount (driver config) followed by the operator/policy
// workspace mounts (specMounts), in that order. Any denied source/target FAILS
// CLOSED with an error, which aborts CreateSandbox and runs its rollback.
//
// SECURITY MODEL (documented here and in runner.SandboxSpec.Mounts):
//   - Workspace mounts come ONLY from a policy's RunPolicySpec.WorkspaceMounts;
//     the create-run HTTP request has no mounts field, so a prompt-injected
//     agent or malicious requester can never choose a host mount.
//   - Deny-list defense-in-depth: re-runs the SAME checks the authoring
//     surface ran (runner.ValidateMountSource, runner.ValidateAuthoredTarget)
//     even though the values came from already-validated policy, and fails
//     closed. The authored-target half catches what a plain runner.ValidateMount
//     would miss: rows predating the drive's reserved-target rule exist.
//   - DEFAULT READ-ONLY: a mount is read-only unless the policy explicitly set
//     ReadOnly=false.
//   - MEMBER MOUNTS (memberRoots non-nil): additionally runs
//     runner.ValidateUserMountSource against the operator/MDM-set roots, but
//     only over binds the MEMBER authored (Mount.MemberAuthored) — operator-
//     authored binds (subscription staging, the Bedrock ~/.aws dir) carry no
//     member root by construction. Runs immediately before the bind is
//     appended, since a symlink benign at onboarding can be repointed before
//     the run.
//   - USER DRIVE (spec.Drive non-nil): appended LAST by driveMount. It arrives
//     as a types.DriveMount rather than an entry in specMounts, so nothing
//     above has to learn about it; a host_path drive runs the same source deny
//     matrix via the roots ceiling that composes it.
//
// rroSupported is the daemon's own answer for the runtime this container will
// run on; only driveMount consults it. Passed in rather than re-derived because
// a second `docker info` call could disagree with the one CreateSandbox already
// resolved.
func (d *Driver) agentMounts(ctx context.Context, spec runner.SandboxSpec, rroSupported bool) ([]mount.Mount, error) {
	specMounts, memberRoots := spec.Mounts, spec.UserMountRoots
	var mounts []mount.Mount
	if d.cfg.RecordingMount != "" {
		// Cast delivery: wardyn-rec writes the finished recording to this
		// shared mount (-out-dir), where the control plane's FSStore reads it.
		mtype := mount.TypeVolume
		if strings.HasPrefix(d.cfg.RecordingMount, "/") {
			mtype = mount.TypeBind
			// A host-path RecordingMount is a real host bind: subject its SOURCE to
			// the same deny-list as workspace binds and FAIL CLOSED. Only binds are
			// validated (a named volume is Docker-managed, not a host path); the
			// target (RecordingMountTarget) is a fixed Wardyn-owned path, never
			// attacker-chosen.
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
		// runner.ValidateMount: every mount here was AUTHORED by a human, and the
		// reserved-target rule is a property of the target namespace, not of the
		// authoring surface — a policy row written before that rule existed could
		// otherwise land inside the member's drive and shadow or be shadowed by it.
		//
		// The drive's own mount does not come through this loop (it rides
		// SandboxSpec.Drive via driveMount), which is what lets this half stay
		// strict without failing the drive's own validation.
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
			ReadOnly: m.ReadOnly, // default false in Go == RW only when policy opted in via ReadOnly=false
		})
	}

	// The user drive, LAST — so its host-path arm's resolved-real-path checks
	// run as the final thing before ContainerCreate.
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
//     (ensureDriveVolume) and mounted as mount.TypeVolume. No host path, so
//     the bind deny-list has nothing to deny.
//   - host_path (SHARE): a bind of this person's subdirectory of a tree the
//     operator mounted host-side. Runs runner.UserDriveHostRootCheck (the host
//     bind deny-list plus the deployment's WARDYN_USER_DRIVE_HOST_ROOTS
//     ceiling on the symlink-resolved real path, fail-closed), plus two checks
//     the deployment ceiling can't make on its own: the resolved path must be
//     inside THIS drive's own host_root, and must still be named after this
//     principal's home.
//
// Both checks run for every host_path drive unconditionally — there is no
// provenance flag deciding whether the ceiling applies, since a gate whose
// only false state is "somebody stopped setting the stamp" fails open when
// that refactor lands.
//
// A Kubernetes backend is an error, never a skip: types.ValidateUserDrive
// refuses a k8s backend on a Docker deployment at the write boundary, so a
// k8s_pvc drive reaching this driver means the deployment's runner target
// changed under a stored row.
func (d *Driver) driveMount(ctx context.Context, drive *types.DriveMount, rroSupported bool) ([]mount.Mount, error) {
	if drive == nil {
		return nil, nil
	}
	// Every backend, before the switch and before any daemon call: a drive
	// binds at the RESERVED path or not at all (equality, not a prefix rule) —
	// a DriveMount addressed to /home/agent/.claude, /home/agent or /work
	// would mount the member's persistent volume on top of credential
	// staging, home, or the workspace.
	//
	// Nothing in the control plane can reach this today, but an in-process
	// caller assembling a runner.SandboxSpec directly (the struct and its
	// Drive field are exported) has no other validating boundary.
	//
	// Named by runner.DriveSubject, never drive.ObjectName: this refusal
	// becomes the run's failure_hint verbatim, and on a SHARE the object name
	// is the operator's absolute share path.
	if drive.Target != runner.DriveTarget {
		return nil, fmt.Errorf("docker: denied user drive (%s) -> %q: %w (%q)",
			runner.DriveSubject(drive), drive.Target, errDriveTargetInvalid, runner.DriveTarget)
	}
	// The reserved path is still checked as a PATH via ValidateTarget (never
	// ValidateAuthoredTarget, which refuses this path by design — the
	// reservation exists to stop a human naming it, and this mount is what it's
	// FOR). This is now an assertion about the CONSTANT: if runner.DriveTarget
	// were ever edited to somewhere no mount may land, this driver refuses
	// rather than binding a member's storage there.
	if err := runner.ValidateTarget(drive.Target); err != nil {
		return nil, fmt.Errorf("docker: denied user drive (%s) -> %q: %w", runner.DriveSubject(drive), drive.Target, err)
	}
	switch drive.Backend {
	case types.DriveBackendDockerVolume:
		// Target only, checked above: a named volume is Docker-managed, not a
		// host path, so the source half has nothing to deny (the same split
		// ValidateMountSource/ValidateTarget already make for the recording
		// mount).
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
		// The SHARE bind, converted here — the only place it happens.
		// DriveAuthored is a driver-local provenance label only (never
		// serialized, nothing gates on it): the ceiling below runs on every
		// host_path drive unconditionally regardless of the stamp.
		m := runner.Mount{
			// The resolver's already-derived <host_root>/<home>; the driver
			// never re-joins root+home itself, so there's one copy of that
			// rule to drift.
			Source:        drive.ObjectName,
			Target:        drive.Target,
			ReadOnly:      drive.ReadOnly,
			DriveAuthored: true,
		}
		// ONE call: UserDriveMountSourceCheck already runs the full host bind
		// deny-list plus the dotfile deny-list before resolving symlinks
		// against the deployment's roots — calling runner.ValidateMount here
		// too would run the source half twice.
		//
		// The MOUNT check, not the authoring one: a bind's source must be a
		// STRICT SUBDIRECTORY of a root, while an authored host_root
		// legitimately IS one — a source resolving to the root would bind the
		// whole share into one member's sandbox.
		//
		// Unset roots refuse every host_path drive; that arm lives inside
		// runner.UserDriveHostRootCheck itself, not a `len(roots) > 0` test
		// here, so the driver and the API write boundary can't drift on what
		// "no roots configured" means.
		//
		// The wrapper names the TARGET, never m.Source; the refusal it wraps
		// names the drive and directory for the log — this becomes the run's
		// failure_hint.
		real, err := runner.UserDriveMountSourceCheck(d.cfg.UserDriveHostRoots)(m.Source)
		if err != nil {
			return nil, fmt.Errorf("docker: denied user drive mount -> %q: %w", m.Target,
				runner.RefuseUserDriveBind(drive, m.Source, "", runner.DriveSourceRefused, err))
		}
		// AND inside THIS drive's own root, not merely some configured one:
		// the ceiling is the operator's outer bound over every drive at once
		// and can't distinguish one share's tree from another's, so a home
		// symlinked to a same-named home under a different root would
		// otherwise pass every check above and bind the wrong drive.
		//
		// Both bounds are deliberate and distinct: the env/MDM ceiling stops a
		// console-compromised admin row from naming a tree the operator never
		// allowed; the drive's own host_root (a DB row) can only narrow inside
		// it. Dropping either opens a real hole.
		//
		// An absent host_root is a refusal, not a skip: "" means the mount was
		// built by something that doesn't carry the field.
		//
		// Both bounds assert the symlink-resolved path AT CHECK TIME;
		// ContainerCreate resolves again itself, so a host-side attacker
		// repointing the home in between is THREAT-MODEL residual #25/#35's
		// TOCTOU — this check runs as late as possible but does not close
		// that window.
		if err := runner.UserDriveHomeWithinItsRoot(drive, real); err != nil {
			return nil, fmt.Errorf("docker: denied user drive mount -> %q: %w", m.Target, err)
		}
		// The resolved path must also still be this person's home: this
		// catches a link that stays inside the drive's tree but lands on
		// somebody ELSE's home (`alice -> ../bob`), which the root check
		// alone would pass.
		//
		// Asserted on the BASE NAME, not the whole path, since a share may
		// legitimately nest homes in subdirectories; a home symlinked onto a
		// different (even configured) export is no longer supported — that
		// layout instead means giving the drive the root its homes actually
		// live under.
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
		// The SUBJECT again, not the object name: a k8s_pvc_static row reaching
		// this driver still carries a name, and a member reading their own
		// failure hint has no use for it.
		return nil, fmt.Errorf("docker: user drive (%s) has backend %q, which this runner cannot mount "+
			"(the docker runner mounts %q and %q; a Kubernetes-backed drive belongs to a Kubernetes deployment)",
			runner.DriveSubject(drive), drive.Backend, types.DriveBackendDockerVolume, types.DriveBackendHostPath)
	}
}

// driveBindOptions decides whether the share bind asks the daemon to make its
// read-only RECURSIVE, or carries no BindOptions at all.
//
// A bind's `ro` only reaches submounts from Linux 5.12 (mount_setattr's
// AT_RECURSIVE); below that the kernel silently leaves submounts writable —
// exactly where a share's per-person home (an autofs home, a nested export)
// turns up. ReadOnlyForceRecursive makes the daemon ERROR instead of handing
// back that half-honoured mount.
//
// Asked only when the runtime declares the OCI `rro` mount option — the daemon
// REFUSES THE CREATE otherwise. gVisor (the Wall tier's own shipped
// confinement floor) does not declare it, so asking unconditionally would fail
// every read-only share on that runtime at ContainerCreate. A writable bind
// gets no BindOptions either way, since the flag asserts a property only a
// read-only mount has.
//
// The loss is logged on the run it affects: an operator needing the recursive
// guarantee must run that drive's workloads on a runtime that declares `rro`
// (runc does); nothing else here can deliver it.
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
