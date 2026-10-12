// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"net/http"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/authz"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/pkg/client"
)

// reachableDrive is one drive the caller's own grants reach: the drive and the
// grant that wins it, resolved for this caller. err is errDriveUnmountable when
// the pair resolves to no mount (newResolvedDrive), and resolved is then nil.
type reachableDrive struct {
	winner   store.UserDriveWinner
	resolved *types.ResolvedDrive
	err      error
	// legacy marks the drive drive.enabled mounts at /home/agent/drive: the
	// first winner, which is ResolveUserDrive's one answer.
	legacy bool
}

// target is where this drive mounts (runner.DriveTargetFor).
func (d reachableDrive) target() string {
	return runner.DriveTargetFor(types.ResolvedDrive{Drive: d.winner.Drive}, d.legacy)
}

// reachableDrives resolves every drive the caller on ctx reaches, each through
// the grant that wins THAT drive, in the resolver's precedence order. It keeps
// resolveUserDrive's doctrine: no store or no per-human subject is no drives, a
// store error is an error, and a disabled winner is paused and never falls
// through to a wider grant on the same drive.
//
// An unanswerable group snapshot reads the user subjects and type alone, and the
// answer is trusted only as selectByTier trusts one: every winner user-tier, or
// no group-tier grant at all. Otherwise it is errGroupsSnapshotStale, for every
// drive at once: a hidden group grant could outrank any non-user winner.
func (s *Server) reachableDrives(ctx context.Context, profileMaxDriveMiB int) ([]reachableDrive, error) {
	cs, ok := s.cfg.Store.(store.UserDriveCatalogueStore)
	if !ok {
		return nil, nil
	}
	subj, err := s.callerSubjects(ctx)
	if err != nil {
		return nil, err
	}
	if len(subj.users) == 0 {
		return nil, nil
	}
	ceiling, err := s.driveSizeCeilingFor(ctx, profileMaxDriveMiB)
	if err != nil {
		return nil, err
	}
	var winners []store.UserDriveWinner
	if subj.stale || oidcGroupsTruncatedFromContext(ctx) {
		winners, _, err = selectByTier(ctx, s, authz.Deny(authz.ReasonGroupsSnapshotStale, "runs.drive", ""),
			func() ([]store.UserDriveWinner, types.CapabilitySubjectType, error) {
				ws, err := cs.ResolveUserDrives(ctx, subj.users, nil, subj.userType)
				switch {
				case err != nil:
					return nil, "", err
				case len(ws) == 0:
					return nil, "", store.ErrNotFound
				}
				for _, w := range ws {
					if w.Grant.SubjectType != types.CapabilitySubjectUser {
						return ws, w.Grant.SubjectType, nil
					}
				}
				return ws, types.CapabilitySubjectUser, nil
			}, s.cfg.Store.HasGroupTierDriveGrants)
		if errors.Is(err, store.ErrNotFound) {
			return nil, nil
		}
	} else {
		winners, err = cs.ResolveUserDrives(ctx, subj.users, subj.groups, subj.userType)
	}
	if err != nil {
		return nil, err
	}
	out := make([]reachableDrive, 0, len(winners))
	for i, w := range winners {
		d := reachableDrive{winner: w, legacy: i == 0}
		d.resolved, d.err = newResolvedDrive(&d.winner.Drive, &d.winner.Grant, w.Grant.SubjectType, subj.users, ceiling)
		if d.err != nil && !errors.Is(d.err, errDriveUnmountable) {
			return nil, d.err
		}
		out = append(out, d)
	}
	return out, nil
}

// handleListMeDrives is GET /me/drives: the drive catalogue New Run attaches
// from (MeDriveCatalogue). A display read: it writes no denial row, counts no
// refusal and answers every failure as a token rather than an error, the way
// /me.user_drive does. Bearers, devices and service identities hold no
// per-human subject and get an empty list.
func (s *Server) handleListMeDrives(w http.ResponseWriter, r *http.Request) {
	ctx := withDisplayRead(r.Context())
	out := client.MeDriveCatalogue{Drives: []client.MeDrive{}}
	name, unresolved := s.userDriveDeniedByProfile(r)
	switch {
	case unresolved != "":
		out.Unavailable = unresolved
	case name != "":
		out.DeniedByProfile = name
	default:
		out.Drives, out.PoolChecked, out.Unavailable = s.meDrives(ctx, r.URL.Query().Get("runner_pool_id"))
	}
	writeJSON(w, http.StatusOK, out)
}

// meDrives lists the caller's catalogue rows, each with the first reason it
// cannot be attached: paused, then unmountable (driveBindFailureHere, the
// launch's own decision), then not_in_pool against the named pool.
func (s *Server) meDrives(ctx context.Context, poolID string) ([]client.MeDrive, bool, string) {
	profileMaxDriveMiB := 0
	if ceiling, err := s.effectiveCeiling(ctx); err == nil {
		profileMaxDriveMiB = ceiling.Limits.MaxDriveSizeMiB
	}
	reach, err := s.reachableDrives(ctx, profileMaxDriveMiB)
	if err != nil {
		return []client.MeDrive{}, false, driveUnavailableReason(err)
	}
	pool, checked := s.catalogueDrivePool(ctx, poolID)
	out := make([]client.MeDrive, 0, len(reach))
	for _, d := range reach {
		drive := d.winner.Drive
		row := client.MeDrive{
			ID: drive.ID.String(), Name: drive.Name, Section: client.MeDriveSectionShared,
			Source: driveSource(d.winner.Grant.SubjectType), Backend: drive.Backend,
			Target: d.target(), Enforcement: types.EnforcementFor(drive.Backend), Legacy: d.legacy,
		}
		if drive.Backend.Kind() == types.DriveKindManaged {
			row.Section = client.MeDriveSectionUser
		}
		if d.resolved != nil {
			row.Writable, row.SizeMiB, row.Enforcement = d.resolved.Writable, d.resolved.SizeMiB, d.resolved.Enforcement
		}
		row.UnavailableReason = s.driveUnavailableHere(ctx, d, pool, checked)
		out = append(out, row)
	}
	return out, checked, ""
}

// driveUnavailableHere is one catalogue row's unavailable_reason.
func (s *Server) driveUnavailableHere(ctx context.Context, d reachableDrive, pool store.RunnerPoolCatalogueRow, checked bool) string {
	switch {
	case d.resolved != nil && d.resolved.Paused:
		return client.MeDriveUnavailablePaused
	case d.resolved == nil || d.target() == "" || s.driveBindFailureHere(ctx, *d.resolved) != nil:
		return client.MeDriveUnavailableUnmountable
	case checked && driveNotInPool(pool.Pool):
		return client.MeDriveUnavailableNotInPool
	}
	return ""
}

// driveNotInPool is the pool half of a drive's availability: no drive mounts on
// a self-hosted pool's runners, whose substrates hold no organisation storage.
//
// ponytail: a remote-provided pool whose executors cannot mount drives waits
// for per-executor pool facts; until then the deployment-wide runner
// capability decides it, as unmountable.
func driveNotInPool(pool types.RunnerPool) bool {
	return pool.HostingType == types.RunnerPoolSelfHosted
}

// catalogueDrivePool resolves runner_pool_id to a pool the caller may use, or
// checked=false: an absent, malformed, unknown or unpermitted id is never an
// error here, since the pool is checked again at launch.
func (s *Server) catalogueDrivePool(ctx context.Context, raw string) (store.RunnerPoolCatalogueRow, bool) {
	id, err := uuid.Parse(raw)
	ps, ok := s.cfg.Store.(store.RunnerPoolStore)
	if raw == "" || err != nil || !ok {
		return store.RunnerPoolCatalogueRow{}, false
	}
	rows, refusal := s.permittedPools(ctx, ps, true)
	if refusal != nil {
		return store.RunnerPoolCatalogueRow{}, false
	}
	return catalogueRow(rows, id)
}

// driveSource names the tier of a drive's winning grant as the catalogue's
// source chip reads it.
func driveSource(tier types.CapabilitySubjectType) string {
	switch tier {
	case types.CapabilitySubjectUser:
		return client.MeDriveSourcePersonal
	case types.CapabilitySubjectGroup:
		return client.MeDriveSourceTeam
	default:
		return client.MeDriveSourceOrganisation
	}
}

// writeUserDriveGrant is POST /drives/grants' store write: the replacing
// upsert, which also reports the drives the subject lost, or with additional an
// allocation beside the subject's others.
func (s *Server) writeUserDriveGrant(ctx context.Context, g types.UserDriveGrant, homeOverrideStated, additional bool) (types.UserDriveGrant, []uuid.UUID, error) {
	w, ok := s.cfg.Store.(store.UserDriveGrantWriter)
	switch {
	case additional && ok:
		saved, err := w.AddUserDriveGrant(ctx, g, homeOverrideStated)
		return saved, nil, err
	case additional:
		return types.UserDriveGrant{}, nil, errors.New("api: this store cannot allocate a drive beside another")
	case ok:
		return w.ReplaceUserDriveGrant(ctx, g, homeOverrideStated)
	}
	saved, err := s.cfg.Store.UpsertUserDriveGrant(ctx, g, homeOverrideStated)
	return saved, nil, err
}
