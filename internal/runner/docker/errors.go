// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build docker

package docker

import (
	"errors"
	"fmt"
	"strings"
)

// errRuntimeUnavailable is the fail-closed sentinel for Confinement-Class
// gating: the policy demanded a class whose enforcing runtime is not
// installed. Wrapped (%w) so callers can errors.Is on it and the control
// plane can refuse the run rather than silently downgrade (invariant 5).
var errRuntimeUnavailable = errors.New("required confinement runtime unavailable")

// errProxyImageUnset is returned when CreateSandbox is asked to build the
// egress sidecar but no proxy image was configured on the driver.
var errProxyImageUnset = errors.New("wardyn-proxy image not configured")

// errCapsUnenforceable is the fail-closed sentinel for resource-cap gating: the
// Docker daemon cannot enforce the CPU/memory/pids limits a sandbox needs (a
// cgroup controller missing or not delegated — classically cgroup v1 under
// rootless Docker). Override on a trusted host with
// WARDYN_ALLOW_UNENFORCEABLE_CAPS=1.
var errCapsUnenforceable = errors.New("resource caps not enforceable on this host")

// errTeardownUnresolved is returned when teardown removed the agent container
// but could not resolve its run id, so the sibling proxy sidecar and per-run
// network cannot be located — surfaced rather than reporting a false success.
var errTeardownUnresolved = errors.New("teardown could not resolve run id; sibling proxy/network may be orphaned")

// errDriveTargetInvalid is driveMount's refusal of a user drive whose Target is
// not runner.DriveTarget — the one in-container path a drive may ever bind at.
// Parity with the Kubernetes driver's sentinel of the same name. A wrong
// Target isn't a mere failure: it shadows a reserved in-container path (e.g.
// the injected credential dir or the workspace) with persistent storage.
var errDriveTargetInvalid = errors.New("a user drive may bind only at the reserved drive path")

// pullFailure is the error ensureImage returns when an image is absent locally
// AND unpullable. Only demo agent tags (demoAgentImagePrefix) get the
// `make agent-images` hint appended — they live in no registry, so the daemon's
// bare error is otherwise unhelpful. Any other ref keeps PullImage's own wrap
// unchanged, since the hint would not apply and would bury the real reason.
func pullFailure(ref string, err error) error {
	if !strings.HasPrefix(ref, demoAgentImagePrefix) {
		return err
	}
	return fmt.Errorf("%w (image %q not present locally and pull failed — for the demo images run: make agent-images)", err, ref)
}
