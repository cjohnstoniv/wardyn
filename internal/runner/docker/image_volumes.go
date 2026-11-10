// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build docker

package docker

import (
	"context"
	"fmt"
	"log/slog"
	"maps"
	"path"
	"slices"
	"strconv"
	"strings"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/api/types/system"
	"github.com/moby/moby/client"

	"github.com/cjohnstoniv/wardyn/internal/runner"
)

// labelDiskSoft marks an agent whose disk limit no policy wrote (a request or a default), so a
// revive of the kept container can tell a limit it warns about from one it enforces.
const labelDiskSoft = "wardyn.disk-soft"

// The daemon backs each image VOLUME with an anonymous volume, a directory of its own under the
// engine's data root. The per-container size limit (StorageOpt "size") bounds only the container's
// writable layer, so the agent could fill that path past disk_mib. A path some Wardyn mount or tmpfs
// already sits on is not backed by one, and is not counted. Both entry points below share
// answerOpenVolumes:
//
//   - Limit enforced on this engine and written by policy (hard): the start is refused, naming the paths.
//   - Limit enforced and not written by policy (a request or a default): it proceeds and a warning names them.
//   - No limit, or an engine that cannot enforce one: nothing to bypass, nothing checked.

// checkImageVolumesUnderCap is the create-time check. It reads the image, so it covers the exec-less
// create (the container does not exist yet) as well as the exec one.
func (d *Driver) checkImageVolumesUnderCap(ctx context.Context, image string, res runner.Resources, host *container.HostConfig, info system.Info) error {
	if res.DiskMiB <= 0 || !storageDriverSupportsQuota(info) {
		return nil
	}
	insp, err := d.cli.ImageInspect(ctx, image)
	if err != nil {
		if res.DiskMiBSoft {
			return nil
		}
		return fmt.Errorf("docker: read image %s to check its volumes against the disk limit: %w", image, err)
	}
	if insp.Config == nil {
		return nil
	}
	open := openVolumes(slices.Collect(maps.Keys(insp.Config.Volumes)), coveredTargets(host))
	return answerOpenVolumes(res.DiskMiB, res.DiskMiBSoft, "run", open,
		fmt.Sprintf("use an image without the VOLUME, or mount something over %s", strings.Join(open, ", ")))
}

// checkKeptVolumesUnderCap is the revive-time check on the kept container itself. A container's
// volumes do not change between runs of it, but one made before the create-time check existed (an
// uncovered image VOLUME under a disk limit) is still unbounded, so it is checked again before it
// starts. The limit is the size the engine was given at create; the soft marker is the agent's label,
// and a container without it (made before the label) is held to the limit as a hard one.
func (d *Driver) checkKeptVolumesUnderCap(ctx context.Context, ref string) error {
	insp, err := d.cli.ContainerInspect(ctx, ref, client.ContainerInspectOptions{})
	if err != nil {
		insp, err = d.cli.ContainerInspect(ctx, ref, client.ContainerInspectOptions{})
	}
	if err != nil {
		return fmt.Errorf("docker: inspect the kept agent to check its volumes against the disk limit: %w", err)
	}
	c := insp.Container
	if c.HostConfig == nil {
		return nil
	}
	size, ok := c.HostConfig.StorageOpt["size"]
	limit, perr := strconv.ParseInt(strings.TrimSuffix(strings.ToLower(size), "m"), 10, 64)
	if !ok || perr != nil || limit <= 0 {
		return nil // no limit was put on this container
	}
	info, err := d.cli.Info(ctx, client.InfoOptions{})
	if err != nil {
		return fmt.Errorf("docker: read the engine to check the kept agent's volumes against the disk limit: %w", err)
	}
	if !storageDriverSupportsQuota(info.Info) {
		return nil
	}
	var declared []string
	if c.Config != nil {
		declared = slices.Collect(maps.Keys(c.Config.Volumes))
	}
	for _, m := range c.Mounts {
		if m.Type == mount.TypeVolume {
			declared = append(declared, m.Destination)
		}
	}
	soft := c.Config != nil && c.Config.Labels[labelDiskSoft] == "true"
	return answerOpenVolumes(limit, soft, "kept sandbox", openVolumes(declared, coveredTargets(c.HostConfig)),
		"start a new run; this kept sandbox was created before the disk limit covered image volumes")
}

// coveredTargets is the set of paths a Wardyn mount or tmpfs sits on.
func coveredTargets(host *container.HostConfig) map[string]bool {
	covered := map[string]bool{}
	if host == nil {
		return covered
	}
	for _, m := range host.Mounts {
		covered[path.Clean(m.Target)] = true
	}
	for tgt := range host.Tmpfs {
		covered[path.Clean(tgt)] = true
	}
	return covered
}

// openVolumes is the declared volume paths, sorted and without repeats, that nothing covers.
func openVolumes(declared []string, covered map[string]bool) []string {
	var open []string
	for _, v := range declared {
		if v = path.Clean(v); !covered[v] && !slices.Contains(open, v) {
			open = append(open, v)
		}
	}
	slices.Sort(open)
	return open
}

// answerOpenVolumes refuses (hard) or warns (soft) about volumes the disk limit does not count.
func answerOpenVolumes(limitMiB int64, soft bool, subject string, open []string, remedy string) error {
	if len(open) == 0 {
		return nil
	}
	list := strings.Join(open, ", ")
	if soft {
		slog.Warn("wardyn/docker: the image declares a VOLUME, whose data is not counted in the disk limit; the run can write past it there",
			slog.Int64("disk_mib", limitMiB), slog.String("subject", subject), slog.String("volumes", list))
		return nil
	}
	return fmt.Errorf("docker: this %s has a disk limit of %d MiB, but declares a VOLUME at %s, and a volume's data is not counted in that limit; %s", subject, limitMiB, list, remedy)
}
