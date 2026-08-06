// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build docker

package envbuild

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/moby/moby/client"
)

// reaper.go — the boot-time orphan sweep for envbuild build containers. A
// build container's AutoRemove is deliberately off (hardenedHostConfig), so
// its only other cleanup is the in-process defer in runBuildAndFinalize,
// which cannot run once its owning process (a crashed or restarted wardynd)
// is gone. envbuildContainerLabel + SweepOrphanedBuilds are the label-and-
// reap pair that closes that gap.

// envbuildContainerLabel names every build container this package creates, so
// one orphaned by a crashed/restarted process can be found and reaped by
// SweepOrphanedBuilds. The value is the build's outputTag, which for a
// workspace build is itself workspace-scoped ("wardyn-workspace/<id>:...",
// see resolveWorkspaceImage in package api), so a stray container's origin is
// identifiable from `docker inspect` alone.
const envbuildContainerLabel = "wardyn.envbuild"

// liveBuildTracker tracks in-flight build container IDs (Builder.liveBuilds)
// so SweepOrphanedBuilds never races a build this same process is actively
// running. Zero value is ready to use.
type liveBuildTracker struct{ m sync.Map }

// track marks id as in-flight for the duration of the returned untrack call.
// Called as `defer b.liveBuilds.track(containerID)()` in runBuildAndFinalize:
// the Store happens immediately (defer evaluates the outer call's operand
// right away), and only the returned Delete is deferred.
func (t *liveBuildTracker) track(id string) (untrack func()) {
	t.m.Store(id, struct{}{})
	return func() { t.m.Delete(id) }
}

func (t *liveBuildTracker) isLive(id string) bool {
	_, ok := t.m.Load(id)
	return ok
}

// envbuilderListerAPI is the narrow docker-client slice SweepOrphanedBuilds
// needs beyond envbuilderDockerAPI's own ContainerRemove — kept separate
// (rather than widening the main seam every other Builder method shares) so
// a docker API client need only grow ContainerList the day something besides
// the reaper wants to list containers.
type envbuilderListerAPI interface {
	ContainerList(ctx context.Context, options client.ContainerListOptions) (client.ContainerListResult, error)
}

// the real client must implement it.
var _ envbuilderListerAPI = (*client.Client)(nil)

// SweepOrphanedBuilds force-removes every build container carrying
// envbuildContainerLabel that this process has no live build tracked for
// (liveBuilds) AND is older than twice the build timeout — the residue of a
// wardynd crash or restart mid-build. A cli that doesn't implement
// envbuilderListerAPI (a narrower test fake) is simply not swept.
//
// Meant to be called once at boot, before this process has started any build
// of its own — wired into api.Server.ReconcileOnBoot via the optional
// api.ImageBuildSweeper capability (see cmd/wardynd/envbuild_docker.go).
//
// liveBuilds ALONE is not enough to prove a labeled container is safe to
// destroy: the label is written by every wardynd sharing this docker daemon
// (a supported configuration, docs/ENV.md), whose in-flight builds this
// process's liveBuilds has no entry for — and even for this process,
// ContainerCreate lands the container on the daemon before the caller's
// `defer b.liveBuilds.track(id)()` runs. The age gate is the actual safety
// net, mirroring undispatchedGrace's reasoning (internal/api/reconcile.go):
// only a container older than any build could legitimately still be running
// is assumed abandoned. There is no cheap per-process identity in the
// container metadata today (the label VALUE is the output tag, not an
// instance id) to additionally skip a different-but-still-alive instance's
// young builds — which is exactly what the age gate already does, so age
// alone is the guard, not merely the must-have half of one.
func (b *Builder) SweepOrphanedBuilds(ctx context.Context) error {
	lister, ok := b.cli.(envbuilderListerAPI)
	if !ok {
		return nil
	}
	res, err := lister.ContainerList(ctx, client.ContainerListOptions{
		All:     true, // exited containers leak too: AutoRemove is off.
		Filters: client.Filters{}.Add("label", envbuildContainerLabel),
	})
	if err != nil {
		return fmt.Errorf("envbuild: list containers for orphan sweep: %w", err)
	}
	timeout := b.BuildTimeout
	if timeout <= 0 {
		timeout = defaultBuildTimeout
	}
	// Doubling timeout is the same deliberately blunt margin undispatchedGrace
	// uses: a build container's age at sweep time can already approach timeout
	// on the happy path (create -> pull -> run), so a bare 1x window risks
	// reaping a build that is merely slow, whether that build is this process's
	// own (started moments before a crash) or another instance's. Being late
	// costs one more grace period of a stray container; being early tears down
	// a live one.
	cutoff := time.Now().Add(-2 * timeout)
	var errs []error
	for _, c := range res.Items {
		if b.liveBuilds.isLive(c.ID) {
			continue
		}
		if time.Unix(c.Created, 0).After(cutoff) {
			continue // too young to call orphaned yet
		}
		if _, rerr := b.cli.ContainerRemove(ctx, c.ID, client.ContainerRemoveOptions{Force: true}); rerr != nil {
			errs = append(errs, fmt.Errorf("envbuild: remove orphaned build container %s: %w", c.ID, rerr))
		}
	}
	return errors.Join(errs...)
}
