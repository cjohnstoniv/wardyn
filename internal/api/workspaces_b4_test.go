// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/internal/workspacescan"
)

// B4: the workspace lane's admission/build/observed-egress regressions. Each
// test here was written RED against the unfixed tree and names the finding it
// pins, so a later reader can tell a deliberate rule from an accident.

// ─── B4-F1: the failed build's raw builder error is the operator's ──────────

// TestB4F1_FailedBuildDetailIsTieredLikeTheLog pins that the ONE non-static
// Detail resolveBuildView can answer — the builder's own error text, which
// quotes the operator's authored base-image coordinate verbatim in its FROM /
// pull line — is projected by the reader's tier exactly as Log already is. The
// other three Details are fixed prose about the host's configuration and stay
// for every tier (TestBuildKeepsWhatTheMemberNeeds pins that they do).
func TestB4F1_FailedBuildDetailIsTieredLikeTheLog(t *testing.T) {
	const authored = "registry.corp.internal/base:1"
	srv := &Server{}
	ws := types.Workspace{ID: uuid.New()}
	srv.builds.finish(ws.ID, "", "failed to pull "+authored+": unauthorized")

	full := srv.resolveBuildView(ws, workspaceReadFull)
	if full.State != "failed" || !strings.Contains(full.Detail, authored) {
		t.Fatalf("full tier view = %+v, want state=failed with the builder's own error kept", full)
	}
	for _, tier := range []workspaceReadTier{workspaceReadSecurity, workspaceReadMember} {
		got := srv.resolveBuildView(ws, tier)
		if got.State != "failed" {
			t.Errorf("tier %d state = %q, want %q — the STATE is not the leak", tier, got.State, "failed")
		}
		if strings.Contains(got.Detail, authored) {
			t.Errorf("tier %d leaked the operator's authored base image through the failed build's detail: %q", tier, got.Detail)
		}
	}
}

// ─── B4-F2: the in-memory tracker outranked the row it was caching ─────────

// b4BuildStore serves one workspace for the /build handlers and is safe for the
// detached build goroutine to write while the test reads.
type b4BuildStore struct {
	store.Store
	lib sourceLibraryFake
	mu  sync.Mutex
	ws  types.Workspace
}

func (s *b4BuildStore) UpsertSource(ctx context.Context, src types.Source) (types.Source, error) {
	return s.lib.UpsertSource(ctx, src)
}
func (s *b4BuildStore) UpsertBaseImage(ctx context.Context, b types.BaseImageEntry) (types.BaseImageEntry, error) {
	return s.lib.UpsertBaseImage(ctx, b)
}
func (s *b4BuildStore) GetSourcesByIDs(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]types.Source, error) {
	return s.lib.GetSourcesByIDs(ctx, ids)
}
func (s *b4BuildStore) UpdateWorkspace(_ context.Context, _ uuid.UUID, ws types.Workspace, _ bool) (types.Workspace, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ws = ws
	return ws, nil
}

func (s *b4BuildStore) GetWorkspace(context.Context, uuid.UUID) (types.Workspace, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.ws, nil
}

func (s *b4BuildStore) GetSiteConfig(context.Context) (types.SiteConfig, error) {
	return types.SiteConfig{}, nil
}

func (s *b4BuildStore) SetWorkspaceBuiltImage(_ context.Context, _ uuid.UUID, ref, hash string) (types.Workspace, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ws.ImageRef, s.ws.BuiltProfileHash = ref, hash
	return s.ws, nil
}

func (s *b4BuildStore) QueryAuditEvents(context.Context, uuid.UUID, int) ([]types.AuditEvent, error) {
	return nil, nil
}

func (s *b4BuildStore) setProfile(p workspacescan.WorkspaceProfile) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ws.Profile = mustJSON(p)
}

func (s *b4BuildStore) setHash(hash string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ws.BuiltProfileHash = hash
}

// blockingImageBuilder parks the detached build goroutine inside the builder so
// the test can assert the single-flight slot was claimed without racing the
// goroutine's own store writes. release() lets it finish.
type blockingImageBuilder struct {
	fakeImageBuilder
	gate  chan struct{}
	mu    sync.Mutex
	calls int
}

func (b *blockingImageBuilder) BuildFromDevcontainerFiles(_ context.Context, _ map[string]string, tag string, _ io.Writer) (string, error) {
	b.mu.Lock()
	b.calls++
	b.mu.Unlock()
	<-b.gate
	return tag, nil
}

func (b *blockingImageBuilder) callCount() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.calls
}

// failingImageBuilder is the ordinary generated-devcontainer lane failing, so a
// test can put a real Error in the tracker through the handler that puts one
// there, rather than seeding the tracker by hand.
type failingImageBuilder struct{ fakeImageBuilder }

func (failingImageBuilder) BuildFromDevcontainerFiles(context.Context, map[string]string, string, io.Writer) (string, error) {
	return "", errors.New("step 3/7 : RUN go build — exit code 2")
}

// TestB4F2_TheTrackerIsSubordinateToTheRow is the invalidation regression: the
// in-memory buildState is this daemon's MEMORY of a build, while the workspace
// row is what a run actually resolves. The tracker answered "done" from
// st.Image before anything consulted the row, so an image invalidated
// underneath it (a PUT that removeStaleImage'd the ref, a rescan that changed
// the profile hash) still read `done` with a ref no run would ever resolve —
// and POST /build short-circuited on that same view, so the workspace could
// never be rebuilt until wardynd restarted.
func TestB4F2_TheTrackerIsSubordinateToTheRow(t *testing.T) {
	h := newHarness(t)
	profile := workspacescan.WorkspaceProfile{
		Languages: []string{"Go"}, Confidence: workspacescan.ConfidenceHigh, Source: workspacescan.SourceDeterministic,
	}
	const built = "wardyn-workspace/x:built"
	st := &b4BuildStore{ws: types.Workspace{
		ID: uuid.New(), Name: "w", Status: types.WorkspaceScanned,
		Sources:  []types.WorkspaceSource{{Type: types.WorkspaceSourceTypeEphemeral, Target: "/home/agent/work"}},
		Profile:  mustJSON(profile),
		ImageRef: built,
	}}
	st.ws.BuiltProfileHash = profile.CacheKey()
	srv := New(baseTestConfig(h, st))
	// What the wizard's own Build step leaves behind: a finished tracker entry
	// carrying the build's log tail.
	srv.builds.begin(st.ws.ID, time.Now())
	srv.builds.appendLog(st.ws.ID, "Step 1/3 : FROM golang:1.26")
	srv.builds.finish(st.ws.ID, built, "")

	get := func() buildResponse {
		t.Helper()
		w := do(t, srv, http.MethodGet, "/api/v1/workspaces/"+st.ws.ID.String()+"/build", adminToken, "")
		if w.Code != http.StatusOK {
			t.Fatalf("GET /build = %d: %s", w.Code, w.Body.String())
		}
		var got buildResponse
		if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
			t.Fatal(err)
		}
		return got
	}

	// NEGATIVE CONTROL: an untouched row still reads done, with its log.
	if v := get(); v.State != "done" || v.Image != built || len(v.Log) == 0 {
		t.Fatalf("untouched row: view = %+v, want done/%s with its log", v, built)
	}

	// A rescan changed the profile the image was built from. The ref is still
	// in the row, and the tracker still remembers building it — but it is no
	// longer the image a run would resolve.
	st.setHash("a-hash-from-a-newer-scan")
	if v := get(); v.State == "done" {
		t.Errorf("after the profile hash moved, GET /build still says done: %+v", v)
	}

	// ... and POST must re-enter the single-flight build rather than
	// short-circuiting on that same stale view.
	builder := &blockingImageBuilder{gate: make(chan struct{})}
	srv.cfg.ImageBuilder = builder
	w := do(t, srv, http.MethodPost, "/api/v1/workspaces/"+st.ws.ID.String()+"/build", adminToken, "")
	if w.Code != http.StatusAccepted {
		t.Fatalf("POST /build = %d, want 202 (begin claimed); body=%s", w.Code, w.Body.String())
	}
	if !srv.builds.get(st.ws.ID).Building {
		t.Error("POST answered 202 without claiming the single-flight slot")
	}
	close(builder.gate)
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); {
		if !srv.builds.get(st.ws.ID).Building {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("the detached build never finished")
}

// ─── B4-F3: a respelling of the same source wiped every reviewed field ─────

// TestB4F3_ARespellingOfTheSameSourceKeepsEveryReviewedField is the data-loss
// regression: sourcesChanged compared the request's RAW source against the
// store's CANONICAL one, so re-PUTting the identical composition with a
// trailing slash or a differently-cased repo slug read as a content change and
// threw away ApprovedEgress, Requirements, RecordResults and the built image —
// with a 200 and nothing anywhere saying it had happened.
func TestB4F3_ARespellingOfTheSameSourceKeepsEveryReviewedField(t *testing.T) {
	for _, tc := range []struct {
		name          string
		stored        types.WorkspaceSource
		respelledBody string
	}{
		{
			"clone URL host case",
			types.WorkspaceSource{Type: types.WorkspaceSourceTypeRepo, Source: "https://git.corp.example/MyGroup/MyRepo.git"},
			`{"name":"w","sources":[{"type":"repo","source":"https://Git.Corp.Example/MyGroup/MyRepo.git"}]}`,
		},
		{
			"repo slug case and .git suffix",
			types.WorkspaceSource{Type: types.WorkspaceSourceTypeRepo, Source: "acme/payments", Ref: "refs/heads/main"},
			`{"name":"w","sources":[{"type":"repo","source":"Acme/Payments.git","ref":"refs/heads/main"}]}`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			id := uuid.New()
			fake := &workspaceStoreFake{ws: types.Workspace{
				ID: id, Name: "w", Sources: []types.WorkspaceSource{tc.stored},
				Status: types.WorkspaceScanned, ImageRef: "wardyn/ws:abc", BuiltProfileHash: "abc",
				ApprovedEgress: []string{"example.com"},
				Requirements:   map[string]types.WorkspaceRequirement{"secret:acme-key": {Level: "required"}},
				RecordResults:  mustJSON(map[string]any{"t": 1}),
			}}
			srv := New(baseTestConfig(h, fake))
			w := do(t, srv, http.MethodPut, "/api/v1/workspaces/"+id.String(), adminToken, tc.respelledBody)
			if w.Code != http.StatusOK {
				t.Fatalf("code = %d, want 200; body=%s", w.Code, w.Body.String())
			}
			got := fake.updated
			if got.ApprovedEgress == nil || got.Requirements == nil || got.RecordResults == nil || got.ImageRef == "" {
				t.Errorf("a respelling of the SAME source cleared what was reviewed against it; got %+v", got)
			}
			if fake.stampedEgressEdit {
				t.Error("a respelling stamped an egress edit that never happened")
			}
		})
	}
}

// ─── B4-F4: an uncapped source list ────────────────────────────────────────

// TestB4F4_SourceCountIsCapped pins the missing sibling of
// maxWorkspaceRequirements/maxApprovedEgress: every source in the body costs an
// UpsertSource plus a hash-chained source.write audit append, and the route is
// member-reachable, so an uncapped list is a serialised write chain a member
// can start with one request.
func TestB4F4_SourceCountIsCapped(t *testing.T) {
	body := func(n int) string {
		srcs := make([]string, n)
		for i := range srcs {
			srcs[i] = fmt.Sprintf(`{"type":"repo","source":"acme/repo-%d"}`, i)
		}
		return `{"name":"w","sources":[` + strings.Join(srcs, ",") + `]}`
	}
	h := newHarness(t)
	w := do(t, h.srv, http.MethodPost, "/api/v1/workspaces", adminToken, body(maxWorkspaceSources+1))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("N+1 sources: code = %d, want 400; body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), fmt.Sprintf("%d", maxWorkspaceSources)) {
		t.Errorf("the refusal does not name the cap: %s", w.Body.String())
	}
	// NEGATIVE CONTROL: exactly N passes the validator (the handler behind it
	// needs a store; what matters here is that the cap itself does not fire).
	r := httptest.NewRequest(http.MethodPost, "/api/v1/workspaces", strings.NewReader(body(maxWorkspaceSources)))
	if _, msg := decodeWorkspaceRequest(httptest.NewRecorder(), r, nil); msg != "" {
		t.Errorf("N sources rejected: %q", msg)
	}
}

// ─── B4-F6 + B4-F9: observed egress offered candidates that can never work ──

// TestB4F6_ObservedEgressWithholdsWhatApprovingCannotHelp pins the two classes
// of candidate a promotion can never make work: a host the git broker or the
// control plane already routes specially (handleSetApprovedEgress 400s the
// WHOLE list on one of those, so a single dead candidate made the panel's
// promote button fail for every host beside it), and a host the operator has
// explicitly DENIED (deny beats allow at the proxy, so promoting it answers 200
// and stays blocked forever).
func TestB4F6_ObservedEgressWithholdsWhatApprovingCannotHelp(t *testing.T) {
	h := newHarness(t)
	wsID, runID := uuid.New(), uuid.New()
	fake := &observedEgressStore{
		ws: types.Workspace{
			ID: wsID, Name: "w", Status: types.WorkspaceScanned,
			Sources:      []types.WorkspaceSource{{Type: types.WorkspaceSourceTypeLocalDir, Path: "/home/u/repo"}},
			DeniedEgress: []string{"blocked.example.com"},
		},
		runs: []types.AgentRun{{ID: runID, WorkspacePath: "/home/u/repo"}},
		events: map[uuid.UUID][]types.AuditEvent{runID: {
			{Action: "egress.deny", Target: "github.com"},          // git-broker managed
			{Action: "egress.deny", Target: "wardynd"},             // the control plane itself
			{Action: "egress.deny", Target: "blocked.example.com"}, // explicitly denied
			{Action: "egress.deny", Target: "feeds.datagolf.com"},  // NEGATIVE CONTROL: promotable
		}},
	}
	srv := New(baseTestConfig(h, fake))
	w := do(t, srv, http.MethodGet, "/api/v1/workspaces/"+wsID.String()+"/observed-egress", adminToken, "")
	if w.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var got struct {
		Denied []string `json:"denied"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if strings.Join(got.Denied, ",") != "feeds.datagolf.com" {
		t.Errorf("denied = %v, want only [feeds.datagolf.com] — a broker/control-plane host and a denied host are not promotable", got.Denied)
	}
}

// b4PagerStore is observedEgressStore with the Pager seam wired, so the handler
// can take the indexed newest-first page every real deployment takes.
type b4PagerStore struct {
	observedEgressStore
	// The embedded nil interface supplies the rest of Pager's method set so the
	// handler's type assertion succeeds; only ListRunsPage is ever called.
	store.Pager
	pagedLimit int
}

func (s *b4PagerStore) ListRunsPage(_ context.Context, p store.Page) ([]types.AgentRun, error) {
	s.pagedLimit = p.Limit
	if p.Limit > 0 && p.Limit < len(s.runs) {
		return s.runs[:p.Limit], nil
	}
	return s.runs, nil
}

// TestB4F9_ObservedEgressReadsABoundedPage pins the bound on a member-reachable
// route that used to read the WHOLE runs table before windowing it in Go.
func TestB4F9_ObservedEgressReadsABoundedPage(t *testing.T) {
	h := newHarness(t)
	wsID := uuid.New()
	runs := make([]types.AgentRun, maxObservedRuns*3)
	events := map[uuid.UUID][]types.AuditEvent{}
	for i := range runs {
		runs[i] = types.AgentRun{ID: uuid.New(), WorkspacePath: "/home/u/repo"}
		events[runs[i].ID] = []types.AuditEvent{{Action: "egress.deny", Target: fmt.Sprintf("h%d.example.com", i)}}
	}
	fake := &b4PagerStore{observedEgressStore: observedEgressStore{
		ws: types.Workspace{
			ID: wsID, Name: "w", Status: types.WorkspaceScanned,
			Sources: []types.WorkspaceSource{{Type: types.WorkspaceSourceTypeLocalDir, Path: "/home/u/repo"}},
		},
		runs: runs, events: events,
	}}
	srv := New(baseTestConfig(h, fake))
	w := do(t, srv, http.MethodGet, "/api/v1/workspaces/"+wsID.String()+"/observed-egress", adminToken, "")
	if w.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	if fake.pagedLimit != maxObservedRuns {
		t.Errorf("ListRunsPage limit = %d, want %d — the route read the whole runs table", fake.pagedLimit, maxObservedRuns)
	}
	var got struct {
		RunsExamined int `json:"runs_examined"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.RunsExamined != maxObservedRuns {
		t.Errorf("runs_examined = %d, want the bound %d", got.RunsExamined, maxObservedRuns)
	}
}

// ─── B4-F7: DELETE stranded a live sandbox ─────────────────────────────────

// b4DeleteStore serves one workspace and records whether the delete happened.
type b4DeleteStore struct {
	store.Store
	ws      types.Workspace
	run     types.AgentRun
	deleted bool
}

func (s *b4DeleteStore) GetWorkspace(context.Context, uuid.UUID) (types.Workspace, error) {
	return s.ws, nil
}
func (s *b4DeleteStore) GetRun(context.Context, uuid.UUID) (types.AgentRun, error) {
	return s.run, nil
}
func (s *b4DeleteStore) SetWorkspaceImportState(_ context.Context, _ uuid.UUID, status types.WorkspaceStatus, active *uuid.UUID, _ *uuid.UUID) (types.Workspace, bool, error) {
	s.ws.Status, s.ws.ActiveRunID = status, active
	return s.ws, true, nil
}
func (s *b4DeleteStore) DeleteWorkspace(context.Context, uuid.UUID) error {
	s.deleted = true
	return nil
}

// TestB4F7_DeleteRefusesWhileARunHoldsTheWorkspace: agent_runs.workspace_id has
// no foreign key (migration 0009), so DELETE happily removed a workspace whose
// record/scan session was still live — stranding an AllowAllEgress sandbox and
// making reconcileRecordRun 404 before recordmode.Capture ever ran, which loses
// the recording silently. The in-use 409 is handleDeleteSource's own shape.
func TestB4F7_DeleteRefusesWhileARunHoldsTheWorkspace(t *testing.T) {
	h := newHarness(t)
	id, runID := uuid.New(), uuid.New()
	live := &b4DeleteStore{
		ws: types.Workspace{
			ID: id, Name: "w", Status: types.WorkspaceScanning, ActiveRunID: &runID,
			Sources: []types.WorkspaceSource{{Type: types.WorkspaceSourceTypeLocalDir, Path: "/home/u/repo"}},
		},
		run: types.AgentRun{ID: runID, State: types.RunRunning, WorkspaceID: &id},
	}
	srv := New(baseTestConfig(h, live))
	w := do(t, srv, http.MethodDelete, "/api/v1/workspaces/"+id.String(), adminToken, "")
	if w.Code != http.StatusConflict {
		t.Fatalf("delete during a live session: code = %d, want 409; body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), runID.String()) {
		t.Errorf("the 409 does not name the run holding the workspace: %s", w.Body.String())
	}
	if live.deleted {
		t.Error("the workspace was deleted anyway")
	}

	// NEGATIVE CONTROL: the pointer is STALE — the run already terminated, so
	// repairStaleWorkspaceRuns settles it and the delete goes through.
	stale := &b4DeleteStore{
		ws: types.Workspace{
			ID: id, Name: "w", Status: types.WorkspaceScanning, ActiveRunID: &runID,
			Sources: []types.WorkspaceSource{{Type: types.WorkspaceSourceTypeLocalDir, Path: "/home/u/repo"}},
		},
		run: types.AgentRun{ID: runID, State: types.RunFailed, WorkspaceID: &id},
	}
	srv = New(baseTestConfig(h, stale))
	if w := do(t, srv, http.MethodDelete, "/api/v1/workspaces/"+id.String(), adminToken, ""); w.Code != http.StatusNoContent {
		t.Fatalf("delete after the stranded run settled: code = %d, want 204; body=%s", w.Code, w.Body.String())
	}
	if !stale.deleted {
		t.Error("the delete answered 204 without deleting")
	}
}

// ─── B4-F8: the repo ref was validated at neither door ─────────────────────

// TestB4F8_RefIsValidatedAtBothDoors: buildRepoRecords (runs_scm.go) DROPS a
// repo whose ref is not repoFieldSafe by a bare return — the agent then starts
// in a workspace missing that clone, with no warning on the run and nothing in
// the audit trail. Both authoring doors must refuse the ref instead.
func TestB4F8_RefIsValidatedAtBothDoors(t *testing.T) {
	bad := []string{"refs/heads/feat x", "main\nrm -rf /", "main\tx"}
	good := []string{"refs/heads/feat/x", "0123456789abcdef0123456789abcdef01234567", "v1.2.3", ""}

	for _, ref := range bad {
		if msg := validateWorkspaceSource(types.WorkspaceSource{
			Type: types.WorkspaceSourceTypeRepo, Source: "acme/payments", Ref: ref,
		}, nil); msg == "" {
			t.Errorf("workspace door accepted ref %q", ref)
		}
		if msg := validateSourceWrite(types.Source{
			Kind: types.SourceRepo, Name: "payments", Locator: "acme/payments", Ref: ref,
		}, nil); msg == "" {
			t.Errorf("library door accepted ref %q", ref)
		}
	}
	// NEGATIVE CONTROL: a real branch ref, a 40-char SHA and an empty ref pass.
	for _, ref := range good {
		if msg := validateWorkspaceSource(types.WorkspaceSource{
			Type: types.WorkspaceSourceTypeRepo, Source: "acme/payments", Ref: ref,
		}, nil); msg != "" {
			t.Errorf("workspace door refused ref %q: %s", ref, msg)
		}
		if msg := validateSourceWrite(types.Source{
			Kind: types.SourceRepo, Name: "payments", Locator: "acme/payments", Ref: ref,
		}, nil); msg != "" {
			t.Errorf("library door refused ref %q: %s", ref, msg)
		}
	}

	// Through the HTTP door too, so the 400 is what a caller actually gets.
	h := newHarness(t)
	w := do(t, h.srv, http.MethodPost, "/api/v1/workspaces", adminToken,
		`{"name":"w","sources":[{"type":"repo","source":"acme/payments","ref":"refs/heads/feat x"}]}`)
	if w.Code != http.StatusBadRequest {
		t.Errorf("POST /workspaces with an unsafe ref: code = %d, want 400; body=%s", w.Code, w.Body.String())
	}
	w = do(t, h.srv, http.MethodPost, "/api/v1/sources", adminToken,
		`{"kind":"repo","name":"payments","locator":"acme/payments","ref":"refs/heads/feat x"}`)
	if w.Code != http.StatusBadRequest {
		t.Errorf("POST /sources with an unsafe ref: code = %d, want 400; body=%s", w.Code, w.Body.String())
	}
}

// ─── F-1: the custom-base lane had no row-backed answer at all ─────────────

// TestB4F2_CustomBaseImageReadsDoneFromTheRow closes the hole the row-decides
// rule opened: resolveBuildView's "nothing_to_build" early return deliberately
// EXCLUDES kind "custom", so a custom base image is the one explicit-image
// composition that reaches the build tracker and the row — and the row's key
// for it is byoiCacheKey, which resolveWorkspaceImage is what actually stores.
// A key derived from the scanned profile alone can never equal it, so a custom
// workspace that built perfectly read `none`: no image, no detail, and a Build
// click that re-ran the whole resolve (and emitted another run.build row) every
// single time.
func TestB4F2_CustomBaseImageReadsDoneFromTheRow(t *testing.T) {
	h := newHarness(t)
	const base = "ghcr.io/acme/base:1"
	st := &resolveImageStoreFake{}
	cfg := baseTestConfig(h, st)
	cfg.ImageBuilder = &capturingByoiImageBuilder{}
	srv := New(cfg)
	ws := types.Workspace{
		ID:        uuid.New(),
		Sources:   []types.WorkspaceSource{{Type: types.WorkspaceSourceTypeEphemeral, Target: "/home/agent/work"}},
		BaseImage: &types.WorkspaceBaseImage{Kind: "custom", Image: base},
	}
	built, ok := srv.resolveWorkspaceImage(context.Background(), uuid.New(), ws, nil)
	if !ok || built == "" {
		t.Fatal("resolveWorkspaceImage failed for a custom base image")
	}
	// What the real store holds once that build lands (the fake records rather
	// than applies the write — same replay TestResolveBuildView_AgreesWithBuiltHash
	// performs).
	ws.ImageRef, ws.BuiltProfileHash = built, st.builtHash
	srv.builds.finish(ws.ID, built, "")

	if v := srv.resolveBuildView(ws, workspaceReadFull); v.State != "done" || v.Image != built {
		t.Errorf("in-process view = %+v, want done/%q", v, built)
	}
	// And from a process that never ran the build — a restart, or the second
	// replica. This is the half the in-memory tracker could never answer.
	fresh := New(cfg)
	if v := fresh.resolveBuildView(ws, workspaceReadFull); v.State != "done" || v.Image != built {
		t.Errorf("after-restart view = %+v, want done/%q", v, built)
	}
}

// ─── F-2: drop must not release a live single-flight slot ──────────────────

// TestB4F2_DropNeverReleasesALiveBuildSlot: the invalidators drop the tracker
// entry, and an edit DURING a build would otherwise hand the single-flight slot
// back while the first build's goroutine is still inside the image builder — so
// the next Build click starts a SECOND envbuilder run for one workspace, both
// of them finishing into the same tracker and the same cache columns.
func TestB4F2_DropNeverReleasesALiveBuildSlot(t *testing.T) {
	h := newHarness(t)
	profile := workspacescan.WorkspaceProfile{
		Languages: []string{"Go"}, Confidence: workspacescan.ConfidenceHigh, Source: workspacescan.SourceDeterministic,
	}
	st := &b4BuildStore{ws: types.Workspace{
		ID: uuid.New(), Name: "w", Status: types.WorkspaceScanned,
		Sources: []types.WorkspaceSource{{Type: types.WorkspaceSourceTypeLocalDir, Path: "/home/u/payments"}},
		Profile: mustJSON(profile),
	}}
	builder := &blockingImageBuilder{gate: make(chan struct{})}
	cfg := baseTestConfig(h, st)
	cfg.ImageBuilder = builder
	srv := New(cfg)
	path := "/api/v1/workspaces/" + st.ws.ID.String()

	if w := do(t, srv, http.MethodPost, path+"/build", adminToken, ""); w.Code != http.StatusAccepted {
		t.Fatalf("first POST /build = %d, want 202; body=%s", w.Code, w.Body.String())
	}
	// Wait until that build is genuinely INSIDE the image builder (parked on the
	// gate) — the slot is only interesting while a goroutine is really holding it.
	for deadline := time.Now().Add(5 * time.Second); builder.callCount() == 0; {
		if time.Now().After(deadline) {
			t.Fatal("the first build never reached the image builder")
		}
		time.Sleep(time.Millisecond)
	}
	// A marker only THIS claim's entry carries: begin() starts every claim with a
	// fresh, empty log, so the marker surviving is proof the slot was never
	// re-claimed — a check that does not race the second build's goroutine.
	const marker = "first build's log line"
	srv.builds.appendLog(st.ws.ID, marker)

	// The operator edits the composition while that build is still running.
	if w := do(t, srv, http.MethodPut, path, adminToken,
		`{"name":"w","sources":[{"type":"local_dir","path":"/home/u/other"}]}`); w.Code != http.StatusOK {
		t.Fatalf("PUT /workspaces = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	if !srv.builds.get(st.ws.ID).Building {
		t.Fatal("the edit released the single-flight slot while the build was still live")
	}
	if w := do(t, srv, http.MethodPost, path+"/build", adminToken, ""); w.Code != http.StatusAccepted {
		t.Fatalf("second POST /build = %d, want 202; body=%s", w.Code, w.Body.String())
	}
	if !slices.Contains(srv.builds.get(st.ws.ID).Log, marker) {
		t.Error("the second click re-claimed the single-flight slot: two concurrent builds for one workspace")
	}

	close(builder.gate)
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); {
		if !srv.builds.get(st.ws.ID).Building {
			if n := builder.callCount(); n != 1 {
				t.Errorf("image builder called %d times for one workspace, want 1", n)
			}
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("the detached build never finished")
}

// ─── F-3: a failed build outlived the composition it failed against ────────

// TestB4F2_ARescanRetiresThePreviousBuildFailure: the tracker's `failed` arm is
// consulted BEFORE the row, so a build failure survived every invalidation the
// row half learned about — after a rescan moved the profile, GET /build still
// reported the previous composition's error until someone clicked Build again
// or wardynd restarted. The tracker now remembers WHICH composition it was
// building, and answers only about that one.
func TestB4F2_ARescanRetiresThePreviousBuildFailure(t *testing.T) {
	h := newHarness(t)
	profile := workspacescan.WorkspaceProfile{
		Languages: []string{"Go"}, Confidence: workspacescan.ConfidenceHigh, Source: workspacescan.SourceDeterministic,
	}
	st := &b4BuildStore{ws: types.Workspace{
		ID: uuid.New(), Name: "w", Status: types.WorkspaceScanned,
		Sources: []types.WorkspaceSource{{Type: types.WorkspaceSourceTypeLocalDir, Path: "/home/u/payments"}},
		Profile: mustJSON(profile),
	}}
	cfg := baseTestConfig(h, st)
	cfg.ImageBuilder = failingImageBuilder{}
	srv := New(cfg)
	path := "/api/v1/workspaces/" + st.ws.ID.String() + "/build"

	if w := do(t, srv, http.MethodPost, path, adminToken, ""); w.Code != http.StatusAccepted {
		t.Fatalf("POST /build = %d, want 202; body=%s", w.Code, w.Body.String())
	}
	for deadline := time.Now().Add(5 * time.Second); ; {
		if !srv.builds.get(st.ws.ID).Building {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the detached build never finished")
		}
		time.Sleep(5 * time.Millisecond)
	}

	get := func() buildResponse {
		t.Helper()
		w := do(t, srv, http.MethodGet, path, adminToken, "")
		if w.Code != http.StatusOK {
			t.Fatalf("GET /build = %d: %s", w.Code, w.Body.String())
		}
		var got buildResponse
		if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
			t.Fatal(err)
		}
		return got
	}
	// NEGATIVE CONTROL: against the composition it actually failed on, the
	// failure is exactly what the operator must see.
	if v := get(); v.State != "failed" {
		t.Fatalf("view = %+v, want failed for the composition the build ran against", v)
	}
	// A rescan lands a new profile: everything about what would be built has
	// changed, so the old attempt is not a report about it.
	st.setProfile(workspacescan.WorkspaceProfile{
		Languages: []string{"Go", "Python"}, Confidence: workspacescan.ConfidenceHigh, Source: workspacescan.SourceDeterministic,
	})
	if v := get(); v.State == "failed" {
		t.Errorf("after the rescan, GET /build still reports the previous composition's failure: %+v", v)
	}
}
