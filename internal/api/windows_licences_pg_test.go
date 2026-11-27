// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// windowsRoutesServer is an admin-token server over a throwaway database with Windows VMs on and ana holding
// the one seat of Engineering, through which the image Server launches.
func windowsRoutesServer(t *testing.T) (*Server, *pgxpool.Pool, store.PG, types.WindowsLicencePool, types.WindowsImage) {
	t.Helper()
	pool := throwawayPGPool(t)
	st := store.NewPG(pool)
	p, img := windowsSeat(t, pool, "ana@example.com")
	return New(baseTestConfig(newHarness(t), st)), pool, st, p, img
}

// liveWindowsRun inserts a running Windows VM run of ana's, which takes her seat.
func liveWindowsRun(t *testing.T, st store.PG, img types.WindowsImage) types.AgentRun {
	t.Helper()
	run := newFinalizeRun()
	run.CreatedBy, run.MachineKind, run.Image, run.Interactive, run.Task = "ana@example.com", types.MachineWindowsVM, img.Ref, true, ""
	run, err := st.CreateRun(t.Context(), run)
	if err != nil {
		t.Fatalf("insert the Windows run: %v", err)
	}
	return run
}

// A pool takes no more holders than seats: the third holder of a two-seat pool is 409 windows_licence_pool_full.
func TestPG_WindowsHolderThirdRefusedPoolFull(t *testing.T) {
	srv, _, st, _, _ := windowsRoutesServer(t)
	two, err := st.CreateWindowsLicencePool(t.Context(), types.WindowsLicencePool{Name: "Design", Seats: 2, CreatedBy: "admin"})
	if err != nil {
		t.Fatal(err)
	}
	path := "/api/v1/windows-licence-pools/" + two.ID.String() + "/holders"
	for _, who := range []string{"bo@example.com", "cy@example.com"} {
		if w := do(t, srv, http.MethodPost, path, adminToken, `{"subject":"`+who+`"}`); w.Code != http.StatusCreated {
			t.Fatalf("add %s = %d %s", who, w.Code, w.Body)
		}
	}
	w := do(t, srv, http.MethodPost, path, adminToken, `{"subject":"di@example.com"}`)
	if w.Code != http.StatusConflict || errorReasonOf(w.Body.String()) != reasonWindowsLicencePoolFull || !strings.Contains(w.Body.String(), "Design") {
		t.Fatalf("third holder = %d %s, want 409 %s naming Design", w.Code, w.Body, reasonWindowsLicencePoolFull)
	}
	if hs, _ := st.ListWindowsLicenceHolders(t.Context(), two.ID); len(hs) != 2 {
		t.Errorf("holders = %d, want 2", len(hs))
	}
}

// Removing a holder whose seat a live run holds is 409 windows_licence_seat_live naming the run; once the run
// ends, the removal goes through.
func TestPG_WindowsHolderRemoveRefusedWhileSeatLive(t *testing.T) {
	srv, _, st, p, img := windowsRoutesServer(t)
	run := liveWindowsRun(t, st, img)
	path := "/api/v1/windows-licence-pools/" + p.ID.String() + "/holders/ana@example.com"
	w := do(t, srv, http.MethodDelete, path, adminToken, "")
	if w.Code != http.StatusConflict || errorReasonOf(w.Body.String()) != reasonWindowsLicenceSeatLive || !strings.Contains(w.Body.String(), run.ID.String()) {
		t.Fatalf("remove = %d %s, want 409 %s naming %s", w.Code, w.Body, reasonWindowsLicenceSeatLive, run.ID)
	}
	if ok, err := srv.casRunState(t.Context(), run.ID, types.RunRunning, types.RunKilled); err != nil || !ok {
		t.Fatalf("kill: %v %v", ok, err)
	}
	if w := do(t, srv, http.MethodDelete, path, adminToken, ""); w.Code != http.StatusOK {
		t.Fatalf("remove after the end = %d %s", w.Code, w.Body)
	}
}

// While a live run holds a seat through an image, the image's retire, its pool's delete and its move to another
// pool are each 409 windows_licence_seat_live naming the run.
func TestPG_WindowsSeatLiveRefusesRetireDeleteMove(t *testing.T) {
	srv, _, st, p, img := windowsRoutesServer(t)
	run := liveWindowsRun(t, st, img)
	other, err := st.CreateWindowsLicencePool(t.Context(), types.WindowsLicencePool{Name: "Design", Seats: 1, CreatedBy: "admin"})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ name, method, path, body string }{
		{"retire", http.MethodDelete, "/api/v1/windows-images/" + img.ID.String(), ""},
		{"pool delete", http.MethodDelete, "/api/v1/windows-licence-pools/" + p.ID.String(), ""},
		{"move", http.MethodPut, "/api/v1/windows-images/" + img.ID.String(), fmt.Sprintf(`{"revision":%d,"licence_pool_id":%q}`, img.Revision, other.ID)},
	} {
		w := do(t, srv, tc.method, tc.path, adminToken, tc.body)
		if w.Code != http.StatusConflict || errorReasonOf(w.Body.String()) != reasonWindowsLicenceSeatLive || !strings.Contains(w.Body.String(), run.ID.String()) {
			t.Errorf("%s = %d %s, want 409 %s naming %s", tc.name, w.Code, w.Body, reasonWindowsLicenceSeatLive, run.ID)
		}
	}
	if got, err := st.GetWindowsImage(t.Context(), img.ID); err != nil || got.State != types.WindowsImageActive || got.LicencePoolID != p.ID {
		t.Errorf("image after the refusals = %+v, %v; want it active in its pool", got, err)
	}
}
