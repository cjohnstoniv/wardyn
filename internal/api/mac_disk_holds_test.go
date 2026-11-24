// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// holdFake is multiDriveStore with a disk drive hold record that answers what it is told and notes each call.
type holdFake struct {
	*multiDriveStore
	holder  uuid.UUID // MacDiskHolder's answer
	readErr error
	held    bool  // HoldMacDisk's ok
	holdErr error // HoldMacDisk's error
	reads   []uuid.UUID
	holds   [][2]uuid.UUID
}

func (s *holdFake) HoldMacDisk(_ context.Context, drive, run uuid.UUID) (bool, error) {
	s.holds = append(s.holds, [2]uuid.UUID{drive, run})
	return s.held, s.holdErr
}

func (s *holdFake) MacDiskHolder(_ context.Context, drive uuid.UUID) (uuid.UUID, error) {
	s.reads = append(s.reads, drive)
	return s.holder, s.readErr
}

var _ store.MacDiskHoldStore = (*holdFake)(nil)

// holdRig is a server whose caller owns one disk drive, Notes, kept on a Mac.
func holdRig(w store.UserDriveWinner) (*Server, *holdFake) {
	srv, ms := multiDriveServer(w)
	fake := &holdFake{multiDriveStore: ms}
	srv.cfg.Store = fake
	return srv, fake
}

func notesDisk() store.UserDriveWinner {
	mac := uuid.New()
	w := winnerOf("Notes", types.DriveBackendMacDisk, types.CapabilitySubjectUser, "sub-drive-bob", true)
	w.Drive.HomeRunnerID = &mac
	return w
}

// stepDriveAt runs the drive step at one door, as the owner of the drive.
func stepDriveAt(srv *Server, mode foldMode, driveID uuid.UUID) (bool, *httptest.ResponseRecorder) {
	req := drivesRequest(driveID.String())
	req.MachineKind = string(types.MachineMacOSVM)
	w := httptest.NewRecorder()
	f := &runFold{mode: mode, w: w, req: &req,
		r: httptest.NewRequest(http.MethodPost, "/api/v1/runs", nil).WithContext(driveMemberCtx(nil, false))}
	return srv.stepDrive(f, &foldRecorder{}), w
}

// A disk drive another run holds is 409 drive_in_another_run with the approved sentence at the preview, Review and
// create, writes no authz.denied row, and a free drive passes.
func TestMacDiskHold_RefusedAtEveryDoor(t *testing.T) {
	const want = "Notes is in another macOS VM on this Mac. A disk drive is in one run at a time: try again when that run ends."
	for _, mode := range []foldMode{foldPreview, foldPreflight, foldCreate} {
		w := notesDisk()
		srv, fake := holdRig(w)
		if ok, _ := stepDriveAt(srv, mode, w.Drive.ID); !ok {
			t.Fatalf("mode %d: a free disk drive was refused", mode)
		}
		fake.holder = uuid.New()
		ok, rec := stepDriveAt(srv, mode, w.Drive.ID)
		if ok || rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), reasonDriveInAnotherRun) || !strings.Contains(rec.Body.String(), want) {
			t.Errorf("mode %d: ok %v, %d %s; want 409 %s %q", mode, ok, rec.Code, rec.Body, reasonDriveInAnotherRun, want)
		}
		if n := len(srv.cfg.Audit.(*recRecorder).snapshot()); n != 0 {
			t.Errorf("mode %d: %d audit rows written, want none", mode, n)
		}
	}
}

// A store that cannot answer refuses, and never reads as "no hold".
func TestMacDiskHold_ReadErrorRefuses(t *testing.T) {
	w := notesDisk()
	for _, mode := range []foldMode{foldPreview, foldPreflight, foldCreate} {
		srv, fake := holdRig(w)
		fake.readErr = errors.New("connection reset")
		ok, rec := stepDriveAt(srv, mode, w.Drive.ID)
		if ok || rec.Code != http.StatusInternalServerError {
			t.Errorf("mode %d: ok %v, %d %s; want a refusal, 500", mode, ok, rec.Code, rec.Body)
		}
	}
}

// Only a person's disk drive reaches the table, on the read and on the write.
func TestMacDiskHold_OtherBackendsUntouched(t *testing.T) {
	mac := uuid.New()
	disk := types.UserDrive{ID: uuid.New(), Name: "Notes", Backend: types.DriveBackendMacDisk}
	var others []types.UserDrive
	for _, b := range []types.DriveBackend{types.DriveBackendDockerVolume, types.DriveBackendHostPath, types.DriveBackendK8sPVC, types.DriveBackendK8sPVCStatic} {
		others = append(others, types.UserDrive{ID: uuid.New(), Name: string(b), Backend: b})
	}
	srv, fake := holdRig(notesDisk())
	fake.holder = uuid.New()
	var runDrives []runDrive
	var mounts []types.DriveMount
	for _, d := range others {
		runDrives = append(runDrives, runDrive{resolved: types.ResolvedDrive{Drive: d}})
		mounts = append(mounts, types.DriveMount{DriveID: d.ID, Backend: d.Backend, RunnerID: &mac})
	}
	if refusal := srv.macDiskHeldRefusal(context.Background(), runDrives); refusal != nil {
		t.Errorf("a drive that is not a disk was refused: %+v", refusal)
	}
	run := types.AgentRun{ID: uuid.New()}
	if !srv.holdMacDiskDrives(context.Background(), run, mounts) || len(fake.reads) != 0 || len(fake.holds) != 0 {
		t.Errorf("reads %v, holds %v; want the table untouched", fake.reads, fake.holds)
	}
	// The one disk is read and written, and a disk mount with no drive id is not.
	refusal := srv.macDiskHeldRefusal(context.Background(), []runDrive{{resolved: types.ResolvedDrive{Drive: disk}}})
	if refusal == nil || len(fake.reads) != 1 || fake.reads[0] != disk.ID {
		t.Errorf("refusal %+v, reads %v; want the disk read once", refusal, fake.reads)
	}
	srv.holdMacDiskDrives(context.Background(), run, []types.DriveMount{
		{DriveID: disk.ID, Backend: types.DriveBackendMacDisk}, {Backend: types.DriveBackendMacDisk}})
	if len(fake.holds) != 1 || fake.holds[0] != [2]uuid.UUID{disk.ID, run.ID} {
		t.Errorf("holds %v, want one write of the disk for the run", fake.holds)
	}
}

// Dispatch never refuses on a hold: an erroring store and a lost race both return true, and a store without the
// record returns true without a read.
func TestMacDiskHold_DispatchNeverRefuses(t *testing.T) {
	mount := []types.DriveMount{{DriveID: uuid.New(), Backend: types.DriveBackendMacDisk}}
	run := types.AgentRun{ID: uuid.New()}
	srv, fake := holdRig(notesDisk())
	fake.holdErr = errors.New("connection reset")
	if !srv.holdMacDiskDrives(context.Background(), run, mount) {
		t.Error("a failed write refused the dispatch")
	}
	fake.holdErr, fake.held = nil, false // another live run holds it
	if !srv.holdMacDiskDrives(context.Background(), run, mount) {
		t.Error("a lost race refused the dispatch")
	}
	if len(fake.holds) != 2 {
		t.Errorf("holds = %d, want 2 writes", len(fake.holds))
	}
	plain, _ := multiDriveServer(notesDisk())
	if !plain.holdMacDiskDrives(context.Background(), run, mount) {
		t.Error("a store without the record refused the dispatch")
	}
}
