// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"maps"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/db"
)

// An edit's ctx ends with its calls, and carries the edit's deadline and, as its end hold's deadline, its hard end:
// that deadline plus a stop's window. A window detached under it ends by the hard end and keeps that hold; off an
// edit it is the plain window, with no hold. The caller's own end never ends it.
func TestEditDetached_EndsByTheEditsHardEnd(t *testing.T) {
	calls, budget := egressEditCallBudget, egressEditBudget
	t.Cleanup(func() { egressEditCallBudget, egressEditBudget = calls, budget })
	egressEditCallBudget, egressEditBudget = time.Minute, 2*time.Minute
	now := time.Now()
	ectx, cancelEdit := editCtx(context.Background())
	callEnd, _ := ectx.Deadline()
	dl, edit := editDeadline(ectx)
	hard, held := holdDeadline(ectx)
	cancelEdit()
	if callEnd.Sub(now.Add(time.Minute)).Abs() > time.Second || !edit || dl.Sub(now.Add(2*time.Minute)).Abs() > time.Second {
		t.Errorf("the edit's calls end %v and its deadline is %v (%v); want 1 and 2 minutes from now", callEnd.Sub(now), dl.Sub(now), edit)
	}
	if !held || !hard.Equal(dl.Add(killCascadeTimeout)) {
		t.Errorf("the edit's end hold is %v (%v), want its hard end, its deadline plus %v", hard.Sub(now), held, killCascadeTimeout)
	}
	for _, c := range []struct {
		name string
		edit bool          // under an edit
		dl   time.Duration // the edit's deadline, from now
		d    time.Duration // the window asked for
		end  time.Duration // the window's end, from now
	}{
		{"off an edit", false, 0, killCascadeTimeout, killCascadeTimeout},
		{"an edit far from its hard end", true, time.Hour, killCascadeTimeout, killCascadeTimeout},
		{"an edit near its hard end", true, 5 * time.Second, time.Minute, 5*time.Second + killCascadeTimeout},
		{"an edit past its hard end", true, -time.Minute, killCascadeTimeout, -time.Minute + killCascadeTimeout},
	} {
		egressEditBudget = c.dl
		now := time.Now()
		caller, cancelCaller := context.WithCancel(context.Background())
		if c.edit {
			var cancelEdit context.CancelFunc
			caller, cancelEdit = editCtx(caller)
			defer cancelEdit()
		}
		ctx, cancel := editDetached(caller, c.d)
		cancelCaller()
		if end, ok := ctx.Deadline(); !ok || end.Sub(now.Add(c.end)).Abs() > time.Second {
			t.Errorf("%s: the window ends %v from now (%v), want %v", c.name, end.Sub(now), ok, c.end)
		}
		if c.end > time.Second && ctx.Err() != nil {
			t.Errorf("%s: the caller's end ended the window: %v", c.name, context.Cause(ctx))
		}
		want, wantHeld := holdDeadline(caller)
		if hold, held := holdDeadline(ctx); held != c.edit || held != wantHeld || !hold.Equal(want) {
			t.Errorf("%s: end hold %v (%v), want the edit's hard end under an edit and none off one", c.name, hold, held)
		}
		cancel()
	}
}

// A stop begun once the edit's hard end has passed has no time left, and a run's end made on it leaves the sandbox
// stop and the teardown rows to the tail owner (holdPassed); off an edit a stop keeps the 30 s it always had, with no
// end hold.
func TestStopCtx_EndsWithTheEdit(t *testing.T) {
	budget := egressEditBudget
	t.Cleanup(func() { egressEditBudget = budget })
	egressEditBudget = -killCascadeTimeout - time.Second
	ectx, cancelEdit := editCtx(context.Background())
	defer cancelEdit()
	ctx, cancel := stopCtx(ectx)
	defer cancel()
	if ctx.Err() == nil {
		t.Error("a stop begun past the edit's hard end still has time")
	}
	if !holdPassed(ctx) {
		t.Error("a run's end made on that stop would still stop the sandbox and write its rows")
	}
	off, cancelOff := stopCtx(context.Background())
	defer cancelOff()
	if dl, ok := off.Deadline(); !ok || time.Until(dl) < killCascadeTimeout-time.Second {
		t.Errorf("off an edit the stop's window ends %v (%v), want %v from now", dl, ok, killCascadeTimeout)
	}
	if _, held := holdDeadline(off); held {
		t.Error("off an edit a stop carries an end hold")
	}
}

// After its claim a revive's compensation opens windows of its own that drop the edit's ends (reviveRunProxy's
// claimed window, then reviveGenerationFailed's), and a run's end made in one, on a run with no computer folder,
// takes no end hold (lockRunEnd) and runs its tail on that window. The tail still stops at the edit's hard end: past
// it the sandbox is not stopped (holdPassed, which finalizeRunEffects asks; TestEndHold_PastDeadlineNoStopNoRecord
// pins the skip), and the tail's fresh windows are already done. Red without the hold on the edit's ctx.
func TestEditCtx_ATailInTheRevivesCompensationStopsAtTheHardEnd(t *testing.T) {
	budget := egressEditBudget
	t.Cleanup(func() { egressEditBudget = budget })
	egressEditBudget = -killCascadeTimeout - time.Second // the hard end passed a second ago
	ectx, cancelEdit := editCtx(context.Background())
	defer cancelEdit()
	claimed, cancelClaimed := context.WithTimeout(context.WithoutCancel(ectx), reviveClaimedTimeout)
	defer cancelClaimed()
	comp, cancelComp := context.WithTimeout(context.WithoutCancel(claimed), killCascadeTimeout)
	defer cancelComp()
	if comp.Err() != nil {
		t.Fatalf("the compensation window is done at once: %v", comp.Err())
	}
	if !holdPassed(comp) {
		t.Error("a tail made in the compensation would still stop the sandbox past the edit's hard end")
	}
	tctx, cancelTail := teardownCtx(comp)
	defer cancelTail()
	if !errors.Is(context.Cause(tctx), errHoldPassed) {
		t.Errorf("the tail's teardown window: %v, want it done at the edit's hard end", context.Cause(tctx))
	}
	settleAfterTeardown(comp, func(sctx context.Context) {
		if !errors.Is(context.Cause(sctx), errHoldPassed) {
			t.Errorf("a settle write's window: %v, want it done at the edit's hard end", context.Cause(sctx))
		}
	})
}

// A stop begun inside the readback wait, and a tail begun on the wait itself (expireProxyStart's), run to the edit's
// hard end, not to the wait's end: the wait keeps the edit's hold, and a tail begun once it is over keeps its floor.
func TestEditDetached_AStopInTheWaitKeepsTheHardEnd(t *testing.T) {
	budget := egressEditBudget
	t.Cleanup(func() { egressEditBudget = budget })
	for _, c := range []struct {
		name string
		dl   time.Duration // the edit's deadline, from now
	}{
		{"the wait still open", time.Minute},
		{"the wait over, the hard end not", -time.Second},
	} {
		egressEditBudget = c.dl
		ectx, cancelEdit := editCtx(context.Background())
		hard, _ := holdDeadline(ectx)
		dl, _ := editDeadline(ectx)
		wait, cancelWait := editDetached(ectx, egressEditWait) // as editRunEgressOnce opens it
		wait, endWait := context.WithDeadline(wait, dl)
		stop, cancelStop := stopCtx(wait)
		if hold, held := holdDeadline(stop); !held || !hold.Equal(hard) {
			t.Errorf("%s: a stop in the wait holds to %v (%v), want the edit's hard end %v", c.name, hold, held, hard)
		}
		if end, _ := stop.Deadline(); end.Sub(minTime(time.Now().Add(killCascadeTimeout), hard)).Abs() > time.Second || stop.Err() != nil {
			t.Errorf("%s: a stop in the wait ends %v (%v), want its own window or the hard end", c.name, time.Until(end), stop.Err())
		}
		tail, cancelTail := teardownCtx(wait)
		if hold, held := holdDeadline(tail); tail.Err() != nil || !held || !hold.Equal(hard) {
			t.Errorf("%s: a tail on the wait: %v, hold %v (%v); want it open to the edit's hard end", c.name, context.Cause(tail), hold, held)
		}
		cancelTail()
		cancelStop()
		endWait()
		cancelWait()
		cancelEdit()
	}
}

// stalledReadStore is the edit fixture's store with one read that answers only once its ctx is done ("agent", the
// agent-start mark read before the revive's lock, or "ends", the run's end read for the answer's fate), which then
// reports that ctx's error on ended.
type stalledReadStore struct {
	*readbackStore
	read  string
	ended chan error
}

func (s *stalledReadStore) stalled(ctx context.Context, read string) error {
	if s.read != read {
		return nil
	}
	<-ctx.Done()
	select {
	case s.ended <- ctx.Err():
	default:
	}
	return ctx.Err()
}

func (s *stalledReadStore) RunAgentStopped(ctx context.Context, _ uuid.UUID) (bool, error) {
	return false, s.stalled(ctx, "agent")
}

func (s *stalledReadStore) RunEndsOpen(ctx context.Context, _ uuid.UUID, _ time.Duration) (bool, bool, error) {
	err := s.stalled(ctx, "ends")
	return false, err == nil, err
}

// stallRead makes read stall on f's store, and answers the channel its ctx's error comes back on.
func stallRead(f *readbackFixture, read string) chan error {
	st := &stalledReadStore{readbackStore: f.rb, read: read, ended: make(chan error, 1)}
	f.srv.cfg.Store = st
	return st.ended
}

// editWithin is f.editEgress, failing the test when the edit has not answered within limit.
func editWithin(t *testing.T, f *readbackFixture, body string, limit time.Duration) *httptest.ResponseRecorder {
	t.Helper()
	answered := make(chan *httptest.ResponseRecorder, 1)
	go func() { answered <- f.editEgress(t, body) }()
	select {
	case w := <-answered:
		return w
	case <-time.After(limit):
		t.Fatalf("the edit did not answer within %v", limit)
		return nil
	}
}

// A read before the revive's lock that never answers ends with the edit's calls, so the claim cannot land later.
func TestEgressEdit_AStalledReadBeforeTheClaimEndsWithTheEditsCalls(t *testing.T) {
	f := newEgressEditFixture(t)
	calls := egressEditCallBudget
	egressEditCallBudget = time.Second
	t.Cleanup(func() { egressEditCallBudget = calls })
	ended := stallRead(f, "agent")
	w := editWithin(t, f, `{"deny_hosts":["evil.example"]}`, 8*time.Second)
	if w.Code != http.StatusServiceUnavailable || errorReason(w) != reasonReviveAgentStatusUnreadable {
		t.Errorf("edit: %d %s, want 503 %s", w.Code, w.Body.String(), reasonReviveAgentStatusUnreadable)
	}
	select {
	case err := <-ended:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("the stalled read ended with %v, want the edit's calls' deadline", err)
		}
	default:
		t.Error("the agent-start read never ran")
	}
	if n := len(f.rr.replaced); n != 0 {
		t.Errorf("%d proxies replaced, want none", n)
	}
}

// The answer's fate read ends by the edit's hard end, not with the request.
func TestEgressEdit_TheFateReadEndsByTheHardEnd(t *testing.T) {
	f := newEgressEditFixture(t)
	budget := egressEditBudget
	egressEditBudget = time.Second - killCascadeTimeout // the deadline passed, the hard end a second away
	t.Cleanup(func() { egressEditBudget = budget })
	ended := stallRead(f, "ends")
	w := editWithin(t, f, `{"deny_hosts":["evil.example"]}`, 8*time.Second)
	if w.Code != http.StatusServiceUnavailable || errorReason(w) != reasonEgressEditContainmentUnresolved ||
		!strings.Contains(w.Body.String(), "lost until revived") {
		t.Errorf("edit: %d %s, want 503 %s saying the run will be lost until revived", w.Code, w.Body.String(), reasonEgressEditContainmentUnresolved)
	}
	select {
	case err := <-ended:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("the stalled fate read ended with %v, want the edit's hard end", err)
		}
	default:
		t.Error("the fate read never ran")
	}
}

// An edit whose start never reports answers by its deadline, not at the end of the readback wait.
func TestEgressEdit_AnswersByItsDeadline(t *testing.T) {
	f := newEgressEditFixture(t) // a readback wait of 10 s
	calls, budget := egressEditCallBudget, egressEditBudget
	egressEditCallBudget, egressEditBudget = time.Second, 2*time.Second
	t.Cleanup(func() { egressEditCallBudget, egressEditBudget = calls, budget })
	w := editWithin(t, f, `{"deny_hosts":["evil.example"]}`, 8*time.Second)
	if w.Code != http.StatusServiceUnavailable || errorReason(w) != reasonEgressEditContainmentUnresolved ||
		!strings.Contains(w.Body.String(), "readback_wait_timeout") {
		t.Errorf("edit: %d %s, want 503 %s (readback_wait_timeout)", w.Code, w.Body.String(), reasonEgressEditContainmentUnresolved)
	}
}

// editPathFiles are the files on an edit's path that open a window dropping the request's end, and the reads
// before the revive's lock. A window opened in another file the path calls into is not counted here.
var editPathFiles = []string{"run_egress_edit.go", "run_egress_narrow.go", "run_revive.go", "run_revive_after_claim.go",
	"run_revive_readback.go", "run_policy_generation.go", "run_proxy_readback.go", "run_placement_revive.go"}

// detachedWindows is each window in files that drops its caller's end (context.WithoutCancel, context.Background,
// context.TODO, db.WithoutCancelUnlessLost), by "file:function", as the bound it is opened with, or as the detaching
// call itself when it is given no bound there; and every function the files declare.
func detachedWindows(t *testing.T, files []string) (map[string][]string, map[string]*ast.FuncDecl) {
	t.Helper()
	detaches := map[string]bool{"context.WithoutCancel": true, "context.Background": true, "context.TODO": true, "db.WithoutCancelUnlessLost": true}
	windows, funcs := map[string][]string{}, map[string]*ast.FuncDecl{}
	fset := token.NewFileSet()
	for _, name := range files {
		f, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, d := range f.Decls {
			key := name + ":(package)"
			if fn, ok := d.(*ast.FuncDecl); ok {
				key = name + ":" + fn.Name.Name
				funcs[key] = fn
			}
			bounded := map[*ast.CallExpr]bool{}
			ast.Inspect(d, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				switch callee := types.ExprString(call.Fun); {
				case (callee == "context.WithTimeout" || callee == "context.WithDeadline") && len(call.Args) == 2:
					if in, ok := call.Args[0].(*ast.CallExpr); ok && detaches[types.ExprString(in.Fun)] {
						bounded[in] = true
						windows[key] = append(windows[key], types.ExprString(call.Args[1]))
					}
				case detaches[callee] && !bounded[call]:
					windows[key] = append(windows[key], callee)
				}
				return true
			})
		}
	}
	return windows, funcs
}

// Every window on an edit's paths that drops the request's end is listed here by what bounds it, and every chain of
// them is counted from the bounds the code opens them with: a revive's own windows fit inside the edit's call
// budget, a start recorded by then is waited out by the edit's deadline, and every window after a claim or an
// in-place write made by then closes by the hard end, so the tails made in them stop there (editCtx). The stated
// answer covers the calls past the hard end that ignore a done ctx. A window added, moved or rebound turns this red.
func TestEgressEditBudget_CoversEveryWindowAfterTheClaim(t *testing.T) {
	got, funcs := detachedWindows(t, editPathFiles)
	want := map[string][]string{
		// The edit's own: the wait, its stops and the fate read, each ending by the hard end.
		"run_egress_edit.go:editDetached": {"db.WithoutCancelUnlessLost"},
		// In place, after the write, a request call.
		"run_egress_narrow.go:deliver": {"narrowDeliverTimeout", "killCascadeTimeout"},
		"run_egress_narrow.go:undo":    {"killCascadeTimeout"},
		// The revive's claimed window, opened only after the claim (below), and its compensation.
		"run_revive.go:reviveRunProxy":                        {"reviveClaimedTimeout"},
		"run_revive_readback.go:replaceRevivedProxyHashed":    {"heldStoreCallBound"},
		"run_policy_generation.go:reviveGenerationFailed":     {"killCascadeTimeout", "killCascadeTimeout"},
		"run_policy_generation.go:abandonRunPolicyGeneration": {"killCascadeTimeout"},
		"run_policy_generation.go:revivePolicyMoved":          {"killCascadeTimeout"},
		"run_policy_generation.go:reviveKeptAfterAdvance":     {"killCascadeTimeout"},
		"run_policy_generation.go:reconcileLostAdvance":       {"killCascadeTimeout"},
		// No window: the run lock's loss, watched inside the claimed window.
		"run_revive_readback.go:whileLockHeld": {"db.WithoutCancelUnlessLost"},
		// Never on an edit: the agent's start (an edit starts none, reviveSupported), a start's deadline timer, the
		// proxy's own report, and a run's create.
		"run_revive_after_claim.go:reviveStartAgent":      {"killCascadeTimeout"},
		"run_proxy_readback.go:watchProxyStart":           {"killCascadeTimeout"},
		"run_proxy_readback.go:handleInternalProxyLoaded": {"context.WithoutCancel"},
		"run_proxy_readback.go:abortUnconfirmedSandbox":   {"killCascadeTimeout"},
	}
	keys := slices.Sorted(maps.Keys(got))
	for k := range want {
		if _, ok := got[k]; !ok {
			keys = append(keys, k)
		}
	}
	for _, k := range keys {
		if !slices.Equal(got[k], want[k]) {
			t.Errorf("%s: windows %v, want %v; classify a new or rebound window here and count it in a chain below", k, got[k], want[k])
		}
	}
	// The reads before the revive's lock open no window, so they end with the edit's calls.
	for _, read := range []string{"run_revive.go:reviveNeedsAgentStart", "run_proxy_readback.go:olderThanFirstRenewGate", "run_proxy_readback.go:proxyStartMode"} {
		if funcs[read] == nil {
			t.Errorf("%s is gone: name the read before the lock that replaced it", read)
		}
	}
	var claim, window token.Pos
	if fn := funcs["run_revive.go:reviveRunProxy"]; fn != nil {
		ast.Inspect(fn, func(n ast.Node) bool {
			if call, ok := n.(*ast.CallExpr); ok {
				switch types.ExprString(call.Fun) {
				case "s.claimReviveOwing":
					claim = firstPos(claim, call.Pos())
				case "context.WithoutCancel":
					window = firstPos(window, call.Pos())
				}
			}
			return true
		})
	}
	if claim == token.NoPos || window == token.NoPos || window < claim {
		t.Errorf("reviveRunProxy's claimed window (at %d) is not opened after its claim (at %d)", window, claim)
	}

	value := map[string]time.Duration{"killCascadeTimeout": killCascadeTimeout, "reviveClaimedTimeout": reviveClaimedTimeout,
		"heldStoreCallBound": heldStoreCallBound, "narrowDeliverTimeout": narrowDeliverTimeout}
	win := func(key string, i int) time.Duration {
		if i < len(got[key]) {
			if d, ok := value[got[key][i]]; ok {
				return d
			}
		}
		t.Errorf("%s: no window %d with a known bound (%v)", key, i, got[key])
		return 0
	}
	lock := db.LockPoolAcquireWait + db.LockWait + 4*time.Second // the lock statement's 2 s margin and one 2 s session close
	verify := 2*db.LockWatchInterval + time.Second               // one check of a held lock
	release := 2*verify + 2*time.Second                          // the wait for the lock's connection, then the 2 s close
	const statedAnswer = 8 * time.Minute                         // docs/operations/run-lifetime.md, docs/sdk.md

	// Docker's two windows on a ctx of their own, read from the driver: a created proxy's removal, and StopSandbox.
	dockerRollback := dockerSeconds(t, "driver.go", "stopTimeout")
	dockerStop := dockerSeconds(t, "driver_network.go", "stopSandboxBudget")

	if before := narrowRouteTimeout + leaseWait + lock + reviveRunnerTimeout; before > egressEditCallBudget {
		t.Errorf("a revive's own windows take %v before its claim, past the edit's call budget %v", before, egressEditCallBudget)
	}
	claimed := win("run_revive.go:reviveRunProxy", 0)
	if waited := egressEditCallBudget + claimed + release + egressEditWait; waited > egressEditBudget {
		t.Errorf("a start recorded in the claimed window is waited out at %v, past the edit's deadline %v", waited, egressEditBudget)
	}
	readBack := win("run_policy_generation.go:reconcileLostAdvance", 0)
	undo := win("run_policy_generation.go:abandonRunPolicyGeneration", 0)
	deliver := win("run_egress_narrow.go:deliver", 0)
	hard := egressEditBudget + killCascadeTimeout
	for _, c := range []struct {
		name string
		d    time.Duration // after the claim, or after the in-place write
	}{
		{"an advance lost, read back and undone, the run kept", claimed + readBack + undo + win("run_policy_generation.go:reviveGenerationFailed", 0)},
		{"an advance lost, read back and not undone, the run contained", claimed + readBack + undo + win("run_policy_generation.go:reviveGenerationFailed", 1)},
		{"a replace failed, its start abandoned, the generation not undone", claimed + dockerRollback + win("run_revive_readback.go:replaceRevivedProxyHashed", 0) + undo + win("run_policy_generation.go:reviveKeptAfterAdvance", 0)},
		{"the run moved since the revive read it", claimed + win("run_policy_generation.go:revivePolicyMoved", 0)},
		{"in place, a narrowing refused and undone", deliver + win("run_egress_narrow.go:undo", 0) + undo},
		{"in place, a narrowing delivered and answered", deliver + win("run_egress_narrow.go:deliver", 1)},
		{"in place, an advance lost and read back", readBack + undo},
	} {
		if end := egressEditCallBudget + c.d; end > hard {
			t.Errorf("%s: its windows close at %v, past the edit's hard end %v", c.name, end, hard)
		}
	}
	if answer := hard + 2*verify + dockerStop + release; answer > statedAnswer {
		t.Errorf("the edit answers within %v, past the stated %v", answer, statedAnswer)
	}
}

// dockerSeconds is the Docker driver's name, declared in file as "<n> * time.Second", read from its source: the
// census counts the driver's two windows on a ctx of their own, which are unexported in that package.
func dockerSeconds(t *testing.T, file, name string) time.Duration {
	t.Helper()
	f, err := parser.ParseFile(token.NewFileSet(), filepath.Join("..", "runner", "docker", file), nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	var d time.Duration
	ast.Inspect(f, func(n ast.Node) bool {
		vs, ok := n.(*ast.ValueSpec)
		if !ok {
			return true
		}
		for i, id := range vs.Names {
			if id.Name != name || i >= len(vs.Values) {
				continue
			}
			be, ok := vs.Values[i].(*ast.BinaryExpr)
			if !ok || be.Op != token.MUL || types.ExprString(be.Y) != "time.Second" {
				continue
			}
			if lit, ok := be.X.(*ast.BasicLit); ok && lit.Kind == token.INT {
				if v, err := strconv.Atoi(lit.Value); err == nil {
					d = time.Duration(v) * time.Second
				}
			}
		}
		return true
	})
	if d == 0 {
		t.Fatalf("internal/runner/docker/%s declares no %s = <n> * time.Second: count that window from its new form", file, name)
	}
	return d
}

// firstPos is the earlier of two positions, token.NoPos meaning none yet.
func firstPos(a, b token.Pos) token.Pos {
	if a == token.NoPos || b < a {
		return b
	}
	return a
}
