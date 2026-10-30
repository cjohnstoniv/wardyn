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
	case d.OwnerSubject != "":
		// A person's own drive never mounts on a pool of people's runners
		// (memberDriveNeverOnPool): its sentence is the pool's, as before.
		return driveNotInPoolMsg(d.Name)
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
// runners, or a kept work folder in a managed volume mounts only on the
// organisation's runners. A disk drive is bound to the Mac it is kept on, and a
// macOS VM's kept work folder goes where its drives do.
func drivesBoundRunner(drives []runDrive, keep *keptWorkFolder) *uuid.UUID {
	if (keep != nil && !keep.onMac()) || len(drives) == 0 {
		return nil
	}
	first := driveBoundRunnerOf(drives[0].resolved.Drive)
	for _, d := range drives {
		if id := driveBoundRunnerOf(d.resolved.Drive); first == nil || id == nil || *id != *first {
			return nil
		}
	}
	return first
}

// driveBoundRunnerOf is the one runner d mounts on, nil for none: the runner its
// second place is bound to, or the Mac a disk drive is kept on.
func driveBoundRunnerOf(d types.UserDrive) *uuid.UUID {
	if d.Backend == types.DriveBackendMacDisk {
		return d.HomeRunnerID
	}
	return d.AltRunnerID
}

// driveOnRunner is driveOnRunnerMsg for any drive a run mounts on a person's own
// runner: a disk drive mounts only in a macOS VM on the Mac it is kept on, and is
// refused anywhere else naming that Mac, which is its owner's own (MAC-D05). A
// person's Docker or cluster volume in a macOS VM is refused as the volume it is.
func (s *Server) driveOnRunner(ctx context.Context, d types.UserDrive, runner *uuid.UUID, macOS bool) string {
	switch {
	case macOS && d.OwnerSubject != "" && d.Backend.RunnerTarget() == "k8s":
		return driveMacOSClusterVolumeMsg(d.Name)
	case macOS && d.OwnerSubject != "" && d.Backend == types.DriveBackendDockerVolume:
		return driveMacOSDockerVolumeMsg(d.Name)
	case d.Backend != types.DriveBackendMacDisk:
		return driveOnRunnerMsg(d, runner)
	}
	if macOS && runner != nil && d.HomeRunnerID != nil && *runner == *d.HomeRunnerID {
		return ""
	}
	_, mac, _ := s.macDiskHome(ctx, d)
	return driveMacDiskElsewhereMsg(d.Name, mac)
}

// macDiskName is the Mac a disk drive is kept on, by name, for its owner's
// catalogue; "" for any other drive.
func (s *Server) macDiskName(ctx context.Context, d types.UserDrive) string {
	if d.Backend != types.DriveBackendMacDisk {
		return ""
	}
	_, mac, _ := s.macDiskHome(ctx, d)
	return mac
}

// macDiskNoName is a disk drive's Mac in a sentence when its row cannot be read.
const macDiskNoName = "your Mac"

// macDiskHome is the Mac a disk drive is kept on: its row, revoked or not, and the
// name its owner knows it by. store.ErrNotFound when the drive names no Mac or the
// row is gone; a store that keeps no runners is an error, never a gone Mac.
func (s *Server) macDiskHome(ctx context.Context, d types.UserDrive) (types.Runner, string, error) {
	st, ok := s.cfg.Store.(store.RunnerStore)
	switch {
	case d.HomeRunnerID == nil:
		return types.Runner{}, macDiskNoName, store.ErrNotFound
	case !ok:
		return types.Runner{}, macDiskNoName, errors.New("api: the store keeps no runners")
	}
	rn, err := st.GetRunner(ctx, *d.HomeRunnerID)
	if err != nil {
		return types.Runner{}, macDiskNoName, err
	}
	return rn, cmp.Or(rn.Name, rn.MachineName, macDiskNoName), nil
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
// --drive-root). The runner checks the bind again against the same roots. A disk
// drive's is verified when it is kept on candidate.
func (s *Server) drivePathsOn(candidate uuid.UUID, drives []types.DriveMount) map[string]bool {
	bound := map[string]boundLocalPath{}
	var disks []string
	for i, d := range drives {
		switch path := "SandboxSpec.Drives[" + strconv.Itoa(i) + "]"; {
		case d.RunnerID != nil && d.Backend == types.DriveBackendHostPath:
			bound[path] = boundLocalPath{Runner: *d.RunnerID, Path: d.ObjectName}
		case d.RunnerID != nil && d.Backend == types.DriveBackendMacDisk && *d.RunnerID == candidate:
			// A disk drive is a file on the Mac it is kept on, so being bound to candidate is all it needs.
			disks = append(disks, path)
		}
	}
	if len(bound) == 0 && len(disks) == 0 {
		return nil
	}
	var out map[string]bool
	if len(bound) > 0 {
		_, _, roots := s.runnerReportedCaps(candidate)
		out = verifiedLocalPaths(candidate, roots, bound)
	}
	for _, path := range disks {
		if out == nil {
			out = map[string]bool{}
		}
		out[path] = true
	}
	return out
}

// boundMountRunner is the runner the first bound drive mount names: the drive's
// stored binding (driveMountFor), the candidate a create-time local plan is
// judged on (kindTargets already holds a run with bound drives to that runner);
// uuid.Nil when none is bound. Dispatch judges the run's own runner instead.
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
	if d.Backend == types.DriveBackendMacDisk {
		return "" // a disk drive mounts only on its Mac, which MeDrive.Mac names
	}
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
