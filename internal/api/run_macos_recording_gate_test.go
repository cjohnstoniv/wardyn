// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/placement"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// Under a recording requirement a macOS VM run asks no runner to record (the organisation records it), so
// a Mac whose runner reports Records: false still takes it; a container or a Linux VM in the same setup is
// still refused runner_recording_off. Placement and the queue's promotion share the one predicate.
func TestPlacementCapability_MacOSVMAsksNoRunnerToRecord(t *testing.T) {
	notRecording := poolTarget{facts: runner.TargetFacts{Records: false}}
	for _, tc := range []struct {
		kind     types.MachineKind
		required bool
		want     placement.Reason
	}{
		{types.MachineMacOSVM, true, ""},
		{types.MachineContainer, true, placement.ReasonRunnerRecordingOff},
		{types.MachineLinuxVM, true, placement.ReasonRunnerRecordingOff},
		{types.MachineMacOSVM, false, ""},
		{types.MachineContainer, false, ""},
	} {
		demand := placementDemand{requireRecording: runnerRecordingDemand(tc.required, tc.kind)}
		got := demand.refusal(notRecording)
		if (tc.want == placement.ReasonRunnerRecordingOff) != (got == placement.ReasonRunnerRecordingOff) {
			t.Errorf("%s, required=%v: refusal %q, want %q", tc.kind, tc.required, got, tc.want)
		}
	}
}

// A sandbox upload for a macOS VM run is refused 409 recording_off before every exit that would let it
// through: with no mark (a recorded macOS run), marked runner_declined, and on a runner that records
// whatever it is asked (RecordsUnasked). A container on that runner still uploads.
func TestRecordingUpload_MacOSVMIsAlwaysRefused(t *testing.T) {
	id := uuid.New()
	s := &Server{cfg: Config{RecordingStore: fsRecordingStore(t),
		Runner: &poolRunner{fakeRunner: &fakeRunner{}, runnerFacts: map[string]runner.TargetFacts{id.String(): {Records: true, RecordsUnasked: true}}}}}
	for _, tc := range []struct {
		name  string
		run   types.AgentRun
		want  bool
		words string
	}{
		{"a macOS VM run with no mark", types.AgentRun{MachineKind: types.MachineMacOSVM, RecordingLevel: types.RecordingTerminal}, true, "a macOS VM run is recorded by the organisation, not uploaded"},
		{"a macOS VM run marked runner_declined", types.AgentRun{MachineKind: types.MachineMacOSVM, NotRecorded: types.NotRecordedRunnerDeclined}, true, "a macOS VM run is recorded by the organisation, not uploaded"},
		{"a macOS VM run on a runner that records unasked", types.AgentRun{MachineKind: types.MachineMacOSVM, NotRecorded: types.NotRecordedRunnerDeclined, RunnerID: &id}, true, "a macOS VM run is recorded by the organisation, not uploaded"},
		{"a container on that runner", types.AgentRun{MachineKind: types.MachineContainer, NotRecorded: types.NotRecordedRunnerDeclined, RunnerID: &id}, false, ""},
		{"a recorded container", types.AgentRun{MachineKind: types.MachineContainer}, false, ""},
	} {
		w := httptest.NewRecorder()
		got := s.refuseDeclinedUpload(w, tc.run)
		if got != tc.want {
			t.Errorf("%s: refused = %v, want %v (%d %s)", tc.name, got, tc.want, w.Code, w.Body.String())
			continue
		}
		if tc.want && (w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), `"`+reasonRecordingOff+`"`) || !strings.Contains(w.Body.String(), tc.words)) {
			t.Errorf("%s: %d %s, want 409 %s saying %q", tc.name, w.Code, w.Body.String(), reasonRecordingOff, tc.words)
		}
	}
}

// A window at start is refused at create by the level dispatch will resolve, not only the request
// policy's: under the organisation's locked Screen and actions level, and under the caller's walls whose
// floor is Screen and actions. The same create under the organisation's Terminal level passes.
func TestMacWindowCreate_RefusedByTheResolvedLevel(t *testing.T) {
	on := true
	step := func(t *testing.T, mutate func(r *macWindowRig, f *runFold)) (*httptest.ResponseRecorder, bool) {
		t.Helper()
		r := newMacWindowRig(t)
		req := httptest.NewRequest(http.MethodPost, "/runs", nil)
		req = req.WithContext(withHumanIdentity(req.Context(), r.h.owner, r.h.owner+"@example.com", oidc.RoleUser, types.UserTypeStandard, nil, false))
		rec := httptest.NewRecorder()
		f := &runFold{mode: foldCreate, w: rec, r: req, ceiling: governanceCeiling{},
			req: &createRunRequest{Agent: "claude-code", MachineKind: "macos_vm", DesktopWindow: "at_start"}}
		mutate(r, f)
		return rec, r.h.srv.stepMacWindow(f, nil)
	}
	refused := func(t *testing.T, rec *httptest.ResponseRecorder, ok bool) {
		t.Helper()
		if ok || rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), reasonMacWindowUnrecorded) || !strings.Contains(rec.Body.String(), macWindowUnrecordedMsg) {
			t.Fatalf("ok=%v %d %s, want 403 %s", ok, rec.Code, rec.Body.String(), reasonMacWindowUnrecorded)
		}
	}
	t.Run("the organisation's locked Screen and actions level", func(t *testing.T) {
		rec, ok := step(t, func(r *macWindowRig, _ *runFold) {
			r.st.sc = types.SiteConfig{MacOSVM: &types.SiteMacOSVM{AllowAttendedWindow: &on},
				Recording: &types.SiteRecording{Level: types.RecordingScreenActions, Locked: true}}
		})
		refused(t, rec, ok)
	})
	t.Run("walls whose floor is Screen and actions", func(t *testing.T) {
		rec, ok := step(t, func(r *macWindowRig, f *runFold) {
			lvl := types.RecordingScreenActions
			doc, err := json.Marshal(types.PersonalWalls{Spec: types.PersonalWallsSpec{Recording: &lvl}})
			if err != nil {
				t.Fatal(err)
			}
			_, principal := actorFromRequest(f.r)
			r.st.prefs[principal+"\x00"+types.PersonalWallsPrefKey] = doc
		})
		refused(t, rec, ok)
	})
	t.Run("the organisation's Terminal level passes", func(t *testing.T) {
		rec, ok := step(t, func(r *macWindowRig, _ *runFold) {
			r.st.sc = types.SiteConfig{MacOSVM: &types.SiteMacOSVM{AllowAttendedWindow: &on},
				Recording: &types.SiteRecording{Level: types.RecordingTerminal, Locked: true}}
		})
		if !ok || strings.Contains(rec.Body.String(), reasonMacWindowUnrecorded) {
			t.Fatalf("ok=%v %d %s, want the create to pass", ok, rec.Code, rec.Body.String())
		}
	})
}
