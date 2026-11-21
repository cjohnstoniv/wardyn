// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// A portal's team drives: a user_drive_grants row whose subject_type is types.DriveSubjectPortal and whose subject
// is the portal's delegates.id. No resolver that matches a person's subjects selects one (subjectMatchOf names
// all, user, group and user_type only); the one door that reads them is ResolvePortalTeamDrives.

// PortalDriveStore resolves a portal's own drive allocations. Optional like UserDriveCatalogueStore: the api
// type-asserts it, and a store without it leaves a portal's run no drive.
type PortalDriveStore interface {
	ResolvePortalTeamDrives(ctx context.Context, portal uuid.UUID) ([]UserDriveWinner, error)
}

var _ PortalDriveStore = PG{}

// portalDriveBackendsSQL is the list of backends a portal's folder can be on: the two whose storage Wardyn names.
var portalDriveBackendsSQL = `('` + string(types.DriveBackendDockerVolume) + `','` + string(types.DriveBackendK8sPVCStatic) + `')`

// portalDriveBackendOK reports whether b is a backend a portal's folder can be on.
func portalDriveBackendOK(b types.DriveBackend) bool {
	return b == types.DriveBackendDockerVolume || b == types.DriveBackendK8sPVCStatic
}

// ResolvePortalTeamDrives returns the drives allocated to the portal: a team drive on a Wardyn-named backend with
// no second place, held through a row naming this portal, while the portal is a live issuer portal. A disabled
// row stays in (it resolves paused). A revoked, unknown or console portal, a drive that took a second place or
// moved its backend after the allocation, and a drive no row names for this portal resolve nothing. Empty, never
// ErrNotFound, when nothing matches.
func (s PG) ResolvePortalTeamDrives(ctx context.Context, portal uuid.UUID) ([]UserDriveWinner, error) {
	q := `SELECT d.id, d.name, d.backend, d.host_root, d.storage_class, d.home_template,
			d.size_mib, d.writable, d.reclaim, d.created_at, d.updated_at, d.created_by, d.object_scheme, d.team,
			COALESCE(d.alt_backend, ''), COALESCE(d.alt_host_root, ''), d.alt_runner_id,
			g.id, g.subject_type, g.subject, g.drive_id, g.priority,
			g.size_mib_override, g.writable_override, g.home_override, g.enabled,
			g.created_at, g.created_by, g.members_can_write
		FROM user_drive_grants g
		JOIN user_drives d ON d.id = g.drive_id
		JOIN delegates p ON p.id = $1::uuid AND p.revoked_at IS NULL AND p.issuer <> ''
		WHERE g.subject_type = '` + string(types.DriveSubjectPortal) + `' AND g.subject = $2::text
		  AND d.team AND d.backend IN ` + portalDriveBackendsSQL + ` AND d.alt_backend IS NULL
		ORDER BY d.name, g.id`
	rows, err := s.Pool.Query(ctx, q, portal, portal.String())
	if err != nil {
		return nil, fmt.Errorf("store: resolve portal team drives: %w", err)
	}
	out, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (UserDriveWinner, error) {
		var w UserDriveWinner
		err := row.Scan(append(userDriveDest(&w.Drive), userDriveGrantDest(&w.Grant)...)...)
		return w, err
	})
	if err != nil {
		return nil, fmt.Errorf("store: resolve portal team drives: %w", err)
	}
	return out, nil
}

// lockPortalAllocation is the check a portal allocation is written under, held to the write's commit: the drive
// is read FOR SHARE and judged (a team drive on a Wardyn-named backend with no second place), then the subject's
// allocation lock is taken, then the portal's delegates row is read FOR SHARE and judged (known, an issuer
// portal, not revoked). The drive row's lock conflicts with a drive edit's, and a revoke's UPDATE of the portal
// conflicts with the delegates lock, so each commits wholly before the check or waits for the write.
// ErrNotFound for an unknown drive.
func lockPortalAllocation(ctx context.Context, tx pgx.Tx, g types.UserDriveGrant) error {
	d, err := scanUserDrive(tx.QueryRow(ctx, `SELECT `+userDriveCols+` FROM user_drives WHERE id = $1 FOR SHARE`, g.DriveID))
	if err != nil {
		return err
	}
	switch {
	case !d.Team:
		return ErrDrivePortalNotTeam
	case !portalDriveBackendOK(d.Backend):
		return ErrDrivePortalBackend
	case d.AltBackend != "":
		return ErrDrivePortalSecondPlace
	}
	if err := lockUserDriveGrants(ctx, tx, g); err != nil {
		return err
	}
	var issuer string
	var revoked bool
	err = tx.QueryRow(ctx, `SELECT issuer, revoked_at IS NOT NULL FROM delegates WHERE id = $1::uuid FOR SHARE`, g.Subject).Scan(&issuer, &revoked)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return ErrDrivePortalNotLive
	case err != nil:
		return fmt.Errorf("store: read portal: %w", err)
	case issuer == "":
		return ErrDrivePortalNotIssuer
	case revoked:
		return ErrDrivePortalNotLive
	}
	return nil
}

// driveEditNeedsPortalCheck reports whether a write of d could break a portal's allocation of it: d is not a team
// drive (a stale edit that read the drive before it became one), has a second place, or has a backend the portal's
// folder cannot be on.
func driveEditNeedsPortalCheck(d types.UserDrive) bool {
	return !d.Team || d.AltBackend != "" || !portalDriveBackendOK(d.Backend)
}

// refuseEditOfPortalDrive is a drive edit's check under the drive's FOR UPDATE lock: when d is not a team drive, has a
// second place or has a backend a portal's folder cannot be on, a drive that a portal holds refuses the edit. Its
// predicate is its own and judges the row being written, never refuseIfAllocated's refusal of any allocation, so a
// second place on a group-allocated drive and a confirmed re-home keep working, and an ordinary edit of a drive no
// portal holds (the common non-team case) is untouched.
func refuseEditOfPortalDrive(ctx context.Context, q rowQuerier, d types.UserDrive) error {
	if !driveEditNeedsPortalCheck(d) {
		return nil
	}
	var held bool
	if err := q.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM user_drive_grants WHERE drive_id = $1 AND subject_type = '`+
		string(types.DriveSubjectPortal)+`')`, d.ID).Scan(&held); err != nil {
		return fmt.Errorf("store: read portal allocations: %w", err)
	}
	switch {
	case !held:
		return nil
	case !d.Team:
		return ErrDrivePortalNotTeam
	case !portalDriveBackendOK(d.Backend):
		return ErrDrivePortalBackend
	default:
		return ErrDrivePortalSecondPlace
	}
}
