// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// heldMemberDriveRetryAfter is how old a held delete's mark must be before the retry sweep lists it. It is longer than
// memberDriveTakeover, so the sweep never meets a delete still in flight, and each retry is audited, so it bounds the
// audit rows one stuck drive writes.
const heldMemberDriveRetryAfter = "5 minutes"

// ListHeldMemberDriveDeletes returns up to limit member drives on one of backends, other than Mac disks, whose delete
// is held: marked longer ago than heldMemberDriveRetryAfter, oldest first. The backend filter is in the statement, not
// the caller's loop, so rows the caller cannot delete never fill the batch. A Mac disk's delete waits for its Mac
// (ListMacDiskDeletes). The caller takes each over with BeginMemberDriveTakeover, which re-reads the mark, so a row
// cleared or finished since this read is skipped there.
func (s PG) ListHeldMemberDriveDeletes(ctx context.Context, backends []string, limit int) ([]types.UserDrive, error) {
	return collect(ctx, s.Pool, "list", "held member drive deletes",
		`SELECT `+driveRowCols+` FROM user_drives
		WHERE owner_subject <> '' AND backend = ANY($1) AND backend <> 'mac_disk' AND delete_started_at IS NOT NULL
		  AND delete_started_at < now() - interval '`+heldMemberDriveRetryAfter+`'
		ORDER BY delete_started_at, id LIMIT $2`,
		[]any{backends, limit}, scanDriveRow)
}
