// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build docker

package docker

import (
	"context"
	"fmt"
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
//   - Workspace mounts come ONLY from a policy's RunPolicySpec.WorkspaceMounts,
//     copied into spec.Mounts by internal/api dispatch. The create-run HTTP
//     request has no mounts field, so a prompt-injected agent / malicious
//     requester can NEVER choose a host mount (invariants 1 & 3).
//   - DENY-LIST DEFENSE-IN-DEPTH: even though the values came from policy
//     (already validated at policy-write time), we re-run runner.ValidateMount
//     here and FAIL CLOSED — any denied Source (/, /proc, /sys, /dev, /run,
//     /var/run, /var/lib/docker, any docker.sock, /etc, /boot, /root, or a
//     non-absolute/non-cleaned path) or a Target outside the allowed
//     in-container prefixes errors the whole CreateSandbox (rollback runs).
//   - DEFAULT READ-ONLY: a mount is read-only unless the policy explicitly set
//     ReadOnly=false, so a workspace bind cannot grant host write by default.
//   - MEMBER MOUNTS (memberRoots non-nil): a run against a member-owned
//     workspace additionally runs runner.ValidateMemberMountSource against the
//     operator/MDM-set roots resolved for that member — over the binds the
//     MEMBER authored (runner.Mount.MemberAuthored: that workspace's local_dir),
//     and only those. The same spec also carries operator/Wardyn-authored binds
//     (the subscription ~/.claude staging, the Bedrock ~/.aws dir), which live
//     under no member root by construction; gating those on the roots too broke
//     every model run against a member-owned workspace. It runs HERE, immediately
//     after ValidateMount and immediately before the bind is appended, for the
//     same reason ValidateMount is re-run here at all: a symlink that was benign
//     at onboarding can be repointed before the run, so the resolved-real-path
//     within-root assertion has to be the LAST thing before ContainerCreate.
//     nil memberRoots (every operator run) skips it entirely — the gate is
//     additive and never narrows an operator mount.
//   - USER DRIVE (spec.Drive non-nil): the acting principal's persistent
//     storage, appended LAST by driveMount below. It arrives as a
//     types.DriveMount rather than as an entry in specMounts — see
//     SandboxSpec.Drive — so nothing above has to learn about it, and a
//     host_path drive runs the SAME source deny matrix every other host bind
//     does, by way of the roots ceiling that composes it (driveMount).
func (d *Driver) agentMounts(ctx context.Context, spec runner.SandboxSpec) ([]mount.Mount, error) {
	specMounts, memberRoots := spec.Mounts, spec.MemberMountRoots
	var mounts []mount.Mount
	if d.cfg.RecordingMount != "" {
		// Cast delivery: wardyn-rec writes the finished recording to this
		// shared mount (-out-dir), where the control plane's FSStore reads it.
		mtype := mount.TypeVolume
		if strings.HasPrefix(d.cfg.RecordingMount, "/") {
			mtype = mount.TypeBind
			// A host-path RecordingMount is a real host bind: subject its SOURCE to
			// the same deny-list as workspace binds and FAIL CLOSED (a source that
			// is/traverses /, /proc, docker.sock, ... must never be bound in). Only
			// binds are validated — a named volume is Docker-managed, not a host path
			// (mirrors how the workspace-bind loop treats host paths). Source half
			// only: the target is RecordingMountTarget, a fixed Wardyn-owned path
			// (never attacker-chosen), not a workspace-prefix target.
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
		if err := runner.ValidateMount(m); err != nil {
			return nil, fmt.Errorf("docker: denied workspace mount %q -> %q: %w", m.Source, m.Target, err)
		}
		if memberRoots != nil && m.MemberAuthored {
			if err := runner.ValidateMemberMountSource(m.Source, memberRoots); err != nil {
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
	// are the final thing that happens before the caller hands this slice to
	// ContainerCreate (the member gate's own argument, one object up).
	driveMounts, err := d.driveMount(ctx, spec.Drive)
	if err != nil {
		return nil, err
	}
	return append(mounts, driveMounts...), nil
}

// driveMount converts SandboxSpec.Drive into the ZERO OR ONE container mount
// that backs it, per backend. A nil drive — every run that did not ask for one,
// which is most of them — returns nothing and touches no daemon state.
//
// The two backends are different MECHANISMS, not two configurations of one:
//
//   - docker_volume (MANAGED): a per-person named volume, created on first use
//     (ensureDriveVolume, driver_volumes.go) and mounted as mount.TypeVolume.
//     There is no host path, so the bind deny-list has nothing to deny.
//   - host_path (SHARE): a bind of THIS PERSON's subdirectory of a tree the
//     OPERATOR mounted host-side (fstab/systemd `mount.cifs -o credentials=…`
//     or nfs). It runs runner.UserDriveHostRootCheck, which is the host bind
//     deny-list (ValidateMountSource) AND the deployment's
//     WARDYN_USER_DRIVE_HOST_ROOTS ceiling on the symlink-resolved real path,
//     fail-closed — the same two-layer shape a member mount has, for the same
//     reason: the row was validated when an admin wrote it, and a symlink that
//     was benign then can be re-pointed before this run.
//
// ONE PATH, NO FLAG. Both checks run for every drive that reaches the matching
// arm — there is no provenance flag deciding whether the ceiling applies, and
// there must never be one: a gate whose only false state is "somebody stopped
// setting the stamp" fails OPEN when that refactor lands.
//
// A KUBERNETES BACKEND IS AN ERROR, NEVER A SKIP. types.ValidateUserDrive
// refuses a k8s backend on a Docker deployment at the write boundary, so a
// k8s_pvc drive reaching this driver means the deployment's runner target
// changed under a stored row. Mounting nothing would hand the member a sandbox
// with no drive and no explanation — and, for a WRITABLE drive, an agent
// happily writing a session's work into a directory that dies with the run.
func (d *Driver) driveMount(ctx context.Context, drive *types.DriveMount) ([]mount.Mount, error) {
	if drive == nil {
		return nil, nil
	}
	// EVERY backend, before the switch: the in-container target is a place a
	// mount can land whatever backs it. ValidateTarget — never
	// ValidateAuthoredTarget, which refuses runner.DriveTarget by design: the
	// reserved-target rule exists to stop a HUMAN naming this path, and this
	// mount is the one thing the reservation is FOR.
	if err := runner.ValidateTarget(drive.Target); err != nil {
		return nil, fmt.Errorf("docker: denied user drive %q -> %q: %w", drive.ObjectName, drive.Target, err)
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
		// The SHARE bind, converted here — the one place it happens, and the
		// whole reason runner.Mount.DriveAuthored exists. The stamp is a
		// PROVENANCE LABEL on the wire and NOTHING GATES ON IT: the ceiling
		// below runs on every host_path drive unconditionally. Gating it on the
		// stamp read as defence but was fail-OPEN by shape — the flag's only
		// false state is a refactor that drops the stamp, and the failure mode
		// of that refactor would be a share bound with NO ceiling at all.
		m := runner.Mount{
			// The resolver's already-derived <host_root>/<home>. The driver does
			// NOT re-join a root and a home name: deriving a path twice, in two
			// packages, from two copies of the rules is how the second copy ends
			// up pointing somewhere the first would have refused.
			Source:        drive.ObjectName,
			Target:        drive.Target,
			ReadOnly:      drive.ReadOnly,
			DriveAuthored: true,
		}
		// ONE call, not two: UserDriveHostRootCheck runs runner.ValidateMountSource
		// itself (the full host bind deny-list) before resolving symlinks and
		// asserting the real path is inside the deployment's roots, so calling
		// runner.ValidateMount here as well would run the source half twice and
		// leave two places for the matrix to drift apart.
		//
		// UNSET ROOTS REFUSE EVERY host_path DRIVE, and that arm lives inside
		// runner.UserDriveHostRootCheck rather than as a `len(roots) > 0` test
		// here, so the driver and the API write boundary cannot drift on what
		// "the operator has not said where" means.
		if err := runner.UserDriveHostRootCheck(d.cfg.UserDriveHostRoots)(m.Source); err != nil {
			return nil, fmt.Errorf("docker: denied user drive mount %q -> %q: %w", m.Source, m.Target, err)
		}
		return []mount.Mount{{
			Type:     mount.TypeBind,
			Source:   m.Source,
			Target:   m.Target,
			ReadOnly: m.ReadOnly,
		}}, nil

	default:
		return nil, fmt.Errorf("docker: user drive %q has backend %q, which this runner cannot mount "+
			"(the docker runner mounts %q and %q; a Kubernetes-backed drive belongs to a Kubernetes deployment)",
			drive.ObjectName, drive.Backend, types.DriveBackendDockerVolume, types.DriveBackendHostPath)
	}
}
