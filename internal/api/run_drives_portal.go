// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/audit"
	"github.com/cjohnstoniv/wardyn/internal/authz"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// A run through an issuer portal mounts only the team drives allocated to that portal. The person's own catalogue
// (reachableDrives) is never read for it, so a drive allocated to everyone, to the person, to a group, or owned by the
// person is not reachable through the portal. Every other caller keeps authorizeRequestDrives' own path.

// portalDriveNotAllocatedMsg is the one sentence every refusal of a portal's drive carries, whatever the cause: it
// names no drive, so another portal's drive, a group's, the person's, nobody's and an id that names nothing read alike.
const portalDriveNotAllocatedMsg = "This drive is not allocated to this portal; ask a super admin to allocate it, or run without it"

// portalRunScopeMsg is the 403 for a run that mounted another portal's drive.
const portalRunScopeMsg = "This run mounted a drive that belongs to another portal, so only that portal can reach it"

// portalDriveDenied is the 403 for a portal's drives[] entry, or its legacy drive flag, that no allocation of the
// portal reaches: one body and one denial row for every cause.
func portalDriveDenied() *runRefusal {
	return runDenied(authz.Deny(authz.ReasonDriveNotAllocatedToPortal, "runs.drive", portalDriveNotAllocatedMsg))
}

// issuerPortalOf is the issuer portal the request came through: its delegates.id, and false for a request that is not
// delegated or came through a console portal. A portal row that cannot be read is a 503, never "not an issuer portal".
func (s *Server) issuerPortalOf(ctx context.Context) (uuid.UUID, bool, *runRefusal) {
	via, ok := audit.DelegationFrom(ctx)
	if !ok {
		return uuid.Nil, false, nil
	}
	unavailable := runError(http.StatusServiceUnavailable, reasonPortalUnitUnavailable, portalUnitUnavailableMsg)
	dr, ok := s.cfg.Store.(store.DelegateReader)
	if !ok {
		return uuid.Nil, false, unavailable
	}
	d, err := dr.GetDelegateByID(ctx, via.Delegate)
	if err != nil {
		slog.ErrorContext(ctx, "portal drives: read the portal", slog.String("delegate", via.Delegate.String()), slog.Any("err", err))
		return uuid.Nil, false, unavailable
	}
	return d.ID, d.Issuer != "", nil
}

// portalReachableDrives resolves every drive allocated to the portal, each resolved for the portal. It keeps
// reachableDrives' doctrine for the part that applies: no portal store is no drives, a store error is an error, and a
// disabled allocation is paused and never falls through to anything else.
func (s *Server) portalReachableDrives(ctx context.Context, portal uuid.UUID, profileMaxDriveMiB int) ([]reachableDrive, error) {
	ps, ok := s.cfg.Store.(store.PortalDriveStore)
	if !ok {
		return nil, nil
	}
	ceiling, err := s.driveSizeCeilingFor(ctx, profileMaxDriveMiB)
	if err != nil {
		return nil, err
	}
	winners, err := ps.ResolvePortalTeamDrives(ctx, portal)
	if err != nil {
		return nil, err
	}
	out := make([]reachableDrive, 0, len(winners))
	for _, w := range winners {
		d := reachableDrive{winner: w}
		d.resolved, d.err = newResolvedDrive(&d.winner.Drive, &d.winner.Grant, types.DriveSubjectPortal, nil, ceiling)
		if d.err != nil && !errors.Is(d.err, errDriveUnmountable) {
			return nil, d.err
		}
		out = append(out, d)
	}
	return out, nil
}

// portalResolvedDrive finishes a portal allocation's resolution beside teamResolvedDrive: ONE folder per portal and
// drive (types.DrivePortalHome), the same for every person who reaches the drive through that portal, and no subject
// fingerprint since the object is nobody's own. A row that breaks the portal rule mounts nothing.
func portalResolvedDrive(resolved *types.ResolvedDrive) (*types.ResolvedDrive, error) {
	portal, err := uuid.Parse(resolved.Grant.Subject)
	grant := types.UserDriveGrant{SubjectType: types.DriveSubjectPortal, HomeOverride: resolved.Grant.HomeOverride}
	if err != nil || types.ValidateTeamDriveGrant(resolved.Drive, grant) != nil || resolved.Drive.AltBackend != "" ||
		(resolved.Drive.Backend != types.DriveBackendDockerVolume && resolved.Drive.Backend != types.DriveBackendK8sPVCStatic) {
		slog.Warn("wardynd: user drive: a portal's allocation breaks the portal rule and mounts nothing",
			slog.String("drive", resolved.Drive.Name))
		return nil, fmt.Errorf("%w: drive: this deployment cannot mount drive %s (a portal's folder needs a team drive on docker_volume or k8s_pvc_static with no second place; ask an admin)",
			errDriveUnmountable, resolved.Drive.Name)
	}
	resolved.HomeName = types.DrivePortalHome(resolved.Drive, portal)
	resolved.ObjectName = types.DrivePortalObjectName(resolved.Drive, resolved.HomeName)
	return resolved, nil
}

// portalDriveMounts maps each drive of the run that came through its portal to that portal, for the run.drive.mount
// audit row. Server-side only: it is never a field of types.DriveMount, which goes to the substrate. nil for a run
// with none, so a console portal's run that mounts a team drive through a group allocation says nothing of a portal.
func portalDriveMounts(drives []runDrive, via *uuid.UUID) map[uuid.UUID]uuid.UUID {
	var out map[uuid.UUID]uuid.UUID
	for _, d := range drives {
		if !d.portal || via == nil {
			continue
		}
		if out == nil {
			out = map[uuid.UUID]uuid.UUID{}
		}
		out[d.resolved.Drive.ID] = *via
	}
	return out
}

// runMountsPortalDrive reports whether any of the run's drives came through its portal: the value of
// agent_runs.portal_drives.
func runMountsPortalDrive(drives []runDrive) bool {
	for _, d := range drives {
		if d.portal {
			return true
		}
	}
	return false
}

// writePortalRevoked answers a create refused because its portal was revoked before the run was written: the 401 every
// request of a revoked portal gets.
func writePortalRevoked(w http.ResponseWriter) {
	writeErrorReason(w, http.StatusUnauthorized, reasonInvalidAdminToken, "invalid admin token")
}

// portalDriveModesCtxKey carries, on a queued run's start, the modes its portal drives were admitted with.
type portalDriveModesCtxKey struct{}

// portalDriveModes is the effective mode of each portal drive of drives by drive id, true for read-only: what a
// queued run is admitted with (queuedLaunch.DriveModes). nil when no drive came through a portal.
func portalDriveModes(drives []runDrive) map[string]bool {
	var out map[string]bool
	for _, d := range drives {
		if !d.portal || d.id == "" {
			continue
		}
		if out == nil {
			out = map[string]bool{}
		}
		out[d.id] = d.readOnly()
	}
	return out
}

// withPortalDriveModes marks ctx as the start of a queued run admitted with modes (nil is none recorded).
func withPortalDriveModes(ctx context.Context, modes map[string]bool) context.Context {
	if modes == nil {
		modes = map[string]bool{}
	}
	return context.WithValue(ctx, portalDriveModesCtxKey{}, modes)
}

// portalDriveModeCap is the read_only a portal drive mounts with, given the request's own (asked): outside a queued
// start it is asked unchanged. At a queued start the fresh resolution never widens what the run was admitted with, so
// a drive admitted read-only, or one the launch recorded no mode for, mounts read-only whatever the allocation says
// now; a drive admitted writable keeps the fresh resolution, which still narrows to read-only if the allocation did.
func portalDriveModeCap(ctx context.Context, drive string, asked *bool) *bool {
	modes, queued := ctx.Value(portalDriveModesCtxKey{}).(map[string]bool)
	if !queued {
		return asked
	}
	if readOnly, recorded := modes[drive]; recorded && !readOnly {
		return asked
	}
	ro := true
	return &ro
}
