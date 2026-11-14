// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// signinConts is how many SIGCONTs f's runner was sent after its first n calls.
func signinConts(f *signinFixture, n int) int {
	return strings.Count(strings.Join(f.rn.seen()[n:], " "), "signal:SIGCONT")
}

// TestDesktopSignin_AnEndStuckAtResumeKillsTheRunAfterThreePasses: an end whose agent cannot be resumed,
// because resume-agent keeps failing or pid 1 is still read stopped, is retried on signinContinueTries
// sweep passes with the count on the row and one failure row in all; then the run is killed with the named
// reason and the row closes with agent_resumed false. Later passes find nothing to do.
func TestDesktopSignin_AnEndStuckAtResumeKillsTheRunAfterThreePasses(t *testing.T) {
	for name, stick := range map[string]func(*signinFakeRunner){
		"resume-agent keeps failing":  func(rn *signinFakeRunner) { rn.exit["resume-agent"] = 1 },
		"pid 1 is still read stopped": func(rn *signinFakeRunner) { rn.states = strings.Repeat("T", 3*(1+signinContinueTries)) },
	} {
		t.Run(name, func(t *testing.T) {
			f := newSigninFixture(t)
			f.begin(t)
			f.rn.mu.Lock()
			stick(f.rn)
			f.rn.mu.Unlock()
			if code, body := f.post(t, "/desktop-signin/end", ""); code != http.StatusBadGateway {
				t.Fatalf("end = %d %s, want 502", code, body)
			}
			if si := f.st.only(t); si.EndedAt != nil || si.EndStage != signinStageResume {
				t.Fatalf("row after the end = %+v, want open at stage %q", si, signinStageResume)
			}
			for pass := 1; pass <= signinContinueTries+2; pass++ {
				_ = f.srv.sweepDesktopSignins(context.Background())
				run, err := f.st.GetRun(context.Background(), f.run.ID)
				si := f.st.only(t)
				switch {
				case pass < signinContinueTries && (si.EndedAt != nil || si.EndStage != signinStage(signinStageResume, pass) || err != nil || run.State != types.RunRunning):
					t.Fatalf("after pass %d: row %+v, run %v %v; want open at stage %q, run RUNNING", pass, si, run.State, err, signinStage(signinStageResume, pass))
				case pass >= signinContinueTries && (si.EndedAt == nil || err != nil || run.State != types.RunKilled):
					t.Fatalf("after pass %d: row %+v, run %v %v; want closed, run KILLED", pass, si, run.State, err)
				}
			}
			if n := strings.Count(strings.Join(f.rn.seen(), " "), "agent:resume-agent"); n != 1+signinContinueTries {
				t.Fatalf("resume-agent ran %d times, want %d (the end's, then one per pass until the kill)", n, 1+signinContinueTries)
			}
			if si := f.st.only(t); si.EndReason != signinEndDone {
				t.Fatalf("row = %+v, want ended %s", si, signinEndDone)
			}
			if ev := findAuditData(f.h.audit.snapshot(), f.run.ID, "run.kill", `"reason":"`+reasonDesktopSigninResumeFailed+`"`); ev == nil {
				t.Fatalf("no run.kill row with reason %s", reasonDesktopSigninResumeFailed)
			}
			if got, want := signinCloseRows(f), []string{"failure false", "success false"}; !slices.Equal(got, want) {
				t.Fatalf("desktop_signin.close rows = %v, want %v (the end's failure, then the kill's close)", got, want)
			}
		})
	}
}

// TestDesktopSignin_AStuckEndThatChangesStageKeepsItsCount: an end stuck first at the agent's resume, whose
// sweep passes then fail at the display's stop, at the resume and at the stop again, is killed on the third
// counted pass. A change of stage carries the count, so stop and resume failing in turn never reset it.
func TestDesktopSignin_AStuckEndThatChangesStageKeepsItsCount(t *testing.T) {
	f := newSigninFixture(t)
	f.begin(t)
	failAt := func(stage string) {
		f.rn.mu.Lock()
		defer f.rn.mu.Unlock()
		f.rn.exit["stop"], f.rn.exit["resume-agent"] = 0, 0
		if stage == signinStageStop {
			f.rn.exit["stop"] = 1
		} else {
			f.rn.exit["resume-agent"] = 1
		}
	}
	failAt(signinStageResume)
	if code, body := f.post(t, "/desktop-signin/end", ""); code != http.StatusBadGateway {
		t.Fatalf("end = %d %s, want 502", code, body)
	}
	if si := f.st.only(t); si.EndedAt != nil || si.EndStage != signinStageResume {
		t.Fatalf("row after the end = %+v, want open at stage %q", si, signinStageResume)
	}
	for i, stage := range []string{signinStageStop, signinStageResume, signinStageStop} {
		pass := i + 1
		failAt(stage)
		_ = f.srv.sweepDesktopSignins(context.Background())
		run, err := f.st.GetRun(context.Background(), f.run.ID)
		si := f.st.only(t)
		switch {
		case pass < signinContinueTries && (si.EndedAt != nil || si.EndStage != signinStage(stage, pass) || err != nil || run.State != types.RunRunning):
			t.Fatalf("after pass %d (%s fails): row %+v, run %v %v; want open at stage %q, run RUNNING", pass, stage, si, run.State, err, signinStage(stage, pass))
		case pass >= signinContinueTries && (si.EndedAt == nil || err != nil || run.State != types.RunKilled):
			t.Fatalf("after pass %d (%s fails): row %+v, run %v %v; want closed, run KILLED", pass, stage, si, run.State, err)
		}
	}
	if ev := findAuditData(f.h.audit.snapshot(), f.run.ID, "run.kill", `"reason":"`+reasonDesktopSigninResumeFailed+`"`); ev == nil {
		t.Fatalf("no run.kill row with reason %s", reasonDesktopSigninResumeFailed)
	}
	if got, want := signinCloseRows(f), []string{"failure false", "success false"}; !slices.Equal(got, want) {
		t.Fatalf("desktop_signin.close rows = %v, want %v (the end's failure only, then the kill's close)", got, want)
	}
}

// TestDesktopSignin_AContinueRetryReadsTheAgentBack: the sweep's retry of a begin left at the continue
// stage reads pid 1's state after its SIGCONT as the begin does. A stopped read gets one more SIGCONT, a
// pid 1 still read stopped is a failed pass, and a state that cannot be read closes with agent_resumed false.
func TestDesktopSignin_AContinueRetryReadsTheAgentBack(t *testing.T) {
	for name, tc := range map[string]struct {
		states  string
		readErr error
		conts   int    // SIGCONTs the pass sent
		stage   string // the row's stage after the pass, "" when it closed
		close   string // the pass's desktop_signin.close row, "" for none
	}{
		"a read finds pid 1 stopped, then running": {states: "T", conts: 2, close: "success true"},
		"every read finds pid 1 stopped":           {states: "TTT", conts: 3, stage: signinStage(signinStageContinue, 1)},
		"a state that cannot be read":              {readErr: errors.New("exec refused"), conts: 1, close: "success false"},
	} {
		t.Run(name, func(t *testing.T) {
			f := newSigninFixture(t)
			signinMissingHelper(f.rn)
			f.rn.contFail = 1
			if code, body := f.post(t, "/desktop-signin", ""); code != http.StatusBadGateway {
				t.Fatalf("begin = %d %s, want 502", code, body)
			}
			if si := f.st.only(t); si.EndedAt != nil || si.EndStage != signinStageContinue {
				t.Fatalf("row after the begin = %+v, want open at stage %q", si, signinStageContinue)
			}
			before := len(f.rn.seen())
			f.rn.mu.Lock()
			f.rn.states, f.rn.readErr = tc.states, tc.readErr
			f.rn.mu.Unlock()
			_ = f.srv.sweepDesktopSignins(context.Background())
			if n := signinConts(f, before); n != tc.conts {
				t.Fatalf("SIGCONTs sent by the pass = %d, want %d", n, tc.conts)
			}
			si := f.st.only(t)
			if closed := si.EndedAt != nil; closed != (tc.stage == "") || !closed && si.EndStage != tc.stage {
				t.Fatalf("row = ended %v, stage %q; want closed %t, else stage %q", si.EndedAt, si.EndStage, tc.stage == "", tc.stage)
			}
			want := []string{"failure false"}
			if tc.close != "" {
				want = append(want, tc.close)
			}
			if got := signinCloseRows(f); !slices.Equal(got, want) {
				t.Fatalf("desktop_signin.close rows = %v, want %v", got, want)
			}
		})
	}
}

// TestDesktopSignin_ANormalEndWhoseStateCannotBeReadClosesWithAgentResumedFalse: an end whose read of
// pid 1 cannot be made after an accepted SIGCONT closes the row, and its close row does not claim the
// agent was resumed.
func TestDesktopSignin_ANormalEndWhoseStateCannotBeReadClosesWithAgentResumedFalse(t *testing.T) {
	f := newSigninFixture(t)
	f.begin(t)
	before := len(f.rn.seen())
	f.rn.mu.Lock()
	f.rn.readErr = errors.New("exec refused")
	f.rn.mu.Unlock()
	if code, body := f.post(t, "/desktop-signin/end", ""); code != http.StatusNoContent {
		t.Fatalf("end = %d %s, want 204", code, body)
	}
	if n := signinConts(f, before); n != 1 {
		t.Fatalf("SIGCONTs sent by the end = %d, want 1", n)
	}
	if si := f.st.only(t); si.EndedAt == nil || si.EndReason != signinEndDone {
		t.Fatalf("row = %+v, want ended %s", si, signinEndDone)
	}
	if got := signinCloseRows(f); !slices.Equal(got, []string{"success false"}) {
		t.Fatalf("desktop_signin.close rows = %v, want [success false]", got)
	}
}

// signinStopUnsupportedRunner is signinFakeRunner whose SIGSTOP answers runner.ErrSigninUnsupported, as a
// substrate without sign-ins does before it sends any signal.
type signinStopUnsupportedRunner struct{ *signinFakeRunner }

func (r signinStopUnsupportedRunner) SignalMain(ctx context.Context, ref, signal string) error {
	if signal == "SIGSTOP" {
		return runner.ErrSigninUnsupported
	}
	return r.signinFakeRunner.SignalMain(ctx, ref, signal)
}

// TestDesktopSignin_AStopRefusedAsUnsupportedSendsNoContinue: a SIGSTOP refused before any signal went out
// closes the row with no SIGCONT and no close row, and the begin answers that the run has no sign-ins.
func TestDesktopSignin_AStopRefusedAsUnsupportedSendsNoContinue(t *testing.T) {
	f := newSigninFixture(t)
	f.srv.cfg.Runner = signinStopUnsupportedRunner{f.rn}
	if code, body := f.post(t, "/desktop-signin", ""); code != http.StatusConflict || !strings.Contains(body, reasonDesktopSigninRunnerUnsupported) {
		t.Fatalf("begin = %d %s, want 409 %s", code, body, reasonDesktopSigninRunnerUnsupported)
	}
	if got := f.rn.seen(); len(got) != 0 {
		t.Fatalf("calls = %v, want none: no signal went out, so none is continued", got)
	}
	if si := f.st.only(t); si.EndedAt == nil || si.EndReason != signinEndFailed {
		t.Fatalf("row = %+v, want ended %s", si, signinEndFailed)
	}
	if got := signinCloseRows(f); len(got) != 0 {
		t.Fatalf("desktop_signin.close rows = %v, want none", got)
	}
}

// signinKillFailsRunner is signinFakeRunner whose KillSandbox fails while *fails is above zero, as a
// docker daemon that cannot kill or remove the container does.
type signinKillFailsRunner struct {
	*signinFakeRunner
	fails, kills *int
}

func (r signinKillFailsRunner) KillSandbox(context.Context, string) error {
	r.signinFakeRunner.mu.Lock()
	defer r.signinFakeRunner.mu.Unlock()
	*r.kills++
	if *r.fails > 0 {
		*r.fails--
		return errors.New("docker: kill: daemon busy")
	}
	return nil
}

// TestDesktopSignin_AStuckEndWhoseKillLeavesTheSandboxStaysOpenUntilItIsGone: the kill of an end stuck at
// the display's stop whose sandbox teardown fails leaves the row open, since the person's display runs in
// that sandbox; the next pass tears the sandbox down and only then closes the row. One failure row in all.
func TestDesktopSignin_AStuckEndWhoseKillLeavesTheSandboxStaysOpenUntilItIsGone(t *testing.T) {
	f := newSigninFixture(t)
	f.begin(t)
	fails, kills := 1, 0
	f.srv.cfg.Runner = signinKillFailsRunner{f.rn, &fails, &kills}
	f.rn.mu.Lock()
	f.rn.exit["stop"] = 1
	f.rn.mu.Unlock()
	if code, body := f.post(t, "/desktop-signin/end", ""); code != http.StatusBadGateway {
		t.Fatalf("end = %d %s, want 502", code, body)
	}
	for range signinContinueTries {
		_ = f.srv.sweepDesktopSignins(context.Background())
	}
	run, err := f.st.GetRun(context.Background(), f.run.ID)
	if si := f.st.only(t); err != nil || run.State != types.RunKilled || kills != 1 || si.EndedAt != nil ||
		si.EndStage != signinStage(signinStageStop, signinContinueTries) {
		t.Fatalf("after the failed kill: run %v %v, kills %d, row %+v; want KILLED, 1, open at stage %q",
			run.State, err, kills, si, signinStage(signinStageStop, signinContinueTries))
	}
	_ = f.srv.sweepDesktopSignins(context.Background())
	if si := f.st.only(t); kills != 2 || si.EndedAt == nil || si.EndReason != signinEndDone {
		t.Fatalf("after the next pass: kills %d, row %+v; want 2 and ended %s", kills, si, signinEndDone)
	}
	if got, want := signinCloseRows(f), []string{"failure false", "success false"}; !slices.Equal(got, want) {
		t.Fatalf("desktop_signin.close rows = %v, want %v", got, want)
	}
}
