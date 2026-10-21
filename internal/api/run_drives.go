// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"cmp"
	"fmt"
	"net/http"
	"slices"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/authz"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/pkg/client"
)

// maxRunDrives bounds drives[]: each entry costs a share probe at create.
const maxRunDrives = 16

// runDrive is one drive a run attaches, authorized through the caller's own
// grant on it: the resolution, the path it mounts at, and the request's
// read-only narrowing. legacy marks the drive at /home/agent/drive.
type runDrive struct {
	resolved types.ResolvedDrive
	target   string
	narrow   *bool
	legacy   bool
	// id is the drives[] id that named it; "" for the legacy drive.enabled flag.
	id string
}

// readOnly is the grant's posture narrowed by the request, never widened.
func (d runDrive) readOnly() bool {
	return !d.resolved.Writable || d.narrow != nil && *d.narrow
}

// driveSelectionRefusal checks drives[]'s shape: never beside drive, at most
// maxRunDrives, each a canonical drive id once. Whether the caller may attach
// each is stepDrive's.
func driveSelectionRefusal(req createRunRequest) *runRefusal {
	switch {
	case len(req.Drives) == 0:
		return nil
	case req.Drive != nil:
		return runError(http.StatusBadRequest, reasonDriveSelectionConflict, driveSelectionConflictMsg())
	case len(req.Drives) > maxRunDrives:
		return runError(http.StatusBadRequest, reasonDriveInvalid, driveTooManyMsg(maxRunDrives))
	}
	seen := make(map[uuid.UUID]bool, len(req.Drives))
	for i, a := range req.Drives {
		id, err := uuid.Parse(a.DriveID)
		if err != nil || id == uuid.Nil || id.String() != a.DriveID || seen[id] {
			return runError(http.StatusBadRequest, reasonDriveInvalid, driveInvalidMsg(i))
		}
		seen[id] = true
	}
	return nil
}

// authorizeRequestDrives resolves the drives a run request asks for: the legacy
// drive.enabled flag through authorizeRequestDrive, or each drives[] id through
// the caller's own grant on THAT drive (reachableDrives). An id the caller's
// grants do not reach is drive_not_found in the same bytes whoever's it is; a
// paused winner refuses and never falls through; two drives deriving one path
// refuse rather than shadow each other.
func (s *Server) authorizeRequestDrives(r *http.Request, req createRunRequest, ceiling governanceCeiling) ([]runDrive, *runRefusal) {
	if len(req.Drives) == 0 {
		resolved, refusal := s.authorizeRequestDrive(r, req, ceiling)
		if resolved == nil {
			return nil, refusal
		}
		return []runDrive{{resolved: *resolved, target: runner.DriveTargetFor(*resolved, true), narrow: req.Drive.ReadOnly, legacy: true}}, nil
	}
	if refusal := s.driveDoorsRefusal(r, ceiling); refusal != nil {
		return nil, refusal
	}
	reach, err := s.reachableDrives(r.Context(), ceiling.Limits.MaxDriveSizeMiB)
	if err != nil {
		return nil, driveRunError(err)
	}
	out := make([]runDrive, 0, len(req.Drives))
	for _, a := range req.Drives {
		i := slices.IndexFunc(reach, func(d reachableDrive) bool { return d.winner.Drive.ID.String() == a.DriveID })
		if i < 0 {
			// The row carries no id (authz.Datum records no sentence), so
			// another person's drive and an id that names nothing leave the
			// same row; only the 422 body echoes the id as sent.
			return nil, driveDenied(authz.Deny(authz.ReasonDriveNotFound, "runs.drive", ""), driveNotFoundMsg(a.DriveID))
		}
		d := reach[i]
		switch {
		case d.err != nil:
			return nil, driveRunError(d.err)
		case d.resolved.Paused:
			return nil, drivePausedRefusal(d.winner.Drive.Name)
		case d.target() == "":
			return nil, driveRunError(fmt.Errorf("%w: drive: this deployment cannot mount drive %s (its name gives it no folder; ask an admin to rename it)",
				errDriveUnmountable, d.winner.Drive.Name))
		}
		if j := slices.IndexFunc(out, func(o runDrive) bool { return o.target == d.target() }); j >= 0 {
			msg := driveTargetConflictMsg(out[j].resolved.Drive.Name, d.winner.Drive.Name, d.target())
			return nil, driveDenied(authz.Deny(authz.ReasonDriveTargetConflict, "runs.drive", msg), msg)
		}
		out = append(out, runDrive{resolved: *d.resolved, target: d.target(), narrow: a.ReadOnly, legacy: d.legacy, id: a.DriveID})
	}
	return out, driveKindMixRefusal(out)
}

// driveDenied is a drives[] refusal that is also d's authz.denied row (target
// runs.drive); the wire answer is msg.
func driveDenied(d authz.Decision, msg string) *runRefusal {
	return &runRefusal{status: http.StatusUnprocessableEntity, body: errorBody{Error: msg, Reason: string(d.Reason)}, decision: &d}
}

// driveKindMixRefusal refuses a shared Kubernetes drive beside a managed one,
// before anything is created: the managed drive needs the pod's fsGroup, which
// is never set beside a share (the k8s substrate refuses the pair too). A
// Kubernetes backend mounts only on a Kubernetes deployment, so its backend is
// the deployment's.
func driveKindMixRefusal(drives []runDrive) *runRefusal {
	var share, managed string
	for _, d := range drives {
		b := d.resolved.Drive.Backend
		switch {
		case b.RunnerTarget() != "k8s":
		case b.Kind() == types.DriveKindManaged:
			managed = cmp.Or(managed, d.resolved.Drive.Name)
		default:
			share = cmp.Or(share, d.resolved.Drive.Name)
		}
	}
	if share == "" || managed == "" {
		return nil
	}
	return runError(http.StatusUnprocessableEntity, reasonDriveShareBesideManaged, driveShareBesideManagedMsg(share, managed))
}

// seedRequestDrives resolves the request's drives into the mounts the runner
// will execute, writing its own HTTP error and returning ok=false once it has
// responded. Every drive mounts or the run is refused: one that cannot is
// never dropped, downgraded or mounted elsewhere.
//
// Called after validateWorkspaceSources at create and at preflight, for the
// reason stepDrive states and because a dry run that previewed a green Review
// for a launch that will 422 is the one thing preflight exists not to do.
// No drive asked for is a provable no-op: no store read, no ceiling read.
func (s *Server) seedRequestDrives(w http.ResponseWriter, r *http.Request,
	req createRunRequest, ceiling governanceCeiling) ([]runDrive, []types.DriveMount, bool) {
	drives, refusal := s.authorizeRequestDrives(r, req, ceiling)
	if refusal.write(s, w, r) {
		return nil, nil, false
	}
	mounts := make([]types.DriveMount, 0, len(drives))
	for _, d := range drives {
		mount, ok := s.driveMountFor(r.Context(), w, d)
		if !ok {
			return nil, nil, false
		}
		mounts = append(mounts, *mount)
	}
	return drives, mounts, true
}

// driveTargets are the run's drives as attachment targets, for the overlap tree,
// the starting folder and the mount map. The legacy flag's drive is shown as
// "your drive" under the "drive" attachment; a drives[] entry under its id.
func driveTargets(drives []runDrive) []runTarget {
	out := make([]runTarget, 0, len(drives))
	for _, d := range drives {
		t := runTarget{kind: attachmentDrive, owner: attachmentDrive, name: runDriveName, target: d.target, readOnly: d.readOnly()}
		if d.id != "" {
			t.owner, t.name, t.driveID = startFolderDriveOwner(d.id), d.resolved.Drive.Name, d.id
		}
		out = append(out, t)
	}
	return out
}

// startFolderDriveOwner is the attachment owner a drives[] entry is held under,
// which start_folder.drive_id names.
func startFolderDriveOwner(id string) string { return attachmentDrive + ":" + id }

// stampDriveAudit records on run.create each drive a drives[] request attaches:
// its id, name, path and mode, never a host path or object name. A legacy
// drive.enabled run records nothing here, as before; its mount is
// run.drive.mount's. A keep_work_folder run adds keep_work_folder and the id
// of the drive it keeps its work folder in.
func stampDriveAudit(data map[string]any, drives []runDrive, keep bool, runID uuid.UUID) {
	if keep {
		data["keep_work_folder"], data["kept_drive_id"] = true, keptWorkFolderDriveID(runID)
	}
	var out []map[string]any
	for _, d := range drives {
		if d.id != "" {
			out = append(out, map[string]any{"drive_id": d.id, "name": d.resolved.Drive.Name, "target": d.target, "read_only": d.readOnly()})
		}
	}
	if out != nil {
		data["drives"] = out
	}
}

// driveStartFolderKnown reports whether start_folder.drive_id names one of the
// request's drives[].
func driveStartFolderKnown(req createRunRequest, id string) bool {
	return slices.ContainsFunc(req.Drives, func(a client.DriveAttachment) bool { return a.DriveID == id })
}
