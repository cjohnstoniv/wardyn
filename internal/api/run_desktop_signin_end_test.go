// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// ClaimDesktopSigninEnd is the test store's compare-and-set of an unmarked end's end_stage, as store.PG's.
func (s *signinTestStore) ClaimDesktopSigninEnd(_ context.Context, id uuid.UUID, read, claim string) (bool, error) {
	s.smu.Lock()
	defer s.smu.Unlock()
	si, ok := s.signins[id]
	if !ok || si.EndedAt != nil || si.EndRequested != "" || si.EndStage != read {
		return false, nil
	}
	si.EndStage = claim
	s.signins[id] = si
	return true, nil
}

// signinPid1Runner is signinFakeRunner whose pid 1 is stopped by a SIGSTOP and running after a SIGCONT, as a real
// agent's is, and whose reads of pid 1's state answer that. When at names an event, "signal:<name>" or "read" (a read
// of pid 1's state), during runs once, just before the first such event.
type signinPid1Runner struct {
	*signinFakeRunner
	stopped bool
	at      string
	during  func()
}

func (r *signinPid1Runner) before(event string) {
	if during := r.during; during != nil && event == r.at {
		r.during = nil
		during()
	}
}

func (r *signinPid1Runner) SignalMain(ctx context.Context, ref, signal string) error {
	r.before("signal:" + signal)
	if err := r.signinFakeRunner.SignalMain(ctx, ref, signal); err != nil {
		return err
	}
	r.stopped = signal == "SIGSTOP"
	return nil
}

func (r *signinPid1Runner) ExecStreamAs(ctx context.Context, ref, user string, spec runner.ExecSpec) (*runner.ExecSession, error) {
	if !slices.Equal(spec.Argv, signinMainState) {
		return r.signinFakeRunner.ExecStreamAs(ctx, ref, user, spec)
	}
	r.before("read")
	state := "S"
	if r.stopped {
		state = "T"
	}
	return &runner.ExecSession{Stdout: strings.NewReader("1 (agent) " + state + " 0 1"), Stderr: strings.NewReader(""),
		Wait: func() (int, error) { return 0, nil }}, nil
}

// TestDesktopSignin_ALateEndNeverContinuesANewerSignInsStop: an end that is reading pid 1's state after its SIGCONT,
// while a second end of the same sign-in closes the row and the person begins again, must not continue pid 1 once
// the newer sign-in stopped it. Whichever end wins, pid 1 is stopped exactly while a sign-in is open.
func TestDesktopSignin_ALateEndNeverContinuesANewerSignInsStop(t *testing.T) {
	f := newSigninFixture(t)
	pid1 := &signinPid1Runner{signinFakeRunner: f.rn}
	f.srv.cfg.Runner = pid1
	f.begin(t)
	pid1.at, pid1.during = "read", func() {
		f.post(t, "/desktop-signin/end", "") // a second end of the same sign-in
		f.post(t, "/desktop-signin", "")     // the person begins again
	}
	if code, body := f.post(t, "/desktop-signin/end", ""); code != http.StatusNoContent {
		t.Fatalf("the first end = %d %s, want 204", code, body)
	}
	if pid1.during != nil {
		t.Fatal("the first end never read pid 1's state")
	}
	_, err := f.st.OpenDesktopSignin(context.Background(), f.run.ID)
	if open := err == nil; open != pid1.stopped {
		t.Fatalf("a sign-in open = %t (%v), pid 1 stopped = %t; want pid 1 stopped exactly while a sign-in is open", open, err, pid1.stopped)
	}
}

// TestDesktopSignin_ALateEndSendsNothingOnceItsRowClosed: an end that read its row before another end closed it, and
// runs only after a newer sign-in stopped the agent, sends no stop, resume-agent or SIGCONT, and leaves the newer
// sign-in as it was.
func TestDesktopSignin_ALateEndSendsNothingOnceItsRowClosed(t *testing.T) {
	f := newSigninFixture(t)
	f.begin(t)
	late := f.st.only(t) // read before the person's end, as the sweep's list or a begin's own row is
	if code, body := f.post(t, "/desktop-signin/end", ""); code != http.StatusNoContent {
		t.Fatalf("end = %d %s, want 204", code, body)
	}
	f.begin(t)
	ctx := context.Background()
	newer, err := f.st.OpenDesktopSignin(ctx, f.run.ID)
	if err != nil || newer.ID == late.ID {
		t.Fatalf("open = %+v %v, want the newer sign-in", newer, err)
	}
	before := len(f.rn.seen())
	if err := f.srv.finishDesktopSignin(ctx, f.run, late, signinEndExpired, types.ActorSystem, "wardynd"); !errors.Is(err, errSigninEndClaimed) {
		t.Fatalf("the late end = %v, want errSigninEndClaimed", err)
	}
	f.srv.abandonDesktopSignin(ctx, f.rn, f.run, late, signinOwner)
	if got := f.rn.seen()[before:]; len(got) != 0 {
		t.Fatalf("the late ends sent %v; want nothing", got)
	}
	if open, err := f.st.OpenDesktopSignin(ctx, f.run.ID); err != nil || open.ID != newer.ID || open.EndStage != "" || open.EndRequested != "" {
		t.Fatalf("open = %+v %v, want the newer sign-in as it was", open, err)
	}
}

// TestDesktopSignin_ASecondEndSendsNothingWhileTheFirstRuns: while one end of a sign-in runs, every other end of it
// (the person's again, the sweep's once the window passed, a begin's abandon or unstarted close) sends no stop,
// resume-agent or SIGCONT and leaves the row open, and no newer sign-in can begin; the first end then finishes alone,
// so nothing it sends lands on a newer sign-in.
func TestDesktopSignin_ASecondEndSendsNothingWhileTheFirstRuns(t *testing.T) {
	f := newSigninFixture(t)
	f.begin(t)
	ctx := context.Background()
	var during []string
	hook := &signinPid1Runner{signinFakeRunner: f.rn, at: "signal:SIGCONT"}
	hook.during = func() {
		before := len(f.rn.seen())
		si := f.st.only(t)
		if err := f.srv.finishDesktopSignin(ctx, f.run, si, signinEndDone, types.ActorHuman, signinOwner); !errors.Is(err, errSigninEndClaimed) {
			t.Errorf("the person's second end = %v, want errSigninEndClaimed", err)
		}
		if err := f.srv.finishDesktopSignin(ctx, f.run, si, signinEndExpired, types.ActorSystem, "wardynd"); !errors.Is(err, errSigninEndClaimed) {
			t.Errorf("the sweep's end = %v, want errSigninEndClaimed", err)
		}
		f.srv.abandonDesktopSignin(ctx, f.rn, f.run, si, signinOwner)
		f.srv.endUnstartedDesktopSignin(ctx, f.st, f.run, si)
		now := time.Now()
		newer := types.DesktopSignin{ID: uuid.New(), RunID: f.run.ID, Holder: signinOwner, BeganAt: now, ExpiresAt: now.Add(desktopSigninWindow)}
		if err := f.st.BeginDesktopSignin(ctx, newer); !errors.Is(err, store.ErrConflict) {
			t.Errorf("a newer begin = %v, want store.ErrConflict", err)
		}
		during = f.rn.seen()[before:]
	}
	f.srv.cfg.Runner = hook
	before := len(f.rn.seen())
	if code, body := f.post(t, "/desktop-signin/end", ""); code != http.StatusNoContent {
		t.Fatalf("the first end = %d %s, want 204", code, body)
	}
	if hook.during != nil {
		t.Fatal("the first end sent no SIGCONT")
	}
	if len(during) != 0 {
		t.Fatalf("sent while the first end ran: %v; want nothing", during)
	}
	if got, want := f.rn.seen()[before:], []string{"person:stop", "agent:resume-agent", "signal:SIGCONT"}; !slices.Equal(got, want) {
		t.Fatalf("calls = %v, want %v", got, want)
	}
	if si := f.st.only(t); si.EndedAt == nil || si.EndReason != signinEndDone {
		t.Fatalf("row = %+v, want closed by the first end (done)", si)
	}
}

// TestDesktopSignin_AnEndWhoseClaimLapsedIsEndedByTheNext: a row an end claimed and never closed or gave back (wardynd
// stopped mid-end) is left alone while the claim holds: the person's end answers 409 and sends nothing. Once the
// claim's hold passed, the next end claims the row again and ends it.
func TestDesktopSignin_AnEndWhoseClaimLapsedIsEndedByTheNext(t *testing.T) {
	for name, tc := range map[string]struct {
		left  time.Duration // how long the earlier claim still holds
		code  int
		calls []string
	}{
		"the claim holds":  {left: time.Minute, code: http.StatusConflict},
		"the claim lapsed": {left: -time.Second, code: http.StatusNoContent, calls: []string{"person:stop", "agent:resume-agent", "signal:SIGCONT"}},
	} {
		t.Run(name, func(t *testing.T) {
			f := newSigninFixture(t)
			f.begin(t)
			si := f.st.only(t)
			claim := signinStageEnding + ":" + strconv.FormatInt(f.srv.cfg.Now().Add(tc.left).Unix(), 10)
			if ok, err := f.st.ClaimDesktopSigninEnd(context.Background(), si.ID, "", claim); err != nil || !ok {
				t.Fatalf("claim = %t %v", ok, err)
			}
			before := len(f.rn.seen())
			code, body := f.post(t, "/desktop-signin/end", "")
			if code != tc.code || code == http.StatusConflict && !strings.Contains(body, reasonDesktopSigninNotOpen) {
				t.Fatalf("end = %d %s, want %d", code, body, tc.code)
			}
			if got := f.rn.seen()[before:]; !slices.Equal(got, tc.calls) {
				t.Fatalf("calls = %v, want %v", got, tc.calls)
			}
			if closed := f.st.only(t).EndedAt != nil; closed != (tc.code == http.StatusNoContent) {
				t.Fatalf("row closed = %t, want %t", closed, tc.code == http.StatusNoContent)
			}
		})
	}
}

// TestSigninStageMade_ReadsAHandEditedCountAsTheLastPass: the counts signinStage writes read back as written; any
// other count after a counted stage's colon reads as the last pass; any other end_stage is not the stage's.
func TestSigninStageMade_ReadsAHandEditedCountAsTheLastPass(t *testing.T) {
	type read struct {
		made int
		ok   bool
	}
	for endStage, want := range map[string]read{
		"stop": {0, true}, "stop:1": {1, true}, "stop:2": {2, true}, "stop:3": {3, true},
		"stop:0": {3, true}, "stop:4": {3, true}, "stop:x": {3, true}, "stop:": {3, true},
		"stop:-2": {3, true}, "stop:+1": {3, true}, "stop:01": {3, true},
		"resume:1": {0, false}, "stopped": {0, false}, "": {0, false},
	} {
		if made, ok := signinStageMade(endStage, signinStageStop); made != want.made || ok != want.ok {
			t.Errorf("signinStageMade(%q, stop) = %d, %t; want %d, %t", endStage, made, ok, want.made, want.ok)
		}
	}
}

// TestDesktopSignin_AnEndAtItsLastPassKillsTheRunOnEntry: a row whose end_stage is a counted stage's last pass, or a
// count no wardynd path writes, is killed by the sweep's next pass before any stop, resume-agent or SIGCONT: the run
// is KILLED with desktop_signin_resume_failed and the row closes with agent_resumed false.
func TestDesktopSignin_AnEndAtItsLastPassKillsTheRunOnEntry(t *testing.T) {
	for _, stage := range []string{
		signinStage(signinStageStop, signinContinueTries), signinStage(signinStageResume, signinContinueTries),
		signinStage(signinStageContinue, signinContinueTries),
		"stop:x", "stop:", "stop:0", "stop:-2", "stop:+1", "resume:x", "continue:x",
	} {
		t.Run(stage, func(t *testing.T) {
			f := newSigninFixture(t)
			f.begin(t)
			if _, err := f.st.RequestDesktopSigninEnd(context.Background(), f.st.only(t).ID, signinEndDone, stage); err != nil {
				t.Fatal(err)
			}
			before := len(f.rn.seen())
			_ = f.srv.sweepDesktopSignins(context.Background())
			for _, c := range f.rn.seen()[before:] {
				if c == "person:stop" || c == "agent:resume-agent" || c == "signal:SIGCONT" {
					t.Fatalf("the pass ran %q; want the kill before any stop, resume-agent or SIGCONT", c)
				}
			}
			run, err := f.st.GetRun(context.Background(), f.run.ID)
			if si := f.st.only(t); err != nil || run.State != types.RunKilled || si.EndedAt == nil {
				t.Fatalf("after the pass: run %v %v, row %+v; want KILLED and closed", run.State, err, si)
			}
			if ev := findAuditData(f.h.audit.snapshot(), f.run.ID, "run.kill", `"reason":"`+reasonDesktopSigninResumeFailed+`"`); ev == nil {
				t.Fatalf("no run.kill row with reason %s", reasonDesktopSigninResumeFailed)
			}
			if got := signinCloseRows(f); !slices.Equal(got, []string{"success false"}) {
				t.Fatalf("desktop_signin.close rows = %v, want [success false]", got)
			}
		})
	}
}

// signinEndUnwrittenStore is signinTestStore whose EndDesktopSignin fails, as a database write that cannot be made.
type signinEndUnwrittenStore struct{ *signinTestStore }

func (s signinEndUnwrittenStore) EndDesktopSignin(context.Context, uuid.UUID, string, time.Time) (bool, error) {
	return false, errors.New("store: write refused")
}

// TestDesktopSignin_AnEndWhoseRecordCannotBeWrittenIsLeftToEndAgain: an end whose display closed and whose agent
// was continued, but whose row could not be closed, answers 502 desktop_signin_end_failed and gives the row back open,
// unmarked and unclaimed, so the person can end it again at once; no close row is written, and the agent got its one
// SIGCONT.
// Its message says the end could not be recorded and asks for the end again.
func TestDesktopSignin_AnEndWhoseRecordCannotBeWrittenIsLeftToEndAgain(t *testing.T) {
	f := newSigninFixture(t)
	f.begin(t)
	before := len(f.rn.seen())
	f.srv.cfg.Store = signinEndUnwrittenStore{f.st}
	code, body := f.post(t, "/desktop-signin/end", "")
	if code != http.StatusBadGateway || !strings.Contains(body, reasonDesktopSigninEndFailed) {
		t.Fatalf("end = %d %s, want 502 %s", code, body, reasonDesktopSigninEndFailed)
	}
	if want := "the sign-in display closed, but its end could not be recorded; end the sign-in again"; !strings.Contains(body, want) {
		t.Fatalf("end = %d %s, want the message %q", code, body, want)
	}
	if si := f.st.only(t); si.EndedAt != nil || si.EndRequested != "" || si.EndStage != "" {
		t.Fatalf("row = %+v, want open, unmarked and unclaimed", si)
	}
	if got := signinCloseRows(f); len(got) != 0 {
		t.Fatalf("desktop_signin.close rows = %v, want none", got)
	}
	if n := signinConts(f, before); n != 1 {
		t.Fatalf("SIGCONTs sent by the end = %d, want 1", n)
	}
}

// TestDesktopSignin_AnEndWhoseResumeAgentFailsSaysTheAgentCouldNotBeResumed: an end whose display closed but whose
// resume-agent failed answers the resume message, leaves the row open at the resume stage with one failure row, and
// sends no SIGCONT.
func TestDesktopSignin_AnEndWhoseResumeAgentFailsSaysTheAgentCouldNotBeResumed(t *testing.T) {
	f := newSigninFixture(t)
	f.begin(t)
	before := len(f.rn.seen())
	f.rn.mu.Lock()
	f.rn.exit["resume-agent"] = 1
	f.rn.mu.Unlock()
	want := "the sign-in display closed, but the agent could not be resumed; it stays stopped while Wardyn retries, and the run ends after 3 failed tries"
	if code, body := f.post(t, "/desktop-signin/end", ""); code != http.StatusBadGateway || !strings.Contains(body, want) {
		t.Fatalf("end = %d %s, want 502 %q", code, body, want)
	}
	if si := f.st.only(t); si.EndedAt != nil || si.EndStage != signinStageResume {
		t.Fatalf("row = %+v, want open at stage %q", si, signinStageResume)
	}
	if got := signinCloseRows(f); !slices.Equal(got, []string{"failure false"}) {
		t.Fatalf("desktop_signin.close rows = %v, want [failure false]", got)
	}
	if n := signinConts(f, before); n != 0 {
		t.Fatalf("SIGCONTs sent by the end = %d, want 0: resume-agent failed before the continue", n)
	}
}

// TestDesktopSignin_AnEndBeforeTheBeginsStopLeavesTheSignInToTheBegin: an end that lands after a begin wrote its row
// but before its SIGSTOP answers 409 and sends nothing, so the stop never lands on a closed sign-in; the sign-in then
// begins as usual, and its end continues pid 1.
func TestDesktopSignin_AnEndBeforeTheBeginsStopLeavesTheSignInToTheBegin(t *testing.T) {
	f := newSigninFixture(t)
	pid1 := &signinPid1Runner{signinFakeRunner: f.rn, at: "signal:SIGSTOP"}
	f.srv.cfg.Runner = pid1
	var endCode int
	var endBody string
	var sent []string
	pid1.during = func() {
		before := len(f.rn.seen())
		endCode, endBody = f.post(t, "/desktop-signin/end", "")
		sent = f.rn.seen()[before:]
	}
	f.begin(t)
	if endCode != http.StatusConflict || !strings.Contains(endBody, reasonDesktopSigninStarting) || len(sent) != 0 {
		t.Fatalf("an end before the begin's stop = %d %s and sent %v; want 409 %s and nothing", endCode, endBody, sent, reasonDesktopSigninStarting)
	}
	if si, err := f.st.OpenDesktopSignin(context.Background(), f.run.ID); err != nil || !pid1.stopped {
		t.Fatalf("after the begin: open = %+v %v, pid 1 stopped = %t; want the sign-in open and pid 1 stopped", si, err, pid1.stopped)
	}
	if code, body := f.post(t, "/desktop-signin/end", ""); code != http.StatusNoContent || pid1.stopped {
		t.Fatalf("the end after the begin = %d %s, pid 1 stopped = %t; want 204 and pid 1 running", code, body, pid1.stopped)
	}
}

// signinHang waits for ctx or five seconds, whichever ends first: when ctx ends first it sets *bounded and answers
// ctx's error.
func signinHang(ctx context.Context, bounded *bool) error {
	select {
	case <-ctx.Done():
		*bounded = true
		return ctx.Err()
	case <-time.After(5 * time.Second):
		return errors.New("never bounded")
	}
}

// signinHungStopRunner is signinFakeRunner whose SIGSTOP never answers on its own (signinHang).
type signinHungStopRunner struct {
	*signinFakeRunner
	bounded *bool
}

func (r *signinHungStopRunner) SignalMain(ctx context.Context, ref, signal string) error {
	if signal == "SIGSTOP" {
		return signinHang(ctx, r.bounded)
	}
	return r.signinFakeRunner.SignalMain(ctx, ref, signal)
}

// signinHungRecordStore is signinTestStore whose pause-record write never answers on its own (signinHang).
type signinHungRecordStore struct {
	*signinTestStore
	bounded *bool
}

func (s signinHungRecordStore) SetDesktopSigninPauseRecord(ctx context.Context, _ uuid.UUID, _ json.RawMessage) error {
	return signinHang(ctx, s.bounded)
}

// signinHungStartRunner is signinFakeRunner whose display start never answers on its own (signinHang).
type signinHungStartRunner struct {
	*signinFakeRunner
	bounded *bool
}

func (r *signinHungStartRunner) ExecStreamAs(ctx context.Context, ref, user string, spec runner.ExecSpec) (*runner.ExecSession, error) {
	if slices.Equal(spec.Argv, []string{signinBinary, "start"}) {
		return nil, signinHang(ctx, r.bounded)
	}
	return r.signinFakeRunner.ExecStreamAs(ctx, ref, user, spec)
}

// TestDesktopSignin_ABeginsStepsBeforeItsRecordEndWithinItsHold: a SIGSTOP, a display start or a pause-record write
// that never answers ends when signinBeginHold passes, so no begin outlives the wait an end gives a row with no pause
// record; the begin then undoes its stop and closes the row. The hold leaves the steps before the one that hangs
// time to run on a loaded machine.
func TestDesktopSignin_ABeginsStepsBeforeItsRecordEndWithinItsHold(t *testing.T) {
	defer func(d time.Duration) { signinBeginHold = d }(signinBeginHold)
	signinBeginHold = 500 * time.Millisecond
	for name, tc := range map[string]struct {
		hang   func(f *signinFixture, bounded *bool)
		code   int
		reason string
	}{
		"the SIGSTOP never answers": {func(f *signinFixture, bounded *bool) {
			f.srv.cfg.Runner = &signinHungStopRunner{signinFakeRunner: f.rn, bounded: bounded}
		}, http.StatusBadGateway, reasonDesktopSigninPauseFailed},
		"the display's start never answers": {func(f *signinFixture, bounded *bool) {
			f.srv.cfg.Runner = &signinHungStartRunner{signinFakeRunner: f.rn, bounded: bounded}
		}, http.StatusBadGateway, reasonDesktopSigninStartFailed},
		"the record's write never answers": {func(f *signinFixture, bounded *bool) {
			f.srv.cfg.Store = signinHungRecordStore{signinTestStore: f.st, bounded: bounded}
		}, http.StatusServiceUnavailable, reasonDesktopSigninUnavailable},
	} {
		t.Run(name, func(t *testing.T) {
			f := newSigninFixture(t)
			var bounded bool
			tc.hang(f, &bounded)
			if code, body := f.post(t, "/desktop-signin", ""); code != tc.code || !strings.Contains(body, tc.reason) {
				t.Fatalf("begin = %d %s, want %d %s", code, body, tc.code, tc.reason)
			}
			if !bounded {
				t.Fatal("the step was not bounded by signinBeginHold")
			}
			if _, err := f.st.OpenDesktopSignin(context.Background(), f.run.ID); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("open = %v, want no sign-in open", err)
			}
		})
	}
}

// signinStartHookRunner is signinPid1Runner whose display start runs during once, just before it starts.
type signinStartHookRunner struct {
	*signinPid1Runner
	during func()
}

func (r *signinStartHookRunner) ExecStreamAs(ctx context.Context, ref, user string, spec runner.ExecSpec) (*runner.ExecSession, error) {
	if during := r.during; during != nil && slices.Equal(spec.Argv, []string{signinBinary, "start"}) {
		r.during = nil
		during()
	}
	return r.signinPid1Runner.ExecStreamAs(ctx, ref, user, spec)
}

// TestDesktopSignin_AnEndBeforeTheDisplayStartsLeavesTheSignInToTheBegin: an end that lands after a begin stopped the
// agent but before its display started answers 409 and sends nothing, so the display never starts on a closed sign-in
// with the agent running again; the sign-in then begins as usual, and its end closes the display and continues pid 1.
func TestDesktopSignin_AnEndBeforeTheDisplayStartsLeavesTheSignInToTheBegin(t *testing.T) {
	f := newSigninFixture(t)
	pid1 := &signinPid1Runner{signinFakeRunner: f.rn}
	hook := &signinStartHookRunner{signinPid1Runner: pid1}
	f.srv.cfg.Runner = hook
	var endCode int
	var endBody string
	var sent []string
	hook.during = func() {
		before := len(f.rn.seen())
		endCode, endBody = f.post(t, "/desktop-signin/end", "")
		sent = f.rn.seen()[before:]
	}
	f.begin(t)
	if hook.during != nil {
		t.Fatal("the begin never started the display")
	}
	if endCode != http.StatusConflict || !strings.Contains(endBody, reasonDesktopSigninStarting) || len(sent) != 0 {
		t.Fatalf("an end before the display started = %d %s and sent %v; want 409 %s and nothing", endCode, endBody, sent, reasonDesktopSigninStarting)
	}
	if si, err := f.st.OpenDesktopSignin(context.Background(), f.run.ID); err != nil || len(si.PauseRecord) == 0 || !pid1.stopped {
		t.Fatalf("after the begin: open = %+v %v, pid 1 stopped = %t; want the sign-in open with its pause record and pid 1 stopped", si, err, pid1.stopped)
	}
	before := len(f.rn.seen())
	if code, body := f.post(t, "/desktop-signin/end", ""); code != http.StatusNoContent || pid1.stopped {
		t.Fatalf("the end after the begin = %d %s, pid 1 stopped = %t; want 204 and pid 1 running", code, body, pid1.stopped)
	}
	if got := f.rn.seen()[before:]; len(got) == 0 || got[0] != "person:stop" {
		t.Fatalf("the end's calls = %v; want the display closed first", got)
	}
}

// signinRecordWatchStore is signinTestStore whose pause-record write fails on a finished context, as PostgreSQL's
// does, and notes the runner's calls at the time of the write.
type signinRecordWatchStore struct {
	*signinTestStore
	rn *signinFakeRunner
	at *[]string
}

func (s signinRecordWatchStore) SetDesktopSigninPauseRecord(ctx context.Context, id uuid.UUID, rec json.RawMessage) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	*s.at = s.rn.seen()
	return s.signinTestStore.SetDesktopSigninPauseRecord(ctx, id, rec)
}

// signinStartEndsRequestRunner is signinFakeRunner whose display start ends the begin's request and fails.
type signinStartEndsRequestRunner struct {
	*signinFakeRunner
	cancel context.CancelFunc
}

func (r *signinStartEndsRequestRunner) ExecStreamAs(ctx context.Context, ref, user string, spec runner.ExecSpec) (*runner.ExecSession, error) {
	if slices.Equal(spec.Argv, []string{signinBinary, "start"}) {
		r.cancel()
		return nil, context.Canceled
	}
	return r.signinFakeRunner.ExecStreamAs(ctx, ref, user, spec)
}

// TestDesktopSignin_AFailedStartKeepsThePauseRecordForTheSweep: a begin whose display did not start stores the pause
// record before it rolls back, on a context its own bound or request cannot end, so a rollback that cannot stop the
// display leaves the row with the record, and the sweep's resume-agent gets exactly that record.
func TestDesktopSignin_AFailedStartKeepsThePauseRecordForTheSweep(t *testing.T) {
	const rec = `{"already_stopped":[7]}`
	for name, failStart := range map[string]func(f *signinFixture, cancel context.CancelFunc){
		"the start exits non-zero": func(f *signinFixture, _ context.CancelFunc) {
			f.rn.exit["start"] = 1
		},
		"the request ends during the start": func(f *signinFixture, cancel context.CancelFunc) {
			f.srv.cfg.Runner = &signinStartEndsRequestRunner{signinFakeRunner: f.rn, cancel: cancel}
		},
	} {
		t.Run(name, func(t *testing.T) {
			f := newSigninFixture(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var atWrite []string
			f.srv.cfg.Store = signinRecordWatchStore{signinTestStore: f.st, rn: f.rn, at: &atWrite}
			f.rn.exit["stop"] = 1
			failStart(f, cancel)
			w := doSSOCtx(t, f.srv, ctx, http.MethodPost, "/api/v1/runs/"+f.run.ID.String()+"/desktop-signin", f.cookie, "")
			if w.Code != http.StatusBadGateway || !strings.Contains(w.Body.String(), reasonDesktopSigninStartFailed) {
				t.Fatalf("begin = %d %s, want 502 %s", w.Code, w.Body.String(), reasonDesktopSigninStartFailed)
			}
			if si := f.st.only(t); si.EndedAt != nil || string(si.PauseRecord) != rec {
				t.Fatalf("row = ended %v, record %q, want open with the record %s", si.EndedAt, si.PauseRecord, rec)
			}
			if len(atWrite) == 0 || slices.Contains(atWrite, "person:stop") {
				t.Fatalf("calls when the record was written = %v, want some, and no stop of the display yet", atWrite)
			}

			f.rn.exit["stop"] = 0
			f.srv.cfg.Now = func() time.Time { return time.Now().Add(time.Second) }
			if err := f.srv.sweepDesktopSignins(context.Background()); err != nil {
				t.Fatal(err)
			}
			if got := string(f.rn.input("agent:resume-agent")); got != rec {
				t.Fatalf("resume-agent stdin = %q, want the stored record %s", got, rec)
			}
			if si := f.st.only(t); si.EndedAt == nil {
				t.Fatalf("row = %+v, want the sweep to have closed it", si)
			}
		})
	}
}

// TestDesktopSignin_TheStatedStartingBoundIsTheBeginHoldTwice: the 409 text for an end before the begin's stop and
// docs/sdk.md state six minutes, so the bound they state must stay twice signinBeginHold.
func TestDesktopSignin_TheStatedStartingBoundIsTheBeginHoldTwice(t *testing.T) {
	if 2*signinBeginHold != 6*time.Minute {
		t.Fatal("the 409 texts and docs/sdk.md state 6 minutes; change them with the bound")
	}
}

// TestDesktopSignin_AnEndOfASignInWithNoPauseRecordWaitsOnlyTwiceTheBeginHold: an end of a sign-in whose begin never
// recorded what it stopped is refused only until twice signinBeginHold after the sign-in began; from then on it runs,
// so a begin that died after writing its row never keeps the sign-in from being ended for its whole window.
// Values: the time half of the wait is false at the wait's end exactly, true a minute before it, and the window's
// 30 minutes is far past it; the record half is empty in both rows.
func TestDesktopSignin_AnEndOfASignInWithNoPauseRecordWaitsOnlyTwiceTheBeginHold(t *testing.T) {
	for name, tc := range map[string]struct {
		age   time.Duration // how long ago the sign-in began
		code  int
		calls []string
	}{
		"inside the wait": {age: 2*signinBeginHold - time.Minute, code: http.StatusConflict},
		"the wait passed": {age: 2 * signinBeginHold, code: http.StatusNoContent, calls: []string{"person:stop", "agent:resume-agent", "signal:SIGCONT"}},
	} {
		t.Run(name, func(t *testing.T) {
			f := newSigninFixture(t)
			began := f.srv.cfg.Now().Add(-tc.age)
			si := types.DesktopSignin{ID: uuid.New(), RunID: f.run.ID, Holder: signinOwner, BeganAt: began, ExpiresAt: began.Add(desktopSigninWindow)}
			if err := f.st.BeginDesktopSignin(context.Background(), si); err != nil {
				t.Fatal(err)
			}
			code, body := f.post(t, "/desktop-signin/end", "")
			if code != tc.code || code == http.StatusConflict && !strings.Contains(body, reasonDesktopSigninStarting) {
				t.Fatalf("end = %d %s, want %d", code, body, tc.code)
			}
			if got := f.rn.seen(); !slices.Equal(got, tc.calls) {
				t.Fatalf("calls = %v, want %v", got, tc.calls)
			}
		})
	}
}

// signinClaimUnwrittenStore is signinTestStore whose ClaimDesktopSigninEnd fails, as a database that cannot take the claim.
type signinClaimUnwrittenStore struct{ *signinTestStore }

func (s signinClaimUnwrittenStore) ClaimDesktopSigninEnd(context.Context, uuid.UUID, string, string) (bool, error) {
	return false, errors.New("store: claim refused")
}

// TestDesktopSignin_AnEndWhoseClaimCannotBeTakenSendsNothing: an end whose claim the store cannot write answers 503
// desktop_signin_unavailable and sends nothing, so a late stop or SIGCONT cannot undo a newer sign-in that another
// end's claim let begin. The person's next end, or the sweep's, claims and ends it.
func TestDesktopSignin_AnEndWhoseClaimCannotBeTakenSendsNothing(t *testing.T) {
	f := newSigninFixture(t)
	f.begin(t)
	before := len(f.rn.seen())
	f.srv.cfg.Store = signinClaimUnwrittenStore{f.st}
	code, body := f.post(t, "/desktop-signin/end", "")
	if code != http.StatusServiceUnavailable || !strings.Contains(body, reasonDesktopSigninUnavailable) {
		t.Fatalf("end = %d %s, want 503 %s", code, body, reasonDesktopSigninUnavailable)
	}
	if got := f.rn.seen()[before:]; len(got) != 0 {
		t.Fatalf("calls = %v, want none", got)
	}
}

// TestDesktopSignin_AnAbandonWhoseClaimCannotBeTakenStillContinuesTheAgent: a begin's abandon whose claim the store
// cannot write runs unclaimed and sends a bare SIGCONT alone, so a store fault does not leave the agent stopped.
func TestDesktopSignin_AnAbandonWhoseClaimCannotBeTakenStillContinuesTheAgent(t *testing.T) {
	f := newSigninFixture(t)
	f.begin(t)
	si := f.st.only(t)
	before := len(f.rn.seen())
	f.srv.cfg.Store = signinClaimUnwrittenStore{f.st}
	f.srv.abandonDesktopSignin(context.Background(), f.rn, f.run, si, signinOwner)
	if got := f.rn.seen()[before:]; !slices.Equal(got, []string{"signal:SIGCONT"}) {
		t.Fatalf("calls = %v, want signal:SIGCONT alone", got)
	}
}
