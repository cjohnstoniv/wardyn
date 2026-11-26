// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build k8s

package k8s

import (
	"context"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

var _ runner.DriveHolderChecker = (*Driver)(nil)

// DriveHolder names a pod in the driver's namespace that mounts the drive's claim, or "" when none does. It only
// lists pods. A drive that is not a managed claim is runner.ErrDriveHolderUnknown, never "no holder".
func (d *Driver) DriveHolder(ctx context.Context, drive types.DriveMount) (string, error) {
	if drive.Backend != types.DriveBackendK8sPVC || drive.ObjectName == "" {
		return "", runner.ErrDriveHolderUnknown
	}
	return driveClaimHolder(ctx, d, d.defaultNamespace(), drive.ObjectName)
}
