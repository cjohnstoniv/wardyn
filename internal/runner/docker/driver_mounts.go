// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build docker

package docker

import (
	"fmt"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/moby/moby/api/types/mount"
	"strings"
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
func (d *Driver) agentMounts(specMounts []runner.Mount, memberRoots []string) ([]mount.Mount, error) {
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
	return mounts, nil
}
