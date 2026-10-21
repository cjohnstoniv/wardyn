// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// A drive's second place (#112, #113, mock M10): the same share as one person's
// own runner sees it. Runs on that runner mount the drive from there; every other
// run mounts it from the first place, on the organisation's runners, as before. A
// drive with no second place behaves exactly as it did.

// drivePlace is where a run mounts its drives: the organisation's runners (the
// zero value) or one person's own runner.
type drivePlace struct{ runner *uuid.UUID }

// driveSecondPlaceMsg is the admin save refusal for a second place of the wrong shape.
func driveSecondPlaceMsg(name string) string {
	return fmt.Sprintf("user_drives: %q: the second place must also be a share, and somewhere the first is not", name)
}

// driveSecondPlaceRunnerMsg refuses a second place whose runner is not a claimed runner of one person's own.
func driveSecondPlaceRunnerMsg(name string) string {
	return fmt.Sprintf("user_drives: %q: the second place's runner must be a person's own runner that is registered and not revoked", name)
}

// driveSecondPlaceRefusal checks a drive write's second place: its shape
// (types.ValidateDriveSecondPlace), then that its runner is a claimed runner of
// one person's own. 0, "" when the write may go on.
func (s *Server) driveSecondPlaceRefusal(ctx context.Context, d *types.UserDrive) (int, string) {
	err := types.ValidateDriveSecondPlace(d)
	switch {
	case errors.Is(err, types.ErrDriveSecondPlace):
		return http.StatusBadRequest, driveSecondPlaceMsg(d.Name)
	case err != nil:
		return http.StatusBadRequest, "invalid drive: " + err.Error()
	case d.AltRunnerID == nil:
		return 0, ""
	}
	st, ok := s.cfg.Store.(store.RunnerStore)
	if !ok {
		return http.StatusUnprocessableEntity, driveSecondPlaceRunnerMsg(d.Name)
	}
	rn, err := st.GetRunner(ctx, *d.AltRunnerID)
	switch {
	case errors.Is(err, store.ErrNotFound):
		return http.StatusUnprocessableEntity, driveSecondPlaceRunnerMsg(d.Name)
	case err != nil:
		return http.StatusServiceUnavailable, "user_drives: the second place's runner could not be read; try again"
	case rn.OwnerKind != types.RunnerOwnerPerson || rn.State != types.RunnerClaimed || rn.RevokedAt != nil:
		return http.StatusUnprocessableEntity, driveSecondPlaceRunnerMsg(d.Name)
	}
	return 0, ""
}

// driveOnRunnerMsg is the member refusal for mounting d on a person's own runner,
// "" when d's second place is bound to that runner. The bound runner is never
// named: the person asking may not own it.
func driveOnRunnerMsg(d types.UserDrive, runner *uuid.UUID) string {
	switch {
	case d.AltRunnerID == nil:
		return driveOrgRunnersOnlyMsg(d.Name)
	case runner == nil || *d.AltRunnerID != *runner:
		return driveBoundElsewhereMsg(d.Name)
	}
	return ""
}

// secondPlaceResolved is resolved as its second place mounts it: the same home,
// under the second place's root.
func secondPlaceResolved(resolved types.ResolvedDrive) types.ResolvedDrive {
	out := resolved
	out.Drive = resolved.Drive.SecondPlace()
	out.Enforcement = types.EnforcementFor(out.Drive.Backend)
	out.ObjectName = types.DriveObjectName(out.Drive, resolved.HomeName)
	return out
}

// drivesBoundRunner is the one runner every drive of a run is bound to, or nil:
// a run with a drive that has no second place, drives bound to different
// runners, or a kept work folder (a managed volume) mounts only on the
// organisation's runners.
func drivesBoundRunner(drives []runDrive, keep *keptWorkFolder) *uuid.UUID {
	if keep != nil || len(drives) == 0 {
		return nil
	}
	first := drives[0].resolved.Drive.AltRunnerID
	for _, d := range drives {
		if id := d.resolved.Drive.AltRunnerID; first == nil || id == nil || *id != *first {
			return nil
		}
	}
	return first
}

// placedRunner is the one person's own runner a pooled local run goes to, nil
// when its candidates are not all one runner.
func (p runPlacement) placedRunner() *uuid.UUID {
	var out *uuid.UUID
	for _, t := range p.eligible {
		if t.runnerID == nil || (out != nil && *out != *t.runnerID) {
			return nil
		}
		out = t.runnerID
	}
	return out
}

// drivePathsOn is a local plan's VerifiedLocalPaths for its drives on candidate:
// a drive's path is verified only when its second place is bound to candidate
// and the path lies inside the roots candidate advertises (its owner's
// --drive-root). The runner checks the bind again against the same roots.
func (s *Server) drivePathsOn(candidate uuid.UUID, drives []types.DriveMount) map[string]bool {
	bound := map[string]boundLocalPath{}
	for i, d := range drives {
		if d.RunnerID != nil && d.Backend == types.DriveBackendHostPath {
			bound["SandboxSpec.Drives["+strconv.Itoa(i)+"]"] = boundLocalPath{Runner: *d.RunnerID, Path: d.ObjectName}
		}
	}
	if len(bound) == 0 {
		return nil
	}
	_, _, roots := s.runnerReportedCaps(candidate)
	return verifiedLocalPaths(candidate, roots, bound)
}

// boundMountRunner is the runner the first bound drive mount names, the
// candidate a create-time local plan is judged on (kindTargets already holds a
// run with bound drives to that runner); uuid.Nil when none is bound.
func boundMountRunner(drives []types.DriveMount) uuid.UUID {
	for _, d := range drives {
		if d.RunnerID != nil {
			return *d.RunnerID
		}
	}
	return uuid.Nil
}

// driveMountsFrom is a drive's "Mounts from" answer, which the console prints as
// it is: the organisation's runners, and the bound runner by name when the viewer
// may see it. Anyone else reads the organisation's runners alone, which is also
// where the drive mounts for them.
func (s *Server) driveMountsFrom(ctx context.Context, d types.UserDrive) string {
	const org = "Organisation runners"
	if name := s.boundRunnerName(ctx, d); name != "" {
		return org + " and " + name
	}
	return org
}

// boundRunnerName is the name of the runner d's second place is bound to, ""
// when it has none, the runner is gone or revoked, or the viewer on ctx may not
// see it: only an operator and the runner's owner are told which runner it is.
func (s *Server) boundRunnerName(ctx context.Context, d types.UserDrive) string {
	st, ok := s.cfg.Store.(store.RunnerStore)
	if d.AltRunnerID == nil || !ok {
		return ""
	}
	rn, err := st.GetRunner(ctx, *d.AltRunnerID)
	if err != nil || rn.RevokedAt != nil {
		return ""
	}
	if owner := poolOwner(ctx); !s.isOperator(ctx) && (owner == "" || owner != rn.Owner) {
		return ""
	}
	return cmp.Or(rn.Name, rn.MachineName)
}
