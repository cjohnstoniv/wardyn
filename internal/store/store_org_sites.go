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

// orgSiteCols are org_sites' columns in scanOrgSite's order, the site's pools last.
const orgSiteCols = `s.id, s.name, s.state, s.default_pool_id, s.storage_label, s.storage_url, s.revision, s.created_at, s.updated_at,
	COALESCE((SELECT array_agg(p.pool_id ORDER BY p.pool_id) FROM org_site_pools p WHERE p.site_id = s.id), '{}')`

func scanOrgSite(row pgx.Row) (types.OrgSite, error) {
	var s types.OrgSite
	var state string
	err := row.Scan(&s.ID, &s.Name, &state, &s.DefaultPoolID, &s.StorageLabel, &s.StorageURL, &s.Revision, &s.CreatedAt, &s.UpdatedAt, &s.PoolIDs)
	if errors.Is(err, pgx.ErrNoRows) {
		return types.OrgSite{}, ErrNotFound
	}
	if err != nil {
		return types.OrgSite{}, fmt.Errorf("store: scan site: %w", err)
	}
	s.State = types.OrgSiteState(state)
	return s, nil
}

func (s PG) ListOrgSites(ctx context.Context) ([]types.OrgSite, error) {
	return collect(ctx, s.Pool, "list", "sites",
		`SELECT `+orgSiteCols+` FROM org_sites s WHERE s.state = 'active' ORDER BY lower(s.name), s.id`, nil, scanOrgSite)
}

func (s PG) GetOrgSite(ctx context.Context, id uuid.UUID) (types.OrgSite, error) {
	return scanOrgSite(s.Pool.QueryRow(ctx, `SELECT `+orgSiteCols+` FROM org_sites s WHERE s.id = $1`, id))
}

// liveSiteForUpdate locks a live site and reads it: ErrNotFound for an unknown or deleted one.
func liveSiteForUpdate(ctx context.Context, q Querier, id uuid.UUID) (types.OrgSite, error) {
	if err := lockSites(ctx, q, id); err != nil {
		return types.OrgSite{}, err
	}
	site, err := scanOrgSite(q.QueryRow(ctx, `SELECT `+orgSiteCols+` FROM org_sites s WHERE s.id = $1`, id))
	if err == nil && site.State != types.OrgSiteActive {
		return types.OrgSite{}, ErrNotFound
	}
	return site, err
}

// CreateOrgSite inserts s as an active site at revision 1. A new site has no pools, so a default pool is
// ErrOrgSiteDefaultPool; a name a live site holds is ErrConflict.
func (s PG) CreateOrgSite(ctx context.Context, site types.OrgSite) (types.OrgSite, error) {
	if site.DefaultPoolID != nil {
		return types.OrgSite{}, ErrOrgSiteDefaultPool
	}
	out, err := scanOrgSite(s.Pool.QueryRow(ctx, `
		WITH s AS (INSERT INTO org_sites (id, name, storage_label, storage_url) VALUES ($1, $2, $3, $4) RETURNING *)
		SELECT `+orgSiteCols+` FROM s`, site.ID, site.Name, site.StorageLabel, site.StorageURL))
	if isUniqueViolation(err) {
		return types.OrgSite{}, ErrConflict
	}
	return out, err
}

// UpdateOrgSite replaces a live site's name, storage note and default pool, only at the revision the caller
// read: ErrOrgSiteStale otherwise, ErrNotFound for an unknown or deleted site, ErrConflict when the name is
// taken. A default pool must be a live pool assigned to this site (ErrOrgSiteDefaultPool), checked under the
// site's row lock, which every pool move also takes. A change that moves nothing writes nothing and keeps the
// revision.
func (s PG) UpdateOrgSite(ctx context.Context, id uuid.UUID, revision int64, name, storageLabel, storageURL string, defaultPoolID *uuid.UUID) (types.OrgSite, error) {
	var out types.OrgSite
	err := s.inTx(ctx, func(q Querier) error {
		cur, err := liveSiteForUpdate(ctx, q, id)
		if err != nil {
			return err
		}
		if cur.Revision != revision {
			return ErrOrgSiteStale
		}
		if defaultPoolID != nil {
			var ok bool
			if err := q.QueryRow(ctx, `
				SELECT EXISTS (SELECT 1 FROM org_site_pools sp JOIN runner_pools p ON p.id = sp.pool_id
				WHERE sp.pool_id = $1 AND sp.site_id = $2 AND p.state <> 'deleted')`, *defaultPoolID, id).Scan(&ok); err != nil {
				return err
			}
			if !ok {
				return ErrOrgSiteDefaultPool
			}
		}
		if cur.Name == name && cur.StorageLabel == storageLabel && cur.StorageURL == storageURL && sameUUIDPtr(cur.DefaultPoolID, defaultPoolID) {
			out = cur
			return nil
		}
		out, err = scanOrgSite(q.QueryRow(ctx, `
			WITH s AS (UPDATE org_sites SET name = $2, storage_label = $3, storage_url = $4, default_pool_id = $5,
				revision = revision + 1, updated_at = now() WHERE id = $1 RETURNING *)
			SELECT `+orgSiteCols+` FROM s`, id, name, storageLabel, storageURL, defaultPoolID))
		if isUniqueViolation(err) {
			return ErrConflict
		}
		return err
	})
	return out, err
}

// DeleteOrgSite marks a live site deleted and clears its default pool. ErrOrgSiteInUse while any pool, live
// or deleted, or any drive is assigned to it: the assignment and the delete both hold the site's row lock, so
// neither slips past the other. ErrNotFound for an unknown or already deleted site.
func (s PG) DeleteOrgSite(ctx context.Context, id uuid.UUID) (types.OrgSite, error) {
	var out types.OrgSite
	err := s.inTx(ctx, func(q Querier) error {
		if _, err := liveSiteForUpdate(ctx, q, id); err != nil {
			return err
		}
		var used bool
		if err := q.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM org_site_pools WHERE site_id = $1)
			OR EXISTS (SELECT 1 FROM org_site_drives WHERE site_id = $1)`, id).Scan(&used); err != nil {
			return err
		}
		if used {
			return ErrOrgSiteInUse
		}
		var err error
		out, err = scanOrgSite(q.QueryRow(ctx, `
			WITH s AS (UPDATE org_sites SET state = 'deleted', default_pool_id = NULL, revision = revision + 1, updated_at = now()
				WHERE id = $1 RETURNING *)
			SELECT `+orgSiteCols+` FROM s`, id))
		return err
	})
	return out, err
}

// orgPoolForUpdate locks a pool row for a site change: ErrNotFound for an unknown pool, ErrRunnerPoolScope for
// a user pool, and, when live is set, ErrNotFound for a deleted pool too.
func orgPoolForUpdate(ctx context.Context, q Querier, id uuid.UUID, live bool) error {
	var p types.RunnerPool
	var err error
	if live {
		p, err = livePoolForUpdate(ctx, q, id)
	} else {
		p, err = getRunnerPoolQ(ctx, q, id, true)
	}
	if err != nil {
		return err
	}
	if p.Scope != types.RunnerPoolOrganization {
		return ErrRunnerPoolScope
	}
	return nil
}

// poolSiteForUpdate is the site a locked pool is at, nil for none.
func poolSiteForUpdate(ctx context.Context, q Querier, poolID uuid.UUID) (*uuid.UUID, error) {
	var site uuid.UUID
	err := q.QueryRow(ctx, `SELECT site_id FROM org_site_pools WHERE pool_id = $1`, poolID).Scan(&site)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &site, nil
}

// lockSites locks the given site rows in id order, so two moves between the same two sites cannot deadlock.
func lockSites(ctx context.Context, q Querier, ids ...uuid.UUID) error {
	_, err := q.Exec(ctx, `SELECT id FROM org_sites WHERE id = ANY($1) ORDER BY id FOR UPDATE`, ids)
	return err
}

// leaveSite takes poolID out of the site it is at (whose row the caller locked): the assignment goes, and so
// does the site's default when it named the pool, which raises the site's revision.
func leaveSite(ctx context.Context, q Querier, poolID, siteID uuid.UUID) error {
	if _, err := q.Exec(ctx, `DELETE FROM org_site_pools WHERE pool_id = $1`, poolID); err != nil {
		return err
	}
	_, err := q.Exec(ctx, `UPDATE org_sites SET default_pool_id = NULL, revision = revision + 1, updated_at = now()
		WHERE id = $1 AND default_pool_id = $2`, siteID, poolID)
	return err
}

// AssignPoolSite puts a live organization pool at a live site. The pool's row is locked first, then both
// sites' rows in id order. Moving a pool raises its revision in the same transaction (bumpRunnerPoolQ), so a
// run admitted against the pool before the move fails the run insert's fence (ErrRunnerPoolStale) and is never
// recorded at the old site. A pool already at the site is left as it is, at its revision. ErrRunnerPoolScope
// for a user pool; ErrNotFound for an unknown or deleted pool or site.
func (s PG) AssignPoolSite(ctx context.Context, poolID, siteID uuid.UUID) (*uuid.UUID, int64, error) {
	var from *uuid.UUID
	var rev int64
	err := s.inTx(ctx, func(q Querier) error {
		if err := orgPoolForUpdate(ctx, q, poolID, true); err != nil {
			return err
		}
		var err error
		if from, err = poolSiteForUpdate(ctx, q, poolID); err != nil {
			return err
		}
		ids := []uuid.UUID{siteID}
		if from != nil && *from != siteID {
			ids = append(ids, *from)
		}
		if err := lockSites(ctx, q, ids...); err != nil {
			return err
		}
		var live bool
		if err := q.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM org_sites WHERE id = $1 AND state = 'active')`, siteID).Scan(&live); err != nil {
			return err
		}
		if !live {
			return ErrNotFound
		}
		if from != nil && *from == siteID {
			return q.QueryRow(ctx, `SELECT revision FROM runner_pools WHERE id = $1`, poolID).Scan(&rev)
		}
		if from != nil {
			if err := leaveSite(ctx, q, poolID, *from); err != nil {
				return err
			}
		}
		if _, err := q.Exec(ctx, `INSERT INTO org_site_pools (pool_id, site_id) VALUES ($1, $2)`, poolID, siteID); err != nil {
			return err
		}
		rev, err = bumpRunnerPoolQ(ctx, q, poolID)
		return err
	})
	return from, rev, err
}

// ClearPoolSite takes an organization pool, live or deleted, from its site, and clears the site's default
// when it named the pool. A deleted pool can be cleared, so a site whose pool was deleted can still be emptied
// and deleted. The pool's revision rises only when it was at a site. ErrRunnerPoolScope for a user pool,
// ErrNotFound for an unknown one.
func (s PG) ClearPoolSite(ctx context.Context, poolID uuid.UUID) (*uuid.UUID, int64, error) {
	var from *uuid.UUID
	var rev int64
	err := s.inTx(ctx, func(q Querier) error {
		if err := orgPoolForUpdate(ctx, q, poolID, false); err != nil {
			return err
		}
		var err error
		if from, err = poolSiteForUpdate(ctx, q, poolID); err != nil {
			return err
		}
		if from == nil {
			return q.QueryRow(ctx, `SELECT revision FROM runner_pools WHERE id = $1`, poolID).Scan(&rev)
		}
		if err := lockSites(ctx, q, *from); err != nil {
			return err
		}
		if err := leaveSite(ctx, q, poolID, *from); err != nil {
			return err
		}
		rev, err = bumpRunnerPoolQ(ctx, q, poolID)
		return err
	})
	return from, rev, err
}

// SetDriveSite homes an admin drive at a live site, replacing any home it had. The drive's row is read under a
// key-share lock (the drive cannot be deleted under it) and the site's under a share lock (a delete waits for
// it). ErrOrgSiteMemberDrive for a member drive; ErrNotFound for an unknown drive or an unknown or deleted site.
func (s PG) SetDriveSite(ctx context.Context, driveID, siteID uuid.UUID) (*uuid.UUID, error) {
	var from *uuid.UUID
	err := s.inTx(ctx, func(q Querier) error {
		var owner string
		err := q.QueryRow(ctx, `SELECT owner_subject FROM user_drives WHERE id = $1 FOR KEY SHARE`, driveID).Scan(&owner)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if owner != "" {
			return ErrOrgSiteMemberDrive
		}
		var state string
		err = q.QueryRow(ctx, `SELECT state FROM org_sites WHERE id = $1 FOR SHARE`, siteID).Scan(&state)
		if errors.Is(err, pgx.ErrNoRows) || (err == nil && state != string(types.OrgSiteActive)) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		var prev uuid.UUID
		switch err := q.QueryRow(ctx, `SELECT site_id FROM org_site_drives WHERE drive_id = $1 FOR UPDATE`, driveID).Scan(&prev); {
		case err == nil:
			from = &prev
		case !errors.Is(err, pgx.ErrNoRows):
			return err
		}
		_, err = q.Exec(ctx, `INSERT INTO org_site_drives (drive_id, site_id) VALUES ($1, $2)
			ON CONFLICT (drive_id) DO UPDATE SET site_id = EXCLUDED.site_id, assigned_at = now()`, driveID, siteID)
		return err
	})
	return from, err
}

// ClearDriveSite takes a drive's home site away; nil when it had none.
func (s PG) ClearDriveSite(ctx context.Context, driveID uuid.UUID) (*uuid.UUID, error) {
	var site uuid.UUID
	err := s.Pool.QueryRow(ctx, `DELETE FROM org_site_drives WHERE drive_id = $1 RETURNING site_id`, driveID).Scan(&site)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("store: clear drive site: %w", err)
	}
	return &site, nil
}

func (s PG) ListOrgSiteDrives(ctx context.Context) ([]types.OrgSiteDrive, error) {
	return collect(ctx, s.Pool, "list", "site drives", `
		SELECT d.id, d.name, s.id, s.name FROM org_site_drives sd
		JOIN user_drives d ON d.id = sd.drive_id JOIN org_sites s ON s.id = sd.site_id
		WHERE s.state = 'active' AND d.owner_subject = '' ORDER BY d.name, d.id`, nil,
		func(row pgx.Row) (types.OrgSiteDrive, error) {
			var d types.OrgSiteDrive
			err := row.Scan(&d.DriveID, &d.DriveName, &d.SiteID, &d.SiteName)
			return d, err
		})
}

func (s PG) DriveHomeSites(ctx context.Context, driveIDs []uuid.UUID) (map[uuid.UUID]uuid.UUID, error) {
	out := map[uuid.UUID]uuid.UUID{}
	if len(driveIDs) == 0 {
		return out, nil
	}
	rows, err := collect(ctx, s.Pool, "read", "drive home sites", `
		SELECT sd.drive_id, sd.site_id FROM org_site_drives sd JOIN org_sites s ON s.id = sd.site_id
		WHERE sd.drive_id = ANY($1) AND s.state = 'active'`, []any{driveIDs},
		func(row pgx.Row) ([2]uuid.UUID, error) {
			var p [2]uuid.UUID
			err := row.Scan(&p[0], &p[1])
			return p, err
		})
	if err != nil {
		return nil, err
	}
	for _, p := range rows {
		out[p[0]] = p[1]
	}
	return out, nil
}

func (s PG) PoolSite(ctx context.Context, poolID uuid.UUID) (types.OrgSite, bool, error) {
	site, err := scanOrgSite(s.Pool.QueryRow(ctx, `
		SELECT `+orgSiteCols+` FROM org_site_pools sp JOIN org_sites s ON s.id = sp.site_id
		WHERE sp.pool_id = $1 AND s.state = 'active'`, poolID))
	if errors.Is(err, ErrNotFound) {
		return types.OrgSite{}, false, nil
	}
	if err != nil {
		return types.OrgSite{}, false, err
	}
	return site, true, nil
}
