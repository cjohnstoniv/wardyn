// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build docker

package docker

import (
	"bytes"
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/moby/moby/api/pkg/stdcopy"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"

	"github.com/cjohnstoniv/wardyn/internal/runner"
)

const (
	// diskProbeTimeout covers create, start, wait and the log read.
	diskProbeTimeout    = 20 * time.Second
	diskProbeCleanup    = 10 * time.Second
	componentDiskProbe  = "disk-probe"
	diskProbeNamePrefix = "wardyn-disk-probe-"
)

// ProbeDiskFreeMiB is the free space, in MiB, a container on this engine sees at /: on a VM-backed
// engine that is the VM's own disk, which no path on this host names. It runs the proxy image's
// -disk-free and never pulls: the caller makes the image present first.
func (d *Driver) ProbeDiskFreeMiB(ctx context.Context) (int64, error) {
	image := d.cfg.ProxyImage
	present, err := d.imagePresent(ctx, image)
	if err != nil {
		return 0, fmt.Errorf("docker: probe disk: %w", err)
	}
	if !present {
		return 0, fmt.Errorf("docker: probe disk: proxy image %q is not present locally", image)
	}

	ctx, cancel := context.WithTimeout(ctx, diskProbeTimeout)
	defer cancel()

	name := diskProbeNamePrefix + uuid.New().String()
	labels := map[string]string{labelManaged: "true", labelComponent: componentDiskProbe}
	if d.cfg.RunnerID != "" {
		labels[runner.LabelRunnerID] = d.cfg.RunnerID // lets a sweeper claim a stray
	}
	remove := func(id string) {
		rctx, rcancel := context.WithTimeout(context.Background(), diskProbeCleanup)
		defer rcancel()
		_, _ = d.cli.ContainerRemove(rctx, id, client.ContainerRemoveOptions{Force: true})
	}
	created, err := d.cli.ContainerCreate(ctx, client.ContainerCreateOptions{
		Name: name,
		Config: &container.Config{
			Image:      image,
			Entrypoint: []string{proxyEntrypointPath},
			Cmd:        []string{"-disk-free", "/"},
			Labels:     labels,
		},
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
		return 0, fmt.Errorf("docker: probe disk: create: %w", err)
	}
	defer remove(created.ID)

	if _, err := d.cli.ContainerStart(ctx, created.ID, client.ContainerStartOptions{}); err != nil {
		return 0, fmt.Errorf("docker: probe disk: start: %w", err)
	}
	wait := d.cli.ContainerWait(ctx, created.ID, client.ContainerWaitOptions{Condition: container.WaitConditionNotRunning})
	select {
	case res := <-wait.Result:
		if res.Error != nil {
			return 0, fmt.Errorf("docker: probe disk: wait: %s", res.Error.Message)
		}
		if res.StatusCode != 0 {
			return 0, fmt.Errorf("docker: probe disk: the proxy image %s did not report its free disk (exit %d); it must be a wardyn-proxy with -disk-free", image, res.StatusCode)
		}
	case werr := <-wait.Error:
		return 0, fmt.Errorf("docker: probe disk: wait: %w", werr)
	case <-ctx.Done():
		return 0, fmt.Errorf("docker: probe disk: no result within %s: %w", diskProbeTimeout, ctx.Err())
	}
	rc, err := d.cli.ContainerLogs(ctx, created.ID, client.ContainerLogsOptions{ShowStdout: true, Tail: "5"})
	if err != nil {
		return 0, fmt.Errorf("docker: probe disk: read result: %w", err)
	}
	defer rc.Close()
	var out bytes.Buffer
	if _, err := stdcopy.StdCopy(&out, &out, rc); err != nil {
		return 0, fmt.Errorf("docker: probe disk: read result: %w", err)
	}
	mib, err := strconv.ParseInt(strings.TrimSpace(out.String()), 10, 64)
	if err != nil || mib <= 0 {
		return 0, fmt.Errorf("docker: probe disk: unreadable result %q", strings.TrimSpace(out.String()))
	}
	return mib, nil
}
