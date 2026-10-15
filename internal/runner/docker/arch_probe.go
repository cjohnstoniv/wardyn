// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build docker

package docker

import (
	"context"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"

	"github.com/cjohnstoniv/wardyn/internal/runner"
)

const (
	componentArchProbe  = "arch-probe"
	archProbeNamePrefix = "wardyn-arch-probe-"
	archProbeUser       = "1000"
	// archProbeSetup bounds create, start, log read and removal around the wait.
	archProbeSetup = 15 * time.Second
	archProbeClean = 10 * time.Second
)

// archProbeWait is how long the probe container may run before the image is taken to start fine.
// A variable so a test need not wait it out.
var archProbeWait = 3 * time.Second

// probeExecFormat starts ref once under its own entrypoint, with no network, no mounts and the
// hardened settings, and reports whether it proved the engine cannot run it: the container
// exited and its log says "exec format error", or the start itself failed with it (crun reports
// it from the start call; runc and crun differ in case, so the match ignores case). Every other
// outcome, including a probe that failed to run, proves nothing and reports false.
func (d *Driver) probeExecFormat(ctx context.Context, ref string) bool {
	ctx, cancel := context.WithTimeout(ctx, archProbeSetup+archProbeWait)
	defer cancel()

	name := archProbeNamePrefix + uuid.New().String()
	labels := map[string]string{labelManaged: "true", labelComponent: componentArchProbe}
	if d.cfg.RunnerID != "" {
		labels[runner.LabelRunnerID] = d.cfg.RunnerID // lets a sweeper claim a stray
	}
	remove := func(id string) {
		rctx, rcancel := context.WithTimeout(context.Background(), archProbeClean)
		defer rcancel()
		_, _ = d.cli.ContainerRemove(rctx, id, client.ContainerRemoveOptions{Force: true})
	}
	created, err := d.cli.ContainerCreate(ctx, client.ContainerCreateOptions{
		Name:   name,
		Config: &container.Config{Image: ref, User: archProbeUser, Labels: labels},
		HostConfig: &container.HostConfig{
			NetworkMode:    "none",
			CapDrop:        []string{"ALL"},
			SecurityOpt:    []string{"no-new-privileges"},
			ReadonlyRootfs: true,
			Resources:      proxyResources(false),
		},
	})
	if err != nil {
		remove(name) // a create that timed out client-side may still have made it
		return false
	}
	defer remove(created.ID)

	if _, err := d.cli.ContainerStart(ctx, created.ID, client.ContainerStartOptions{}); err != nil {
		return isExecFormat(err.Error())
	}
	wait := d.cli.ContainerWait(ctx, created.ID, client.ContainerWaitOptions{Condition: container.WaitConditionNotRunning})
	timer := time.NewTimer(archProbeWait)
	defer timer.Stop()
	select {
	case res := <-wait.Result:
		return res.Error == nil && isExecFormat(d.proxyExitLogTail(ctx, created.ID))
	case <-wait.Error:
	case <-timer.C: // still running: it started, so it can run
	case <-ctx.Done():
	}
	return false
}

func isExecFormat(s string) bool {
	return strings.Contains(strings.ToLower(s), "exec format error")
}
