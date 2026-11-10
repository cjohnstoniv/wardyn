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
	"strings"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/system"

	"github.com/cjohnstoniv/wardyn/internal/runner"
)

// checkImageVolumesUnderCap answers a disk limit that an image's VOLUME would sit outside of. The
// daemon backs each VOLUME with an anonymous volume, a directory of its own under the engine's data
// root, and the per-container size limit (StorageOpt "size") bounds only the container's writable
// layer, so the agent could fill that path past disk_mib. A path some Wardyn mount or tmpfs already
// sits on is not backed by one, and is not counted.
//
//   - Limit enforced on this engine and written by policy (hard): the create is refused, naming the paths.
//   - Limit enforced and not written by policy (a request or a default): the run starts and a warning names them.
//   - No limit, or an engine that cannot enforce one: nothing to bypass, nothing checked.
//
// It reads the image, so it covers the exec-less create (the container does not exist yet) as well as
// the exec one. A revive starts the container the create made, whose volumes cannot change.
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
	if insp.Config == nil || len(insp.Config.Volumes) == 0 {
		return nil
	}
	covered := map[string]bool{}
	for _, m := range host.Mounts {
		covered[path.Clean(m.Target)] = true
	}
	for tgt := range host.Tmpfs {
		covered[path.Clean(tgt)] = true
	}
	var open []string
	for _, v := range slices.Sorted(maps.Keys(insp.Config.Volumes)) {
		if v = path.Clean(v); !covered[v] {
			open = append(open, v)
		}
	}
	if len(open) == 0 {
		return nil
	}
	list := strings.Join(open, ", ")
	if res.DiskMiBSoft {
		slog.Warn("wardyn/docker: the image declares a VOLUME, whose data is not counted in the disk limit; the run can write past it there",
			slog.Int64("disk_mib", res.DiskMiB), slog.String("image", image), slog.String("volumes", list))
		return nil
	}
	return fmt.Errorf("docker: this run has a disk limit of %d MiB, but its image declares a VOLUME at %s, and a volume's data is not counted in that limit; use an image without the VOLUME, or mount something over %s", res.DiskMiB, list, list)
}
