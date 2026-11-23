// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/authz"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/pkg/client"
)

// heldFake is the member drive fake store with the retry sweep's list, and a count of the sweep's reads and takeovers.
type heldFake struct {
	*memberDriveStore
	mu        sync.Mutex
	lists     int
	takeovers int
}

// ListHeldMemberDriveDeletes answers as the statement does: owned rows on a listed backend, not a Mac disk, marked
// more than five minutes ago, oldest first.
func (h *heldFake) ListHeldMemberDriveDeletes(_ context.Context, backends []string, limit int) ([]types.UserDrive, error) {
	h.mu.Lock()
	h.lists++
	h.mu.Unlock()
	var out []types.UserDrive
	for _, d := range h.drives {
		if d.OwnerSubject != "" && slices.Contains(backends, string(d.Backend)) && d.Backend != types.DriveBackendMacDisk &&
			d.DeleteStartedAt != nil && h.now.Sub(*d.DeleteStartedAt) > 5*time.Minute {
			out = append(out, d)
		}
	}
	slices.SortFunc(out, func(a, b types.UserDrive) int { return a.DeleteStartedAt.Compare(*b.DeleteStartedAt) })
	return out[:min(len(out), limit)], nil
}

func (h *heldFake) BeginMemberDriveTakeover(ctx context.Context, id uuid.UUID) (types.UserDrive, time.Time, error) {
	h.mu.Lock()
	h.takeovers++
	h.mu.Unlock()
	return h.memberDriveStore.BeginMemberDriveTakeover(ctx, id)
}

// heldDrive is alice's drive on backend whose delete was marked age ago with issues calls counted.
func heldDrive(st *memberDriveStore, name string, backend types.DriveBackend, age time.Duration, issues int) types.UserDrive {
	d := seedMemberDrive(st, aliceSub, name)
	at := st.now.Add(-age)
	d.Backend, d.DeleteStartedAt, d.DeleteIssues = backend, &at, issues
	st.drives[d.ID] = d
	return d
}

func heldServer(rn runner.Runner, target string) (*Server, *heldFake, *recRecorder) {
	st := newMemberDriveStore()
	hf := &heldFake{memberDriveStore: st}
	srv, rec := memberDriveServer(st, rn, target)
	srv.cfg.Store = hf
	return srv, hf, rec
}

func reclaiming(outcome runner.DriveReclaimOutcome, err error) *memberDriveRunner {
	return &memberDriveRunner{reclaimRunner: reclaimRunner{Runner: &fakeRunner{}, outcome: outcome, err: err}}
}

// A sweep failure never clears the mark: an unknown outcome, a time-out, an inspect error and an untouched refusal each
// leave the row deleting at the count the issue returned, and audit the attempt as the sweep's.
func TestHeldDriveRetry_FailuresKeepTheMark(t *testing.T) {
	cases := []struct {
		name        string
		outcome     runner.DriveReclaimOutcome
		err         error
		issues      int
		wantIssues  int
		wantOutcome string
	}{
		{"unknown outcome", "weird", nil, 1, 3, driveReclaimOutcomeFailed},
		{"time-out", "", context.DeadlineExceeded, 1, 3, driveReclaimOutcomeFailed},
		{"inspect error", "", errors.New("inspect volume: daemon not answering"), 0, 1, driveReclaimOutcomeFailed},
		// Over an even count the owner's own delete would clear this claim; the sweep keeps it.
		{"untouched refusal", "", runner.ErrDriveUntouched, 0, 1, driveReclaimOutcomeRefused},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rn := reclaiming(tc.outcome, tc.err)
			srv, hf, rec := heldServer(rn, "docker")
			d := heldDrive(hf.memberDriveStore, "Notes", types.DriveBackendDockerVolume, 6*time.Minute, tc.issues)
			if err := srv.sweepHeldMemberDriveDeletes(context.Background()); err != nil {
				t.Fatalf("sweep: %v", err)
			}
			got, kept := hf.drives[d.ID]
			if !kept || got.DeleteStartedAt == nil || got.DeleteIssues != tc.wantIssues {
				t.Errorf("row kept = %v, marked = %v, issued count = %d; want kept, marked, %d", kept, got.DeleteStartedAt != nil, got.DeleteIssues, tc.wantIssues)
			}
			if len(rn.calls) != 1 {
				t.Errorf("substrate calls = %d, want 1", len(rn.calls))
			}
			data := driveAuditData(t, rec, "drive.member.delete")
			if data["by"] != memberDriveByRetrySweep || data["outcome"] != tc.wantOutcome {
				t.Errorf("audit by = %v, outcome = %v; want %s, %s", data["by"], data["outcome"], memberDriveByRetrySweep, tc.wantOutcome)
			}
		})
	}
}

// The sweep finishes the row on deleted or already_absent only, and audits the attempt as the sweep's. A Kubernetes claim
// that was still terminating answers already_absent once it is gone.
func TestHeldDriveRetry_FinishesOnAffirmativeOutcome(t *testing.T) {
	for _, tc := range []struct {
		name    string
		backend types.DriveBackend
		target  string
		outcome runner.DriveReclaimOutcome
	}{
		{"deleted", types.DriveBackendDockerVolume, "docker", runner.DriveReclaimDeleted},
		{"already_absent on a claim that was terminating", types.DriveBackendK8sPVC, "k8s", runner.DriveReclaimAlreadyAbsent},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rn := reclaiming(tc.outcome, nil)
			srv, hf, rec := heldServer(rn, tc.target)
			d := heldDrive(hf.memberDriveStore, "Notes", tc.backend, 6*time.Minute, 1)
			if err := srv.sweepHeldMemberDriveDeletes(context.Background()); err != nil {
				t.Fatalf("sweep: %v", err)
			}
			if _, kept := hf.drives[d.ID]; kept {
				t.Error("the row is still there after an affirmative outcome")
			}
			if len(rn.calls) != 1 {
				t.Errorf("substrate calls = %d, want 1", len(rn.calls))
			}
			data := driveAuditData(t, rec, "drive.member.delete")
			if data["by"] != memberDriveByRetrySweep || data["outcome"] != string(tc.outcome) {
				t.Errorf("audit by = %v, outcome = %v; want %s, %s", data["by"], data["outcome"], memberDriveByRetrySweep, tc.outcome)
			}
		})
	}
}

// Only the leader sweeps: a follower reads nothing, takes nothing over and calls nothing.
func TestHeldDriveRetry_FollowerSkips(t *testing.T) {
	rn := reclaiming(runner.DriveReclaimDeleted, nil)
	srv, hf, _ := heldServer(rn, "docker")
	srv.cfg.SweeperLease = staticLease{leader: false}
	d := heldDrive(hf.memberDriveStore, "Notes", types.DriveBackendDockerVolume, 6*time.Minute, 1)
	if err := srv.sweepHeldMemberDriveDeletes(context.Background()); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if hf.lists != 0 || hf.takeovers != 0 || len(rn.calls) != 0 {
		t.Errorf("follower: lists = %d, takeovers = %d, calls = %d; want none", hf.lists, hf.takeovers, len(rn.calls))
	}
	if got := hf.drives[d.ID]; got.DeleteIssues != 1 || !got.DeleteStartedAt.Equal(hf.now.Add(-6*time.Minute)) {
		t.Errorf("follower wrote the row: %+v", got)
	}
	srv.cfg.SweeperLease = staticLease{leader: true}
	if err := srv.sweepHeldMemberDriveDeletes(context.Background()); err != nil || len(rn.calls) != 1 {
		t.Errorf("leader: err = %v, calls = %d; want one call", err, len(rn.calls))
	}
}

// A replica that cannot delete takes nothing over and lists nothing: no reclaimer, a failing lifecycle check, an
// empty runner target.
func TestHeldDriveRetry_NotDeletableHereTakesNothing(t *testing.T) {
	cases := []struct {
		name   string
		rn     runner.Runner
		target string
	}{
		{"no reclaimer", &noReclaimRunner{&fakeRunner{}}, "docker"},
		{"lifecycle check fails", &memberDriveRunner{reclaimRunner: reclaimRunner{Runner: &fakeRunner{}}, lifecycleErr: errors.New("daemon down")}, "docker"},
		{"empty runner target", reclaiming(runner.DriveReclaimDeleted, nil), ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, hf, _ := heldServer(tc.rn, tc.target)
			d := heldDrive(hf.memberDriveStore, "Notes", types.DriveBackendDockerVolume, 6*time.Minute, 1)
			if err := srv.sweepHeldMemberDriveDeletes(context.Background()); err != nil {
				t.Fatalf("sweep: %v", err)
			}
			if hf.lists != 0 || hf.takeovers != 0 {
				t.Errorf("lists = %d, takeovers = %d; want 0 and 0", hf.lists, hf.takeovers)
			}
			if got := hf.drives[d.ID]; got.DeleteIssues != 1 {
				t.Errorf("the row moved: %+v", got)
			}
		})
	}
}

// The backend set a server of each runner target deletes. A Mac disk and an empty target give none.
func TestHeldDriveBackends(t *testing.T) {
	for target, want := range map[string][]string{
		"docker": {"docker_volume", "host_path"},
		"k8s":    {"k8s_pvc", "k8s_pvc_static"},
		"":       nil,
		"other":  nil,
	} {
		if got := heldDriveBackends(target); !slices.Equal(got, want) {
			t.Errorf("heldDriveBackends(%q) = %v, want %v", target, got, want)
		}
	}
}

// A deleting drive is listed to its owner, marked deleting and owned, to no one else, and a run naming it is refused as
// for a drive that is not there. The drive an owner has not marked stays plain.
func TestMeDrives_DeletingRowOwnerOnlyNotAttachable(t *testing.T) {
	st := newMemberDriveStore()
	srv, _ := memberDriveServer(st, catalogueRunner(), "docker")
	deleting := heldDrive(st, "Stuck", types.DriveBackendDockerVolume, 10*time.Minute, 1)
	plain := seedMemberDrive(st, aliceSub, "Plain")
	alice, bob := personCtx(aliceSub), personCtx(bobSub)

	reasons := map[string]client.MeDrive{}
	for _, d := range meDrivesOf(t, srv, alice).Drives {
		reasons[d.ID] = d
	}
	if got, ok := reasons[deleting.ID.String()]; !ok || got.UnavailableReason != client.MeDriveUnavailableDeleting || !got.Owned {
		t.Errorf("the owner's row for the deleting drive = %+v (listed %v); want owned, unavailable_reason %q", got, ok, client.MeDriveUnavailableDeleting)
	}
	if got, ok := reasons[plain.ID.String()]; !ok || got.UnavailableReason != "" {
		t.Errorf("the owner's row for the plain drive = %+v (listed %v); want listed with no reason", got, ok)
	}
	if got := meDrivesOf(t, srv, bob).Drives; len(got) != 0 {
		t.Errorf("another person's catalogue = %+v, want empty", got)
	}
	_, f := authorizeDrives(srv, alice, drivesRequest(deleting.ID.String()))
	if f == nil || f.body.Reason != string(authz.ReasonDriveNotFound) {
		t.Errorf("attaching the deleting drive = %+v, want drive_not_found", f)
	}
}
