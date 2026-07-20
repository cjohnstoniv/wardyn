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
