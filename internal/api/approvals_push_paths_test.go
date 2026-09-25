// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/approval"
	"github.com/cjohnstoniv/wardyn/internal/audit"
	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/secretstore"
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

// pathListStore is grantStore plus the path-list capability store.PG has,
// its cap held under one mutex as store.PG holds it under one lock.
type pathListStore struct {
	grantStore
	mu    sync.Mutex
	lists map[uuid.UUID]types.PushPathList
	runOf func(uuid.UUID) uuid.UUID
}

func (s *pathListStore) RecordPushPathList(_ context.Context, id, runID uuid.UUID, l types.PushPathList, limit int) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.lists[id]; ok {
		return false, nil
	}
	if s.countLocked(runID) >= limit {
		return false, store.ErrPushPathListCap
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
	return s.countLocked(runID), nil
}

func (s *pathListStore) countLocked(runID uuid.UUID) int {
	n := 0
	for id := range s.lists {
		if s.runOf(id) == runID {
			n++
		}
	}
	return n
}

// pathFixture is one attended run with a github_token grant, on the fake
// stores, with SSO wired so the read route can be probed as each role.
type pathFixture struct {
	srv    *Server
	h      *harness
	aap    *authzApprovals
	st     *pathListStore
	runID  uuid.UUID
	token  string
	actsAs string
}

func newPathFixture(t *testing.T, owner string) *pathFixture {
	t.Helper()
	h := newHarness(t)
	ast := newAuthzStore()
	aap := newAuthzApprovals(ast)
	runID, app := uuid.New(), uuid.New()
	st := &pathListStore{
		grantStore: grantStore{authzStore: ast, grants: []types.CredentialGrant{
			{ID: app, RunID: runID, Spec: types.GrantSpec{Kind: types.GrantGitHubToken}},
		}},
		lists: map[uuid.UUID]types.PushPathList{},
		runOf: func(id uuid.UUID) uuid.UUID { ap, _ := aap.Get(context.Background(), id); return ap.RunID },
	}
	cfg := baseTestConfig(h, st)
	cfg.OIDC = &oidc.Authenticator{}
	cfg.Secrets = getErrStore{getErr: secretstore.ErrNotFound}
	cfg.Approvals = aap
	cfg.SessionRevocations = fakeAuthzSessionRevocations{}
	ast.siteCfg = authzMatrixSiteConfig()
	ast.mu.Lock()
	ast.runs[runID] = types.AgentRun{ID: runID, State: types.RunRunning, Interactive: true, CreatedBy: owner, Agent: "claude-code"}
	ast.mu.Unlock()
	return &pathFixture{srv: New(cfg), h: h, aap: aap, st: st, runID: runID,
		token: h.mintRunToken(t, runID), actsAs: "github_token:" + app.String()}
}

func (f *pathFixture) raise(t *testing.T, body string) (int, uuid.UUID) {
	t.Helper()
	w := do(t, f.srv, http.MethodPost, "/api/v1/internal/approvals", f.token, body)
	var ap types.ApprovalRequest
	_ = json.Unmarshal(w.Body.Bytes(), &ap)
	return w.Code, ap.ID
}

func (f *pathFixture) counts() (approvals, lists int) {
	f.aap.mu.Lock()
	approvals = len(f.aap.byID)
	f.aap.mu.Unlock()
	f.st.mu.Lock()
	defer f.st.mu.Unlock()
	return approvals, len(f.st.lists)
}

// TestPushPathListRaise: a raise's path list is verified against its scope,
// then stored with the approval, and its digest — not the list — goes into the
// run's audit trail. (a) 150 paths store 150; (b) 10,001 store the first
// 10,000, truncated; (c) a list the scope does not vouch for, a truncated one
// included, is a 400 that writes nothing.
func TestPushPathListRaise(t *testing.T) {
	f := newPathFixture(t, "alice@example.com")
	read := func(id uuid.UUID) types.PushPathList {
		w := do(t, f.srv, http.MethodGet, "/api/v1/approvals/"+id.String()+"/paths", adminToken, "")
		var l types.PushPathList
		if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &l) != nil {
			t.Fatalf("GET paths: status %d body %.200s", w.Code, w.Body.String())
		}
		return l
	}

	// (a)
	scope, list := heldPush(150, f.actsAs)
	code, id := f.raise(t, raiseBody(types.ApprovalPushContent, scope, &list))
	if code != http.StatusCreated {
		t.Fatalf("150-path raise: status %d, want 201", code)
	}
	if got := read(id); !slices.Equal(got.Paths, list.Paths) || got.Truncated {
		t.Errorf("(a) read back %d paths truncated=%v, want the 150 raised", len(got.Paths), got.Truncated)
	}
	var recorded []types.AuditEvent
	for _, ev := range f.h.audit.snapshot() {
		if ev.Action == "approval.push_paths.record" {
			recorded = append(recorded, ev)
		}
	}
	var data map[string]any
	if len(recorded) != 1 || recorded[0].Target != id.String() || json.Unmarshal(recorded[0].Data, &data) != nil {
		t.Fatalf("(a) audit rows %+v, want one naming the approval", recorded)
	}
	if _, has := data["paths"]; has || len(recorded[0].Data) >= 1024 || data["stored_list_digest"] != scope.PathsDigest {
		t.Errorf("(a) audit row data (%d bytes) = %s, want under 1 KiB, no paths, stored_list_digest = paths_digest",
			len(recorded[0].Data), recorded[0].Data)
	}

	// (b) an honest truncation at the count bound.
	scope, list = heldPush(types.PushPathListMaxPaths+1, f.actsAs)
	if code, id = f.raise(t, raiseBody(types.ApprovalPushContent, scope, &list)); code != http.StatusCreated {
		t.Fatalf("10,001-path raise: status %d, want 201", code)
	}
	if got := read(id); len(got.Paths) != types.PushPathListMaxPaths || !got.Truncated {
		t.Errorf("(b) stored %d paths truncated=%v, want 10000 truncated", len(got.Paths), got.Truncated)
	}

	// (c)
	scope, list = heldPush(150, f.actsAs)
	lie := func(fn func(*types.PushPathList)) *types.PushPathList {
		l := types.PushPathList{Paths: slices.Clone(list.Paths)}
		fn(&l)
		return &l
	}
	rowsBefore, listsBefore := f.counts()
	for name, body := range map[string]string{
		"a path swapped":      raiseBody(types.ApprovalPushContent, scope, lie(func(l *types.PushPathList) { l.Paths[149] = ".github/workflows/zzz.yml" })),
		"a path dropped":      raiseBody(types.ApprovalPushContent, scope, lie(func(l *types.PushPathList) { l.Paths = l.Paths[:149] })),
		"unsorted":            raiseBody(types.ApprovalPushContent, scope, lie(func(l *types.PushPathList) { l.Paths[0], l.Paths[1] = l.Paths[1], l.Paths[0] })),
		"truncated but whole": raiseBody(types.ApprovalPushContent, scope, lie(func(l *types.PushPathList) { l.Truncated = true })),
		"truncated, empty":    raiseBody(types.ApprovalPushContent, scope, &types.PushPathList{Paths: []string{}, Truncated: true}),
		"truncated shorter than the card": raiseBody(types.ApprovalPushContent, scope,
			lie(func(l *types.PushPathList) { l.Paths, l.Truncated = l.Paths[:5], true })),
		"truncated off the card's paths": raiseBody(types.ApprovalPushContent, scope,
			lie(func(l *types.PushPathList) { l.Paths, l.Truncated = l.Paths[1:15], true })),
		"truncated short of both bounds": raiseBody(types.ApprovalPushContent, scope,
			lie(func(l *types.PushPathList) { l.Paths, l.Truncated = l.Paths[:20], true })),
		"truncated, tail invented": raiseBody(types.ApprovalPushContent, scope,
			lie(func(l *types.PushPathList) { l.Paths, l.Truncated = append(l.Paths[:10], "zzz/invented"), true })),
		"on an egress raise": raiseBody(types.ApprovalEgressDomain, map[string]string{"host": "example.com"}, &list),
	} {
		if code, _ := f.raise(t, body); code != http.StatusBadRequest {
			t.Errorf("(c) %s: status %d, want 400", name, code)
		}
	}
	if rows, lists := f.counts(); rows != rowsBefore || lists != listsBefore {
		t.Errorf("(c) refused raises wrote rows: approvals %d -> %d, lists %d -> %d", rowsBefore, rows, listsBefore, lists)
	}
}

// TestPushPathListRouteVisibility: GET /approvals/{id}/paths answers whoever
// GET /approvals shows the approval to, and everyone else the byte-identical
// 404 a missing approval gets. (Adopted from the #1087 review's probe.)
func TestPushPathListRouteVisibility(t *testing.T) {
	f := newPathFixture(t, "sub-owner")
	scope, list := heldPush(150, f.actsAs)
	code, id := f.raise(t, raiseBody(types.ApprovalPushContent, scope, &list))
	if code != http.StatusCreated {
		t.Fatalf("raise: %d", code)
	}
	p := "/api/v1/approvals/" + id.String() + "/paths"
	other := ssoSession(t, "sub-other", "other@corp.example", oidc.RoleUser)
	cases := []struct {
		name string
		w    *httptest.ResponseRecorder
		want int
	}{
		{"owner", doSSO(t, f.srv, http.MethodGet, p, ssoSession(t, "sub-owner", "owner@corp.example", oidc.RoleUser), ""), 200},
		{"another member", doSSO(t, f.srv, http.MethodGet, p, other, ""), 404},
		{"security_admin", doSSO(t, f.srv, http.MethodGet, p, ssoSession(t, "sub-sec", "sec@corp.example", oidc.RoleSecurityAdmin), ""), 200},
		{"admin", doSSO(t, f.srv, http.MethodGet, p, ssoSession(t, "sub-admin", "admin@corp.example", oidc.RoleAdmin), ""), 200},
		{"the run's own token", do(t, f.srv, http.MethodGet, p, f.token, ""), 401},
		{"anonymous", do(t, f.srv, http.MethodGet, p, "", ""), 401},
		{"another member, missing id", doSSO(t, f.srv, http.MethodGet, "/api/v1/approvals/"+uuid.NewString()+"/paths", other, ""), 404},
	}
	for _, c := range cases {
		if c.w.Code != c.want {
			t.Errorf("%s: status %d, want %d", c.name, c.w.Code, c.want)
		}
	}
	if a, b := cases[1].w.Body.String(), cases[6].w.Body.String(); a != b {
		t.Errorf("existence oracle: a foreign approval answers %q, a missing one %q", a, b)
	}
}

// TestPushPathListFromAPreviousReleaseProxy: a raise with no path_list is
// accepted and stores nothing; the route answers with the scope's own paths.
// (Adopted from the #1087 review's probe.)
func TestPushPathListFromAPreviousReleaseProxy(t *testing.T) {
	f := newPathFixture(t, "sub-owner")
	for i, n := range []int{150, 7} {
		scope, _ := heldPush(n, f.actsAs)
		scope.Commits = []string{fmt.Sprintf("%040d", i)}
		code, id := f.raise(t, string(mustJSON(map[string]any{"kind": types.ApprovalPushContent, "requested_scope": scope})))
		if code != http.StatusCreated {
			t.Fatalf("n=%d raise without path_list: %d", n, code)
		}
		w := do(t, f.srv, http.MethodGet, "/api/v1/approvals/"+id.String()+"/paths", adminToken, "")
		var l types.PushPathList
		_ = json.Unmarshal(w.Body.Bytes(), &l)
		if w.Code != 200 || !slices.Equal(l.Paths, scope.Paths) || l.Truncated != (n > 10) {
			t.Errorf("n=%d: GET paths %d, %d paths truncated=%v; want the scope's %d, truncated=%v",
				n, w.Code, len(l.Paths), l.Truncated, len(scope.Paths), n > 10)
		}
	}
	if _, lists := f.counts(); lists != 0 {
		t.Errorf("raises without a path_list stored %d lists", lists)
	}
}

// burstRaise fires n concurrent raises of distinct pushes and counts each
// status.
func burstRaise(t *testing.T, n int, raise func(body string) int, actsAs string) map[int]int {
	t.Helper()
	var wg sync.WaitGroup
	codes := make([]int, n)
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			scope, list := heldPush(20, actsAs)
			scope.Commits = []string{fmt.Sprintf("%040d", i)}
			codes[i] = raise(raiseBody(types.ApprovalPushContent, scope, &list))
		}()
	}
	wg.Wait()
	out := map[int]int{}
	for _, c := range codes {
		out[c]++
	}
	return out
}

// TestPushPathListCapUnderBurst: 100 concurrent raises of distinct pushes on
// one run store exactly maxPushPathListsPerRun lists; every other raise is a
// 429 and leaves no PENDING question without its list. At rest, the next raise
// is a 429 that writes nothing. (Adopted from the #1087 review's probes.)
func TestPushPathListCapUnderBurst(t *testing.T) {
	f := newPathFixture(t, "sub-owner")
	got := burstRaise(t, 100, func(body string) int { c, _ := f.raise(t, body); return c }, f.actsAs)
	if got[http.StatusCreated] != maxPushPathListsPerRun || got[http.StatusTooManyRequests] != 100-maxPushPathListsPerRun {
		t.Errorf("burst statuses %v, want %d x 201 and the rest 429", got, maxPushPathListsPerRun)
	}
	if _, lists := f.counts(); lists != maxPushPathListsPerRun {
		t.Errorf("burst stored %d lists, want %d", lists, maxPushPathListsPerRun)
	}
	f.aap.mu.Lock()
	byID := maps.Clone(f.aap.byID)
	f.aap.mu.Unlock()
	for id, ap := range byID {
		if _, err := f.st.GetPushPathList(context.Background(), id); ap.State == types.ApprovalPending && err != nil {
			t.Errorf("approval %s is PENDING without its list", id)
		}
	}

	rows, lists := f.counts()
	scope, list := heldPush(20, f.actsAs)
	scope.Commits = []string{strings.Repeat("f", 40)}
	if code, _ := f.raise(t, raiseBody(types.ApprovalPushContent, scope, &list)); code != http.StatusTooManyRequests {
		t.Errorf("a raise at rest past the cap: %d, want 429", code)
	}
	if r, l := f.counts(); r != rows || l != lists {
		t.Errorf("the refused raise wrote rows: approvals %d -> %d, lists %d -> %d", rows, r, lists, l)
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

// pgPathFixture is a Server over a throwaway, migrated Postgres with one
// attended run holding a github_token grant.
func pgPathFixture(t *testing.T) (*Server, store.PG, uuid.UUID, string, string) {
	t.Helper()
	pool := throwawayPGPool(t)
	ctx := context.Background()
	pg := store.NewPG(pool)
	h := newHarness(t)
	cfg := baseTestConfig(h, pg)
	cfg.Audit = store.Recorder{Pool: pool}
	cfg.Approvals = pgApprovalService{PG: pg, rec: cfg.Audit}
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
	return New(cfg), pg, runID, h.mintRunToken(t, runID), "github_token:" + grantID.String()
}

// exportedPushPaths reads the run's audit export and returns the data of each
// approval.push_paths.record row for approvalID.
func exportedPushPaths(t *testing.T, srv *Server, runID, approvalID uuid.UUID) []map[string]json.RawMessage {
	t.Helper()
	w := do(t, srv, http.MethodGet, "/api/v1/audit/export?run_id="+runID.String(), adminToken, "")
	if w.Code != http.StatusOK {
		t.Fatalf("audit export: status %d", w.Code)
	}
	var rows []map[string]json.RawMessage
	sc := bufio.NewScanner(strings.NewReader(w.Body.String()))
	sc.Buffer(nil, 4<<20)
	for sc.Scan() {
		var ev types.AuditEvent
		var data map[string]json.RawMessage
		if json.Unmarshal(sc.Bytes(), &ev) == nil && ev.Action == "approval.push_paths.record" &&
			ev.Target == approvalID.String() && json.Unmarshal(ev.Data, &data) == nil {
			rows = append(rows, data)
		}
	}
	return rows
}

// TestPG_PushPathListOutlivesTheRun drives the whole chain through Postgres:
// (a) a 150-path raise stores 150 in push_content_paths and a chained audit
// row of under 1 KiB; (c) a list off the digest leaves no approval row and no
// list row; (d) once the run is terminal and its approval cancelled,
// GET /approvals/{id}/paths and the run's audit export both still return the
// same 150. The table row refuses UPDATE, DELETE and TRUNCATE, and one altered
// behind its triggers no longer matches the chained row's stored_list_digest.
func TestPG_PushPathListOutlivesTheRun(t *testing.T) {
	srv, pg, runID, token, actsAs := pgPathFixture(t)
	ctx := context.Background()
	scope, list := heldPush(150, actsAs)

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
	var rowBytes int
	if err := pg.Pool.QueryRow(ctx, `SELECT octet_length(data::text) FROM audit_events WHERE action = $1 AND target = $2`,
		"approval.push_paths.record", ap.ID.String()).Scan(&rowBytes); err != nil || rowBytes >= 1024 {
		t.Errorf("(a) the chained audit row's data is %d bytes (err %v), want under 1 KiB", rowBytes, err)
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
	rows := exportedPushPaths(t, srv, runID, ap.ID)
	var exported []string
	var storedDigest string
	if len(rows) == 1 {
		_ = json.Unmarshal(rows[0]["paths"], &exported)
		_ = json.Unmarshal(rows[0]["stored_list_digest"], &storedDigest)
	}
	if len(rows) != 1 || !slices.Equal(exported, list.Paths) || types.PushPathsDigest(exported) != storedDigest {
		t.Errorf("(d) audit export after the run ended: %d rows, %d paths, want one row of the 150 matching stored_list_digest",
			len(rows), len(exported))
	}
	t.Logf("stored=%d read=%d exported=%d paths_total=%d audit_row_bytes=%d", len(stored.Paths), len(read.Paths), len(exported), scope.PathsTotal, rowBytes)

	for _, q := range []string{
		`UPDATE push_content_paths SET truncated = true WHERE approval_id = '` + ap.ID.String() + `'`,
		`DELETE FROM push_content_paths WHERE approval_id = '` + ap.ID.String() + `'`,
		`TRUNCATE push_content_paths`,
	} {
		if _, err := pg.Pool.Exec(ctx, q); err == nil || !strings.Contains(err.Error(), "immutable") {
			t.Errorf("%s: err = %v, want the immutability refusal", q, err)
		}
	}

	// Tamper behind the triggers, as only the table's owner can: the export
	// still inlines what the table holds, and it no longer hashes to the
	// chained row's stored_list_digest.
	for _, q := range []string{
		`ALTER TABLE push_content_paths DISABLE TRIGGER push_content_paths_no_update`,
		`UPDATE push_content_paths SET paths = paths[1:149] WHERE approval_id = '` + ap.ID.String() + `'`,
		`ALTER TABLE push_content_paths ENABLE TRIGGER push_content_paths_no_update`,
	} {
		if _, err := pg.Pool.Exec(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	rows = exportedPushPaths(t, srv, runID, ap.ID)
	exported = nil
	if len(rows) == 1 {
		_ = json.Unmarshal(rows[0]["paths"], &exported)
	}
	if len(exported) != 149 || types.PushPathsDigest(exported) == storedDigest {
		t.Errorf("tampered list: exported %d paths, digest match %v; want 149 and a mismatch",
			len(exported), types.PushPathsDigest(exported) == storedDigest)
	}
}

// TestPG_PushPathListCapUnderBurst: the cap holds in Postgres, where the
// count and the insert are one locked step: 100 concurrent raises of
// distinct pushes store exactly maxPushPathListsPerRun lists, and every
// PENDING approval the burst leaves has its list.
func TestPG_PushPathListCapUnderBurst(t *testing.T) {
	srv, pg, runID, token, actsAs := pgPathFixture(t)
	ctx := context.Background()
	got := burstRaise(t, 100, func(body string) int {
		return do(t, srv, http.MethodPost, "/api/v1/internal/approvals", token, body).Code
	}, actsAs)
	if got[http.StatusCreated] != maxPushPathListsPerRun || got[http.StatusTooManyRequests] != 100-maxPushPathListsPerRun {
		t.Errorf("burst statuses %v, want %d x 201 and the rest 429", got, maxPushPathListsPerRun)
	}
	if n, err := pg.CountPushPathLists(ctx, runID); err != nil || n != maxPushPathListsPerRun {
		t.Errorf("burst stored %d lists (err %v), want %d", n, err, maxPushPathListsPerRun)
	}
	pending, err := pg.ListApprovals(ctx, types.ApprovalPending)
	if err != nil {
		t.Fatal(err)
	}
	for _, ap := range pending {
		if _, err := pg.GetPushPathList(ctx, ap.ID); ap.RunID == runID && err != nil {
			t.Errorf("approval %s is PENDING without its list", ap.ID)
		}
	}
	before, _ := pg.CountApprovalsForRun(ctx, runID)
	scope, list := heldPush(20, actsAs)
	scope.Commits = []string{strings.Repeat("f", 40)}
	if w := do(t, srv, http.MethodPost, "/api/v1/internal/approvals", token,
		raiseBody(types.ApprovalPushContent, scope, &list)); w.Code != http.StatusTooManyRequests {
		t.Errorf("a raise at rest past the cap: %d, want 429", w.Code)
	}
	if after, _ := pg.CountApprovalsForRun(ctx, runID); after != before {
		t.Errorf("the refused raise wrote an approval: %d -> %d", before, after)
	}
}
