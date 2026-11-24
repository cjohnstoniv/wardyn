// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"log/slog"
	"net/http"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// holdMacDiskDrives records the run as the holder of each disk drive it mounts, in dispatch's last doors. It never
// refuses: a hold another live run keeps, or a write that fails, leaves the Mac's own lock to decide, as before
// mac_disk_holds existed. Always true, so it sits in sandboxCreateAdmitted's chain.
func (s *Server) holdMacDiskDrives(ctx context.Context, run types.AgentRun, drives []types.DriveMount) bool {
	hs, ok := s.cfg.Store.(store.MacDiskHoldStore)
	if !ok {
		return true
	}
	for _, d := range drives {
		if d.Backend != types.DriveBackendMacDisk || d.DriveID == uuid.Nil {
			continue
		}
		if held, err := hs.HoldMacDisk(ctx, d.DriveID, run.ID); err != nil || !held {
			slog.WarnContext(ctx, "api: disk drive hold not recorded; the Mac's lock decides",
				slog.String("run_id", run.ID.String()), slog.String("drive_id", d.DriveID.String()), slog.Any("err", err))
		}
	}
	return true
}

// macDiskHeldRefusal refuses a create or a Review whose disk drive another run that is not finished holds
// (#3851). A store that cannot answer refuses, as every drive read here does.
func (s *Server) macDiskHeldRefusal(ctx context.Context, drives []runDrive) *runRefusal {
	hs, ok := s.cfg.Store.(store.MacDiskHoldStore)
	if !ok {
		return nil
	}
	for _, d := range drives {
		if d.resolved.Drive.Backend != types.DriveBackendMacDisk {
			continue
		}
		holder, err := hs.MacDiskHolder(ctx, d.resolved.Drive.ID)
		if err != nil {
			return driveRunError(err)
		}
		if holder != uuid.Nil {
			return runError(http.StatusConflict, reasonDriveInAnotherRun, driveMacDiskInAnotherRunMsg(d.resolved.Drive.Name))
		}
	}
	return nil
}
