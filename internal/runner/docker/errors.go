// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build docker

package docker

import "errors"

// errRuntimeUnavailable is the fail-closed sentinel for Confinement-Class
// gating: the policy demanded a class whose enforcing runtime is not
// installed. Wrapped (%w) so callers can errors.Is on it and the control
// plane can refuse the run rather than silently downgrade (invariant 5).
var errRuntimeUnavailable = errors.New("required confinement runtime unavailable")

// errProxyImageUnset is returned when CreateSandbox is asked to build the
// egress sidecar but no proxy image was configured on the driver.
var errProxyImageUnset = errors.New("wardyn-proxy image not configured")

// errCapsUnenforceable is the fail-closed sentinel for resource-cap gating: the
// Docker daemon reports it cannot enforce the CPU/memory/pids limits a sandbox
// needs (a cgroup controller is missing or not delegated — classically cgroup v1
// under rootless Docker). Wrapped (%w) so callers can errors.Is on it and refuse
// the run rather than launch an untrusted workload effectively uncapped. Override
// on a trusted host with WARDYN_ALLOW_UNENFORCEABLE_CAPS=1.
var errCapsUnenforceable = errors.New("resource caps not enforceable on this host")

// errTeardownUnresolved is returned when teardown removed the agent container but
// could not resolve its run id (neither the run-id label nor the deterministic
// agent container name), so the sibling proxy sidecar (routable network, run
// token) and per-run network cannot be located. Surfaced instead of reporting a
// false success — an orphaned proxy is a real leak, so teardown reports honestly.
var errTeardownUnresolved = errors.New("teardown could not resolve run id; sibling proxy/network may be orphaned")

// errDriveTargetInvalid is driveMount's refusal of a user drive whose Target is
// not runner.DriveTarget — the one in-container path a drive may ever bind at.
//
// PARITY WITH THE KUBERNETES DRIVER, which has refused exactly this since D4
// (k8s/errors.go's sentinel of the same name, raised by validateDriveMount).
// One rule, two substrates: a Target that is merely a legal mount point on one
// runner and the reserved path on the other is the same drift a shared constant
// exists to prevent.
//
// The target is the field with the least excuse for being trusted and the most
// to lose by it. Nobody authors it — the control plane copies the constant
// (user_drives_run.go's driveMountFor) — so a wrong one is never a typo to be
// forgiven, and it is the only field on a DriveMount whose wrong value puts the
// member's persistent, cross-run storage ON TOP of something else inside the
// sandbox rather than merely failing: `/home/agent/.claude` shadows the injected
// credential directory with a writable volume that survives the run, and
// `/home/agent` or `/work` shadow the home and the workspace. runner.
// ValidateTarget alone accepts all three, because they are legal mount points —
// which is the point: legal is not the same question as reserved.
var errDriveTargetInvalid = errors.New("a user drive may bind only at the reserved drive path")
