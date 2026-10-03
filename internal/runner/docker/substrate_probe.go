// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build docker

package docker

import (
	"context"
	"errors"
	"os"

	"github.com/containerd/errdefs"
	"github.com/moby/moby/client"

	"github.com/cjohnstoniv/wardyn/internal/runner"
)

// ProbeSubstrate implements runner.SubstrateProber with a daemon ping: it
// proves the socket answers without listing or creating anything. A daemon the
// process may not talk to (a socket permission error) is forbidden, a refused
// credential on a remote daemon is unauthorized, and everything else, a ctx that
// ended included, is unreachable.
func (d *Driver) ProbeSubstrate(ctx context.Context) runner.SubstrateState {
	_, err := d.cli.Ping(ctx, client.PingOptions{})
	switch {
	case err == nil:
		return runner.SubstrateOK
	case ctx.Err() != nil:
		return runner.SubstrateUnreachable
	case errdefs.IsUnauthorized(err):
		return runner.SubstrateUnauthorized
	case errdefs.IsPermissionDenied(err), errors.Is(err, os.ErrPermission):
		return runner.SubstrateForbidden
	default:
		return runner.SubstrateUnreachable
	}
}
