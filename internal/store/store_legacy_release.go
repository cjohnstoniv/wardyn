// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Settling the person's drives that existed when migration 0299 ran (user_drives.legacy_unsettled): a Mac disk
// is released by its own runner's evidence, a Docker volume by an administrator's call, which also leaves a record
// that is replayed to the audit log until the audit table holds it (drive_legacy_releases).
package store

import (
	"context"
	"fmt"

	"github.com/google/uuid"
)

// ReleaseLegacyMacDrives clears legacy_unsettled on the Mac disks kept on runnerID, and on no other row: the
// runner's evidence (a new process, which no earlier delete of this Mac can outlive) is about its own Mac. It
// answers how many rows it cleared; calling it again is a no-op.
func (s PG) ReleaseLegacyMacDrives(ctx context.Context, runnerID uuid.UUID) (int64, error) {
	tag, err := s.Pool.Exec(ctx, `UPDATE user_drives SET legacy_unsettled = false
		WHERE backend = 'mac_disk' AND home_runner_id = $1 AND legacy_unsettled`, runnerID)
	if err != nil {
		return 0, fmt.Errorf("store: release legacy Mac drives: %w", err)
	}
	return tag.RowsAffected(), nil
}
