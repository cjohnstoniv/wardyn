// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/db"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// The start fence (run_admit.go): its per-run gate, a start whose lock is lost while the runner starts it, the writer
// wait, the revoke's multi-run hold, and the writes that must hold it.

// startAsks is the in-process locker that reports each ask for a run start lock on asked.
type startAsks struct {
	db.Locker
	asked chan struct{}
}

func (l *startAsks) Lock(ctx context.Context, k db.LockKey, wait time.Duration) (context.Context, func(), error) {
	if k.Class == db.RunStartLockClass {
		l.asked <- struct{}{}
	}
	return l.Locker.Lock(ctx, k, wait)
}

// TestStartLockOneStartPerRunAsksForTheLock: while one start holds a run's start lock, a second start on that run
// waits in this process and asks the locker for nothing, so it holds no lock connection; a start on another run is
// not held up; and the second start takes the lock once the first lets it go.
// Catches: every waiting start asking the locker at once (one run's starts holding the whole lock pool).
func TestStartLockOneStartPerRunAsksForTheLock(t *testing.T) {
	l := &startAsks{Locker: db.NewLocalLocker(), asked: make(chan struct{}, 8)}
	e := newExecHarness(t, nil)
	e.srv.locks.override = l
	ctx := context.Background()
	first, err := e.srv.lockStart(ctx, e.id)
	if err != nil {
		t.Fatal(err)
	}
	<-l.asked
	other, err := e.srv.lockStart(ctx, uuid.New())
	if err != nil {
		t.Fatalf("a start on another run was refused: %v", err)
	}
	<-l.asked
	other.release()
	second := make(chan startHold, 1)
	go func() {
		h, err := e.srv.lockStart(ctx, e.id)
		if err != nil {
			t.Error(err)
			h.release = func() {}
		}
		second <- h
	}()
	select {
	case <-l.asked:
		t.Fatal("a second start asked for the run's start lock while the first held it")
	case <-second:
		t.Fatal("a second start took the run's start lock while the first held it")
	case <-time.After(300 * time.Millisecond):
	}
	first.release()
	select {
	case h := <-second:
		h.release()
	case <-time.After(10 * time.Second):
		t.Fatal("the second start had not taken the lock 10 s after the first let it go")
	}
	e.srv.locks.starts.mu.Lock()
	defer e.srv.locks.starts.mu.Unlock()
	if n := len(e.srv.locks.starts.m); n != 0 {
		t.Fatalf("%d run gates were kept after every start left, want 0", n)
	}
}

// poolLocker is the in-process locker with a lock pool as the Postgres one has: each lock holds one of slots from
// its ask until its release, and an ask that finds no slot free within a moment is refused.
type poolLocker struct {
	db.Locker
	slots chan struct{}
}

func (l *poolLocker) Lock(ctx context.Context, k db.LockKey, wait time.Duration) (context.Context, func(), error) {
	select {
	case l.slots <- struct{}{}:
	case <-time.After(100 * time.Millisecond):
		return ctx, nil, db.ErrLockNoCapacity
	}
	lctx, release, err := l.Locker.Lock(ctx, k, wait)
	if err != nil {
		<-l.slots
		return lctx, nil, err
	}
	return lctx, sync.OnceFunc(func() { release(); <-l.slots }), nil
}

// TestStartLockOneRunsStartsLeaveThePoolToOtherRuns: with a pool of db.LockPoolMaxConns connections, one run with
// maxUIConnsPerRun starts in flight (one holding its start lock, the rest waiting) holds one connection, and a start
// on another run takes its lock at once; the waiting starts then take the lock in turn.
// Catches: no gate (each waiting start holds a connection, the pool is full and the other run is refused), and one
// gate for every run.
func TestStartLockOneRunsStartsLeaveThePoolToOtherRuns(t *testing.T) {
	l := &poolLocker{Locker: db.NewLocalLocker(), slots: make(chan struct{}, db.LockPoolMaxConns)}
	e := newExecHarness(t, nil)
	e.srv.locks.override = l
	ctx := context.Background()
	first, err := e.srv.lockStart(ctx, e.id)
	if err != nil {
		t.Fatal(err)
	}
	held := make(chan startHold, maxUIConnsPerRun)
	for range maxUIConnsPerRun - 1 {
		go func() {
			h, err := e.srv.lockStart(ctx, e.id)
			if err != nil {
				t.Error(err)
				h.release = func() {}
			}
			held <- h
		}()
	}
	atGate := func() int {
		e.srv.locks.starts.mu.Lock()
		defer e.srv.locks.starts.mu.Unlock()
		if g := e.srv.locks.starts.m[e.id]; g != nil {
			return g.users
		}
		return 0
	}
	for end := time.Now().Add(10 * time.Second); atGate() < maxUIConnsPerRun && len(l.slots) < cap(l.slots); time.Sleep(5 * time.Millisecond) {
		if time.Now().After(end) {
			t.Fatalf("%d of the run's %d starts reached its gate in 10 s", atGate(), maxUIConnsPerRun)
		}
	}
	if n := len(l.slots); n != 1 {
		t.Fatalf("one run's %d starts in flight hold %d of %d lock connections, want 1", maxUIConnsPerRun, n, cap(l.slots))
	}
	other := make(chan error, 1)
	go func() {
		h, err := e.srv.lockStart(ctx, uuid.New())
		if err == nil {
			h.release()
		}
		other <- err
	}()
	select {
	case err := <-other:
		if err != nil {
			t.Fatalf("a start on another run was refused while one run had %d starts in flight: %v", maxUIConnsPerRun, err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a start on another run waited 5 s behind one run's starts")
	}
	first.release()
	for range maxUIConnsPerRun - 1 {
		select {
		case h := <-held:
			h.release()
		case <-time.After(10 * time.Second):
			t.Fatal("a waiting start had not taken its turn 10 s after the one before it let the lock go")
		}
	}
}

// ctxExecRunner is a runner whose ExecStream is exec, which is handed the start's context.
type ctxExecRunner struct {
	runner.Runner
	exec func(context.Context) (*runner.ExecSession, error)
}

func (r ctxExecRunner) ExecStream(ctx context.Context, _ string, _ runner.ExecSpec) (*runner.ExecSession, error) {
	return r.exec(ctx)
}

// TestRunExecCutsAStartWhoseLockIsLostWhileItRuns: a start still inside the runner when its lock is lost is cut at
// once, not at its budget: the runner call's context ends with the loss, and the start is refused.
// Catches: the loss not watched while the runner call runs.
func TestRunExecCutsAStartWhoseLockIsLostWhileItRuns(t *testing.T) {
	l := &losesStarts{Locker: db.NewLocalLocker()}
	e := newExecHarness(t, nil)
	e.srv.locks.override = l
	e.srv.cfg.Runner = ctxExecRunner{Runner: e.srv.cfg.Runner, exec: func(ctx context.Context) (*runner.ExecSession, error) {
		l.lose()
		<-ctx.Done()
		return nil, context.Cause(ctx)
	}}
	h, err := e.srv.lockStart(context.Background(), e.id)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := e.srv.execAdmitted(context.Background(), h, "sbx-1", runner.ExecSpec{Argv: []string{"id"}})
		done <- err
	}()
	select {
	case err := <-done:
		if !errors.Is(err, errStartLockLost) {
			t.Fatalf("err = %v, want the lost-lock refusal", err)
		}
	case <-time.After(execStartBudget / 2):
		t.Fatalf("the start was still inside the runner %s after its lock was lost", execStartBudget/2)
	}
}

// TestStartFence_AnEndBehindAStartLandsAfterTheStartIsHandedOn: a write that ends the run, arriving while a start
// holds the fence, with 2 s left on its context, behind a runner call of 1.5 s (inside the start's budget), is
// written only once the runner has returned the session, and the start is handed on.
// Catches: a writer that gives the fence up early (any cap below a start's budget) and ends the run while a start
// is inside the runner.
func TestStartFence_AnEndBehindAStartLandsAfterTheStartIsHandedOn(t *testing.T) {
	var mu sync.Mutex
	var returned time.Time
	e := newExecHarness(t, func(runner.ExecSpec) (*runner.ExecSession, error) {
		time.Sleep(1500 * time.Millisecond)
		mu.Lock()
		returned = time.Now()
		mu.Unlock()
		return fakeExecSession("", "", 0), nil
	})
	h, err := e.srv.lockStart(context.Background(), e.id)
	if err != nil {
		t.Fatal(err)
	}
	handed := make(chan error, 1)
	go func() {
		_, err := e.srv.execAdmitted(context.Background(), h, "sbx-1", runner.ExecSpec{Argv: []string{"id"}})
		handed <- err
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	var wrote time.Time
	if _, err := e.srv.fenced(ctx, e.id, func() (bool, error) { wrote = time.Now(); return true, ctx.Err() }); err != nil {
		t.Fatalf("the end was written on a spent context: %v", err)
	}
	if err := <-handed; err != nil {
		t.Fatalf("the start was refused: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if !wrote.After(returned) {
		t.Fatalf("the end was written %s before the runner returned the start's session", returned.Sub(wrote))
	}
}

// honoursCtx is a store whose run state write refuses an ended context, as a Postgres statement does.
type honoursCtx struct{ store.Store }

func (s honoursCtx) UpdateRunStateIf(ctx context.Context, id uuid.UUID, from, to types.RunState) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	return s.Store.UpdateRunStateIf(ctx, id, from, to)
}

// TestStartFence_AWriterWhoseContextEndsLeavesTheRunForTheNextPass: a kill whose context ends while a start holds
// the fence writes nothing, and the run stays RUNNING; the next pass, once the start has let the fence go, writes it.
// Catches: a writer that gives the fence up while its context is still live and ends the run under a start.
func TestStartFence_AWriterWhoseContextEndsLeavesTheRunForTheNextPass(t *testing.T) {
	e := newExecHarness(t, nil)
	e.srv.cfg.Store = honoursCtx{e.srv.cfg.Store}
	h, err := e.srv.lockStart(context.Background(), e.id)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	if applied, err := e.srv.casRunState(ctx, e.id, types.RunRunning, types.RunKilled); applied || err == nil {
		t.Fatalf("a kill whose context ended behind a start = %v, %v; want nothing written and an error", applied, err)
	}
	if run, _ := e.ast.GetRun(context.Background(), e.id); run.State != types.RunRunning {
		t.Fatalf("the run is %s after a kill whose context ended behind a start, want RUNNING", run.State)
	}
	h.release()
	if applied, err := e.srv.casRunState(context.Background(), e.id, types.RunRunning, types.RunKilled); !applied || err != nil {
		t.Fatalf("the next pass = %v, %v; want the kill written", applied, err)
	}
}

// TestStartFence_ARevokeHoldsEveryLiveRunsStartLock: the revoke's fence (fenceRunsStarts) takes each live run's start
// lock once, a run listed twice included, holds all of them until its release, and then lets every one go.
// Catches: a run left out of the revoke's fence, a key asked twice, and a key kept after the release.
func TestStartFence_ARevokeHoldsEveryLiveRunsStartLock(t *testing.T) {
	l := &countingLocker{next: db.NewLocalLocker(), class: db.RunStartLockClass}
	e := newExecHarness(t, nil)
	e.srv.locks.override = l
	ids := []uuid.UUID{uuid.New(), uuid.New(), uuid.New()}
	runs := []types.AgentRun{{ID: ids[2]}, {ID: ids[0]}, {ID: ids[1]}, {ID: ids[0]}}
	release := e.srv.fenceRunsStarts(context.Background(), runs)
	if n := l.n.Load(); n != int32(len(ids)) {
		t.Errorf("the revoke asked for %d start locks, want %d", n, len(ids))
	}
	for _, id := range ids {
		if startLockFree(e.srv, id) {
			t.Errorf("run %s's start lock is free under the revoke's fence", id)
		}
	}
	release()
	for _, id := range ids {
		if !startLockFree(e.srv, id) {
			t.Errorf("run %s's start lock is still held after the revoke's fence let go", id)
		}
	}
}

// failsFirstLoginKill is the login-run store whose first write of KILLED fails, as a kill's does when its wait for
// the run's start fence spent the kill's context.
type failsFirstLoginKill struct {
	*killableLoginRunStore
	failed atomic.Bool
}

func (s *failsFirstLoginKill) UpdateRunStateIf(ctx context.Context, id uuid.UUID, from, to types.RunState) (bool, error) {
	if to == types.RunKilled && s.failed.CompareAndSwap(false, true) {
		return false, fmt.Errorf("store: update run state: %w", context.DeadlineExceeded)
	}
	return s.killableLoginRunStore.UpdateRunStateIf(ctx, id, from, to)
}

// TestKillSignInRunAfterCapture_AFailedKillIsTriedAgain: a captured sign-in run whose kill fails to write is killed
// on the next attempt, with the capture's reason.
// Catches: a failed kill that leaves the sign-in sandbox running to its idle cap.
func TestKillSignInRunAfterCapture_AFailedKillIsTriedAgain(t *testing.T) {
	runID := uuid.New()
	st := &failsFirstLoginKill{killableLoginRunStore: &killableLoginRunStore{
		ssoLoginRunStore: ssoLoginRunStore{run: types.AgentRun{
			ID: runID, Task: harnessLoginTask, Agent: awsSSOAgent,
			CreatedBy: "alice@example.com", UpdatedAt: time.Now().UTC(),
		}},
		state: types.RunRunning,
	}}
	audit := &memAudit{}
	cfg := baseTestConfig(newHarness(t), st)
	cfg.Audit = audit
	srv := New(cfg)
	srv.signInCaptureKillGrace = 0
	srv.killSignInRunAfterCapture(context.Background(), runID, "alice@example.com")
	srv.WaitBackground()
	if got := st.stateNow(); got != types.RunKilled {
		t.Fatalf("sign-in run state = %s after one failed kill, want KILLED", got)
	}
	if data := killData(t, killRow(t, audit, runID.String())); data["reason"] != signInCapturedReason {
		t.Errorf("run.kill reason = %v, want %q", data["reason"], signInCapturedReason)
	}
}

// failsFirstSupersede is the sign-in fixture's store whose first write of KILLED fails, as failsFirstLoginKill's.
type failsFirstSupersede struct {
	*supersedeStore
	failed atomic.Bool
}

func (s *failsFirstSupersede) UpdateRunStateIf(ctx context.Context, id uuid.UUID, from, to types.RunState) (bool, error) {
	if to == types.RunKilled && s.failed.CompareAndSwap(false, true) {
		return false, fmt.Errorf("store: update run state: %w", context.DeadlineExceeded)
	}
	return s.supersedeStore.UpdateRunStateIf(ctx, id, from, to)
}

// TestHarnessLogin_ASupersedeWhoseKillFailsTriesAgain: an older sign-in run whose supersede kill fails to write is
// killed on the next attempt, and its teardown runs.
// Catches: a failed supersede kill that leaves a second live sign-in sandbox.
func TestHarnessLogin_ASupersedeWhoseKillFailsTriesAgain(t *testing.T) {
	f := newSupersedeFixture(t, nil, nil)
	old := f.store.seed(types.AgentRun{ID: uuid.New(), CreatedBy: "sub-member", Task: harnessLoginTask, Agent: awsSSOAgent})
	f.srv.cfg.Store = &failsFirstSupersede{supersedeStore: f.store}
	f.srv.supersedeOneLoginRun(context.Background(), old, "sub-member", uuid.New())
	if got := f.store.stateOf(t, old.ID.String()); got != types.RunKilled {
		t.Fatalf("the older sign-in run is %s after one failed kill, want KILLED", got)
	}
	waitForRevoke(t, f.idp, old.ID)
}

// TestAbortQueuedRun_AFailedKillIsTriedAgain: a run a promoter took before its create had recorded it in full, whose
// kill fails to write, is killed on the next attempt, with the create's abort reason. The login-run store stands in
// for any run's store: it has no queue, so the abort goes straight to the kill.
// Catches: a failed kill that leaves running a run whose create answered an error.
func TestAbortQueuedRun_AFailedKillIsTriedAgain(t *testing.T) {
	runID := uuid.New()
	st := &failsFirstLoginKill{killableLoginRunStore: &killableLoginRunStore{
		ssoLoginRunStore: ssoLoginRunStore{run: types.AgentRun{
			ID: runID, Task: harnessLoginTask, Agent: awsSSOAgent,
			CreatedBy: "alice@example.com", UpdatedAt: time.Now().UTC(),
		}},
		state: types.RunRunning,
	}}
	audit := &memAudit{}
	cfg := baseTestConfig(newHarness(t), st)
	cfg.Audit = audit
	srv := New(cfg)
	srv.abortQueuedRun(context.Background(), runID, createRunComponentsAbortHint)
	srv.WaitBackground()
	if got := st.stateNow(); got != types.RunKilled {
		t.Fatalf("promoted run state = %s after one failed kill, want KILLED", got)
	}
	if data := killData(t, killRow(t, audit, runID.String())); data["reason"] != queueCancelCreateAborted {
		t.Errorf("run.kill reason = %v, want %q", data["reason"], queueCancelCreateAborted)
	}
}

// runEndWrites are the store writes that end a RUNNING run or mark it kept, called from internal/api and
// cmd/wardynd. Each must run under the run's start fence.
var runEndWrites = []string{"UpdateRunStateIf", "UpdateRunStateIfIdle", "UpdateRunStateIfCreatedBefore",
	"KillRunAtLifetimeIf", "MarkRunEnded", "KillRunMarkingKeptMachine", "AcceptRunnerExit", "MarkRunLost",
	"MarkRunLostAtGeneration", "RestoreRunLostFateAtGeneration", "MarkRunnerRunGone", "MarkRunnerRunGoneProbed",
	"RevokeRunner"}

// TestRunEndWritersHoldTheStartFence: every call of a runEndWrites method in the non-test files of internal/api
// and cmd/wardynd is fenced: the function around it takes the fence first (FenceRunStarts, fenceRunsStarts, the idle
// reaper's fenceStarts), or it runs in a function literal handed to a call that runs it under the fence (fenced,
// endGoneRun, and the idle reaper's stop in adapters.go). Each of those wrappers takes the fence itself, and each
// write is called somewhere.
// Catches: a new or edited end of a run written outside the fence.
func TestRunEndWritersHoldTheStartFence(t *testing.T) {
	takes := func(name string) bool {
		return name == "FenceRunStarts" || name == "fenceRunsStarts" || name == "fenceStarts" || name == "fenced"
	}
	wraps := func(file, name string) bool {
		return name == "fenced" || name == "endGoneRun" || file == "adapters.go" && name == "stop"
	}
	callee := func(c *ast.CallExpr) string {
		switch f := c.Fun.(type) {
		case *ast.Ident:
			return f.Name
		case *ast.SelectorExpr:
			return f.Sel.Name
		}
		return ""
	}
	files, _ := filepath.Glob("*.go")
	more, _ := filepath.Glob("../../cmd/wardynd/*.go")
	seen := map[string]bool{}
	for _, path := range append(files, more...) {
		if filepath.Ext(path) != ".go" || len(path) > 8 && path[len(path)-8:] == "_test.go" {
			continue
		}
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		file := filepath.Base(path)
		var bodies []ast.Node      // every function body, a literal's or a declaration's
		var wrapped []*ast.FuncLit // literals handed to a wrapper
		var fences []token.Pos     // calls that take the fence
		var writes []*ast.CallExpr // calls of a runEndWrites method
		ast.Inspect(f, func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.FuncDecl:
				if n.Body != nil {
					bodies = append(bodies, n.Body)
					if wraps(file, n.Name.Name) && !slices.ContainsFunc(callsIn(n.Body), func(c *ast.CallExpr) bool { return takes(callee(c)) }) {
						t.Errorf("%s: %s runs a write under the fence but takes none", fset.Position(n.Pos()), n.Name.Name)
					}
				}
			case *ast.FuncLit:
				bodies = append(bodies, n.Body)
			case *ast.CallExpr:
				name := callee(n)
				if takes(name) {
					fences = append(fences, n.Pos())
				}
				if wraps(file, name) {
					for _, a := range n.Args {
						if lit, ok := a.(*ast.FuncLit); ok {
							wrapped = append(wrapped, lit)
						}
					}
				}
				if _, ok := n.Fun.(*ast.SelectorExpr); ok && slices.Contains(runEndWrites, name) {
					writes = append(writes, n)
				}
			}
			return true
		})
		for _, w := range writes {
			seen[callee(w)] = true
			in := func(n ast.Node) bool { return n.Pos() <= w.Pos() && w.End() <= n.End() }
			if slices.ContainsFunc(wrapped, func(l *ast.FuncLit) bool { return in(l) }) {
				continue
			}
			var inner ast.Node
			for _, b := range bodies {
				if in(b) && (inner == nil || inner.Pos() < b.Pos()) {
					inner = b
				}
			}
			if inner == nil || !slices.ContainsFunc(fences, func(p token.Pos) bool { return inner.Pos() <= p && p < w.Pos() }) {
				t.Errorf("%s: %s ends a run outside the start fence (run_admit.go)", fset.Position(w.Pos()), callee(w))
			}
		}
	}
	for _, name := range runEndWrites {
		if !seen[name] {
			t.Errorf("no call of %s found: drop it from runEndWrites or name the write that replaced it", name)
		}
	}
}

// callsIn lists the calls in n.
func callsIn(n ast.Node) []*ast.CallExpr {
	var calls []*ast.CallExpr
	ast.Inspect(n, func(n ast.Node) bool {
		if c, ok := n.(*ast.CallExpr); ok {
			calls = append(calls, c)
		}
		return true
	})
	return calls
}
