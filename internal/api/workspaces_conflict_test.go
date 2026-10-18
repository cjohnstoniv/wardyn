// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// refusingWorkspaceStore answers every workspace write with err, as the PG store
// does when its fenced UPDATE or its UNIQUE(name) loses.
type refusingWorkspaceStore struct {
	*workspaceStoreFake
	err error
}

func (s *refusingWorkspaceStore) CreateWorkspace(context.Context, types.Workspace) (types.Workspace, error) {
	return types.Workspace{}, s.err
}

func (s *refusingWorkspaceStore) UpdateWorkspace(context.Context, uuid.UUID, types.Workspace, bool) (types.Workspace, error) {
	return types.Workspace{}, s.err
}

func workspaceWriteReason(t *testing.T, method, path, body string, storeErr error) (int, string) {
	t.Helper()
	h := newHarness(t)
	id := uuid.New()
	fake := &refusingWorkspaceStore{
		workspaceStoreFake: &workspaceStoreFake{ws: types.Workspace{
			ID: id, Name: "w", Status: types.WorkspaceScanned,
			Sources: []types.WorkspaceSource{{Type: types.WorkspaceSourceTypeLocalDir, Path: "/home/u/old"}},
		}},
		err: storeErr,
	}
	srv := New(baseTestConfig(h, fake))
	if method == http.MethodPut {
		path += "/" + id.String()
	}
	w := do(t, srv, method, path, adminToken, body)
	var out struct {
		Reason string `json:"reason"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	return w.Code, out.Reason
}

// A taken name is the caller's to change: 409 with a reason, not the bare 500.
func TestWorkspaceWrite_TakenNameIs409(t *testing.T) {
	body := `{"name":"w","kind":"local_dir","source":"/home/u/repo"}`
	for _, m := range []string{http.MethodPost, http.MethodPut} {
		code, reason := workspaceWriteReason(t, m, "/api/v1/workspaces", body, store.ErrConflict)
		if code != http.StatusConflict || reason != reasonWorkspaceNameTaken {
			t.Errorf("%s with a taken name = %d %q, want 409 %q", m, code, reason, reasonWorkspaceNameTaken)
		}
	}
}

// An edit built from a snapshot the store refused as stale is a conflict the
// caller retries, not a server fault and not a name clash.
func TestUpdateWorkspace_StaleSnapshotIs409(t *testing.T) {
	code, reason := workspaceWriteReason(t, http.MethodPut, "/api/v1/workspaces",
		`{"name":"w2","kind":"local_dir","source":"/home/u/old"}`, store.ErrWorkspaceStale)
	if code != http.StatusConflict || reason != reasonWorkspaceUpdateConflict {
		t.Errorf("stale edit = %d %q, want 409 %q", code, reason, reasonWorkspaceUpdateConflict)
	}
}

// Editing a workspace's sources clears what was reviewed against the old ones, but a
// record session still running keeps its entry: the capture that settles it is written
// there, and dropping it loses the recording and strands active_run_id.
func TestUpdateWorkspace_SourceChangeKeepsAnInFlightRecording(t *testing.T) {
	h := newHarness(t)
	id := uuid.New()
	run := uuid.New()
	fake := &workspaceStoreFake{ws: types.Workspace{
		ID: id, Name: "w", Status: types.WorkspaceScanned,
		Sources: []types.WorkspaceSource{{Type: types.WorkspaceSourceTypeLocalDir, Path: "/home/u/old"}},
		RecordResults: mustJSON(map[string]RecordTaskResult{
			"live": {Status: recordStatusRecording, RunID: run},
			"done": {Status: "done", RunID: uuid.New()},
		}),
	}}
	srv := New(baseTestConfig(h, fake))
	w := do(t, srv, http.MethodPut, "/api/v1/workspaces/"+id.String(), adminToken,
		`{"name":"w","kind":"local_dir","source":"/home/u/new"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	kept := recordResultsMap(fake.updated)
	if len(kept) != 1 || kept["live"].RunID != run || kept["live"].Status != recordStatusRecording {
		t.Errorf("record results after the source edit = %+v, want only the in-flight session", kept)
	}
}

func TestInFlightRecordResults(t *testing.T) {
	if got := inFlightRecordResults(nil); got != nil {
		t.Errorf("nil in = %s", got)
	}
	if got := inFlightRecordResults(json.RawMessage(`not json`)); got != nil {
		t.Errorf("garbage in = %s", got)
	}
	if got := inFlightRecordResults(mustJSON(map[string]RecordTaskResult{"a": {Status: "done"}})); got != nil {
		t.Errorf("no recording entry in, got %s", got)
	}
}

// repairFakeStore backs repairStaleWorkspaceRuns's pointer arm.
type repairFakeStore struct {
	store.Store
	run     types.AgentRun
	ws      types.Workspace
	cleared []uuid.UUID
}

func (s *repairFakeStore) GetRun(context.Context, uuid.UUID) (types.AgentRun, error) {
	return s.run, nil
}
func (s *repairFakeStore) GetWorkspace(context.Context, uuid.UUID) (types.Workspace, error) {
	return s.ws, nil
}
func (s *repairFakeStore) QueryAuditEvents(context.Context, uuid.UUID, int) ([]types.AuditEvent, error) {
	return nil, errors.New("transient") // reconcileRecordRun leaves the entry `recording` and returns
}
func (s *repairFakeStore) ClearWorkspaceActiveRun(_ context.Context, _, runID uuid.UUID) (bool, error) {
	s.cleared = append(s.cleared, runID)
	s.ws.ActiveRunID = nil
	return true, nil
}

// After a sources edit dropped its entry (or a clear failed), a workspace pointing at a
// terminal run that owns no recording entry could never be deleted again. Reading it
// repairs the pointer; a live run, or one still owning an entry, is left alone.
func TestRepairStaleWorkspaceRuns_ClearsAPointerToATerminalRunWithNoEntry(t *testing.T) {
	h := newHarness(t)
	run := uuid.New()
	mk := func(state types.RunState, results json.RawMessage) (*repairFakeStore, types.Workspace) {
		ws := types.Workspace{ID: uuid.New(), Name: "w", Status: types.WorkspaceScanned, ActiveRunID: &run, RecordResults: results}
		st := &repairFakeStore{run: types.AgentRun{ID: run, State: state, Task: "workspace record"}, ws: ws}
		return st, ws
	}

	st, ws := mk(types.RunKilled, nil)
	got := New(baseTestConfig(h, st)).repairStaleWorkspaceRuns(context.Background(), ws)
	if len(st.cleared) != 1 || st.cleared[0] != run || got.ActiveRunID != nil {
		t.Errorf("terminal run, no entry: cleared=%v active=%v, want the pointer cleared", st.cleared, got.ActiveRunID)
	}

	st, ws = mk(types.RunRunning, nil)
	New(baseTestConfig(h, st)).repairStaleWorkspaceRuns(context.Background(), ws)
	if len(st.cleared) != 0 {
		t.Errorf("a live run's pointer was cleared: %v", st.cleared)
	}

	st, ws = mk(types.RunKilled, mustJSON(map[string]RecordTaskResult{"t": {Status: recordStatusRecording, RunID: run}}))
	New(baseTestConfig(h, st)).repairStaleWorkspaceRuns(context.Background(), ws)
	for _, c := range st.cleared {
		t.Errorf("a pointer whose run still owns a recording entry was cleared directly (%s): the reconcile owns that path", c)
	}
}

// staleStore refuses the first stale UpdateWorkspace calls as the PG store does when a
// concurrent writer stamped updated_at, and lands that writer's change in the row the
// next read returns.
type staleStore struct {
	*workspaceStoreFake
	stale int // how many updates to refuse
	calls int
	run   uuid.UUID
}

func (s *staleStore) UpdateWorkspace(ctx context.Context, id uuid.UUID, ws types.Workspace, stamp bool) (types.Workspace, error) {
	s.calls++
	if s.calls <= s.stale {
		s.ws.ActiveRunID = &s.run // the concurrent claim, visible to the next read
		return types.Workspace{}, store.ErrWorkspaceStale
	}
	return s.workspaceStoreFake.UpdateWorkspace(ctx, id, ws, stamp)
}

func staleEdit(t *testing.T, stale int) (code int, st *staleStore, rr *imageRemoverRunner) {
	t.Helper()
	h := newHarness(t)
	id := uuid.New()
	st = &staleStore{stale: stale, run: uuid.New(), workspaceStoreFake: &workspaceStoreFake{ws: types.Workspace{
		ID: id, Name: "w", Status: types.WorkspaceScanned,
		Sources:  []types.WorkspaceSource{{Type: types.WorkspaceSourceTypeLocalDir, Path: "/home/u/old"}},
		ImageRef: "wardyn/ws:abc", BuiltProfileHash: "abc",
	}}}
	cfg := baseTestConfig(h, st)
	rr = &imageRemoverRunner{fakeRunner: &fakeRunner{}}
	cfg.Runner = rr
	w := do(t, New(cfg), http.MethodPut, "/api/v1/workspaces/"+id.String(), adminToken,
		`{"name":"w","kind":"local_dir","source":"/home/u/new"}`)
	return w.Code, st, rr
}

// One concurrent stamp between the read and the write is not the caller's problem: the
// edit is redone on the fresh row, lands, keeps the concurrent write, and the superseded
// image is reclaimed exactly once.
func TestUpdateWorkspace_OneConcurrentStampIsRetriedAndKept(t *testing.T) {
	code, st, rr := staleEdit(t, 1)
	if code != http.StatusOK {
		t.Fatalf("code = %d, want 200 after one retry", code)
	}
	if st.calls != 2 {
		t.Errorf("UpdateWorkspace calls = %d, want 2", st.calls)
	}
	if st.updated.ActiveRunID == nil || *st.updated.ActiveRunID != st.run {
		t.Errorf("the concurrent claim was reverted: active_run_id = %v, want %s", st.updated.ActiveRunID, st.run)
	}
	rr.mu.Lock()
	defer rr.mu.Unlock()
	if len(rr.removed) != 1 || rr.removed[0] != "wardyn/ws:abc" {
		t.Errorf("ImageRemove calls = %v, want exactly [wardyn/ws:abc]", rr.removed)
	}
}

// A writer that wins every time gets the 409 after the bounded attempts, and the image
// the row still points at is left alone.
func TestUpdateWorkspace_StaleEveryTimeIs409AfterThreeAttempts(t *testing.T) {
	code, st, rr := staleEdit(t, 99)
	if code != http.StatusConflict {
		t.Fatalf("code = %d, want 409", code)
	}
	if st.calls != workspaceEditAttempts {
		t.Errorf("UpdateWorkspace calls = %d, want %d", st.calls, workspaceEditAttempts)
	}
	rr.mu.Lock()
	defer rr.mu.Unlock()
	if len(rr.removed) != 0 {
		t.Errorf("a refused edit reclaimed an image the row still points at: %v", rr.removed)
	}
}
