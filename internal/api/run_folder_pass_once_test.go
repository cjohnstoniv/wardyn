// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// passSeen reports whether a pass back reached the sandbox's sftp-server.
func passSeen(sb *folderSandboxRunner) *atomic.Bool {
	var seen atomic.Bool
	sb.onPass = func() { seen.Store(true) }
	return &seen
}

// A pass back moves the record to a generation of its own and, answered by the runner, marks it settled.
func TestPassBack_ACompletedPassIsSettledAtItsOwnGeneration(t *testing.T) {
	s, cs, _, run, sb, _ := computerFolderFixture(t, types.ComputerFolderBoth)
	if !s.holdForLocalFolder(context.Background(), run, "sbx") {
		t.Fatal(cs.FailureHint())
	}
	agentWrites(t, sb, "a.txt")
	if s.bringComputerFolderBack(context.Background(), run.ID, "sbx") {
		t.Error("a completed pass kept the sandbox")
	}
	// The seed owns gen 1, the pass back gen 2.
	if st := cs.folderStatus(); st.Back == nil || st.Back.Error != "" || st.Gen != 2 || st.Settled != 2 {
		t.Errorf("record %+v, want the completed pass at gen 2, settled", st)
	}
}

// passFixture is computerFolderFixture seeded, on a clock the test moves, with the record then set to gen
// and settled, sent now, with resume, and the folder's runner set by runnerAs.
type passFixture struct {
	s     *Server
	cs    *computerFolderStore
	audit *recRecorder
	run   types.AgentRun
	sb    *folderSandboxRunner
	home  string
	now   atomic.Int64
}

func newPassFixture(t *testing.T, gen, settled int, resume *types.FolderResume) *passFixture {
	t.Helper()
	s, cs, audit, run, sb, app := computerFolderFixture(t, types.ComputerFolderBoth)
	if !s.holdForLocalFolder(context.Background(), run, "sbx") {
		t.Fatal(cs.FailureHint())
	}
	f := &passFixture{s: s, cs: cs, audit: audit, run: run, sb: sb, home: filepath.Dir(filepath.Dir(app))}
	f.now.Store(time.Now().UnixNano())
	s.cfg.Now = func() time.Time { return time.Unix(0, f.now.Load()) }
	f.set(func(st *types.RunLocalFolderStatus) {
		st.Gen, st.Settled, st.GenAt, st.Resume = gen, settled, s.cfg.Now(), resume
	})
	agentWrites(t, sb, "a.txt")
	return f
}

// set changes the stored record.
func (f *passFixture) set(change func(*types.RunLocalFolderStatus)) {
	f.cs.smu.Lock()
	defer f.cs.smu.Unlock()
	change(&f.cs.status)
}

// advance moves the fixture's clock on by d.
func (f *passFixture) advance(d time.Duration) { f.now.Add(int64(d)) }

// back is one visit's pass back, and whether it kept the sandbox.
func (f *passFixture) back() bool {
	return f.s.bringComputerFolderBack(context.Background(), f.run.ID, "sbx")
}

// runnerAs connects the folder's runner advertising caps, or with caps nil takes it offline. It answers
// the server's side of the new connection.
func (f *passFixture) runnerAs(t *testing.T, caps *runnerwire.Caps) *runnerwire.Peer {
	t.Helper()
	id := f.cs.runner.ID
	var ep runnerEndpoint
	var peer *runnerwire.Peer
	if caps != nil {
		ep, peer = folderRunnerEndpointPeer(t, id, *caps, f.home)
	}
	f.s.runnerStreams.subsMu.Lock()
	defer f.s.runnerStreams.subsMu.Unlock()
	if caps == nil {
		delete(f.s.runnerStreams.subs, id)
		return nil
	}
	f.s.runnerStreams.subs[id] = ep
	return peer
}

var (
	olderRunnerCaps  = runnerwire.Caps{Folders: true, MachineName: "alice-laptop"}
	onePerRunnerCaps = runnerwire.Caps{Folders: true, FolderPassOnce: true, MachineName: "alice-laptop"}
)

// An older runner's unanswered pass keeps the sandbox, with nothing sent or written, until oldPassBound has
// passed since it was sent; then the pass is sent. One that was a stall in a row of them gives up there
// instead, and the sandbox is stopped with the changes not brought back.
func TestPassBack_AnOlderRunnersUnansweredPassKeepsTheSandboxUntilItsBound(t *testing.T) {
	f := newPassFixture(t, 2, 1, &types.FolderResume{Stalled: 1, Unsynced: 1})
	f.runnerAs(t, &olderRunnerCaps)
	seen := passSeen(f.sb)
	before := f.cs.folderStatus()
	if !f.back() || seen.Load() || f.sb.stopCount() != 0 {
		t.Fatalf("pass %v, stops %d; want no pass and the sandbox kept", seen.Load(), f.sb.stopCount())
	}
	if st := f.cs.folderStatus(); !reflect.DeepEqual(st, before) {
		t.Errorf("record %+v, want it unchanged from %+v", st, before)
	}
	f.advance(oldPassBound)
	f.back()
	if st := f.cs.folderStatus(); !seen.Load() || st.Gen != 3 || st.Settled != 3 {
		t.Errorf("pass %v, record %+v; want the pass sent past the bound, at gen 3, settled", seen.Load(), st)
	}

	f = newPassFixture(t, 2, 1, &types.FolderResume{Stalled: computerFolderStalls, Unsynced: 1})
	f.runnerAs(t, &olderRunnerCaps)
	seen = passSeen(f.sb)
	f.advance(oldPassBound)
	if err := f.s.finalizeRunEffects(context.Background(), f.run.ID, "sbx", "run.complete", nil, false, false); err != nil {
		t.Errorf("finalize: %v", err)
	}
	if seen.Load() {
		t.Error("a pass was sent at the give-up")
	}
	assertFolderBackTornDown(t, f.cs, f.audit, f.run, f.sb, "the runner did not answer for its last pass back; 2 passes in a row brought nothing back")
	assertFolderBackTornDown(t, f.cs, f.audit, f.run, f.sb, "changed files not brought back: 1")
}

// While the last pass is unanswered, a runner not known to run one pass per run at a time gets none,
// whatever it is at the visit: offline, then back as an older runner. Neither visit writes anything. A
// runner that says it runs one per run gets the pass.
func TestPassBack_AnUnsettledPassGoesOnlyToARunnerThatRunsOnePerRun(t *testing.T) {
	f := newPassFixture(t, 2, 1, &types.FolderResume{Stalled: 1, Unsynced: 1})
	seen := passSeen(f.sb)
	before := f.cs.folderStatus()
	f.runnerAs(t, nil)
	if !f.back() || seen.Load() {
		t.Fatalf("offline: pass %v; want no pass and the sandbox kept", seen.Load())
	}
	f.runnerAs(t, &olderRunnerCaps)
	if !f.back() || seen.Load() {
		t.Fatalf("older runner back: pass %v; want no pass and the sandbox kept", seen.Load())
	}
	if st := f.cs.folderStatus(); !reflect.DeepEqual(st, before) {
		t.Errorf("record %+v, want it unchanged from %+v", st, before)
	}
	f.runnerAs(t, &onePerRunnerCaps)
	f.back()
	if st := f.cs.folderStatus(); !seen.Load() || st.Gen != 3 || st.Settled != 3 || st.Back == nil || st.Back.Error != "" {
		t.Errorf("pass %v, record %+v; want the pass at gen 3, completed and settled", seen.Load(), st)
	}
}

// An unanswered pass whose record has no send time is not past its bound: the first visit stamps it and
// keeps the sandbox, and the give-up comes only once oldPassBound has passed since that visit.
func TestPassBack_AnUnansweredPassWithNoSendTimeIsBoundFromItsFirstVisit(t *testing.T) {
	f := newPassFixture(t, 2, 1, &types.FolderResume{Stalled: computerFolderStalls, Unsynced: 1})
	f.set(func(st *types.RunLocalFolderStatus) { st.GenAt = time.Time{} })
	f.runnerAs(t, &olderRunnerCaps)
	seen := passSeen(f.sb)
	stamped := f.s.cfg.Now()
	if !f.back() || seen.Load() || f.sb.stopCount() != 0 {
		t.Fatalf("pass %v, stops %d; want no pass and the sandbox kept", seen.Load(), f.sb.stopCount())
	}
	if st := f.cs.folderStatus(); !st.GenAt.Equal(stamped) || st.Gen != 2 {
		t.Errorf("record %+v, want gen 2 stamped %v", st, stamped)
	}
	f.advance(oldPassBound - time.Second)
	if !f.back() || seen.Load() || f.sb.stopCount() != 0 {
		t.Fatalf("inside the bound: pass %v, stops %d; want no pass and the sandbox kept", seen.Load(), f.sb.stopCount())
	}
	f.advance(time.Second)
	if err := f.s.finalizeRunEffects(context.Background(), f.run.ID, "sbx", "run.complete", nil, false, false); err != nil {
		t.Errorf("finalize: %v", err)
	}
	assertFolderBackTornDown(t, f.cs, f.audit, f.run, f.sb, "the runner did not answer for its last pass back")
}

// rc17Record makes the fixture's record one an earlier release wrote: its seed, and no generation, settled
// count or send time.
func (f *passFixture) rc17Record() {
	f.set(func(st *types.RunLocalFolderStatus) { st.Gen, st.Settled, st.GenAt = 0, 0, time.Time{} })
}

// A record an earlier release wrote, whose pass back that release may still be running, gets no pass from an
// older runner until oldPassBound has passed since the first visit that finds it; a runner that says it runs
// one pass per run at a time gets the pass at once.
func TestPassBack_AnEarlierReleasesRecordWaitsOnAnOlderRunner(t *testing.T) {
	f := newPassFixture(t, 0, 0, &types.FolderResume{Stalled: 1, Unsynced: 1})
	f.rc17Record()
	f.runnerAs(t, &olderRunnerCaps)
	seen := passSeen(f.sb)
	if !f.back() || seen.Load() {
		t.Fatalf("pass %v; want no pass and the sandbox kept", seen.Load())
	}
	if st := f.cs.folderStatus(); st.Gen != 0 || st.GenAt.IsZero() || st.Seed == nil {
		t.Errorf("record %+v, want it still the earlier release's, stamped now", st)
	}
	f.advance(oldPassBound)
	f.back()
	if st := f.cs.folderStatus(); !seen.Load() || st.Gen != 1 || st.Settled != 1 {
		t.Errorf("pass %v, record %+v; want the pass sent past the bound, at gen 1, settled", seen.Load(), st)
	}

	f = newPassFixture(t, 0, 0, &types.FolderResume{Stalled: 1, Unsynced: 1})
	f.rc17Record()
	f.runnerAs(t, &onePerRunnerCaps)
	seen = passSeen(f.sb)
	f.back()
	if st := f.cs.folderStatus(); !seen.Load() || st.Gen != 1 || st.Settled != 1 {
		t.Errorf("pass %v, record %+v; want the pass at once, at gen 1, settled", seen.Load(), st)
	}
}

// Found open at a run's first visit, before any keep, the sandbox has its agent and egress stopped first and
// is kept with a resume, with no pass sent and no generation moved.
func TestPassBack_AnOpenPassAtTheFirstVisitStopsTheAgentAndKeepsTheSandbox(t *testing.T) {
	f := newPassFixture(t, 0, 0, nil)
	f.rc17Record()
	f.runnerAs(t, &olderRunnerCaps)
	seen := passSeen(f.sb)
	if !f.back() || seen.Load() || f.sb.stopCount() != 0 {
		t.Fatalf("pass %v, stops %d; want no pass and the sandbox kept", seen.Load(), f.sb.stopCount())
	}
	if !f.sb.proxyStopped.Load() || !f.sb.agentKilled.Load() {
		t.Errorf("proxy stopped %v, agent killed %v; want both before the keep", f.sb.proxyStopped.Load(), f.sb.agentKilled.Load())
	}
	if st := f.cs.folderStatus(); st.Resume == nil || st.Gen != 0 || st.GenAt.IsZero() {
		t.Errorf("record %+v, want a resume at gen 0, stamped", st)
	}
}

// Found open at a run's first visit, a sandbox whose agent and egress cannot be stopped is not kept running:
// it is stopped with no pass sent, and the record says its changes were not brought back.
func TestPassBack_AnOpenPassWhoseSandboxCannotBeQuiescedStopsItNotBroughtBack(t *testing.T) {
	f := newPassFixture(t, 0, 0, nil)
	f.rc17Record()
	f.runnerAs(t, &olderRunnerCaps)
	f.sb.proxyErr = errors.New("the proxy is wedged")
	seen := passSeen(f.sb)
	if err := f.s.finalizeRunEffects(context.Background(), f.run.ID, "sbx", "run.complete", nil, false, false); err != nil {
		t.Errorf("finalize: %v", err)
	}
	if seen.Load() {
		t.Error("a pass was sent with the agent and egress not stopped")
	}
	assertFolderBackTornDown(t, f.cs, f.audit, f.run, f.sb, "no pass ran; the runner may still be running its last pass back")
	assertFolderBackTornDown(t, f.cs, f.audit, f.run, f.sb, "its changes not brought back yet, uncounted")
}

// Found open at a resume visit, the sandbox was kept with its agent and egress stopped already: it stays
// kept, and is not quiesced again.
func TestPassBack_AnOpenPassAtAResumeVisitKeepsTheSandboxAsItIs(t *testing.T) {
	f := newPassFixture(t, 2, 1, &types.FolderResume{Stalled: 1, Unsynced: 1})
	f.runnerAs(t, &olderRunnerCaps)
	if !f.back() || f.sb.stopCount() != 0 {
		t.Fatalf("stops %d; want the sandbox kept", f.sb.stopCount())
	}
	if f.sb.proxyStopped.Load() || f.sb.agentKilled.Load() {
		t.Error("a resume visit quiesced the kept sandbox again")
	}
}

// dropOnPass has the next pass's runner connection drop as the pass starts, so the runner never answers for
// it.
func (f *passFixture) dropOnPass(peer *runnerwire.Peer) {
	var dropped atomic.Bool
	f.sb.onPass = func() {
		if !dropped.Swap(true) {
			peer.Close()
		}
	}
}

// keptUnanswered runs one finalize whose pass the runner never answers for, and checks it kept the sandbox
// with stalled stalls, its generation unsettled.
func (f *passFixture) keptUnanswered(t *testing.T, stalled int) {
	t.Helper()
	_, err := boundedFinalize(t, f.s, f.run, time.Second)
	if !errors.Is(err, errComputerFolderResumes) || f.sb.stopCount() != 0 {
		t.Fatalf("finalize = %v, stops %d; want the sandbox kept", err, f.sb.stopCount())
	}
	st := f.cs.folderStatus()
	if st.Gen == st.Settled || st.Resume == nil || st.Resume.Stalled != stalled || st.Back == nil || !strings.Contains(st.Back.Error, "did not answer for this pass") {
		t.Fatalf("record %+v, want an unsettled generation kept with %d stalls, the unanswered pass named", st, stalled)
	}
}

// A pass the runner never answered for (its connection dropped as the pass started) does not stop the
// sandbox at its visit: it is kept, one stall. The next visit inside the bound sends nothing; once the runner
// says it runs one pass per run at a time, the next visit sends the pass, and its completion tears down.
func TestPassBack_AnUnansweredPassKeepsTheSandboxUntilTheRunnerIsUpdated(t *testing.T) {
	f := newPassFixture(t, 1, 1, nil)
	f.dropOnPass(f.runnerAs(t, &olderRunnerCaps))
	f.keptUnanswered(t, 1)
	seen := passSeen(f.sb)
	f.runnerAs(t, &olderRunnerCaps)
	if !f.back() || seen.Load() {
		t.Fatalf("inside the bound: pass %v; want no pass and the sandbox kept", seen.Load())
	}
	f.runnerAs(t, &onePerRunnerCaps)
	if f.back() || !seen.Load() {
		t.Fatalf("updated runner: pass %v; want the pass sent and the sandbox left to its stop", seen.Load())
	}
	if st := f.cs.folderStatus(); st.Gen != 3 || st.Settled != 3 || st.Back == nil || st.Back.Error != "" {
		t.Errorf("record %+v, want the completed pass at gen 3, settled", st)
	}
}

// With the runner still an older one, an unanswered pass gets its retry past the bound; only a second
// attempt in a row that brings nothing back, unanswered too, earns the give-up, past its own bound.
func TestPassBack_AnOlderRunnersUnansweredPassIsRetriedOnceBeforeItGivesUp(t *testing.T) {
	f := newPassFixture(t, 1, 1, nil)
	f.dropOnPass(f.runnerAs(t, &olderRunnerCaps))
	f.keptUnanswered(t, 1)
	f.advance(oldPassBound)
	seen := passSeen(f.sb)
	peer := f.runnerAs(t, &olderRunnerCaps)
	var dropped atomic.Bool
	f.sb.onPass = func() {
		seen.Store(true)
		if !dropped.Swap(true) {
			peer.Close()
		}
	}
	f.keptUnanswered(t, computerFolderStalls)
	if !seen.Load() {
		t.Fatal("past the bound, the retry was not sent")
	}
	f.advance(oldPassBound)
	if err := f.s.finalizeRunEffects(context.Background(), f.run.ID, "sbx", "run.complete", nil, false, false); err != nil {
		t.Errorf("finalize: %v", err)
	}
	assertFolderBackTornDown(t, f.cs, f.audit, f.run, f.sb, "the runner did not answer for its last pass back; 2 passes in a row brought nothing back")
}

// runnerTurnsAfterBump is computerFolderStore whose folder runner turns into another, once, at the first
// runner read after the pass record moved to gen: between a pass back's check and its send. It records the
// sandbox refs the sweeps clear.
type runnerTurnsAfterBump struct {
	*computerFolderStore
	gen     int
	turn    func()
	once    sync.Once
	cleared atomic.Int32
}

func (s *runnerTurnsAfterBump) GetRunner(ctx context.Context, id uuid.UUID) (types.Runner, error) {
	if s.folderStatus().Gen == s.gen {
		s.once.Do(s.turn)
	}
	return s.computerFolderStore.GetRunner(ctx, id)
}

func (s *runnerTurnsAfterBump) SetSandboxRef(ctx context.Context, id uuid.UUID, ref string) error {
	if ref == "" {
		s.cleared.Add(1)
	}
	return s.computerFolderStore.SetSandboxRef(ctx, id, ref)
}

// A first visit whose runner turns into an older one between the check and the send keeps the sandbox with
// a resume written, so neither the resume sweep nor the orphan sweep stops it or clears its ref while the
// pass is still owed; the first pass the runner answers then tears it down.
func TestPassBack_ARunnerThatTurnsOlderBeforeTheSendKeepsTheSandboxForTheSweeps(t *testing.T) {
	f := newPassFixture(t, 0, 0, nil)
	f.rc17Record()
	f.runnerAs(t, &onePerRunnerCaps)
	turns := &runnerTurnsAfterBump{computerFolderStore: f.cs, gen: 1, turn: func() { f.runnerAs(t, &olderRunnerCaps) }}
	f.s.cfg.Store = turns
	seen := passSeen(f.sb)
	if !f.back() || seen.Load() || f.sb.stopCount() != 0 {
		t.Fatalf("pass %v, stops %d; want no pass and the sandbox kept", seen.Load(), f.sb.stopCount())
	}
	if st := f.cs.folderStatus(); st.Resume == nil || st.Gen != 1 || st.Settled != 0 {
		t.Fatalf("record %+v, want a resume at gen 1, unsettled", st)
	}
	ds := f.cs.localFolderStore.dispatchTestStore
	ds.mu.Lock()
	ds.state, ds.run.SandboxRef = types.RunCompleted, "sbx"
	ds.mu.Unlock()
	run, err := ds.GetRun(context.Background(), f.run.ID)
	if err != nil {
		t.Fatal(err)
	}
	f.s.reconcileOrphanedRun(context.Background(), run)
	f.s.resumeComputerFolderBack(context.Background(), run.ID)
	if seen.Load() || f.sb.stopCount() != 0 || turns.cleared.Load() != 0 {
		t.Fatalf("pass %v, stops %d, refs cleared %d; want the owed sandbox left as it is", seen.Load(), f.sb.stopCount(), turns.cleared.Load())
	}
	f.runnerAs(t, &onePerRunnerCaps)
	f.s.resumeComputerFolderBack(context.Background(), run.ID)
	if st := f.cs.folderStatus(); !seen.Load() || f.sb.stopCount() != 1 || st.Back == nil || st.Back.Error != "" {
		t.Errorf("pass %v, stops %d, record %+v; want the completed pass, then the stop", seen.Load(), f.sb.stopCount(), st)
	}
}

// onARunner makes the fixture's run one whose sandbox is on a runner, which cannot be quiesced, with an agent
// that can be stopped alone; stopFails leaves a process the stop cannot end.
func (f *passFixture) onARunner(t *testing.T, stopFails bool) *agentSandbox {
	t.Helper()
	ag := &agentSandbox{folderSandboxRunner: f.sb, t: t, pid1: taskIdlePID1, stopFails: stopFails}
	f.s.cfg.Runner = ag
	id := uuid.New()
	ds := f.cs.localFolderStore.dispatchTestStore
	ds.mu.Lock()
	ds.run.RunnerID = &id
	ds.mu.Unlock()
	return ag
}

// On a sandbox that cannot be quiesced but whose agent can be stopped alone (one on a runner), an open
// record keeps the sandbox with its agent stopped and a resume written, and sends no pass.
func TestPassBack_AnOpenPassOnARunnersSandboxStopsItsAgentAndKeepsIt(t *testing.T) {
	f := newPassFixture(t, 0, 0, nil)
	f.rc17Record()
	f.runnerAs(t, &olderRunnerCaps)
	ag := f.onARunner(t, false)
	seen := passSeen(f.sb)
	if !f.back() || seen.Load() || f.sb.stopCount() != 0 {
		t.Fatalf("pass %v, stops %d; want no pass and the sandbox kept", seen.Load(), f.sb.stopCount())
	}
	if ag.stops != 1 || !ag.agentDown {
		t.Errorf("agent stops %d, down %v; want the agent stopped once before the keep", ag.stops, ag.agentDown)
	}
	if st := f.cs.folderStatus(); st.Resume == nil || st.Gen != 0 || st.GenAt.IsZero() {
		t.Errorf("record %+v, want a resume at gen 0, stamped", st)
	}
}

// An open record on a runner's sandbox whose agent cannot be stopped is not kept running: no pass is sent,
// and the record says the changes were not brought back.
func TestPassBack_AnOpenPassOnARunnersSandboxWhoseAgentLivesStopsItNotBroughtBack(t *testing.T) {
	f := newPassFixture(t, 0, 0, nil)
	f.rc17Record()
	f.runnerAs(t, &olderRunnerCaps)
	f.onARunner(t, true)
	seen := passSeen(f.sb)
	if f.back() || seen.Load() {
		t.Fatalf("pass %v; want no pass and the sandbox left to its stop", seen.Load())
	}
	st := f.cs.folderStatus()
	for _, want := range []string{"no pass ran; the runner may still be running its last pass back", "the agent could not be stopped", "its changes not brought back yet, uncounted"} {
		if st.Back == nil || !strings.Contains(st.Back.Error, want) || st.Resume != nil {
			t.Errorf("record %+v, want no resume and the error naming %q", st, want)
		}
		if ev := findAuditData(f.audit.snapshot(), f.run.ID, "run.folder.transfer", want); ev == nil || ev.Outcome != "failure" {
			t.Errorf("no failure row naming %q", want)
		}
	}
}

// A virtual machine's sandbox cannot be kept with its agent stopped: an open record's pass is sent as before
// this release.
func TestPassBack_AnOpenPassOnAVirtualMachineIsSent(t *testing.T) {
	f := newPassFixture(t, 0, 0, nil)
	f.rc17Record()
	f.runnerAs(t, &olderRunnerCaps)
	ds := f.cs.localFolderStore.dispatchTestStore
	ds.mu.Lock()
	ds.run.MachineKind = types.MachineLinuxVM
	ds.mu.Unlock()
	seen := passSeen(f.sb)
	f.back()
	if st := f.cs.folderStatus(); !seen.Load() || st.Gen != 1 || st.Settled != 1 || st.Back == nil || st.Back.Error != "" {
		t.Errorf("pass %v, record %+v; want the pass sent and completed at gen 1", seen.Load(), st)
	}
}

// statusReadFailsOnce is computerFolderStore whose first read of the pass record fails.
type statusReadFailsOnce struct {
	*computerFolderStore
	failed atomic.Bool
}

func (s *statusReadFailsOnce) RunLocalFolderStatus(ctx context.Context, id uuid.UUID) (types.RunLocalFolderStatus, error) {
	if !s.failed.Swap(true) {
		return types.RunLocalFolderStatus{}, errors.New("the read failed")
	}
	return s.computerFolderStore.RunLocalFolderStatus(ctx, id)
}

// A pass back whose record could not be read sends nothing to an older runner, and keeps the sandbox; a
// runner that runs one pass per run at a time gets the pass, as before this release.
func TestPassBack_AnUnreadRecordSendsNoPassToAnOlderRunner(t *testing.T) {
	f := newPassFixture(t, 0, 0, nil)
	f.rc17Record()
	f.runnerAs(t, &olderRunnerCaps)
	f.s.cfg.Store = &statusReadFailsOnce{computerFolderStore: f.cs}
	seen := passSeen(f.sb)
	if !f.back() || seen.Load() || f.sb.stopCount() != 0 {
		t.Fatalf("pass %v, stops %d; want no pass and the sandbox kept", seen.Load(), f.sb.stopCount())
	}
	if st := f.cs.folderStatus(); st.Resume == nil {
		t.Errorf("record %+v, want a resume", st)
	}

	f = newPassFixture(t, 0, 0, nil)
	f.rc17Record()
	f.runnerAs(t, &onePerRunnerCaps)
	f.s.cfg.Store = &statusReadFailsOnce{computerFolderStore: f.cs}
	seen = passSeen(f.sb)
	f.back()
	if st := f.cs.folderStatus(); !seen.Load() || st.Gen != 1 || st.Settled != 1 {
		t.Errorf("pass %v, record %+v; want the pass sent at gen 1, settled", seen.Load(), st)
	}
}

// A pass back refused at the send keeps the last pass's send time, so the wait does not start again.
func TestPassBack_ARefusalAtTheSendKeepsTheLastPassesSendTime(t *testing.T) {
	f := newPassFixture(t, 2, 1, nil)
	sent := f.cs.folderStatus().GenAt
	f.runnerAs(t, &onePerRunnerCaps)
	f.s.cfg.Store = &runnerTurnsAfterBump{computerFolderStore: f.cs, gen: 3, turn: func() { f.runnerAs(t, &olderRunnerCaps) }}
	f.advance(time.Minute)
	if !f.back() {
		t.Fatal("the sandbox was not kept")
	}
	if st := f.cs.folderStatus(); st.Gen != 3 || st.Resume == nil || !st.GenAt.Equal(sent) {
		t.Errorf("record %+v, want gen 3 kept with the last pass's send time %v", st, sent)
	}
}

// A pass the runner refused as busy is settled at its generation and kept for a resume.
func TestPassBack_ABusyRunnerIsTriedAgainLater(t *testing.T) {
	s, cs, audit, run, _, _ := computerFolderFixture(t, types.ComputerFolderBoth)
	if !s.holdForLocalFolder(context.Background(), run, "sbx") {
		t.Fatal(cs.FailureHint())
	}
	// The seed owns gen 1; the pass back moves the record to gen 2.
	rec, ok := s.bumpComputerFolderGen(context.Background(), run.ID, 1)
	if !ok || rec.Gen != 2 {
		t.Fatalf("bump = %+v %v", rec, ok)
	}
	if !s.recordFolderPassBusy(context.Background(), run.ID, *cs.folder, rec.Gen) {
		t.Fatal("a busy pass was not kept")
	}
	if st := cs.folderStatus(); st.Resume == nil || st.Settled != 2 || st.Back != nil {
		t.Errorf("record %+v, want a resume kept, settled at gen 2, no back", st)
	}
	if ev := findAudit(audit.snapshot(), run.ID, "run.folder.transfer", "failure"); ev == nil || !strings.Contains(string(ev.Data), "tries again") {
		t.Errorf("row %+v, want the busy pass audited", ev)
	}
	if !errors.Is(&runnerwire.Error{Code: runnerwire.CodeFolderPassBusy}, runner.ErrFolderPassBusy) {
		t.Error("a folder_pass_busy reply does not read as runner.ErrFolderPassBusy")
	}
}

// A bump from a generation the record is not at does not land: that pass back is another owner's.
func TestPassBack_ARecordThatMovedFirstRunsNoPass(t *testing.T) {
	s, cs, _, run, _, _ := computerFolderFixture(t, types.ComputerFolderBoth)
	if !s.holdForLocalFolder(context.Background(), run, "sbx") {
		t.Fatal(cs.FailureHint())
	}
	if _, ok := s.bumpComputerFolderGen(context.Background(), run.ID, 5); ok {
		t.Error("a bump from a generation the record is not at landed")
	}
}

// A delayed owner whose generation another owner moved on writes nothing and leaves the sandbox up: owner
// A's pass is held while owner B moves the record on, passes and settles; A's record then does not land,
// and A does not let its caller stop B's sandbox.
func TestPassBack_ADelayedOwnerNeitherWritesNorStops(t *testing.T) {
	s, cs, _, run, sb, _ := computerFolderFixture(t, types.ComputerFolderBoth)
	if !s.holdForLocalFolder(context.Background(), run, "sbx") {
		t.Fatal(cs.FailureHint())
	}
	agentWrites(t, sb, "a.txt")
	var nested atomic.Bool
	var bKept atomic.Bool
	sb.onPass = func() {
		if nested.Swap(true) {
			return
		}
		bKept.Store(s.bringComputerFolderBack(context.Background(), run.ID, "sbx"))
	}
	if !s.bringComputerFolderBack(context.Background(), run.ID, "sbx") {
		t.Error("the delayed owner let its caller stop the sandbox")
	}
	if bKept.Load() {
		t.Error("owner B's completed pass kept the sandbox")
	}
	// The seed owns gen 1, owner A gen 2, owner B gen 3.
	if st := cs.folderStatus(); st.Gen != 3 || st.Settled != 3 || st.Back == nil || st.Back.Error != "" {
		t.Errorf("record %+v, want owner B's settled pass at gen 3 and nothing from A", st)
	}
}

// A delayed seed whose generation another owner moved on during its pass records, audits and publishes
// nothing: the run's first sync is left to that owner.
func TestSeed_ADelayedSeedNeitherRecordsNorPublishes(t *testing.T) {
	s, cs, audit, run, sb, _ := computerFolderFixture(t, types.ComputerFolderBoth)
	var moved atomic.Bool
	sb.onPass = func() {
		if _, ok := s.bumpComputerFolderGen(context.Background(), run.ID, 1); ok {
			moved.Store(true)
		}
	}
	s.seedComputerFolder(context.Background(), run, *cs.folder, 20*time.Second)
	if !moved.Load() {
		t.Fatal("the other owner did not move the record on during the seed's pass")
	}
	if st := cs.folderStatus(); st.Gen != 2 || st.Seed != nil {
		t.Errorf("record %+v, want gen 2 and no seed from the delayed one", st)
	}
	if _, seed, err := cs.RunLocalFolderSeed(context.Background(), run.ID); err != nil || seed.SeededAt != nil || seed.Error != "" {
		t.Errorf("first sync %+v %v, want none published", seed, err)
	}
	events := audit.snapshot()
	for _, outcome := range []string{"success", "failure"} {
		if findAudit(events, run.ID, "run.folder.transfer", outcome) != nil || findAudit(events, run.ID, "run.seed", outcome) != nil {
			t.Errorf("a %s pass or first sync was audited for the delayed seed", outcome)
		}
	}
}

// statusLandsAfterRead is computerFolderStore on which another owner's completed pass back lands once, just
// after the first read of the record.
type statusLandsAfterRead struct {
	*computerFolderStore
	once sync.Once
}

func (s *statusLandsAfterRead) RunLocalFolderStatus(ctx context.Context, id uuid.UUID) (types.RunLocalFolderStatus, error) {
	rec, err := s.computerFolderStore.RunLocalFolderStatus(ctx, id)
	s.once.Do(func() {
		s.smu.Lock()
		defer s.smu.Unlock()
		s.status.Back, s.status.Settled = &types.FolderPassRecord{At: time.Now()}, s.status.Gen
	})
	return rec, err
}

// A pass back that completed between the read of the record and the bump is seen in the bumped record:
// no second pass runs, and the sandbox is left to its stop.
func TestPassBack_ABackThatLandedBeforeTheBumpRunsNoPass(t *testing.T) {
	s, cs, _, run, sb, _ := computerFolderFixture(t, types.ComputerFolderBoth)
	if !s.holdForLocalFolder(context.Background(), run, "sbx") {
		t.Fatal(cs.FailureHint())
	}
	agentWrites(t, sb, "a.txt")
	seen := passSeen(sb)
	s.cfg.Store = &statusLandsAfterRead{computerFolderStore: cs}
	if s.bringComputerFolderBack(context.Background(), run.ID, "sbx") || seen.Load() {
		t.Fatalf("pass %v; want no pass and the sandbox left to its stop", seen.Load())
	}
	if st := cs.folderStatus(); st.Gen != 2 || st.Back == nil || st.Back.Error != "" {
		t.Errorf("record %+v, want the landed pass kept, moved to gen 2", st)
	}
}

// Only caps read from the runner that name one pass per run make it a new runner; unread caps are an
// older runner's.
func TestRunsOnePassPerRun_UnreadCapsAreAnOlderRunner(t *testing.T) {
	for _, tc := range []struct {
		caps runnerwire.Caps
		read bool
		want bool
	}{
		{runnerwire.Caps{FolderPassOnce: true}, true, true},
		{runnerwire.Caps{}, true, false},
		{runnerwire.Caps{FolderPassOnce: true}, false, false},
		{runnerwire.Caps{}, false, false},
	} {
		if got := runsOnePassPerRun(tc.caps, tc.read); got != tc.want {
			t.Errorf("runsOnePassPerRun(%+v, read %v) = %v, want %v", tc.caps, tc.read, got, tc.want)
		}
	}
}

// holdsAPass has the folder's runner run a pass for the run that nothing finishes: its stream is never
// read, so the runner holds the run's slot, and a pass sent next is refused as busy (folder_pass_busy).
func (f *passFixture) holdsAPass(t *testing.T) {
	t.Helper()
	sub := f.s.runnerStreams.subs[*f.cs.folder.RunnerID].sub
	pass, err := sub.SyncFolder(context.Background(), runnerwire.SyncFolderArgs{
		RunID: f.run.ID, Path: f.cs.folder.Path, RemoteDir: f.cs.folder.RemoteDir, Pull: true,
	})
	if err != nil {
		t.Fatalf("the runner would not start the pass that is to stay open: %v", err)
	}
	t.Cleanup(func() { _ = pass.Stream.Close() })
}

// finalizeWithin is one finalize whose caller allows limit, as a runner exit (30 s) or a lease end (60 s)
// does, so the pass back's budget is the caller's deadline less the teardown's: 15 s and 45 s.
func (f *passFixture) finalizeWithin(limit time.Duration) error {
	ctx, cancel := context.WithTimeout(context.Background(), limit)
	defer cancel()
	return f.s.finalizeRunEffects(ctx, f.run.ID, "sbx", "run.complete", nil, false, false)
}

// backRows counts the audit rows of passes back.
func (f *passFixture) backRows() int {
	n := 0
	for _, ev := range f.audit.snapshot() {
		if ev.RunID != nil && *ev.RunID == f.run.ID && ev.Action == "run.folder.transfer" && strings.Contains(string(ev.Data), `"direction":"back"`) {
			n++
		}
	}
	return n
}

// A pass back the runner refuses as busy, because it still runs the run's last, keeps a sandbox that can be
// held (a runner's, whose agent can be stopped alone): its agent is stopped alone, the record keeps a resume
// with no stall counted, and neither the sandbox is stopped nor its ref cleared.
func TestPassBack_ABusyHoldableSandboxIsKeptWithAResumeAndNoStall(t *testing.T) {
	f := newPassFixture(t, 1, 1, nil)
	ag := f.onARunner(t, false)
	f.s.cfg.Store = refStore{f.cs}
	ds := f.cs.localFolderStore.dispatchTestStore
	ds.mu.Lock()
	ds.run.SandboxRef = "sbx"
	ds.mu.Unlock()
	f.holdsAPass(t)
	if err := f.finalizeWithin(30 * time.Second); !errors.Is(err, errComputerFolderResumes) {
		t.Fatalf("finalize = %v, want the kept sandbox's errComputerFolderResumes", err)
	}
	if n := f.sb.stopCount(); n != 0 {
		t.Errorf("StopSandbox succeeded %d times at a busy visit on a holdable sandbox, want 0", n)
	}
	ds.mu.Lock()
	ref := ds.run.SandboxRef
	ds.mu.Unlock()
	if ref != "sbx" {
		t.Errorf("sandbox ref = %q, want it kept", ref)
	}
	if ag.stops != 1 || !ag.agentDown {
		t.Errorf("agent stops %d, down %v; want the agent stopped alone once", ag.stops, ag.agentDown)
	}
	if st := f.cs.folderStatus(); st.Resume == nil || st.Resume.Stalled != 0 || st.Gen != 2 || st.Settled != 2 || st.Back != nil {
		t.Errorf("record %+v, want gen 2 settled with a resume, no stall and no back", st)
	}
	if ev := findAuditData(f.audit.snapshot(), f.run.ID, "run.folder.transfer", "tries again"); ev == nil || ev.Outcome != "failure" {
		t.Error("the busy visit is not audited")
	}
	if n := f.backRows(); n != 1 {
		t.Errorf("%d audit rows of passes back, want 1", n)
	}
}

// busyStopped is the checks of a busy visit on a sandbox that cannot be held: the sandbox is stopped
// once, the record is settled at the visit's generation with no resume and says the changes were not
// brought back, and one audit row says so.
func (f *passFixture) busyStopped(t *testing.T, err error, why string) {
	t.Helper()
	if errors.Is(err, errComputerFolderResumes) {
		t.Errorf("finalize = %v, want the sandbox left to its stop", err)
	}
	for _, want := range []string{"no pass ran; the runner is still running this run's last pass back", why, "its changes not brought back yet, uncounted"} {
		assertFolderBackTornDown(t, f.cs, f.audit, f.run, f.sb, want)
	}
	if st := f.cs.folderStatus(); st.Gen != 2 || st.Settled != 2 {
		t.Errorf("record %+v, want gen 2 settled", st)
	}
	if n := f.backRows(); n != 1 {
		t.Errorf("%d audit rows of passes back, want 1", n)
	}
}

// A busy visit on a virtual machine, whose agent cannot be stopped alone, stops the sandbox once and
// records that the changes were not brought back: the loss a busy re-visit costs is explicit.
func TestPassBack_ABusyVirtualMachineIsStoppedOnceWithItsChangesNotBroughtBack(t *testing.T) {
	f := newPassFixture(t, 1, 1, nil)
	ds := f.cs.localFolderStore.dispatchTestStore
	ds.mu.Lock()
	ds.run.MachineKind = types.MachineLinuxVM
	ds.mu.Unlock()
	f.holdsAPass(t)
	err := f.finalizeWithin(30 * time.Second)
	f.busyStopped(t, err, "a virtual machine's agent cannot be stopped alone")
}

// A busy visit on a runner's sandbox whose agent cannot be stopped alone is stopped once, not kept.
func TestPassBack_ABusyAgentThatCannotBeStoppedAloneIsStoppedOnce(t *testing.T) {
	f := newPassFixture(t, 1, 1, nil)
	ag := f.onARunner(t, false)
	ag.pid1 = "agent\x00"
	f.holdsAPass(t)
	err := f.finalizeWithin(30 * time.Second)
	f.busyStopped(t, err, "its agent is not one that can be stopped alone")
	if ag.stops != 0 {
		t.Errorf("agent stops %d, want the agent not stopped alone", ag.stops)
	}
}

// An open last pass on an older runner, found at a run's first visit on a runner's sandbox whose agent
// cannot be stopped alone, sends no pass: the sandbox is stopped once and the record says the changes were
// not brought back.
func TestPassBack_AnOpenPassOnAnUnholdableSandboxStopsOnceNotBroughtBack(t *testing.T) {
	f := newPassFixture(t, 0, 0, nil)
	f.rc17Record()
	f.runnerAs(t, &olderRunnerCaps)
	ag := f.onARunner(t, false)
	ag.pid1 = "agent\x00"
	seen := passSeen(f.sb)
	if err := f.finalizeWithin(30 * time.Second); errors.Is(err, errComputerFolderResumes) {
		t.Errorf("finalize = %v, want the sandbox left to its stop", err)
	}
	if seen.Load() {
		t.Error("a pass was sent while the last may still run")
	}
	for _, want := range []string{"no pass ran; the runner may still be running its last pass back", "its agent is not one that can be stopped alone", "its changes not brought back yet, uncounted"} {
		assertFolderBackTornDown(t, f.cs, f.audit, f.run, f.sb, want)
	}
	if st := f.cs.folderStatus(); st.Gen != 0 || st.Settled != 0 || ag.stops != 0 {
		t.Errorf("record %+v, agent stops %d; want gen 0 unsettled and the agent not stopped alone", st, ag.stops)
	}
	if n := f.backRows(); n != 1 {
		t.Errorf("%d audit rows of passes back, want 1", n)
	}
}

// A busy visit that finds a resume already recorded keeps it as it was: the stall count stays, and none is
// added for a pass the runner refused.
func TestPassBack_ABusyVisitKeepsTheResumeItFound(t *testing.T) {
	f := newPassFixture(t, 1, 1, &types.FolderResume{Stalled: 1})
	ag := f.onARunner(t, false)
	f.s.cfg.Store = refStore{f.cs}
	ds := f.cs.localFolderStore.dispatchTestStore
	ds.mu.Lock()
	ds.run.SandboxRef = "sbx"
	ds.mu.Unlock()
	f.holdsAPass(t)
	if err := f.finalizeWithin(30 * time.Second); !errors.Is(err, errComputerFolderResumes) {
		t.Fatalf("finalize = %v, want the kept sandbox's errComputerFolderResumes", err)
	}
	if n := f.sb.stopCount(); n != 0 || ag.stops != 1 {
		t.Errorf("sandbox stops %d, agent stops %d; want the sandbox kept and the agent stopped alone once", n, ag.stops)
	}
	if st := f.cs.folderStatus(); st.Resume == nil || st.Resume.Stalled != 1 || st.Gen != 2 || st.Settled != 2 {
		t.Errorf("record %+v, want gen 2 settled with the resume still at one stall", st)
	}
}

// A pass back not sent because the runner turned into one that may still run the last, found on a sandbox
// whose agent cannot be stopped alone, stops the sandbox once and records that the changes were not brought
// back, at the generation the visit owned and unsettled.
func TestFolderPassOnce_AnUnconfirmedPassOnAnUnholdableSandboxStopsOnce(t *testing.T) {
	f := newPassFixture(t, 0, 0, nil)
	f.rc17Record()
	f.runnerAs(t, &onePerRunnerCaps)
	ag := f.onARunner(t, false)
	ag.pid1 = "agent\x00"
	f.s.cfg.Store = &runnerTurnsAfterBump{computerFolderStore: f.cs, gen: 1, turn: func() { f.runnerAs(t, &olderRunnerCaps) }}
	seen := passSeen(f.sb)
	if err := f.finalizeWithin(30 * time.Second); errors.Is(err, errComputerFolderResumes) {
		t.Errorf("finalize = %v, want the sandbox left to its stop", err)
	}
	if seen.Load() {
		t.Error("a pass was sent while the last may still run")
	}
	for _, want := range []string{"no pass ran; the runner may still be running its last pass back", "its agent is not one that can be stopped alone", "its changes not brought back yet, uncounted"} {
		assertFolderBackTornDown(t, f.cs, f.audit, f.run, f.sb, want)
	}
	if st := f.cs.folderStatus(); st.Gen != 1 || st.Settled != 0 || ag.stops != 0 {
		t.Errorf("record %+v, agent stops %d; want gen 1 unsettled and the agent not stopped alone", st, ag.stops)
	}
}
