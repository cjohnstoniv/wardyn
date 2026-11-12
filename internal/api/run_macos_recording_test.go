// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/recording"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/secretmask"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// The notices a macOS VM run is shown at Review and in the 201's warnings, and the sentence a task run under
// a requirement fails at start with when its agent output can't be recorded, are the board's words.
func TestMacOSVMRecordingNotices(t *testing.T) {
	for got, want := range map[string]string{
		macOSVMScreenNotice: "This run's desktop is recorded while someone watches it from the run page. The screen recording isn't masked: " +
			"anything the desktop shows, a secret included, is kept. A window on your Mac isn't recorded, so this run can't open one. " +
			"Terminal sessions are recorded and masked.",
		macOSVMTerminalNotice:       "At the Terminal level this run's desktop isn't recorded. Terminal sessions and the agent's output are recorded and masked.",
		macOSAgentOutputRequiredMsg: "Recording is required, and this macOS VM run's agent output can't be recorded on this deployment, so the run didn't start.",
	} {
		if got != want {
			t.Errorf("%q, want %q", got, want)
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

// Nothing about recording refuses a macOS VM run at create or Review, whatever asks for a recording; the
// notice says what the level dispatch will resolve records: Screen and actions (its desktop, while
// watched, unmasked) or Terminal. None at off, and none where the organisation keeps no recordings. A
// container is never touched.
func TestStepPlacementCapability_MacOSVMRecording(t *testing.T) {
	locked := func(l types.RecordingLevel) *types.SiteRecording { return &types.SiteRecording{Level: l, Locked: true} }
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
		policy  types.RecordingLevel
		place   func(uuid.UUID) runPlacement
		noStore bool
		want    string // the notice; "" for none
	}{
		{name: "nothing asks for a recording", ceiling: profile, want: macOSVMTerminalNotice},
		{name: "the organisation's unlocked default", site: types.SiteConfig{Recording: &types.SiteRecording{Level: types.RecordingScreenActions}}, ceiling: profile, want: macOSVMScreenNotice},
		{name: "a locked level of off", site: types.SiteConfig{Recording: locked(types.RecordingOff)}, ceiling: profile},
		{name: "an operator's run whose policy is off", ceiling: operator, policy: types.RecordingOff},
		{name: "the organisation requires recording", org: types.RunnerPoolDefaults{Recording: types.RecordingRequired}, ceiling: profile, want: macOSVMTerminalNotice},
		{name: "the pool requires recording", ceiling: profile, place: pooled, want: macOSVMTerminalNotice},
		{name: "the organisation locks Terminal", site: types.SiteConfig{Recording: locked(types.RecordingTerminal)}, ceiling: profile, want: macOSVMTerminalNotice},
		{name: "the organisation locks Screen and actions", site: types.SiteConfig{Recording: locked(types.RecordingScreenActions)}, ceiling: profile, want: macOSVMScreenNotice},
		{name: "the ceiling sets Terminal", ceiling: withLevel(types.RecordingTerminal), want: macOSVMTerminalNotice},
		{name: "the ceiling sets Screen and actions", ceiling: withLevel(types.RecordingScreenActions), policy: types.RecordingScreenActions, want: macOSVMScreenNotice},
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
			f.spec.Recording = tc.policy
			if tc.place != nil {
				f.place = tc.place(id)
			}
			if !s.stepPlacementCapability(f, nil) {
				t.Fatalf("refused: %d %s", w.Code, w.Body.String())
			}
			// The notice rides the fold's warnings, which Review and the 201 both carry.
			var notices []string
			for _, warn := range f.policyWarns {
				if warn == macOSVMScreenNotice || warn == macOSVMTerminalNotice || warn == macOSVMTerminalNoticeNoAgentOutput {
					notices = append(notices, warn)
				}
			}
			if want := map[bool]int{false: 0, true: 1}[tc.want != ""]; len(notices) != want || (want == 1 && notices[0] != tc.want) {
				t.Fatalf("warnings %q, want the notice %q", f.policyWarns, tc.want)
			}
		})
	}

	// The Terminal notice names the agent's output only for a task run whose output tail is on: an interactive run has
	// no agent exec, and with the tail off a task run has no agent output cast. The Screen and actions notice names
	// no agent output, so it is the same either way.
	t.Run("the Terminal notice names the agent's output only where it is kept", func(t *testing.T) {
		const noAgent = "At the Terminal level this run's desktop isn't recorded. Terminal sessions are recorded and masked."
		if macOSVMTerminalNoticeNoAgentOutput != noAgent {
			t.Fatalf("notice %q, want %q", macOSVMTerminalNoticeNoAgentOutput, noAgent)
		}
		for _, tc := range []struct {
			name        string
			interactive bool
			tailOff     bool
			site        types.SiteConfig
			want        string
		}{
			{name: "a task run, tail on", want: macOSVMTerminalNotice},
			{name: "an interactive run", interactive: true, want: noAgent},
			{name: "a task run with the output tail off", tailOff: true, want: noAgent},
			{name: "an interactive run with the output tail off", interactive: true, tailOff: true, want: noAgent},
			{name: "Screen and actions, interactive", interactive: true, site: types.SiteConfig{Recording: &types.SiteRecording{Level: types.RecordingScreenActions}}, want: macOSVMScreenNotice},
			{name: "Screen and actions, tail off", tailOff: true, site: types.SiteConfig{Recording: &types.SiteRecording{Level: types.RecordingScreenActions}}, want: macOSVMScreenNotice},
		} {
			s := capsServer(&fakeRunner{})
			id := macRunner(t, s, readyMac(), roomyRunner)
			s.cfg.Store = &macRecordingStore{ownedRunners: s.cfg.Store.(ownedRunners), site: tc.site}
			s.cfg.RecordingStore = fsRecordingStore(t)
			s.cfg.ExecOutputTailOff = tc.tailOff
			req := macReq(id)
			req.Interactive = tc.interactive
			f := &runFold{w: httptest.NewRecorder(), r: httptest.NewRequest(http.MethodPost, "/api/v1/runs", nil).WithContext(capsCtx), req: &req, ceiling: profile}
			if !s.stepPlacementCapability(f, nil) {
				t.Fatalf("%s: refused: %s", tc.name, f.w.(*httptest.ResponseRecorder).Body.String())
			}
			var notices []string
			for _, warn := range f.policyWarns {
				if warn == macOSVMScreenNotice || warn == macOSVMTerminalNotice || warn == macOSVMTerminalNoticeNoAgentOutput {
					notices = append(notices, warn)
				}
			}
			if len(notices) != 1 || notices[0] != tc.want {
				t.Errorf("%s: notices %q, want only %q", tc.name, notices, tc.want)
			}
		}
	})

	t.Run("a container on the same Mac is not touched", func(t *testing.T) {
		s := capsServer(&fakeRunner{})
		id := macRunner(t, s, readyMac(), roomyRunner)
		s.cfg.Store = &macRecordingStore{ownedRunners: s.cfg.Store.(ownedRunners), org: types.RunnerPoolDefaults{Recording: types.RecordingRequired},
			site: types.SiteConfig{Recording: locked(types.RecordingTerminal)}}
		s.cfg.RecordingStore = fsRecordingStore(t)
		req := createRunRequest{Placement: types.PlacementLocal, RunnerID: id.String()}
		w := httptest.NewRecorder()
		f := &runFold{w: w, r: httptest.NewRequest(http.MethodPost, "/api/v1/runs", nil).WithContext(capsCtx), req: &req, ceiling: withLevel(types.RecordingTerminal)}
		if !s.stepPlacementCapability(f, nil) || len(f.policyWarns) != 0 {
			t.Fatalf("a container: ok body %s, warnings %q; want placed with none", w.Body.String(), f.policyWarns)
		}
	})

	t.Run("a site config that cannot be read is a server error", func(t *testing.T) {
		s := capsServer(&fakeRunner{})
		id := macRunner(t, s, readyMac(), roomyRunner)
		s.cfg.Store = &macRecordingStore{ownedRunners: s.cfg.Store.(ownedRunners), siteErr: context.DeadlineExceeded}
		s.cfg.RecordingStore = fsRecordingStore(t)
		req := macReq(id)
		w := httptest.NewRecorder()
		f := &runFold{w: w, r: httptest.NewRequest(http.MethodPost, "/api/v1/runs", nil).WithContext(capsCtx), req: &req, ceiling: profile}
		if s.stepPlacementCapability(f, nil) || w.Code != http.StatusInternalServerError || len(f.policyWarns) != 0 {
			t.Fatalf("an unreadable site config: %d %s, warnings %q; want a server error and no notice", w.Code, w.Body.String(), f.policyWarns)
		}
	})
}

// At dispatch a macOS VM run is asked not to record, even on a runner that records, so the runner's own
// backstop never refuses it, and it is marked recorded: the organisation records it (a task run's mark is
// then settled by its agent output cast, recordMacOSAgent). A container run on the same runner is asked to
// record as before.
func TestApplyRunRecording_MacOSVMIsMarkedRecorded(t *testing.T) {
	records := uuid.New()
	for _, tc := range []struct {
		name       string
		kind       types.MachineKind
		wantRecord bool
	}{
		{"a macOS VM on a runner that records", types.MachineMacOSVM, false},
		{"a container on the same runner", types.MachineContainer, true},
		{"a Linux VM on the same runner", types.MachineLinuxVM, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := &Server{cfg: Config{RecordingStore: fsRecordingStore(t), Store: &recordingReadFake{},
				Runner: &poolRunner{fakeRunner: &fakeRunner{}, runnerFacts: map[string]runner.TargetFacts{records.String(): {Records: true}}}}}
			run := types.AgentRun{ID: uuid.New(), RunnerID: &records, MachineKind: tc.kind}
			var spec runner.SandboxSpec
			if !s.applyRunRecording(context.Background(), &run, &spec, types.RecordingChoice{}) {
				t.Fatal("dispatch held a run that nothing requires recording for")
			}
			if spec.Record == nil || *spec.Record != tc.wantRecord || run.NotRecorded != "" {
				t.Fatalf("record=%v not_recorded=%q, want %v and no mark", spec.Record, run.NotRecorded, tc.wantRecord)
			}
		})
	}
}

// A run under its owner's recording wall is never started below it: every kind is raised to the wall's
// level. A macOS VM run, once held here, now starts at that level, recorded by the organisation.
func TestApplyRunRecording_WallsFloorByMachineKind(t *testing.T) {
	records := uuid.New()
	for _, tc := range []struct {
		name   string
		kind   types.MachineKind
		record bool
	}{
		{"a container", types.MachineContainer, true},
		{"a Linux VM", types.MachineLinuxVM, true},
		{"a macOS VM", types.MachineMacOSVM, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := &Server{cfg: Config{RecordingStore: fsRecordingStore(t), Store: &recordingReadFake{},
				Runner: &poolRunner{fakeRunner: &fakeRunner{}, runnerFacts: map[string]runner.TargetFacts{records.String(): {Records: true}}}}}
			run := types.AgentRun{ID: uuid.New(), RunnerID: &records, MachineKind: tc.kind}
			choice := types.RecordingChoice{Policy: types.RecordingOff, WallsFloor: types.RecordingScreenActions}
			var spec runner.SandboxSpec
			if !s.applyRunRecording(context.Background(), &run, &spec, choice) {
				t.Fatal("dispatch held a run under walls")
			}
			if spec.Record == nil || *spec.Record != tc.record || run.RecordingLevel != types.RecordingScreenActions || run.NotRecorded != "" {
				t.Fatalf("record=%v level=%q not_recorded=%q, want record=%v at %q, no mark", spec.Record, run.RecordingLevel, run.NotRecorded, tc.record, types.RecordingScreenActions)
			}
		})
	}
}

// Dispatch no longer holds a macOS VM run for what asks it to be recorded: a requirement, a locked level,
// a ceiling, the owner's walls, or a requirement that cannot be read. Each starts marked recorded.
func TestApplyRunRecording_MacOSVMIsNeverHeld(t *testing.T) {
	boom := errors.New("connection reset")
	for _, tc := range []struct {
		name   string
		fake   recordingReadFake
		choice types.RecordingChoice
	}{
		{"organisation requires", recordingReadFake{org: types.RunnerPoolDefaults{Recording: types.RecordingRequired}}, types.RecordingChoice{Required: true}},
		{"locked level", recordingReadFake{}, types.RecordingChoice{Org: &types.SiteRecording{Locked: true}}},
		{"ceiling level", recordingReadFake{}, types.RecordingChoice{Ceiling: types.RecordingTerminal, CeilingBy: "Platform"}},
		{"the owner's walls", recordingReadFake{}, types.RecordingChoice{WallsFloor: types.RecordingTerminal}},
		{"requirement unreadable", recordingReadFake{orgErr: boom}, types.RecordingChoice{}},
	} {
		s := &Server{cfg: Config{RecordingStore: fsRecordingStore(t), Store: &tc.fake}}
		run := types.AgentRun{ID: uuid.New(), MachineKind: types.MachineMacOSVM}
		var spec runner.SandboxSpec
		if !s.applyRunRecording(context.Background(), &run, &spec, tc.choice) || run.NotRecorded != "" || spec.Record == nil || *spec.Record {
			t.Errorf("%s: held or marked (not_recorded %q, record %v), want started recorded and the runner asked not to record", tc.name, run.NotRecorded, spec.Record)
		}
	}
}

// A macOS VM run's desktop is recorded at Screen and actions as a Linux VM's is, marked runner_declined
// too (on macOS the mark is about the agent's output only); at Terminal neither is.
func TestMacOSVMDesktopIsRecordedAtScreenAndActions(t *testing.T) {
	s := &Server{cfg: Config{RecordingStore: fsRecordingStore(t)}}
	for _, tc := range []struct {
		kind  types.MachineKind
		level types.RecordingLevel
		mark  types.RunNotRecorded
		want  bool
	}{
		{types.MachineMacOSVM, types.RecordingScreenActions, "", true},
		{types.MachineMacOSVM, types.RecordingScreenActions, types.NotRecordedRunnerDeclined, true},
		{types.MachineLinuxVM, types.RecordingScreenActions, "", true},
		{types.MachineMacOSVM, types.RecordingTerminal, "", false},
	} {
		run := types.AgentRun{ID: uuid.New(), MachineKind: tc.kind, RecordingLevel: tc.level, NotRecorded: tc.mark}
		if got := s.desktopRecorded(run); got != tc.want {
			t.Errorf("%s at %s marked %q: desktopRecorded = %v, want %v", tc.kind, tc.level, tc.mark, got, tc.want)
		}
	}
}

// A macOS VM run's console and SSH terminal sessions are recorded at the run's recording level, masked,
// as any run's are, marked recorded or not: its mark is about the agent's output, not the organisation's
// terminal sessions. At a level of off they are not.
func TestMacOSVMTerminalSessionsAreRecordedAboveOff(t *testing.T) {
	const secret = "macos-vm-secret-token-1234"
	for _, tc := range []struct {
		level    types.RecordingLevel
		notRec   types.RunNotRecorded
		recorded bool
	}{
		{types.RecordingTerminal, "", true},
		{types.RecordingTerminal, types.NotRecordedRunnerDeclined, true},
		{types.RecordingScreenActions, types.NotRecordedRunnerDeclined, true},
		{types.RecordingOff, types.NotRecordedPolicyOff, false},
	} {
		t.Run(string(tc.level), func(t *testing.T) {
			runID := uuid.New()
			reg := secretmask.NewRegistry()
			reg.Add(runID, []byte(secret))
			casts, audit := fsRecordingStore(t), &recRecorder{}
			srv := New(Config{RecordingStore: casts, MaskRegistry: reg, Audit: audit, AdminToken: adminToken})
			run := types.AgentRun{ID: runID, MachineKind: types.MachineMacOSVM, RecordingLevel: tc.level, NotRecorded: tc.notRec}
			tee, finish := srv.newSessionRecorder(run, "console-1", runner.AttachOptions{Cols: 80, Rows: 24})
			if (tee != nil) != tc.recorded {
				t.Fatalf("tee non-nil = %v, want %v", tee != nil, tc.recorded)
			}
			if tee != nil {
				_, _ = tee.Write([]byte("token is " + secret + "\r\n"))
			}
			finish(context.Background(), types.ActorHuman, "alice@example.com")

			key := recording.CastKey(runID.String(), "console-1")
			rc, err := casts.OpenCast(context.Background(), key)
			if err == nil {
				defer rc.Close()
			}
			if (err == nil) != tc.recorded {
				t.Fatalf("OpenCast error = %v, want a cast stored = %v", err, tc.recorded)
			}
			var rows int
			for _, ev := range audit.snapshot() {
				if ev.Action == "session.recording.write" && ev.Outcome == "success" && ev.Target == key {
					rows++
				}
			}
			if want := map[bool]int{true: 1, false: 0}[tc.recorded]; rows != want {
				t.Errorf("session.recording.write rows = %d, want %d", rows, want)
			}
			if !tc.recorded {
				return
			}
			body, _ := io.ReadAll(rc)
			assertCastHasNoSecret(t, string(body), secret, "macOS VM console session")
			if !strings.Contains(decodeCastOutput(t, string(body)), "<secret-hidden>") {
				t.Errorf("the cast lacks the mask placeholder:\n%s", body)
			}
		})
	}
}
