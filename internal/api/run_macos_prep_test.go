// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// macos_preparing is true only for a RUNNING interactive macOS VM run, not kept, whose prep has not recorded
// done; a run still STARTING is not yet one, whatever it says.
func TestMacOSPrepRunning(t *testing.T) {
	now := time.Now()
	prep := types.AgentRun{State: types.RunRunning, Interactive: true, MachineKind: types.MachineMacOSVM, StatusDetail: statusDetailMacPrepRunning}
	for _, tc := range []struct {
		what string
		edit func(*types.AgentRun)
		want bool
	}{
		{"prep running", func(*types.AgentRun) {}, true},
		{"prep not yet marked", func(r *types.AgentRun) { r.StatusDetail = "" }, true},
		{"prep done", func(r *types.AgentRun) { r.StatusDetail = statusDetailMacPrepDone }, false},
		{"starting", func(r *types.AgentRun) { r.State = types.RunStarting }, false},
		{"ended", func(r *types.AgentRun) { r.State = types.RunFailed }, false},
		{"kept", func(r *types.AgentRun) { r.LostAt = &now }, false},
		{"background", func(r *types.AgentRun) { r.Interactive = false }, false},
		{"a Linux VM", func(r *types.AgentRun) { r.MachineKind = types.MachineLinuxVM }, false},
	} {
		run := prep
		tc.edit(&run)
		if got := macOSPrepRunning(run); got != tc.want {
			t.Errorf("%s: macOSPrepRunning = %v, want %v", tc.what, got, tc.want)
		}
	}
}

// GET /runs/{id} says a macOS VM run is still being set up on its own field, read off the stored row the
// projection blanks, even while mac_window answers an earlier row (here the site switch, off).
func TestGetRun_MacOSPreparingBesideTheWindow(t *testing.T) {
	for _, tc := range []struct {
		name   string
		kind   types.MachineKind
		detail string
		want   bool
	}{
		{"prep running", types.MachineMacOSVM, statusDetailMacPrepRunning, true},
		{"prep done", types.MachineMacOSVM, statusDetailMacPrepDone, false},
		{"a Linux VM", types.MachineLinuxVM, "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := runProviderStore{runTypeStore: runTypeStore{newUIMemStore()}}
			cfg := baseTestConfig(newHarness(t), st)
			cfg.OIDC = newAccessAuth(t, nil, "", nil, nil)
			srv := New(cfg)
			owner := accessSessionOfType(t, "sub-pm", "pat@corp.example", oidc.RoleUser, "portfolio-manager", []string{})
			run := types.AgentRun{ID: uuid.New(), CreatedBy: "sub-pm", State: types.RunRunning, Interactive: true, MachineKind: tc.kind, StatusDetail: tc.detail}
			st.putRun(run)
			w := doSSO(t, srv, http.MethodGet, "/api/v1/runs/"+run.ID.String(), owner, "")
			if w.Code != http.StatusOK {
				t.Fatalf("GET run = %d; body=%s", w.Code, w.Body.String())
			}
			var body struct {
				MacOSPreparing *bool          `json:"macos_preparing"`
				MacWindow      *macWindowView `json:"mac_window"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			if got := body.MacOSPreparing != nil && *body.MacOSPreparing; got != tc.want || (!tc.want && body.MacOSPreparing != nil) {
				t.Fatalf("macos_preparing = %v, want %v (absent when false); body=%s", body.MacOSPreparing, tc.want, w.Body.String())
			}
			if tc.want && (body.MacWindow == nil || body.MacWindow.State != macWindowStateSiteOff) {
				t.Errorf("mac_window = %+v, want site_off beside macos_preparing", body.MacWindow)
			}
		})
	}
}
