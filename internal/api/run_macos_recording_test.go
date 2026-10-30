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

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/runnerpool"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

const macOSNoRecording = ", and a macOS VM run isn't recorded in this release. Choose a container or a Linux VM for this run."

// The sentences a macOS VM run is refused with, and the notice it starts under, are the canon's (MAC-U01
// to U04), naming the cause that asks for the recording.
func TestMacOSVMRecordingSentences(t *testing.T) {
	if macOSVMUnrecordedNotice != "A macOS VM run isn't recorded in this release, so this run starts unrecorded. The audit log still shows what happened." {
		t.Errorf("notice = %q", macOSVMUnrecordedNotice)
	}
	locked := func(l types.RecordingLevel) *types.SiteRecording { return &types.SiteRecording{Level: l, Locked: true} }
	for _, tc := range []struct {
		name   string
		choice types.RecordingChoice
		want   string
	}{
		{"required", types.RecordingChoice{Required: true}, "Recording is required" + macOSNoRecording},
		{"required beats a locked level", types.RecordingChoice{Required: true, Org: locked(types.RecordingTerminal)},
			"Recording is required" + macOSNoRecording},
		{"locked, the default level", types.RecordingChoice{Org: locked("")},
			"Your organisation's recording level, Terminal, is locked" + macOSNoRecording},
		{"locked, screen", types.RecordingChoice{Org: locked(types.RecordingScreenActions)},
			"Your organisation's recording level, Screen and actions, is locked" + macOSNoRecording},
		{"ceiling, a profile's", types.RecordingChoice{Ceiling: types.RecordingTerminal, CeilingBy: "Platform"},
			"Your governance profile Platform sets recording to Terminal" + macOSNoRecording},
		{"ceiling, screen", types.RecordingChoice{Ceiling: types.RecordingScreenActions, CeilingBy: "Platform"},
			"Your governance profile Platform sets recording to Screen and actions" + macOSNoRecording},
		{"ceiling, the default policy's", types.RecordingChoice{Ceiling: types.RecordingTerminal},
			"Your organisation's default policy sets recording to Terminal" + macOSNoRecording},
	} {
		if got := macOSVMRecordingRefusal(tc.choice); got != tc.want {
			t.Errorf("%s: %q, want %q", tc.name, got, tc.want)
		}
	}
}

// macRecordingStore answers the reads the recording gate makes, over the runners a test installed.
type macRecordingStore struct {
	ownedRunners
	store.RunnerPoolStore
	org     types.RunnerPoolDefaults
	site    types.SiteConfig
	siteErr error
}

func (m *macRecordingStore) GetRunnerPoolOrgDefaults(context.Context) (types.RunnerPoolDefaults, error) {
	return m.org, nil
}

func (m *macRecordingStore) GetSiteConfig(context.Context) (types.SiteConfig, error) {
	return m.site, m.siteErr
}

// A macOS VM run goes unrecorded with a notice unless a requirement or a floor asks for a recording,
// then it is refused with the sentence naming the cause. Both sides: nothing that is only a default (the
// organisation's unlocked level, the run's own policy) refuses it, and a container is never touched.
func TestStepPlacementCapability_MacOSVMRecording(t *testing.T) {
	locked := &types.SiteRecording{Level: types.RecordingTerminal, Locked: true}
	profile := governanceCeiling{Unit: &ResolvedUnit{Name: "Platform"}}
	withLevel := func(l types.RecordingLevel) governanceCeiling {
		c := profile
		c.Spec.Recording = l
		return c
	}
	operator := withLevel(types.RecordingTerminal)
	operator.Operator = true
	pooled := func(id uuid.UUID) runPlacement {
		return runPlacement{pooled: true, placement: types.PlacementLocal, row: types.RunnerPool{Name: "Macs", Recording: types.RecordingRequired},
			eligible: []poolTarget{{runnerID: &id}}}
	}
	for _, tc := range []struct {
		name    string
		org     types.RunnerPoolDefaults
		site    types.SiteConfig
		ceiling governanceCeiling
		place   func(uuid.UUID) runPlacement
		noStore bool
		kind    string
		want    string // the refusal's sentence; "" means the run is placed with the notice
	}{
		{name: "nothing asks for a recording", ceiling: profile},
		{name: "the organisation's unlocked default", site: types.SiteConfig{Recording: &types.SiteRecording{Level: types.RecordingScreenActions}}, ceiling: profile},
		{name: "a locked level of off", site: types.SiteConfig{Recording: &types.SiteRecording{Level: types.RecordingOff, Locked: true}}, ceiling: profile},
		{name: "a ceiling of off", ceiling: withLevel(types.RecordingOff)},
		{name: "an operator's ceiling binds nobody", ceiling: operator},
		{name: "the organisation requires recording", org: types.RunnerPoolDefaults{Recording: types.RecordingRequired}, ceiling: profile,
			want: "Recording is required" + macOSNoRecording},
		{name: "the pool requires recording", ceiling: profile, place: pooled, want: "Recording is required" + macOSNoRecording},
		{name: "the organisation locks a level", site: types.SiteConfig{Recording: locked}, ceiling: profile,
			want: "Your organisation's recording level, Terminal, is locked" + macOSNoRecording},
		{name: "the ceiling sets a level", ceiling: withLevel(types.RecordingTerminal),
			want: "Your governance profile Platform sets recording to Terminal" + macOSNoRecording},
		// Nothing is recorded when the organisation keeps no recordings, so nothing is refused or said.
		{name: "no recording store, required", org: types.RunnerPoolDefaults{Recording: types.RecordingRequired}, ceiling: profile, noStore: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := capsServer(&fakeRunner{})
			id := macRunner(t, s, readyMac(), roomyRunner)
			s.cfg.Store = &macRecordingStore{ownedRunners: s.cfg.Store.(ownedRunners), org: tc.org, site: tc.site}
			if !tc.noStore {
				s.cfg.RecordingStore = fsRecordingStore(t)
			}
			req := macReq(id)
			w := httptest.NewRecorder()
			f := &runFold{w: w, r: httptest.NewRequest(http.MethodPost, "/api/v1/runs", nil).WithContext(capsCtx), req: &req, ceiling: tc.ceiling}
			if tc.place != nil {
				f.place = tc.place(id)
			}
			ok := s.stepPlacementCapability(f, nil)
			if tc.want != "" {
				if ok || w.Code != http.StatusUnprocessableEntity || !strings.Contains(w.Body.String(), `"placement_capability"`) ||
					!strings.Contains(w.Body.String(), `"machine_kind"`) || !strings.Contains(w.Body.String(), tc.want) {
					t.Fatalf("ok = %v, %d %s; want 422 placement_capability on machine_kind saying %q", ok, w.Code, w.Body.String(), tc.want)
				}
				return
			}
			if !ok {
				t.Fatalf("refused: %d %s", w.Code, w.Body.String())
			}
			// The notice rides the fold's warnings, which Review and the 201 both carry.
			var notices int
			for _, warn := range f.policyWarns {
				if warn == macOSVMUnrecordedNotice {
					notices++
				}
			}
			if want := map[bool]int{false: 1, true: 0}[tc.noStore]; notices != want {
				t.Fatalf("warnings %q carry the notice %d times, want %d", f.policyWarns, notices, want)
			}
		})
	}

	t.Run("a container on the same Mac is not touched", func(t *testing.T) {
		s := capsServer(&fakeRunner{})
		id := macRunner(t, s, readyMac(), roomyRunner)
		s.cfg.Store = &macRecordingStore{ownedRunners: s.cfg.Store.(ownedRunners), org: types.RunnerPoolDefaults{Recording: types.RecordingRequired},
			site: types.SiteConfig{Recording: locked}}
		s.cfg.RecordingStore = fsRecordingStore(t)
		req := createRunRequest{Placement: types.PlacementLocal, RunnerID: id.String()}
		w := httptest.NewRecorder()
		f := &runFold{w: w, r: httptest.NewRequest(http.MethodPost, "/api/v1/runs", nil).WithContext(capsCtx), req: &req, ceiling: withLevel(types.RecordingTerminal)}
		if !s.stepPlacementCapability(f, nil) || len(f.policyWarns) != 0 {
			t.Fatalf("a container: ok body %s, warnings %q; want placed with none", w.Body.String(), f.policyWarns)
		}
	})

	t.Run("a requirement that cannot be read is a server error", func(t *testing.T) {
		s := capsServer(&fakeRunner{})
		id := macRunner(t, s, readyMac(), roomyRunner)
		s.cfg.Store = &macRecordingStore{ownedRunners: s.cfg.Store.(ownedRunners), siteErr: context.DeadlineExceeded}
		s.cfg.RecordingStore = fsRecordingStore(t)
		req := macReq(id)
		f := &runFold{w: httptest.NewRecorder(), r: httptest.NewRequest(http.MethodPost, "/api/v1/runs", nil).WithContext(capsCtx), req: &req, ceiling: profile}
		if notice, refusal := s.macOSVMRecordingGate(capsCtx, f); notice != "" || refusal == nil || refusal.err == nil {
			t.Fatalf("an unreadable site config = (%q, %+v); want a server error and no notice", notice, refusal)
		}
	})
}

// At dispatch a macOS VM run is asked not to record and marked runner_declined, even on a runner that
// records, so the runner's own backstop never refuses it; a container run on the same runner is asked to
// record as before. This is the side that fails on the old code, which asked every run to record.
func TestApplyRunRecording_MacOSVMIsANonRecordingTarget(t *testing.T) {
	records := uuid.New()
	for _, tc := range []struct {
		name       string
		kind       types.MachineKind
		wantRecord bool
		wantMark   types.RunNotRecorded
	}{
		{"a macOS VM on a runner that records", types.MachineMacOSVM, false, types.NotRecordedRunnerDeclined},
		{"a container on the same runner", types.MachineContainer, true, ""},
		{"a Linux VM on the same runner", types.MachineLinuxVM, true, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := &Server{cfg: Config{RecordingStore: fsRecordingStore(t), Store: &recordingReadFake{},
				Runner: &poolRunner{fakeRunner: &fakeRunner{}, runnerFacts: map[string]runner.TargetFacts{records.String(): {Records: true}}}}}
			run := types.AgentRun{ID: uuid.New(), RunnerID: &records, MachineKind: tc.kind}
			var spec runner.SandboxSpec
			if !s.applyRunRecording(context.Background(), &run, &spec, types.RecordingChoice{}) {
				t.Fatal("dispatch held a run that nothing requires recording for")
			}
			if spec.Record == nil || *spec.Record != tc.wantRecord || run.NotRecorded != tc.wantMark {
				t.Fatalf("record=%v not_recorded=%q, want %v %q", spec.Record, run.NotRecorded, tc.wantRecord, tc.wantMark)
			}
		})
	}
}

// A macOS VM run is held at dispatch under the same causes create refuses it for, with the same
// sentence, and a requirement that cannot be read holds it too. Nothing that is only a default holds it.
func TestMacOSVMRecordingHold(t *testing.T) {
	boom := errors.New("connection reset")
	for _, tc := range []struct {
		name   string
		fake   recordingReadFake
		choice types.RecordingChoice
		want   string // "" means not held
	}{
		{"nothing asks", recordingReadFake{}, types.RecordingChoice{}, ""},
		{"an unlocked default", recordingReadFake{}, types.RecordingChoice{Org: &types.SiteRecording{Level: types.RecordingScreenActions}}, ""},
		{"organisation requires", recordingReadFake{org: types.RunnerPoolDefaults{Recording: types.RecordingRequired}}, types.RecordingChoice{},
			"Recording is required" + macOSNoRecording},
		{"locked level", recordingReadFake{}, types.RecordingChoice{Org: &types.SiteRecording{Locked: true}},
			"Your organisation's recording level, Terminal, is locked" + macOSNoRecording},
		{"ceiling level", recordingReadFake{}, types.RecordingChoice{Ceiling: types.RecordingTerminal, CeilingBy: "Platform"},
			"Your governance profile Platform sets recording to Terminal" + macOSNoRecording},
		{"requirement unreadable", recordingReadFake{orgErr: boom}, types.RecordingChoice{}, runnerpool.RecordingSettingUnreadableMsg()},
	} {
		s := &Server{cfg: Config{Store: &tc.fake}}
		msg, held := s.macOSVMRecordingHold(context.Background(), &types.AgentRun{ID: uuid.New()}, tc.choice)
		if held != (tc.want != "") || msg != tc.want {
			t.Errorf("%s: (%q, %v), want (%q, %v)", tc.name, msg, held, tc.want, tc.want != "")
		}
	}
}
