// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build docker

package docker

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/client"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// ProbeDrive implements runner.DriveProber: a short-lived container, run AS THE AGENT'S OWN UID
// rather than the daemon's root, tests whether a host_path drive's resolved directory is actually
// readable. An inline daemon-side os.Stat can't establish this — the daemon runs as root, which
// can read a directory the agent's uid 1000 cannot.
//
// Scoped to host_path: a docker_volume is Docker's own managed object (never chowned away from
// default by ensureDriveVolume), so there's nothing worth inspecting — DriveProbeUnknown is the
// honest answer here, not a guessed pass.
func (d *Driver) ProbeDrive(ctx context.Context, drive types.DriveMount) (runner.DriveProbe, error) {
	if drive.Backend != types.DriveBackendHostPath {
		return runner.DriveProbe{Result: runner.DriveProbeUnknown,
			Detail: fmt.Sprintf("backend %q has no host path to probe", drive.Backend)}, nil
	}
	if drive.ObjectName == "" {
		return runner.DriveProbe{}, errors.New("docker: probe drive: empty host path")
	}

	image := d.driveProbeImage()
	// A presence check, never a pull: this runs inside driveShareProbe's
	// bounded budget, and a cold registry pull would burn it on the first
	// request instead of PrewarmImages' background one.
	present, err := d.imagePresent(ctx, image)
	if err != nil {
		return runner.DriveProbe{}, fmt.Errorf("docker: probe drive: %w", err)
	}
	if !present {
		return runner.DriveProbe{}, fmt.Errorf("docker: probe drive: image %q not present locally (PrewarmImages pulls it in the background)", image)
	}

	created, err := d.cli.ContainerCreate(ctx, client.ContainerCreateOptions{
		Name: "wardyn-drive-probe-" + uuid.New().String(),
		Config: &container.Config{
			Image: image,
			// The agent image's own contract (uid 1000), not the daemon's root.
			User: driveProbeUser,
			// -r AND -x: a directory must be both readable and searchable to
			// be usable. $1, not an interpolated literal, closes off a
			// shell-injection seam.
			Cmd: []string{"sh", "-c", `test -r "$1" && test -x "$1"`, "sh", driveProbeTarget},
		},
		HostConfig: &container.HostConfig{
			// No network, no capabilities, no privilege escalation: this
			// container's only job is one stat-shaped syscall pair.
			NetworkMode:    "none",
			CapDrop:        []string{"ALL"},
			SecurityOpt:    []string{"no-new-privileges"},
			ReadonlyRootfs: true,
			AutoRemove:     false, // removed explicitly below
			Resources:      proxyResources(false),
			Mounts: []mount.Mount{{
				Type:     mount.TypeBind,
				Source:   drive.ObjectName,
				Target:   driveProbeTarget,
				ReadOnly: true,
			}},
		},
	})
	if err != nil {
		return runner.DriveProbe{}, fmt.Errorf("docker: probe drive: create: %w", err)
	}
	defer func() {
		// Background, not ctx: the caller's bound may already be exhausted
		// by the time the probe answers, and a leak is worse than cleanup
		// outliving the request.
		_, _ = d.cli.ContainerRemove(context.Background(), created.ID, client.ContainerRemoveOptions{Force: true})
	}()

	if _, err := d.cli.ContainerStart(ctx, created.ID, client.ContainerStartOptions{}); err != nil {
		return runner.DriveProbe{}, fmt.Errorf("docker: probe drive: start: %w", err)
	}

	wait := d.cli.ContainerWait(ctx, created.ID, client.ContainerWaitOptions{Condition: container.WaitConditionNotRunning})
	select {
	case res := <-wait.Result:
		if res.Error != nil {
			return runner.DriveProbe{}, fmt.Errorf("docker: probe drive: wait: %s", res.Error.Message)
		}
		if res.StatusCode == 0 {
			return runner.DriveProbe{Result: runner.DriveProbeReadable}, nil
		}
		return runner.DriveProbe{Result: runner.DriveProbeUnreadable,
			Detail: fmt.Sprintf("probe exited %d", res.StatusCode)}, nil
	case werr := <-wait.Error:
		return runner.DriveProbe{}, fmt.Errorf("docker: probe drive: wait: %w", werr)
	case <-ctx.Done():
		return runner.DriveProbe{}, ctx.Err()
	}
}

// driveProbeUser is the fixed uid:gid every wardyn agent image runs as,
// never the daemon's root. driveProbeTarget is the throwaway probe
// container's own bind target, internal to this file (never the reserved
// runner.DriveTarget the real agent mounts at).
const (
	driveProbeUser   = "1000:1000"
	driveProbeTarget = "/wardyn-probe"
	// defaultDriveProbeImage is the busybox-class placeholder ProbeDrive runs
	// `sh`/`test` in when Config.DriveProbeImage is unset. Pinned by digest,
	// not `:latest`: it's bind-mounted with a host directory on the request
	// path, so a floating tag would be one registry push (or MITM) away from
	// running something other than busybox against that mount. Refresh by
	// re-pulling `busybox:latest` and updating the digest.
	defaultDriveProbeImage = "busybox@sha256:cac8f90bbee42dc962a6b38bb1a235948d070385bb9d996bba15a6db8d364008"
)

func (d *Driver) driveProbeImage() string {
	if d.cfg.DriveProbeImage != "" {
		return d.cfg.DriveProbeImage
	}
	return defaultDriveProbeImage
}
