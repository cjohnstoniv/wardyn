// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"log/slog"
	"math"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// revokeOwedFake is store.RevokeOwed in memory: the column and the page walk, with PG's order and cursor.
type revokeOwedFake struct {
	mu       sync.Mutex
	owed     map[uuid.UUID]time.Time
	markErrs int // the next markErrs MarkRevokeOwed calls fail
	marks    int // MarkRevokeOwed calls, failed ones included
}

func newRevokeOwedFake() *revokeOwedFake { return &revokeOwedFake{owed: map[uuid.UUID]time.Time{}} }

func (f *revokeOwedFake) MarkRevokeOwed(_ context.Context, id uuid.UUID) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.marks++
	if f.markErrs > 0 {
		f.markErrs--
		return errors.New("injected: the mark could not be written")
	}
	if _, ok := f.owed[id]; !ok {
		f.owed[id] = time.Now()
	}
	return nil
}

func (f *revokeOwedFake) ClearRevokeOwed(_ context.Context, id uuid.UUID) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.owed, id)
	return nil
}

func (f *revokeOwedFake) RevokeOwedSet(_ context.Context, id uuid.UUID) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.owed[id]
	return ok, nil
}

func (f *revokeOwedFake) ListRevokeOwedAfter(_ context.Context, olderThan time.Duration, after *store.RevokeOwedCursor, limit int) ([]uuid.UUID, *store.RevokeOwedCursor, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	type row struct {
		at time.Time
		id uuid.UUID
	}
	var rows []row
	for id, at := range f.owed {
		if at.After(time.Now().Add(-olderThan)) {
			continue
		}
		if after != nil && !(at.After(after.At) || (at.Equal(after.At) && bytes.Compare(id[:], after.ID[:]) > 0)) {
			continue
		}
		rows = append(rows, row{at, id})
	}
	sort.Slice(rows, func(i, j int) bool {
		if !rows[i].at.Equal(rows[j].at) {
			return rows[i].at.Before(rows[j].at)
		}
		return bytes.Compare(rows[i].id[:], rows[j].id[:]) < 0
	})
	if len(rows) > limit {
		rows = rows[:limit]
	}
	ids := make([]uuid.UUID, len(rows))
	for i, r := range rows {
		ids[i] = r.id
	}
	if len(rows) < limit {
		return ids, nil, nil
	}
	return ids, &store.RevokeOwedCursor{At: rows[len(rows)-1].at, ID: rows[len(rows)-1].id}, nil
}

// setOwed stamps id as owed at the given time, as a revoke that started then.
func (f *revokeOwedFake) setOwed(id uuid.UUID, at time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.owed[id] = at
}

// age moves id's stamp back past the age the worker waits for.
func (f *revokeOwedFake) age(id uuid.UUID) { f.setOwed(id, time.Now().Add(-2*revokeOwedAge)) }

func (f *revokeOwedFake) isOwed(id uuid.UUID) bool {
	ok, _ := f.RevokeOwedSet(context.Background(), id)
	return ok
}

func (f *revokeOwedFake) markCalls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.marks
}

// owedStore is a store with the mark and nothing else: every other Store call would panic on the nil embed.
type owedStore struct {
	store.Store
	*revokeOwedFake
}

func newOwedStore() *owedStore { return &owedStore{revokeOwedFake: newRevokeOwedFake()} }

// flakyBroker fails its next failLeft revokes, then succeeds, and counts the calls per run.
type flakyBroker struct {
	*fakeBroker
	fmu      sync.Mutex
	failLeft int
	calls    map[uuid.UUID]int
}

func newFlakyBroker(fail int) *flakyBroker {
	return &flakyBroker{fakeBroker: &fakeBroker{}, failLeft: fail, calls: map[uuid.UUID]int{}}
}

func (b *flakyBroker) RevokeRun(_ context.Context, id uuid.UUID) error {
	b.fmu.Lock()
	defer b.fmu.Unlock()
	b.calls[id]++
	if b.failLeft > 0 {
		b.failLeft--
		return errors.New("injected: the broker could not revoke")
	}
	return nil
}

func (b *flakyBroker) callsFor(id uuid.UUID) int {
	b.fmu.Lock()
	defer b.fmu.Unlock()
	return b.calls[id]
}

func (b *flakyBroker) total() int {
	b.fmu.Lock()
	defer b.fmu.Unlock()
	n := 0
	for _, c := range b.calls {
		n += c
	}
	return n
}

// revokeOwedServer is a Server over st with brk as its broker. Two of them over one store are a restart.
func revokeOwedServer(t *testing.T, st store.Store, brk *flakyBroker) (*Server, *recRecorder) {
	t.Helper()
	h := newHarness(t)
	cfg := baseTestConfig(h, st)
	cfg.Broker = brk
	return testServer(t, cfg), h.audit
}

func countRevokeAudit(events []types.AuditEvent, runID uuid.UUID, action, outcome string) int {
	n := 0
	for _, ev := range events {
		if ev.Action == action && ev.Outcome == outcome && ev.RunID != nil && *ev.RunID == runID {
			n++
		}
	}
	return n
}

// A revoke that fails is owed in the database, so a server started after the failure retries it from the
// column. An in-memory retry would lose it.
func TestRevoke_FailedRevokeIsRetriedAfterRestart(t *testing.T) {
	ctx := context.Background()
	st, brk, id := newOwedStore(), newFlakyBroker(2), uuid.New()
	srv1, _ := revokeOwedServer(t, st, brk)

	if err := srv1.revokeRunCascade(ctx, id); err == nil {
		t.Fatal("the first revoke succeeded; the broker was set to fail")
	}
	if !st.isOwed(id) {
		t.Fatal("a failed revoke left no mark")
	}
	st.age(id)

	srv2, _ := revokeOwedServer(t, st, brk) // the restart: no state but the store
	cursor := srv2.revokeOwedPass(ctx, nil)
	if !st.isOwed(id) || brk.callsFor(id) != 2 {
		t.Fatalf("after the second failure: owed=%v broker calls=%d, want owed and 2", st.isOwed(id), brk.callsFor(id))
	}
	srv2.revokeOwedPass(ctx, cursor)
	if st.isOwed(id) || brk.callsFor(id) != 3 {
		t.Fatalf("after the third try: owed=%v broker calls=%d, want cleared after 3", st.isOwed(id), brk.callsFor(id))
	}
}

// The mark belongs to the run, not to its sandbox ref: a start that lost to a kill has no ref and is still
// retried.
func TestRevoke_RefLessRunIsRetried(t *testing.T) {
	ctx := context.Background()
	st, brk, id := newOwedStore(), newFlakyBroker(1), uuid.New()
	srv, _ := revokeOwedServer(t, st, brk)

	srv.revokeKilledStart(&queuedStart{base: ctx, run: types.AgentRun{ID: id}})
	if !st.isOwed(id) {
		t.Fatal("a failed revoke of a run with no sandbox ref left no mark")
	}
	st.age(id)
	srv.revokeOwedPass(ctx, nil)
	if st.isOwed(id) {
		t.Fatal("the worker did not retry a run with no sandbox ref")
	}
}

// The boot orphan sweep clears the ref after a teardown whether or not the revoke worked; the revoke stays
// owed and the worker retries it.
func TestRevoke_OrphanSweepClearsRefButRevokeStaysOwed(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	id := uuid.New()
	now := time.Now().UTC()
	orphans := &orphanSweepStore{runs: []types.AgentRun{{
		ID: id, CreatedAt: now, UpdatedAt: now, CreatedBy: "t@example.com",
		ConfinementClass: types.CC1, State: types.RunFailed,
		RunnerTarget: "docker", SandboxRef: "orphan-container-1",
	}}}
	type owedOrphans struct {
		*orphanSweepStore
		*revokeOwedFake
	}
	st := &owedOrphans{orphans, newRevokeOwedFake()}
	brk := newFlakyBroker(1)
	cfg := baseTestConfig(h, st)
	cfg.Runner = &orphanSweepRunner{fakeRunner: &fakeRunner{}}
	cfg.Broker = brk
	srv := testServer(t, cfg)

	if err := srv.reconcileOrphanedSandbox(ctx); err != nil {
		t.Fatalf("reconcileOrphanedSandbox: %v", err)
	}
	orphans.mu.Lock()
	ref := orphans.runs[0].SandboxRef
	orphans.mu.Unlock()
	if ref != "" {
		t.Fatalf("the ref is %q after a confirmed teardown, want it cleared", ref)
	}
	if !st.isOwed(id) {
		t.Fatal("the revoke failed and the ref is gone, and nothing owes the revoke")
	}
	st.age(id)
	srv.revokeOwedPass(ctx, nil)
	if st.isOwed(id) {
		t.Fatal("the worker did not retry the revoke the orphan sweep left owed")
	}
}

// A revoke that keeps failing writes its run.revoke row once per process, however many ticks retry it.
func TestRevoke_FailureRowOncePerProcess(t *testing.T) {
	ctx := context.Background()
	st, brk, id := newOwedStore(), newFlakyBroker(math.MaxInt32), uuid.New()
	srv, audit := revokeOwedServer(t, st, brk)
	st.age(id)

	var cursor *store.RevokeOwedCursor
	for range 3 {
		cursor = srv.revokeOwedPass(ctx, cursor)
	}
	if brk.callsFor(id) != 3 {
		t.Fatalf("broker calls = %d, want one per tick (3)", brk.callsFor(id))
	}
	if n := countRevokeAudit(audit.snapshot(), id, "run.revoke", "failure"); n != 1 {
		t.Fatalf("%d run.revoke failure rows after 3 ticks, want 1", n)
	}
}

// owedPATStore is a store with the mark and one Azure DevOps token it can never close, so a revoke always
// finds a token revoke pending.
type owedPATStore struct {
	store.Store
	*revokeOwedFake
}

func (owedPATStore) InsertRunPAT(context.Context, store.RunPAT) error { return nil }
func (owedPATStore) MarkRunPATRevoked(context.Context, uuid.UUID, uuid.UUID, string, string) (bool, error) {
	return false, nil
}
func (owedPATStore) NoteRunPATRevokeFailed(context.Context, uuid.UUID, uuid.UUID, string) error {
	return nil
}
func (owedPATStore) ListUnrevokedRunPATs(_ context.Context, f store.RunPATFilter) ([]store.RunPAT, error) {
	return []store.RunPAT{{RunID: f.RunID, AuthorizationID: uuid.New(), ValidTo: time.Now().Add(time.Hour)}}, nil
}

// A pending token revoke alone is the token sweep's, so the revoke is done and the mark goes; with another
// step failing too, the mark stays.
func TestRevoke_PendingPATOnlyClearsTheMark(t *testing.T) {
	ctx := context.Background()
	st := &owedPATStore{revokeOwedFake: newRevokeOwedFake()}
	brk, id := newFlakyBroker(0), uuid.New()
	srv, _ := revokeOwedServer(t, st, brk)

	if err := srv.revokeRunCascade(ctx, id); err != nil {
		t.Fatalf("a revoke with only a token revoke pending = %v, want nil", err)
	}
	if st.isOwed(id) {
		t.Fatal("a revoke with only a token revoke pending left the mark")
	}

	brk.fmu.Lock()
	brk.failLeft = 1
	brk.fmu.Unlock()
	if err := srv.revokeRunCascade(ctx, id); err == nil {
		t.Fatal("a failed broker revoke returned nil")
	}
	if !st.isOwed(id) {
		t.Fatal("a failed broker revoke beside a pending token revoke cleared the mark")
	}
}

// Rows that keep failing cannot starve a newer one: the cursor moves past a full page.
func TestRevoke_FailingRowsDoNotStarveNewer(t *testing.T) {
	ctx := context.Background()
	st, brk := newOwedStore(), newFlakyBroker(math.MaxInt32)
	srv, _ := revokeOwedServer(t, st, brk)
	base := time.Now().Add(-3 * time.Hour)
	for i := range revokeOwedPage + 10 {
		st.setOwed(uuid.New(), base.Add(time.Duration(i)*time.Second))
	}
	newer := uuid.New()
	st.setOwed(newer, time.Now().Add(-2*revokeOwedAge))

	cursor := srv.revokeOwedPass(ctx, nil)
	if cursor == nil || brk.callsFor(newer) != 0 || brk.total() != revokeOwedPage {
		t.Fatalf("tick 1: cursor=%v newer tried=%d total=%d, want a cursor, 0 and %d", cursor, brk.callsFor(newer), brk.total(), revokeOwedPage)
	}
	srv.revokeOwedPass(ctx, cursor)
	if brk.callsFor(newer) == 0 {
		t.Fatalf("tick 2 did not reach the newer row; %d tries in all", brk.total())
	}
}

// The first mark fails and the steps fail: the second mark, written after them, makes the revoke owed. With
// both failing the run is not marked, one error is logged, and nothing panics.
func TestRevoke_MarkRetriedAfterFailedSteps(t *testing.T) {
	ctx := context.Background()
	st, brk, id := newOwedStore(), newFlakyBroker(1), uuid.New()
	srv, _ := revokeOwedServer(t, st, brk)

	st.markErrs = 1
	if err := srv.revokeRunCascade(ctx, id); err == nil {
		t.Fatal("the revoke succeeded; the broker was set to fail")
	}
	if !st.isOwed(id) || st.markCalls() != 2 {
		t.Fatalf("owed=%v mark calls=%d, want owed after the second mark (2 calls)", st.isOwed(id), st.markCalls())
	}
	st.age(id)
	srv.revokeOwedPass(ctx, nil)
	if st.isOwed(id) {
		t.Fatal("the worker did not retry a run marked by the second mark")
	}

	logs := &lockedBuffer{}
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(logs, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	other := uuid.New()
	st.markErrs, brk.failLeft = 2, 1
	if err := srv.revokeRunCascade(ctx, other); err == nil {
		t.Fatal("the revoke succeeded; the broker was set to fail")
	}
	if st.isOwed(other) {
		t.Fatal("a run is marked though both marks failed")
	}
	n := 0
	for _, line := range strings.Split(logs.String(), "\n") {
		if strings.Contains(line, "could not be recorded") && strings.Contains(line, "level=ERROR") && strings.Contains(line, other.String()) {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("%d error logs for the run whose marks both failed, want 1; log:\n%s", n, logs.String())
	}
}

// killRefStore serves a run for a kill and records the writes to its ref.
type killRefStore struct {
	killRekillStore
	mu   sync.Mutex
	sets []string
}

func (s *killRefStore) SetSandboxRef(_ context.Context, _ uuid.UUID, ref string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sets = append(s.sets, ref)
	return nil
}

func (s *killRefStore) ClearSandboxRefRetaining(context.Context, uuid.UUID) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sets = append(s.sets, "")
	return nil
}

func (s *killRefStore) writes() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.sets)
}

// killErrRunner answers KillSandbox with err.
type killErrRunner struct {
	*fakeRunner
	err error
}

func (r killErrRunner) KillSandbox(context.Context, string) error { return r.err }

func killRefServer(t *testing.T, kerr error) (*Server, *killRefStore) {
	t.Helper()
	h := newHarness(t)
	st := &killRefStore{killRekillStore: killRekillStore{run: types.AgentRun{ID: uuid.New(), State: types.RunKilled, SandboxRef: "sbx-1"}}}
	cfg := baseTestConfig(h, st)
	cfg.Runner = killErrRunner{&fakeRunner{}, kerr}
	return testServer(t, cfg), st
}

// A kill the runner refused leaves the ref: the sandbox may still exist.
func TestKill_FailedKillKeepsRef(t *testing.T) {
	srv, st := killRefServer(t, errors.New("injected: the runner could not kill it"))
	data := srv.killTeardownTail(context.Background(), st.run, types.ActorSystem, "test", nil, true)
	if data["runner_error"] == nil {
		t.Fatalf("kill data = %v, want the runner error", data)
	}
	if w := st.writes(); len(w) != 0 {
		t.Fatalf("a failed kill wrote the ref %q, want no write", w)
	}
}

// A kill queued for an offline runner is not confirmed: the ref stays until the runner reports.
func TestKill_QueuedKillKeepsRef(t *testing.T) {
	srv, st := killRefServer(t, fmt.Errorf("%w: queued", runner.ErrPendingOnRunner))
	data := srv.killTeardownTail(context.Background(), st.run, types.ActorSystem, "test", nil, true)
	if len(data) != 0 {
		t.Fatalf("kill data = %v, want a queued kill to count as a clean teardown", data)
	}
	if w := st.writes(); len(w) != 0 {
		t.Fatalf("a queued kill wrote the ref %q, want no write", w)
	}
}

// A confirmed kill clears the ref once, through the retaining clear; the two tests above see no write.
func TestKill_ConfirmedKillClearsRef(t *testing.T) {
	srv, st := killRefServer(t, nil)
	if data := srv.killTeardownTail(context.Background(), st.run, types.ActorSystem, "test", nil, true); data["runner_error"] != nil {
		t.Fatalf("kill data = %v, want a confirmed kill", data)
	}
	if w := st.writes(); !slices.Equal(w, []string{""}) {
		t.Fatalf("ref writes = %q, want one clear", w)
	}
}

// killRecordStore is recordStore whose run keeps a ref that SetSandboxRef or the retaining clear clears.
type killRecordStore struct {
	*recordStore
	mu sync.Mutex
}

func (s *killRecordStore) GetRun(context.Context, uuid.UUID) (types.AgentRun, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.recordStore.run, nil
}

func (s *killRecordStore) SetSandboxRef(_ context.Context, _ uuid.UUID, ref string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.recordStore.run.SandboxRef = ref
	return nil
}

func (s *killRecordStore) ClearSandboxRefRetaining(context.Context, uuid.UUID) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.recordStore.run.SandboxRef = ""
	return nil
}

// A record run killed with an empty capture reached its sandbox, so it is not labelled a dispatch failure,
// and the ref is cleared only after the capture read it.
func TestKill_ClearsRefAfterReconcileRecordRun(t *testing.T) {
	h := newHarness(t)
	runID, wsID := uuid.New(), uuid.New()
	rec := &recordStore{
		run:             types.AgentRun{ID: runID, WorkspaceID: &wsID, Task: "workspace record", State: types.RunKilled, SandboxRef: "agent-run-abc"},
		importStateFake: importStateFake{ws: recordingWorkspace(wsID, runID, "build")},
	}
	st := &killRecordStore{recordStore: rec}
	cfg := baseTestConfig(h, st)
	cfg.Runner = killErrRunner{&fakeRunner{}, nil}
	srv := testServer(t, cfg)

	srv.killTeardownTail(context.Background(), rec.run, types.ActorSystem, "test", nil, true)

	if res := rec.savedResult(t, "build"); res.FailureHint != recordEmptyCaptureHint {
		t.Errorf("failure hint = %q, want the empty-capture hint of a run that had a sandbox", res.FailureHint)
	}
	st.mu.Lock()
	ref := rec.run.SandboxRef
	st.mu.Unlock()
	if ref != "" {
		t.Errorf("the ref is %q after a confirmed kill, want it cleared", ref)
	}
}

// goFilesOfAPI parses every non-test source file of this package.
func goFilesOfAPI(t *testing.T) map[string]*ast.File {
	t.Helper()
	paths, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]*ast.File{}
	for _, p := range paths {
		if strings.HasSuffix(p, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(token.NewFileSet(), p, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", p, err)
		}
		out[p] = f
	}
	return out
}

// callsOf walks every function of every file and calls visit for each call of a method named name, with the
// path from the function down to the call.
func callsOf(files map[string]*ast.File, name string, visit func(file, fn string, path []ast.Node)) {
	for file, f := range files {
		for _, d := range f.Decls {
			fd, ok := d.(*ast.FuncDecl)
			if !ok || fd.Body == nil {
				continue
			}
			var path []ast.Node
			ast.Inspect(fd, func(n ast.Node) bool {
				if n == nil {
					path = path[:len(path)-1]
					return true
				}
				path = append(path, n)
				if call, ok := n.(*ast.CallExpr); ok {
					if sel, ok := call.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == name {
						visit(file, fd.Name.Name, slices.Clone(path))
					}
				}
				return true
			})
		}
	}
}

func mentionsAll(e ast.Expr, names []string) bool {
	seen := map[string]bool{}
	ast.Inspect(e, func(n ast.Node) bool {
		if id, ok := n.(*ast.Ident); ok {
			seen[id.Name] = true
		}
		return true
	})
	for _, n := range names {
		if !seen[n] {
			return false
		}
	}
	return true
}

// dominatedBy reports whether the call at the end of path runs only when a condition naming all of guards
// held: inside the body of an if on them, or after an if on them whose body returns.
func dominatedBy(path []ast.Node, guards []string) bool {
	for i, n := range path[:len(path)-1] {
		switch n := n.(type) {
		case *ast.IfStmt:
			if path[i+1] == ast.Node(n.Body) && mentionsAll(n.Cond, guards) {
				return true
			}
		case *ast.BlockStmt:
			for _, st := range n.List {
				if ast.Node(st) == path[i+1] {
					break
				}
				if is, ok := st.(*ast.IfStmt); ok && mentionsAll(is.Cond, guards) && len(is.Body.List) > 0 {
					if _, ret := is.Body.List[len(is.Body.List)-1].(*ast.ReturnStmt); ret {
						return true
					}
				}
			}
		}
	}
	return false
}

// A ref means "a sandbox may exist". It is cleared in four places, each after a confirmed teardown; a fifth,
// or one that lost its condition, would drop the only record of a sandbox still running. Three go through
// clearSandboxRef; killTeardownTail calls ClearSandboxRefRetaining on the store it asserted to
// sandboxRefRetainingClearer.
func TestSandboxRefClearCensus(t *testing.T) {
	type site struct {
		call   string
		guards []string
	}
	frozen := map[string]site{
		"run_kept_stop_forced.go:retryForcedStop":    {"clearSandboxRef", []string{"stopped"}},
		"reconcile.go:reconcileOrphanedRun":          {"clearSandboxRef", []string{"torn"}},
		"run_terminal_effects.go:finalizeRunEffects": {"clearSandboxRef", []string{"teardownOK"}},
		"runs_lifecycle.go:killTeardownTail":         {"ClearSandboxRefRetaining", []string{"kerr", "queued"}},
	}
	files := goFilesOfAPI(t)
	found := map[string]bool{}
	for _, call := range []string{"clearSandboxRef", "ClearSandboxRefRetaining"} {
		callsOf(files, call, func(file, fn string, path []ast.Node) {
			key := file + ":" + fn
			if call == "ClearSandboxRefRetaining" && key == "run_terminal_effects.go:clearSandboxRef" {
				return // clearSandboxRef's own store.PG branch
			}
			want, ok := frozen[key]
			if !ok || want.call != call {
				t.Errorf("%s clears a run's sandbox ref with %s and is not in the frozen list: the ref means a sandbox may exist, so add a clear only after a confirmed teardown, and add it here in the same change", key, call)
				return
			}
			found[key] = true
			if !dominatedBy(path, want.guards) {
				t.Errorf("%s clears the ref outside a condition on %v: it must run only after a confirmed teardown", key, want.guards)
			}
			if call == "ClearSandboxRefRetaining" && !assertedTo(path, "sandboxRefRetainingClearer") {
				t.Errorf("%s calls ClearSandboxRefRetaining outside an if that asserts the store to sandboxRefRetainingClearer", key)
			}
		})
	}
	if len(frozen) != 4 {
		t.Errorf("the frozen list has %d clears, want four", len(frozen))
	}
	for key := range frozen {
		if !found[key] {
			t.Errorf("%s is in the frozen list and no longer clears the ref: remove it from the list", key)
		}
	}
}

// assertedTo reports whether the call at the end of path is inside the body of an if whose init asserts a
// value to the named interface.
func assertedTo(path []ast.Node, iface string) bool {
	for i, n := range path[:len(path)-1] {
		is, ok := n.(*ast.IfStmt)
		if !ok || path[i+1] != ast.Node(is.Body) {
			continue
		}
		as, ok := is.Init.(*ast.AssignStmt)
		if !ok || len(as.Rhs) != 1 {
			continue
		}
		if ta, ok := as.Rhs[0].(*ast.TypeAssertExpr); ok {
			if id, ok := ta.Type.(*ast.Ident); ok && id.Name == iface {
				return true
			}
		}
	}
	return false
}

// revokeRunCascade writes and clears the revoke owed mark itself, so a caller needs nothing more to be
// retried. The callers are frozen: a new one is already covered.
func TestRevokeCascadeCallers_Frozen(t *testing.T) {
	frozen := []string{
		"reconcile.go:reconcileOrphanedSandbox",
		"run_kept_stop_forced.go:clearForcedMarkIf",
		"run_queue_start.go:revokeKilledStart",
		"run_revoke_queue.go:retryOwedRevoke",
		"run_terminal_effects.go:finalizeRunEffects",
		"runs_lifecycle.go:failedRunEffects",
		"sandbox_sweep.go:sweepTerminalSandboxRun",
	}
	found := map[string]bool{}
	callsOf(goFilesOfAPI(t), "revokeRunCascade", func(file, fn string, _ []ast.Node) {
		key := file + ":" + fn
		found[key] = true
		if !slices.Contains(frozen, key) {
			t.Errorf("%s calls revokeRunCascade and is not in the frozen list: a revoke started anywhere is already covered by the mark revokeRunCascade writes and the worker retries, so a new call site needs no retry of its own; add it here only if it needs the revoke itself", key)
		}
	})
	for _, key := range frozen {
		if !found[key] {
			t.Errorf("%s is in the frozen list and no longer calls revokeRunCascade: remove it from the list", key)
		}
	}
}
