// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build docker

package docker

import (
	"context"
	"log/slog"

	"github.com/moby/moby/client"
)

// awsCacheWipeScript deletes the two caches where AWS clients in the sandbox keep what they
// derive from the person's sign-in: ~/.aws/cli/cache (the CLI's role credentials) and
// ~/.aws/sso/cache (the SSO token cache Wardyn writes and the CLI rewrites). The glob first
// empties a cache that is a link elsewhere, then the link or directory itself goes.
const awsCacheWipeScript = `h="${HOME:-/home/agent}"; rm -rf "$h/.aws/cli/cache/"* "$h/.aws/cli/cache" "$h/.aws/sso/cache"`

// awsCacheWipeArgv runs the wipe after SIGKILLing every other process the sandbox user owns, so
// no AWS client writes a cache back between the wipe and the stop. PID 1 survives (the kernel
// drops a signal its own namespace sends it without a handler), and the stop that follows ends
// it; the processes killed here are the ones its exit would SIGKILL a moment later anyway.
var awsCacheWipeArgv = []string{"sh", "-c", "kill -9 -1 2>/dev/null; " + awsCacheWipeScript}

// wipeAWSCaches runs awsCacheWipeArgv in the agent ref as its own user before a kept end stops
// it: the one terminal path whose container (writable layer and all) outlives the run. Every
// other path removes the container, and Kubernetes has no kept end. Best effort: a sandbox the
// exec cannot reach (paused, already stopped) is logged and the stop goes ahead, since
// containment outranks the wipe.
func (d *Driver) wipeAWSCaches(ctx context.Context, ref string) {
	created, err := d.cli.ExecCreate(ctx, ref, client.ExecCreateOptions{Cmd: awsCacheWipeArgv})
	if err == nil {
		_, err = d.cli.ExecStart(ctx, created.ID, client.ExecStartOptions{})
	}
	if err != nil {
		if !isNotFound(err) {
			slog.WarnContext(ctx, "wardynd: could not wipe the AWS credential caches before keeping an ended sandbox",
				slog.String("ref", ref), slog.Any("err", err))
		}
		return
	}
	d.waitExec(ctx, created.ID)
}
