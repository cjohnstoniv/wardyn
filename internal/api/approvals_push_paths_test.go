// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/approval"
	"github.com/cjohnstoniv/wardyn/internal/audit"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// heldPush is a raise for a push matching n review paths, acting as actsAs: the
// scope the sidecar builds (ten paths, the exact count, the digest over all n)
// and the complete path list beside it.
func heldPush(n int, actsAs string) (types.PushContentScope, types.PushPathList) {
	paths := make([]string, n)
	for i := range paths {
		paths[i] = fmt.Sprintf(".github/workflows/w%05d.yml", i)
	}
	return types.PushContentScope{
		Repo: "github.com/octocat/hello-world", Branch: "refs/heads/wardyn/run/work", ActsAs: actsAs,
		Paths: paths[:min(n, types.PushContentMaxPaths)], PathsTotal: n,
		Commits: []string{strings.Repeat("a", 40)}, PathsDigest: types.PushPathsDigest(paths),
	}, types.NewPushPathList(paths)
}

func raiseBody(kind types.ApprovalKind, scope any, list *types.PushPathList) string {
	return string(mustJSON(map[string]any{"kind": kind, "requested_scope": scope, "path_list": list}))
}

// pathListStore is grantStore plus the path-list capability store.PG has.
type pathListStore struct {
	grantStore
	mu    sync.Mutex
	lists map[uuid.UUID]types.PushPathList
	runOf func(uuid.UUID) uuid.UUID
}

func (s *pathListStore) RecordPushPathList(_ context.Context, id uuid.UUID, l types.PushPathList) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.lists[id]; ok {
		return false, nil
	}
	s.lists[id] = l
	return true, nil
}

func (s *pathListStore) GetPushPathList(_ context.Context, id uuid.UUID) (types.PushPathList, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	l, ok := s.lists[id]
	if !ok {
		return types.PushPathList{}, store.ErrNotFound
	}
	return l, nil
}

func (s *pathListStore) CountPushPathLists(_ context.Context, runID uuid.UUID) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for id := range s.lists {
		if s.runOf(id) == runID {
			n++
		}
	}
	return n, nil
}

// TestPushPathListRaise: a raise's path list is verified against its scope,
// then stored with the approval and written into the run's audit trail.
// (a) 150 paths store 150; (b) 10,001 store the first 10,000, truncated;
// (c) a list the scope does not vouch for is a 400 that writes nothing.
func TestPushPathListRaise(t *testing.T) {
	h := newHarness(t)
	ast := newAuthzStore()
	runID, app := uuid.New(), uuid.New()
	aap := newAuthzApprovals(ast)
	st := &pathListStore{
		grantStore: grantStore{authzStore: ast, grants: []types.CredentialGrant{
			{ID: app, RunID: runID, Spec: types.GrantSpec{Kind: types.GrantGitHubToken}},
		}},
		lists: map[uuid.UUID]types.PushPathList{},
		runOf: func(id uuid.UUID) uuid.UUID { ap, _ := aap.Get(context.Background(), id); return ap.RunID },
	}
	cfg := baseTestConfig(h, st)
	cfg.Approvals = aap
	srv := New(cfg)
	ast.mu.Lock()
	ast.runs[runID] = types.AgentRun{ID: runID, State: types.RunRunning, Interactive: true, CreatedBy: "alice@example.com"}
	ast.mu.Unlock()
	token := h.mintRunToken(t, runID)
	actsAs := "github_token:" + app.String()

	raise := func(body string) (int, uuid.UUID) {
		w := do(t, srv, http.MethodPost, "/api/v1/internal/approvals", token, body)
		var ap types.ApprovalRequest
		_ = json.Unmarshal(w.Body.Bytes(), &ap)
		return w.Code, ap.ID
	}
	read := func(id uuid.UUID) types.PushPathList {
		w := do(t, srv, http.MethodGet, "/api/v1/approvals/"+id.String()+"/paths", adminToken, "")
		var l types.PushPathList
		if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &l) != nil {
			t.Fatalf("GET paths: status %d body %.200s", w.Code, w.Body.String())
		}
		return l
	}

	// (a)
	scope, list := heldPush(150, actsAs)
	code, id := raise(raiseBody(types.ApprovalPushContent, scope, &list))
	if code != http.StatusCreated {
		t.Fatalf("150-path raise: status %d, want 201", code)
	}
	if got := read(id); !slices.Equal(got.Paths, list.Paths) || got.Truncated {
		t.Errorf("(a) read back %d paths truncated=%v, want the 150 raised", len(got.Paths), got.Truncated)
	}
	var recorded []types.AuditEvent
	for _, ev := range h.audit.snapshot() {
		if ev.Action == "approval.push_paths.record" {
			recorded = append(recorded, ev)
		}
	}
	var data struct {
		Paths       []string `json:"paths"`
		PathsDigest string   `json:"paths_digest"`
	}
	if len(recorded) != 1 || recorded[0].Target != id.String() || json.Unmarshal(recorded[0].Data, &data) != nil ||
		types.PushPathsDigest(data.Paths) != scope.PathsDigest {
		t.Errorf("(a) audit rows %+v, want one naming the approval with the 150 paths", recorded)
	}

	// (b)
	scope, list = heldPush(types.PushPathListMaxPaths+1, actsAs)
	if code, id = raise(raiseBody(types.ApprovalPushContent, scope, &list)); code != http.StatusCreated {
		t.Fatalf("10,001-path raise: status %d, want 201", code)
	}
	if got := read(id); len(got.Paths) != types.PushPathListMaxPaths || !got.Truncated {
		t.Errorf("(b) stored %d paths truncated=%v, want 10000 truncated", len(got.Paths), got.Truncated)
	}

	// (c)
	scope, list = heldPush(150, actsAs)
	lie := func(f func(*types.PushPathList)) *types.PushPathList {
		l := types.PushPathList{Paths: slices.Clone(list.Paths)}
		f(&l)
		return &l
	}
	rowsBefore, listsBefore := len(aap.byID), len(st.lists)
	for name, body := range map[string]string{
		"a path swapped":      raiseBody(types.ApprovalPushContent, scope, lie(func(l *types.PushPathList) { l.Paths[149] = ".github/workflows/zzz.yml" })),
		"a path dropped":      raiseBody(types.ApprovalPushContent, scope, lie(func(l *types.PushPathList) { l.Paths = l.Paths[:149] })),
		"unsorted":            raiseBody(types.ApprovalPushContent, scope, lie(func(l *types.PushPathList) { l.Paths[0], l.Paths[1] = l.Paths[1], l.Paths[0] })),
		"truncated but whole": raiseBody(types.ApprovalPushContent, scope, lie(func(l *types.PushPathList) { l.Truncated = true })),
		"truncated off the card's paths": raiseBody(types.ApprovalPushContent, scope,
			lie(func(l *types.PushPathList) { l.Paths, l.Truncated = l.Paths[1:5], true })),
		"on an egress raise": raiseBody(types.ApprovalEgressDomain, map[string]string{"host": "example.com"}, &list),
	} {
		if code, _ := raise(body); code != http.StatusBadRequest {
			t.Errorf("(c) %s: status %d, want 400", name, code)
		}
	}
	if len(aap.byID) != rowsBefore || len(st.lists) != listsBefore {
		t.Errorf("(c) refused raises wrote rows: approvals %d -> %d, lists %d -> %d",
			rowsBefore, len(aap.byID), listsBefore, len(st.lists))
	}
}

// pgApprovalService is cmd/wardynd's approvalService over the real store.
type pgApprovalService struct {
	store.PG
	rec audit.Recorder
}

func (a pgApprovalService) Record(ctx context.Context, ev types.AuditEvent) error {
	return a.rec.Record(ctx, ev)
}
func (a pgApprovalService) Request(ctx context.Context, req types.ApprovalRequest) (types.ApprovalRequest, error) {
	return approval.RequestApproval(ctx, a, req)
}
func (a pgApprovalService) Decide(ctx context.Context, id uuid.UUID, by types.ActorType, d types.ApprovalDecision) (types.ApprovalRequest, error) {
	return approval.Decide(ctx, a, id, by, d)
}
func (a pgApprovalService) Get(ctx context.Context, id uuid.UUID) (types.ApprovalRequest, error) {
	return a.GetApproval(ctx, id)
}
func (a pgApprovalService) List(ctx context.Context, state types.ApprovalState) ([]types.ApprovalRequest, error) {
	return a.ListApprovals(ctx, state)
}
func (a pgApprovalService) CancelForRun(ctx context.Context, runID uuid.UUID, reason string) (map[string]int, error) {
	return approval.CancelForRun(ctx, a, runID, reason)
}
func (a pgApprovalService) ExpireOne(ctx context.Context, id uuid.UUID, actor, reason string) error {
	return approval.ExpireOne(ctx, a, id, actor, reason)
}
func (a pgApprovalService) CountForRun(ctx context.Context, runID uuid.UUID) (int, error) {
	return a.CountApprovalsForRun(ctx, runID)
}

// TestPG_PushPathListOutlivesTheRun drives the whole chain through Postgres:
// (a) a 150-path raise stores 150 in push_content_paths; (c) a list off the
// digest leaves no approval row and no list row; (d) once the run is terminal
// and its approval cancelled, GET /approvals/{id}/paths and the run's audit
// export both still return the same 150. The row refuses UPDATE, DELETE and
// TRUNCATE.
func TestPG_PushPathListOutlivesTheRun(t *testing.T) {
	pool := throwawayPGPool(t)
	ctx := context.Background()
	pg := store.NewPG(pool)
	h := newHarness(t)
	cfg := baseTestConfig(h, pg)
	cfg.Audit = store.Recorder{Pool: pool}
	cfg.Approvals = pgApprovalService{PG: pg, rec: cfg.Audit}
	srv := New(cfg)

	now := time.Now().UTC()
	runID, grantID := uuid.New(), uuid.New()
	if _, err := pg.CreateRun(ctx, types.AgentRun{
		ID: runID, CreatedAt: now, UpdatedAt: now, CreatedBy: "alice@example.com", Agent: "claude-code",
		Repo: "octocat/hello-world", ConfinementClass: types.CC2, State: types.RunRunning, Interactive: true,
		SPIFFEID: "spiffe://wardyn.local/agent-run/" + runID.String(), RunnerTarget: "docker",
	}); err != nil {
		t.Fatalf("create run: %v", err)
	}
	if _, err := pg.CreateGrant(ctx, types.CredentialGrant{ID: grantID, RunID: runID, CreatedAt: now,
		Spec: types.GrantSpec{Kind: types.GrantGitHubToken}}); err != nil {
		t.Fatalf("create grant: %v", err)
	}
	token := h.mintRunToken(t, runID)
	scope, list := heldPush(150, "github_token:"+grantID.String())

	// (c) first, on an empty run: nothing may land.
	bad := types.PushPathList{Paths: slices.Clone(list.Paths)}
	bad.Paths[149] = ".github/workflows/zzz.yml"
	if w := do(t, srv, http.MethodPost, "/api/v1/internal/approvals", token,
		raiseBody(types.ApprovalPushContent, scope, &bad)); w.Code != http.StatusBadRequest {
		t.Fatalf("(c) off-digest raise: status %d body %s, want 400", w.Code, w.Body.String())
	}
	if n, _ := pg.CountApprovalsForRun(ctx, runID); n != 0 {
		t.Fatalf("(c) the refused raise left %d approval rows", n)
	}
	if n, _ := pg.CountPushPathLists(ctx, runID); n != 0 {
		t.Fatalf("(c) the refused raise left %d path lists", n)
	}

	// (a)
	w := do(t, srv, http.MethodPost, "/api/v1/internal/approvals", token, raiseBody(types.ApprovalPushContent, scope, &list))
	var ap types.ApprovalRequest
	if w.Code != http.StatusCreated || json.Unmarshal(w.Body.Bytes(), &ap) != nil {
		t.Fatalf("(a) raise: status %d body %s", w.Code, w.Body.String())
	}
	stored, err := pg.GetPushPathList(ctx, ap.ID)
	if err != nil || len(stored.Paths) != 150 || stored.Truncated || types.PushPathsDigest(stored.Paths) != scope.PathsDigest {
		t.Fatalf("(a) stored %d paths truncated=%v err=%v, want the 150 under paths_digest", len(stored.Paths), stored.Truncated, err)
	}
	// The same push raised again joins the same row and records nothing new.
	w = do(t, srv, http.MethodPost, "/api/v1/internal/approvals", token, raiseBody(types.ApprovalPushContent, scope, &list))
	var again types.ApprovalRequest
	if _ = json.Unmarshal(w.Body.Bytes(), &again); w.Code != http.StatusCreated || again.ID != ap.ID {
		t.Fatalf("re-raise: status %d id %s, want 201 and the same row %s", w.Code, again.ID, ap.ID)
	}

	// (d)
	if ok, err := pg.UpdateRunStateIf(ctx, runID, types.RunRunning, types.RunCompleted); !ok || err != nil {
		t.Fatalf("complete the run: %v %v", ok, err)
	}
	srv.CancelTerminalRunApprovals(ctx, runID)
	if got, _ := pg.GetApproval(ctx, ap.ID); got.State != types.ApprovalCancelled {
		t.Fatalf("approval state after the run ended = %s, want CANCELLED", got.State)
	}
	w = do(t, srv, http.MethodGet, "/api/v1/approvals/"+ap.ID.String()+"/paths", adminToken, "")
	var read types.PushPathList
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &read) != nil || !slices.Equal(read.Paths, list.Paths) {
		t.Errorf("(d) GET paths after the run ended: status %d, %d paths", w.Code, len(read.Paths))
	}
	w = do(t, srv, http.MethodGet, "/api/v1/audit/export?run_id="+runID.String(), adminToken, "")
	var exported []string
	rows := 0
	sc := bufio.NewScanner(strings.NewReader(w.Body.String()))
	sc.Buffer(nil, 4<<20)
	for sc.Scan() {
		var ev types.AuditEvent
		var data struct {
			Paths []string `json:"paths"`
		}
		if json.Unmarshal(sc.Bytes(), &ev) == nil && ev.Action == "approval.push_paths.record" && ev.Target == ap.ID.String() &&
			json.Unmarshal(ev.Data, &data) == nil {
			exported = data.Paths
			rows++
		}
	}
	if w.Code != http.StatusOK || rows != 1 || !slices.Equal(exported, list.Paths) {
		t.Errorf("(d) audit export after the run ended: status %d, %d rows, %d paths for the approval, want one row of the 150",
			w.Code, rows, len(exported))
	}
	t.Logf("stored=%d read=%d exported=%d paths_total=%d", len(stored.Paths), len(read.Paths), len(exported), scope.PathsTotal)

	for _, q := range []string{
		`UPDATE push_content_paths SET truncated = true WHERE approval_id = '` + ap.ID.String() + `'`,
		`DELETE FROM push_content_paths WHERE approval_id = '` + ap.ID.String() + `'`,
		`TRUNCATE push_content_paths`,
	} {
		if _, err := pool.Exec(ctx, q); err == nil || !strings.Contains(err.Error(), "immutable") {
			t.Errorf("%s: err = %v, want the immutability refusal", q, err)
		}
	}
}
