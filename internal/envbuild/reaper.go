// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build docker

package envbuild

import (
	"context"
	"errors"
	"fmt"
	"sync"

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
// (liveBuilds) — the residue of a wardynd crash or restart mid-build. A cli
// that doesn't implement envbuilderListerAPI (a narrower test fake) is simply
// not swept.
//
// Meant to be called once at boot, before this process has started any build
// of its own — wired into api.Server.ReconcileOnBoot via the optional
// api.ImageBuildSweeper capability (see cmd/wardynd/envbuild_docker.go) — so
// in practice every labeled container found here is an orphan; liveBuilds is
// still consulted so a concurrent or future call can never tear down a build
// this process is itself running.
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
	var errs []error
	for _, c := range res.Items {
		if b.liveBuilds.isLive(c.ID) {
			continue
		}
		if _, rerr := b.cli.ContainerRemove(ctx, c.ID, client.ContainerRemoveOptions{Force: true}); rerr != nil {
			errs = append(errs, fmt.Errorf("envbuild: remove orphaned build container %s: %w", c.ID, rerr))
		}
	}
	return errors.Join(errs...)
}
