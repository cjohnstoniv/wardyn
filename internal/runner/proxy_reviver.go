// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package runner

import (
	"context"
	"errors"
)

// ProxyReviver is an OPTIONAL Runner capability: replace a sandbox's proxy
// sidecar, running or stopped, with a new one while the agent keeps running.
// The control plane reads the run's stored config (#1176), rewrites its token
// and denies, and hands it to ReplaceProxy.
//
// CanReplaceProxy answers ErrReviveUnsupported when ref's substrate cannot
// replace a proxy in place. ReplaceProxy removes the old proxy, then starts
// the new one at the address the agent's hosts entry pins. An error wrapping
// ErrProxyReplaceFailed means the old proxy is, or may be, gone; any other
// error left it untouched.
type ProxyReviver interface {
	CanReplaceProxy(ctx context.Context, ref string) error
	ReplaceProxy(ctx context.Context, ref string, cfgJSON []byte) error
	// EnsureProxyImage pulls the proxy sidecar image if not already present.
	// Called BEFORE the revive claim so a slow pull doesn't widen the window
	// a watcher sweep must leave the run alone in.
	EnsureProxyImage(ctx context.Context) error
}

// ProxyReviverHashed is ReplaceProxy that also answers proxy.ConfigSHA256 of the exact bytes the
// new proxy was handed, which that proxy reports back once it loads them. An empty hash is a runner
// that predates it.
type ProxyReviverHashed interface {
	ReplaceProxyHashed(ctx context.Context, ref string, cfgJSON []byte) (string, error)
}

// ErrReviveUnsupported is ProxyReviver's answer from a router whose substrate
// for ref cannot replace a proxy in place.
var ErrReviveUnsupported = errors.New("runner: this substrate cannot replace a sandbox's proxy")

// ErrProxyReplaceFailed marks a ReplaceProxy that removed, or may have
// removed, the old proxy and did not start the new one.
var ErrProxyReplaceFailed = errors.New("runner: the old proxy may be gone and the new one did not start")
