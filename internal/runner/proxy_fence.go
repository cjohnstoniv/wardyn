// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package runner

import (
	"context"
	"errors"
)

// ErrProxyFenced refuses a proxy replace for a run whose agent is cut off its networks (the run was contained).
var ErrProxyFenced = errors.New("runner: the run's proxy is fenced")

// ErrProxyGenerationNewer refuses a proxy replace or stop that names a policy generation older than the one
// the running proxy was started for; nothing is touched.
var ErrProxyGenerationNewer = errors.New("runner: the run's proxy belongs to a newer policy generation")

// ProxyFencer is an OPTIONAL Runner capability: cut a contained run's agent off every network it is
// attached to, so no egress is possible from it. Idempotent. A runner that cannot cut the agent off
// returns an error and leaves the sandbox as it was.
type ProxyFencer interface {
	FenceProxy(ctx context.Context, ref string) error
}

// ProxyGenerationStopper is an OPTIONAL Runner capability: StopProxy and EndSandbox that name the policy
// generation the caller acts for. A call naming a generation older than the one the running proxy was
// started for is refused with ErrProxyGenerationNewer and touches nothing. gen 0 means none: the call
// behaves as its plain twin.
type ProxyGenerationStopper interface {
	StopProxyAt(ctx context.Context, ref string, gen int) error
	EndSandboxAt(ctx context.Context, ref string, gen int) error
}

// ProxyReviverAt is an OPTIONAL Runner capability: ProxyReviverHashed's replace that names the policy
// generation the caller acts for. A generation older than the one the running proxy was started for is
// refused with ErrProxyGenerationNewer and touches nothing. gen 0 means none: the call behaves as
// ReplaceProxyHashed.
type ProxyReviverAt interface {
	ReplaceProxyHashedAt(ctx context.Context, ref string, cfgJSON []byte, gen int) (string, error)
}
