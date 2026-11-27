// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build docker

package docker

import (
	"context"
	"fmt"
	"strings"

	"github.com/moby/moby/client"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

var _ runner.DriveHolderChecker = (*Driver)(nil)

// DriveHolder names a container that mounts the drive's volume, or "" when none does. It only lists containers.
// All is set so a stopped container that still references the volume, a kept run's, counts as a holder. A drive
// that is not a Docker volume, and a daemon client that cannot list, are runner.ErrDriveHolderUnknown, never "no
// holder"; a list error is returned.
func (d *Driver) DriveHolder(ctx context.Context, drive types.DriveMount) (string, error) {
	if drive.Backend != types.DriveBackendDockerVolume || drive.ObjectName == "" {
		return "", runner.ErrDriveHolderUnknown
	}
	lister, ok := d.cli.(containerListerAPI)
	if !ok {
		return "", runner.ErrDriveHolderUnknown
	}
	res, err := lister.ContainerList(ctx, client.ContainerListOptions{
		All:     true,
		Filters: client.Filters{}.Add("volume", drive.ObjectName),
	})
	if err != nil {
		return "", fmt.Errorf("docker: drive: list containers that mount volume %q: %w", drive.ObjectName, err)
	}
	if len(res.Items) == 0 {
		return "", nil
	}
	if names := res.Items[0].Names; len(names) > 0 {
		return strings.TrimPrefix(names[0], "/"), nil
	}
	return res.Items[0].ID, nil
}
