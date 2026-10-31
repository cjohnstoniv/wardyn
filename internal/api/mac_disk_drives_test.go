// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/authz"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/runner/remote"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// A disk drive is bound to the Mac it is kept on: it is on a pool only when the pool holds that Mac, a
// run with it goes to that Mac, a macOS VM's kept work folder follows it there, and its mount is
// verified for that Mac alone.
func TestDiskDriveIsBoundToItsMac(t *testing.T) {
	mac, other := uuid.New(), uuid.New()
	disk := types.UserDrive{Name: "Notes", Backend: types.DriveBackendMacDisk, OwnerSubject: "alice", HomeRunnerID: &mac}
	withMac := store.RunnerPoolCatalogueRow{Pool: types.RunnerPool{Scope: types.RunnerPoolUser}, OwnRunnerIDs: []uuid.UUID{mac}}
	withoutMac := store.RunnerPoolCatalogueRow{Pool: types.RunnerPool{Scope: types.RunnerPoolUser}, OwnRunnerIDs: []uuid.UUID{other}}
	if memberDriveNeverOnPool(withMac, disk) || !memberDriveNeverOnPool(withoutMac, disk) {
		t.Fatal("a disk drive must be on a pool exactly when the pool holds its Mac")
	}
	drives := []runDrive{{resolved: types.ResolvedDrive{Drive: disk}}}
	if got := drivesBoundRunner(drives, nil); got == nil || *got != mac {
		t.Fatalf("drivesBoundRunner = %v, want the drive's Mac", got)
	}
	macKeep := &keptWorkFolder{row: types.UserDrive{Backend: types.DriveBackendMacDisk}}
	if got := drivesBoundRunner(drives, macKeep); got == nil || *got != mac {
		t.Fatalf("with a macOS VM's kept work folder, drivesBoundRunner = %v, want the drive's Mac", got)
	}
	if got := drivesBoundRunner(drives, &keptWorkFolder{row: types.UserDrive{Backend: types.DriveBackendDockerVolume}}); got != nil {
		t.Fatalf("a kept volume binds the run to %v, want none", got)
	}
	s := &Server{}
	mounts := []types.DriveMount{{Backend: types.DriveBackendMacDisk, RunnerID: &mac}}
	if got := s.drivePathsOn(mac, mounts); !got["SandboxSpec.Drives[0]"] {
		t.Fatalf("drivePathsOn(its Mac) = %v, want the disk verified", got)
	}
	if got := s.drivePathsOn(other, mounts); got["SandboxSpec.Drives[0]"] {
		t.Fatalf("drivePathsOn(another runner) = %v, want the disk unverified", got)
	}
}

// macDiskStore is memberDriveStore with the one Mac its disk drives are kept on.
type macDiskStore struct {
	*memberDriveStore
	mac types.Runner
}

func (s *macDiskStore) GetRunner(_ context.Context, id uuid.UUID) (types.Runner, error) {
	if id != s.mac.ID {
		return types.Runner{}, store.ErrNotFound
	}
	return s.mac, nil
}

func (s *macDiskStore) ListMacDiskDeletes(context.Context) ([]types.UserDrive, error) {
	var out []types.UserDrive
	for _, d := range s.drives {
		if d.Backend == types.DriveBackendMacDisk && d.DeleteStartedAt != nil {
			out = append(out, d)
		}
	}
	return out, nil
}

// diskMacTransport is a Mac that keeps disk drives: reclaim_drive answers in use while inUse is set.
type diskMacTransport struct {
	capsTransport
	inUse bool
	asked int
}

func (t *diskMacTransport) Call(_ context.Context, method string, _, out any, _ ...runnerwire.CallOption) error {
	if method != runnerwire.MethodReclaimDrive {
		return t.capsTransport.Call(context.Background(), method, nil, nil)
	}
	t.asked++
	*out.(*runnerwire.ReclaimDriveResult) = runnerwire.ReclaimDriveResult{InUse: t.inUse, Outcome: runner.DriveReclaimDeleted}
	if t.inUse {
		out.(*runnerwire.ReclaimDriveResult).Outcome = ""
	}
	return nil
}

// macDiskDeleteRig is a server holding one online Mac that keeps disk drives, and Alice's disk drive on it.
func macDiskDeleteRig(t *testing.T) (*Server, *macDiskStore, *diskMacTransport, types.UserDrive) {
	t.Helper()
	mac := uuid.New()
	st := &macDiskStore{memberDriveStore: newMemberDriveStore(), mac: types.Runner{ID: mac, Owner: aliceSub, State: types.RunnerClaimed, Name: "Studio Mac"}}
	d := types.UserDrive{
		ID: uuid.New(), Name: "Notes", Backend: types.DriveBackendMacDisk, HomeTemplate: types.HomeTemplateHash,
		ObjectScheme: types.DriveObjectSchemeID, Writable: true, Reclaim: types.DriveReclaimDelete, OwnerSubject: aliceSub,
		HomeRunnerID: &mac, SizeMiB: 1024,
	}
	st.drives[d.ID] = d
	srv := New(Config{BaseCtx: context.Background(), Store: st, Audit: &recRecorder{}, RunnerTarget: "docker",
		RunnerOnline: func(id uuid.UUID) bool { return id == mac }})
	tr := &diskMacTransport{capsTransport: capsTransport{online: true}}
	sub := remote.New(mac.String(), tr, remote.Options{})
	data, err := json.Marshal(runnerwire.Caps{MacOSVM: &runnerwire.MacVMCaps{DiskDrives: true}})
	if err != nil {
		t.Fatal(err)
	}
	sub.HandleEvent(runnerwire.Event{Kind: runnerwire.EventCaps, Data: data})
	srv.runnerStreams.subsMu.Lock()
	if srv.runnerStreams.subs == nil {
		srv.runnerStreams.subs = map[uuid.UUID]runnerEndpoint{}
	}
	srv.runnerStreams.subs[mac] = runnerEndpoint{sub: sub, link: &remote.Link{}}
	srv.runnerStreams.subsMu.Unlock()
	return srv, st, tr, d
}

// A delete that waited for its Mac, met by the reconnect sweep while a run there still has the disk,
// keeps its tombstone; the next sweep, once the run has let go, deletes the drive.
func TestMacReconnectSweepKeepsADeleteItsMacStillHolds(t *testing.T) {
	srv, st, tr, d := macDiskDeleteRig(t)
	asked := st.now.Add(-2 * time.Minute) // the owner's delete, deferred while the Mac was away
	d.DeleteStartedAt = &asked
	st.drives[d.ID] = d
	tr.inUse = true
	if err := srv.finishMacDiskDeletes(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got, kept := st.drives[d.ID]; !kept || got.DeleteStartedAt == nil || tr.asked != 1 {
		t.Fatalf("kept = %v, tombstone %v, Mac asked %d times; want kept, still deleting, asked once", kept, got.DeleteStartedAt, tr.asked)
	}
	st.now = st.now.Add(2 * time.Minute)
	tr.inUse = false
	if err := srv.finishMacDiskDeletes(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, kept := st.drives[d.ID]; kept || tr.asked != 2 {
		t.Fatalf("after the run let go: kept = %v, Mac asked %d times; want deleted", kept, tr.asked)
	}
}

// The owner's own delete of a disk a run on its Mac has is refused and leaves the drive as it was.
func TestOwnerDeleteOfAHeldDiskStillAborts(t *testing.T) {
	srv, st, tr, d := macDiskDeleteRig(t)
	tr.inUse = true
	tomb, started, err := st.BeginMemberDriveDelete(context.Background(), aliceSub, d.ID)
	if err != nil {
		t.Fatal(err)
	}
	res := srv.deleteMemberDrive(context.Background(), st, tomb, started, memberDriveByOwner)
	if res.reason != reasonMemberDriveInUse || res.driverErr == nil {
		t.Fatalf("deletion = %+v, want refused in use by the Mac", res)
	}
	if got, kept := st.drives[d.ID]; !kept || got.DeleteStartedAt != nil {
		t.Fatalf("kept = %v, tombstone %v; want kept with the tombstone cleared", kept, got.DeleteStartedAt)
	}
}

// A run with a disk drive that is not placed on one person's own runner is refused before Review ends,
// under the reason the same sentence carries on the wrong runner.
func TestDiskDriveOffItsMacIsRefusedAsNotInPool(t *testing.T) {
	mac := uuid.New()
	disk := types.UserDrive{Name: "Notes", Backend: types.DriveBackendMacDisk, OwnerSubject: aliceSub, HomeRunnerID: &mac}
	w := httptest.NewRecorder()
	f := &runFold{w: w, req: &createRunRequest{}, r: httptest.NewRequest(http.MethodPost, "/api/v1/runs", nil),
		drives: []runDrive{{resolved: types.ResolvedDrive{Drive: disk}}}, mode: foldPreview}
	if (&Server{}).stepDriveEligibility(f, &foldRecorder{}) || w.Code != http.StatusUnprocessableEntity ||
		!strings.Contains(w.Body.String(), string(authz.ReasonDriveNotInPool)) || !strings.Contains(w.Body.String(), driveMacDiskElsewhereMsg("Notes", macDiskNoName)) {
		t.Fatalf("a disk drive on the organisation's runners = %d %s, want 422 drive_not_in_pool", w.Code, w.Body)
	}
}
