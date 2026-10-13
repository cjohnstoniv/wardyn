// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// UserDriveWinner is one drive a caller reaches and the grant that wins it.
// The tier is Grant.SubjectType.
type UserDriveWinner struct {
	Drive types.UserDrive
	Grant types.UserDriveGrant
}

// UserDriveCatalogueStore resolves every drive a caller reaches. Like
// PrincipalPrefStore it is not part of Store, so the api test doubles need not
// grow it; the api type-asserts and treats its absence as "no drives".
type UserDriveCatalogueStore interface {
	// ResolveUserDrives returns, for each drive any of the caller's grants
	// names, the grant that wins THAT drive, ranked across drives by the same
	// precedence ResolveUserDrive uses, so its first entry is ResolveUserDrive's
	// answer. Disabled grants stay in: a disabled winner comes back Enabled
	// false and never falls through to a wider grant on the same drive. Empty,
	// never ErrNotFound, when nothing matches.
	ResolveUserDrives(ctx context.Context, userSubjects, groups []string, userType string) ([]UserDriveWinner, error)
}

var _ UserDriveCatalogueStore = PG{}

// UserDriveGrantWriter is POST /drives/grants' two verbs: AddUserDriveGrant
// allocates a drive beside a subject's others (additional), and
// ReplaceUserDriveGrant is UpsertUserDriveGrant returning the drives the
// subject lost, for the audit row. Optional like UserDriveCatalogueStore.
type UserDriveGrantWriter interface {
	AddUserDriveGrant(ctx context.Context, g types.UserDriveGrant, homeOverrideStated bool) (types.UserDriveGrant, error)
	ReplaceUserDriveGrant(ctx context.Context, g types.UserDriveGrant, homeOverrideStated bool) (types.UserDriveGrant, []uuid.UUID, error)
}

var _ UserDriveGrantWriter = PG{}

// ResolveUserDrives is ResolveUserDrive per drive: DISTINCT ON keeps each
// drive's top-ranked row, then the outer ORDER BY ranks the winners. Both
// orders are subjectPrecedence, so the per-drive pick and the single-drive pick
// cannot rank differently.
func (s PG) ResolveUserDrives(ctx context.Context, userSubjects, groups []string, userType string) ([]UserDriveWinner, error) {
	if userSubjects == nil {
		userSubjects = []string{}
	}
	if groups == nil {
		groups = []string{}
	}
	q := `SELECT * FROM (
		SELECT DISTINCT ON (g.drive_id)
			d.id, d.name, d.backend, d.host_root, d.storage_class, d.home_template,
			d.size_mib, d.writable, d.reclaim, d.created_at, d.updated_at, d.created_by, d.object_scheme,
			g.id AS grant_id, g.subject_type, g.subject, g.drive_id, g.priority,
			g.size_mib_override, g.writable_override, g.home_override, g.enabled,
			g.created_at AS grant_created_at, g.created_by AS grant_created_by
		FROM user_drive_grants g
		JOIN user_drives d ON d.id = g.drive_id
		WHERE ` + subjectMatch("g") + `
		ORDER BY g.drive_id, ` + subjectPrecedence("g", "d") + `
	) w
	ORDER BY ` + subjectPrecedence("w", "w")
	rows, err := s.Pool.Query(ctx, q, userSubjects, groups, userType)
	if err != nil {
		return nil, fmt.Errorf("store: resolve user drives: %w", err)
	}
	out, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (UserDriveWinner, error) {
		var w UserDriveWinner
		err := row.Scan(append(userDriveDest(&w.Drive), userDriveGrantDest(&w.Grant)...)...)
		return w, err
	})
	if err != nil {
		return nil, fmt.Errorf("store: resolve user drives: %w", err)
	}
	return out, nil
}
