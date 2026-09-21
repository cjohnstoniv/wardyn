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

// ProbeDrive implements runner.DriveProber (#165): a short-lived container,
// run AS THE AGENT'S OWN UID rather than the daemon's own root process, tests
// whether a host_path drive's resolved directory is actually readable. This is
// the one fact an inline daemon-side os.Stat can never establish — the daemon
// runs as root, and root can read a directory the agent's uid 1000 cannot —
// so a share readable only by root passed create, preflight and /me and only
// failed once the run was already inside the sandbox.
//
// Scoped to host_path: a docker_volume is Docker's own managed object,
// created (never chowned away from the default) by ensureDriveVolume, so
// there is nothing on this host worth spinning a container to inspect —
// DriveProbeUnknown is the honest answer, not a guessed pass.
func (d *Driver) ProbeDrive(ctx context.Context, drive types.DriveMount) (runner.DriveProbe, error) {
	if drive.Backend != types.DriveBackendHostPath {
		return runner.DriveProbe{Result: runner.DriveProbeUnknown,
			Detail: fmt.Sprintf("backend %q has no host path to probe", drive.Backend)}, nil
	}
	if drive.ObjectName == "" {
		return runner.DriveProbe{}, errors.New("docker: probe drive: empty host path")
	}

	image := d.driveProbeImage()
	// ensureImage pulls the probe image on first use, exactly as CreateSandbox
	// does for the agent/proxy images — a missing image is a genuine failure to
	// run the probe (caller falls back to "cannot tell"), not a refusal.
	if err := d.ensureImage(ctx, image, nil); err != nil {
		return runner.DriveProbe{}, fmt.Errorf("docker: probe drive: %w", err)
	}

	created, err := d.cli.ContainerCreate(ctx, client.ContainerCreateOptions{
		Name: "wardyn-drive-probe-" + uuid.New().String(),
		Config: &container.Config{
			Image: image,
			// The agent image's own contract (uid 1000) — see
			// driver.go's "1000 per the image contract" note. NOT the
			// daemon's own root, which is exactly the gap #165 closes.
			User: driveProbeUser,
			// -r AND -x: a directory must be both readable and searchable
			// (enterable) to be usable, and either missing is the same
			// refusal the agent would hit inside the real sandbox. $1
			// rather than an interpolated literal so a future caller can
			// never turn this into a shell-injection seam.
			Cmd: []string{"sh", "-c", `test -r "$1" && test -x "$1"`, "sh", driveProbeTarget},
		},
		HostConfig: &container.HostConfig{
			// No network, no capabilities, no privilege escalation: this
			// container's only job is one stat-shaped syscall pair.
			NetworkMode:    "none",
			CapDrop:        []string{"ALL"},
			SecurityOpt:    []string{"no-new-privileges"},
			ReadonlyRootfs: true,
			AutoRemove:     false, // this driver removes it itself, below — see prepareRecordingDirs's own one-shot execs for the same posture
			Resources:      proxyResources(),
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
		// Background, not ctx: the caller's bound (driveShareProbe's 5s) may
		// already be exhausted by the time the probe itself answers, and
		// leaking a throwaway container is worse than a cleanup outliving the
		// request that asked for it.
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

// driveProbeUser is the fixed uid:gid every wardyn agent image runs its agent
// process as (see driver.go's recording-dirs note) — never the daemon's own
// root. driveProbeTarget is the throwaway probe container's own bind target,
// entirely internal to this file (never the reserved runner.DriveTarget the
// real agent mounts at).
const (
	driveProbeUser   = "1000:1000"
	driveProbeTarget = "/wardyn-probe"
	// defaultDriveProbeImage is the busybox-class placeholder ProbeDrive execs
	// `sh`/`test` in when Config.DriveProbeImage is unset — the same image the
	// conformance suite already leans on elsewhere in this package for a
	// minimal image with no daemon of its own.
	defaultDriveProbeImage = "busybox:latest"
)

func (d *Driver) driveProbeImage() string {
	if d.cfg.DriveProbeImage != "" {
		return d.cfg.DriveProbeImage
	}
	return defaultDriveProbeImage
}
