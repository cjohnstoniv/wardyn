// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// winvmPersonRequest is a create request signed in as person.
func winvmPersonRequest(ctx context.Context, person string) *http.Request {
	return httptest.NewRequest(http.MethodPost, "/api/v1/runs", nil).WithContext(withOIDCRole(withOIDCHuman(ctx, person), oidc.RoleUser))
}

// winvmRow is the row the create's fold inserts for person's launch of the registered image: newRunRow's, with
// the fold's windows ref and launch.
func winvmRow(t *testing.T, srv *Server, person string, state types.RunState) types.AgentRun {
	t.Helper()
	r := winvmPersonRequest(t.Context(), person)
	req := createRunRequest{MachineKind: string(types.MachineWindowsVM), WindowsImage: "Server", Interactive: true}
	launch, rf := srv.windowsImageDecision(r, req, types.RunPolicySpec{})
	if rf != nil {
		t.Fatalf("%s's launch refused: %d %s %s", person, rf.status, rf.body.Reason, rf.body.Error)
	}
	f := runFold{windowsFold: windowsFold{windowsRef: launch.Image.Ref, windowsLaunch: &launch}}
	run := newRunRow(r.Context(), runRowInput{runID: uuid.New(), now: time.Now().UTC(), createdBy: person, req: req, f: f,
		enforced: types.CC1, runnerTarget: "docker"})
	run.State = state
	return run
}

// A create and a queue insert each take a seat: the row newRunRow builds carries the registered ref, which the
// take trigger reads.
func TestPG_WindowsCreateAndQueueInsertTakeASeat(t *testing.T) {
	pool := throwawayPGPool(t)
	st := store.NewPG(pool)
	ctx := t.Context()
	windowsSeat(t, pool, "ana@example.com")
	srv := New(baseTestConfig(newHarness(t), st))
	created, err := srv.createRun(ctx, winvmRow(t, srv, "ana@example.com", types.RunPending))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if created.Image != winvmRefOf(t, st, "Server") || windowsLeaseCount(t, pool, created.ID) != 1 {
		t.Fatalf("create: image %q, %d seats; want the registered ref and one seat", created.Image, windowsLeaseCount(t, pool, created.ID))
	}
	p, err := st.CreateWindowsLicencePool(ctx, types.WindowsLicencePool{Name: "Design", Seats: 1, CreatedBy: "admin"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddWindowsLicenceHolder(ctx, p.ID, "bo@example.com", "admin"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.RegisterWindowsImage(ctx, types.WindowsImage{Name: "Desk", Ref: "registry.example/windows/desk@sha256:" + strings.Repeat("ef", 32), LicencePoolID: p.ID, CreatedBy: "admin"}); err != nil {
		t.Fatal(err)
	}
	rp, err := st.CreateRunnerPool(ctx, types.RunnerPool{ID: uuid.New(), Name: "Org", Scope: types.RunnerPoolOrganization}, "admin")
	if err != nil {
		t.Fatal(err)
	}
	req := createRunRequest{MachineKind: string(types.MachineWindowsVM), WindowsImage: "Desk", Interactive: true}
	launch, rf := srv.windowsImageDecision(winvmPersonRequest(ctx, "bo@example.com"), req, types.RunPolicySpec{})
	if rf != nil {
		t.Fatalf("bo's launch refused: %s", rf.body.Error)
	}
	row := newRunRow(ctx, runRowInput{runID: uuid.New(), now: time.Now().UTC(), createdBy: "bo@example.com", req: req,
		f: runFold{windowsFold: windowsFold{windowsRef: launch.Image.Ref, windowsLaunch: &launch}}, enforced: types.CC1, runnerTarget: "docker"})
	row.State, row.RunnerPoolID = types.RunQueued, &rp.ID
	now := time.Now().UTC()
	queued, err := srv.createQueuedRun(ctx, row, store.RunQueueEntry{QueuedAt: now, DeadlineAt: now.Add(time.Hour)})
	if err != nil {
		t.Fatalf("queue insert: %v", err)
	}
	if windowsLeaseCount(t, pool, queued.ID) != 1 {
		t.Fatalf("the queued Windows run took no seat")
	}
}

func winvmRefOf(t *testing.T, st store.PG, name string) string {
	t.Helper()
	imgs, err := st.ListWindowsImages(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	for _, img := range imgs {
		if img.Name == name {
			return img.Ref
		}
	}
	t.Fatalf("no image %s", name)
	return ""
}

// A queued Windows run whose switch went off while it waited is refused at its start: the start's fold (a
// promoted start, foldCreate) answers 403 windows_vms_off, and the run's terminal end frees the seat its
// insert took.
func TestPG_QueuedWindowsRunSwitchOffAtPromotion(t *testing.T) {
	pool := throwawayPGPool(t)
	st := store.NewPG(pool)
	ctx := t.Context()
	windowsSeat(t, pool, "ana@example.com")
	srv := New(baseTestConfig(newHarness(t), st))
	rp, err := st.CreateRunnerPool(ctx, types.RunnerPool{ID: uuid.New(), Name: "Org", Scope: types.RunnerPoolOrganization}, "admin")
	if err != nil {
		t.Fatal(err)
	}
	row := winvmRow(t, srv, "ana@example.com", types.RunQueued)
	row.RunnerPoolID = &rp.ID
	now := time.Now().UTC()
	queued, err := srv.createQueuedRun(ctx, row, store.RunQueueEntry{QueuedAt: now, DeadlineAt: now.Add(time.Hour)})
	if err != nil {
		t.Fatalf("queue insert: %v", err)
	}
	if windowsLeaseCount(t, pool, queued.ID) != 1 {
		t.Fatal("the queued run took no seat")
	}
	if _, err := pool.Exec(ctx, `UPDATE site_config SET config = '{"features":{"windows_vm":{"enabled":false}}}'::jsonb`); err != nil {
		t.Fatal(err)
	}
	start := context.WithValue(ctx, promotedStartCtxKey{}, promotedStart{run: queued.ID})
	req := createRunRequest{MachineKind: string(types.MachineWindowsVM), WindowsImage: "Server", Interactive: true}
	rec := httptest.NewRecorder()
	f := &runFold{mode: foldCreate, w: rec, r: winvmPersonRequest(start, "ana@example.com"), req: &req}
	if srv.stepWindowsImage(f, &foldRecorder{}) || rec.Code != http.StatusForbidden || errorReasonOf(rec.Body.String()) != reasonWindowsVMsOff {
		t.Fatalf("the start's fold = %d %s, want 403 %s", rec.Code, rec.Body, reasonWindowsVMsOff)
	}
	if ok, err := srv.casRunState(ctx, queued.ID, types.RunQueued, types.RunFailed); err != nil || !ok {
		t.Fatalf("end the refused start: %v %v", ok, err)
	}
	if n := windowsLeaseCount(t, pool, queued.ID); n != 0 {
		t.Errorf("the refused start left %d seats", n)
	}
}

// Ana's second concurrent launch is 409 windows_licence_in_use naming the run that holds her seat; a queued
// run's own start is not refused by its own seat.
func TestPG_WindowsLaunchSecondConcurrentInUse(t *testing.T) {
	srv, _, st, _, img := windowsRoutesServer(t)
	first := liveWindowsRun(t, st, img)
	req := createRunRequest{MachineKind: string(types.MachineWindowsVM), WindowsImage: "Server", Interactive: true}
	_, rf := srv.windowsImageDecision(winvmPersonRequest(t.Context(), "ana@example.com"), req, types.RunPolicySpec{})
	if rf == nil || rf.status != http.StatusConflict || rf.body.Reason != reasonWindowsLicenceInUse || !strings.Contains(rf.body.Error, first.ID.String()) {
		t.Fatalf("second launch = %+v, want 409 %s naming %s", rf, reasonWindowsLicenceInUse, first.ID)
	}
	own := context.WithValue(t.Context(), promotedStartCtxKey{}, promotedStart{run: first.ID})
	if _, rf := srv.windowsImageDecision(winvmPersonRequest(own, "ana@example.com"), req, types.RunPolicySpec{}); rf != nil {
		t.Errorf("the seat's own run's start refused: %s %s", rf.body.Reason, rf.body.Error)
	}
}

// The launch's run.create row names the registered image, its digest and the pool, from the seat check.
func TestPG_WindowsLaunchAuditNamesImageDigestPool(t *testing.T) {
	srv, _, _, p, img := windowsRoutesServer(t)
	req := createRunRequest{MachineKind: string(types.MachineWindowsVM), WindowsImage: "Server", Interactive: true}
	launch, rf := srv.windowsImageDecision(winvmPersonRequest(t.Context(), "ana@example.com"), req, types.RunPolicySpec{})
	if rf != nil {
		t.Fatalf("launch refused: %s", rf.body.Error)
	}
	data := map[string]any{}
	stampWindowsLicence(data, &launch)
	_, digest, _ := strings.Cut(img.Ref, "@")
	if data["windows_image"] != "Server" || data["windows_image_digest"] != digest || data["windows_licence_pool_id"] != p.ID.String() || data["windows_licence_pool"] != p.Name {
		t.Errorf("run.create data = %v, want Server, %s and %s", data, digest, p.Name)
	}
}

// Killing a Windows run frees its seat; marking it lost does not, since a lost VM may still be running.
func TestPG_WindowsKillFreesSeatLostKeepsIt(t *testing.T) {
	srv, pool, st, _, img := windowsRoutesServer(t)
	run := liveWindowsRun(t, st, img)
	if _, err := pool.Exec(t.Context(), `UPDATE agent_runs SET lost_at = now(), lost_reason = 'gone' WHERE id = $1`, run.ID); err != nil {
		t.Fatal(err)
	}
	if n := windowsLeaseCount(t, pool, run.ID); n != 1 {
		t.Fatalf("a lost run holds %d seats, want 1", n)
	}
	if ok, err := srv.casRunState(t.Context(), run.ID, types.RunRunning, types.RunKilled); err != nil || !ok {
		t.Fatalf("kill: %v %v", ok, err)
	}
	if n := windowsLeaseCount(t, pool, run.ID); n != 0 {
		t.Errorf("a killed run holds %d seats, want 0", n)
	}
}
