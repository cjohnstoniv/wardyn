// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build docker

package docker

import (
	"errors"
	"fmt"
	"strings"

	"github.com/cjohnstoniv/wardyn/internal/dockerutil"
)

// errRuntimeUnavailable is the fail-closed sentinel when a policy's required
// Confinement Class has no installed enforcing runtime; wrapped so callers
// can errors.Is it and the control plane refuses the run rather than
// silently downgrading (invariant 5).
var errRuntimeUnavailable = errors.New("required confinement runtime unavailable")

// errProxyImageUnset: CreateSandbox was asked to build the egress sidecar but
// no proxy image was configured on the driver.
var errProxyImageUnset = errors.New("wardyn-proxy image not configured")

// errCapsUnenforceable is the fail-closed sentinel when Docker cannot enforce
// the CPU/memory/pids caps a sandbox needs (an undelegated cgroup controller,
// classically cgroup v1 under rootless Docker). Override on a trusted host
// via WARDYN_ALLOW_UNENFORCEABLE_CAPS=1.
var errCapsUnenforceable = fmt.Errorf("resource caps not enforceable on this host: %w", dockerutil.ErrCapsDiscarded)

// errTeardownUnresolved: teardown removed the agent container but could not
// resolve its run id, so the sibling proxy/network can't be located —
// surfaced rather than reporting a false success.
var errTeardownUnresolved = errors.New("teardown could not resolve run id; sibling proxy/network may be orphaned")

// errDriveTargetInvalid is driveMount's refusal of a drive whose Target isn't
// the one reserved in-container bind path (parity with the Kubernetes
// driver's sentinel). A wrong Target would shadow a reserved path — e.g. the
// injected credential dir or the workspace — with persistent storage.
var errDriveTargetInvalid = errors.New("a user drive may bind only at the reserved drive path")

// pullFailure is ensureImage's error when an image is absent locally and
// unpullable. Only demo agent tags get the `make agent-images` hint
// appended, since they live in no registry; any other ref keeps PullImage's
// own wrap so the hint doesn't bury the real reason.
func pullFailure(ref string, err error) error {
	if !strings.HasPrefix(ref, demoAgentImagePrefix) {
		return err
	}
	return fmt.Errorf("%w (image %q not present locally and pull failed — for the demo images run: make agent-images)", err, ref)
}
