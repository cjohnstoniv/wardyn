// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"errors"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// OrgSiteStore is the optional store capability behind organisation sites (migration 0400_org_sites): the
// site catalogue, which organization pools are at which site, and which admin drives are homed where. A store
// without it has no sites, as a store without RunnerPoolStore has no pools.
type OrgSiteStore interface {
	// ListOrgSites is every live site, by name, each with its pools.
	ListOrgSites(ctx context.Context) ([]types.OrgSite, error)
	// GetOrgSite is one site, live or deleted, with its pools; ErrNotFound for an unknown id.
	GetOrgSite(ctx context.Context, id uuid.UUID) (types.OrgSite, error)
	// CreateOrgSite inserts an active site at revision 1, with no pools and so no default pool.
	CreateOrgSite(ctx context.Context, s types.OrgSite) (types.OrgSite, error)
	// UpdateOrgSite replaces the name, the storage note and the default pool of a live site, only at the
	// revision the caller read.
	UpdateOrgSite(ctx context.Context, id uuid.UUID, revision int64, name, storageLabel, storageURL string, defaultPoolID *uuid.UUID) (types.OrgSite, error)
	// DeleteOrgSite marks a live site deleted, only while no pool and no drive is assigned to it.
	DeleteOrgSite(ctx context.Context, id uuid.UUID) (types.OrgSite, error)

	// AssignPoolSite puts an organization pool at a live site, moving it from any other, and ClearPoolSite
	// takes it from its site. Each answers the site the pool was at before (nil for none) and the pool's
	// revision, raised when the pool's site changed.
	AssignPoolSite(ctx context.Context, poolID, siteID uuid.UUID) (from *uuid.UUID, revision int64, err error)
	ClearPoolSite(ctx context.Context, poolID uuid.UUID) (from *uuid.UUID, revision int64, err error)

	// SetDriveSite homes an admin drive at a live site, and ClearDriveSite takes its home away. Each answers
	// the drive's home before (nil for none).
	SetDriveSite(ctx context.Context, driveID, siteID uuid.UUID) (from *uuid.UUID, err error)
	ClearDriveSite(ctx context.Context, driveID uuid.UUID) (from *uuid.UUID, err error)
	// ListOrgSiteDrives is every admin drive homed at a live site, by drive name.
	ListOrgSiteDrives(ctx context.Context) ([]types.OrgSiteDrive, error)

	// DriveHomeSites maps each of driveIDs that is homed at a live site to that site. A drive with no home,
	// or homed at a deleted site, is absent.
	DriveHomeSites(ctx context.Context, driveIDs []uuid.UUID) (map[uuid.UUID]uuid.UUID, error)
	// PoolSite is the live site a pool is at; ok is false when it is at none.
	PoolSite(ctx context.Context, poolID uuid.UUID) (site types.OrgSite, ok bool, err error)
}

var _ OrgSiteStore = PG{}

var (
	// ErrOrgSiteStale: the caller's revision is not the site's.
	ErrOrgSiteStale = errors.New("store: site revision is stale")
	// ErrOrgSiteInUse: a pool or a drive is still assigned to the site, so it is not deleted.
	ErrOrgSiteInUse = errors.New("store: the site still has pools or drives")
	// ErrOrgSiteDefaultPool: the default pool named is not a live pool assigned to the site.
	ErrOrgSiteDefaultPool = errors.New("store: a site's default pool is one of its own live pools")
	// ErrOrgSiteMemberDrive: a member drive has no home site; only admin drives do.
	ErrOrgSiteMemberDrive = errors.New("store: only an admin drive has a home site")
)
