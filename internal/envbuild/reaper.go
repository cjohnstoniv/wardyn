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

// The boot-time orphan sweep for envbuild build containers: AutoRemove is
// deliberately off (hardenedHostConfig), so a container whose owning process
// (wardynd) crashed before its in-process defer ran needs a separate reaper.

// envbuildContainerLabel names every build container this package creates, so
// one orphaned by a crashed/restarted process can be found and reaped by
// SweepOrphanedBuilds.
const envbuildContainerLabel = "wardyn.envbuild"

// liveBuildTracker tracks in-flight build container IDs (Builder.liveBuilds)
// so SweepOrphanedBuilds never races a build this same process is actively
// running. Zero value is ready to use.
type liveBuildTracker struct{ m sync.Map }

// track marks id as in-flight for the duration of the returned untrack call.
// Called as `defer b.liveBuilds.track(containerID)()` in runBuildAndFinalize.
func (t *liveBuildTracker) track(id string) (untrack func()) {
	t.m.Store(id, struct{}{})
	return func() { t.m.Delete(id) }
}

func (t *liveBuildTracker) isLive(id string) bool {
	_, ok := t.m.Load(id)
	return ok
}

// envbuilderListerAPI is the narrow docker-client slice SweepOrphanedBuilds
// needs beyond envbuilderDockerAPI's own ContainerRemove.
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
// liveBuilds alone cannot prove a labeled container safe to destroy — other
// wardynd instances sharing this daemon label containers this process's
// liveBuilds has no entry for, and ContainerCreate lands on the daemon before
// this process's own tracking defer runs — so the age gate (mirroring
// undispatchedGrace in internal/api/reconcile.go) is the real safety net:
// only a container older than any build could legitimately take is reaped.
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
	// 2x timeout: a bare 1x window risks reaping a build that is merely slow.
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
