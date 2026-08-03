// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/internal/workspacescan"
)

// ─── clone-grant FK ordering (verify/record) ─────────────────────────────────

// fkGrantStore is the fake this regression needs: unlike the other api fakes
// (whose CreateGrant has NO referential integrity — which is why an always-
// failing grant INSERT survived the whole unit suite), it enforces exactly what
// Postgres enforces — credential_grants.run_id REFERENCES agent_runs(id),
// immediate — by rejecting a grant whose run row does not exist yet.
type fkGrantStore struct {
	store.Store
	importStateFake
	mu     sync.Mutex
	runs   map[uuid.UUID]types.AgentRun
	grants []types.CredentialGrant
	built  string // last SetWorkspaceBuiltImage image_ref
	failed bool   // UpdateRunStateIf drove the run to a terminal state
	// skipRunInsert drops the run row on the floor so every CreateGrant hits the
	// fake's FK — stands in for any store-side grant failure.
	skipRunInsert bool
}

func (s *fkGrantStore) SetWorkspaceImportState(ctx context.Context, id uuid.UUID, status types.WorkspaceStatus, active *uuid.UUID, expectedActive *uuid.UUID, vr json.RawMessage, vh string, va *time.Time) (types.Workspace, bool, error) {
	return s.importStateFake.SetWorkspaceImportState(ctx, id, status, active, expectedActive, vr, vh, va)
}
func (s *fkGrantStore) ClaimWorkspaceActiveRun(_ context.Context, _ uuid.UUID, runID uuid.UUID, _ *uuid.UUID) (types.Workspace, bool, error) {
	ws := s.ws
	ws.ActiveRunID = &runID
	return ws, true, nil
}
func (s *fkGrantStore) ClearWorkspaceActiveRun(context.Context, uuid.UUID, uuid.UUID) (bool, error) {
	return true, nil
}
func (s *fkGrantStore) SetWorkspaceRecordResult(_ context.Context, _ uuid.UUID, _ string, _ json.RawMessage, _ string) (types.Workspace, bool, error) {
	return s.ws, true, nil
}
func (s *fkGrantStore) SetWorkspaceBuiltImage(_ context.Context, _ uuid.UUID, imageRef, hash string) (types.Workspace, error) {
	s.mu.Lock()
	s.built = imageRef
	s.mu.Unlock()
	ws := s.ws
	ws.ImageRef, ws.BuiltProfileHash = imageRef, hash
	return ws, nil
}

// builtImage reports the image resolveWorkspaceImage cached on the workspace —
// "" when the build never produced one (e.g. it was cancelled).
func (s *fkGrantStore) builtImage() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.built
}
func (s *fkGrantStore) CreateRun(_ context.Context, run types.AgentRun) (types.AgentRun, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.skipRunInsert {
		s.runs[run.ID] = run
	}
	return run, nil
}
func (s *fkGrantStore) GetRun(_ context.Context, id uuid.UUID) (types.AgentRun, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	run, ok := s.runs[id]
	if !ok {
		return types.AgentRun{}, store.ErrNotFound
	}
	return run, nil
}
func (s *fkGrantStore) UpdateRunStateIf(_ context.Context, _ uuid.UUID, _, _ types.RunState) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failed = true
	return false, nil // no cascade: keeps the fake's surface minimal
}
func (s *fkGrantStore) CreateGrant(_ context.Context, g types.CredentialGrant) (types.CredentialGrant, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.runs[g.RunID]; !ok {
		// 23503 foreign_key_violation, exactly as PG raises it on this INSERT.
		return types.CredentialGrant{}, fmt.Errorf(
			"insert credential_grants: foreign key violation: run %s has no agent_runs row", g.RunID)
	}
	s.grants = append(s.grants, g)
	return g, nil
}
func (s *fkGrantStore) grantKinds() []types.GrantKind {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []types.GrantKind
	for _, g := range s.grants {
		out = append(out, g.Spec.Kind)
	}
	return out
}

func newFKGrantStore(wsID uuid.UUID) *fkGrantStore {
	return &fkGrantStore{
		runs: map[uuid.UUID]types.AgentRun{},
		importStateFake: importStateFake{ws: types.Workspace{
			ID: wsID, Kind: types.WorkspaceKindRepo, Source: "acme/private-thing",
			Status: types.WorkspaceScanned,
			SetupCommands: mustJSON([]workspacescan.SetupCommand{
				{Stage: "install", Command: "npm ci"},
			}),
		}},
	}
}

// TestLaunchVerifyRun_CloneGrantCreatedAfterRunRow pins the ORDERING that
// credential_grants' FK to agent_runs(id) requires: a repo workspace's verify run
// must create its github_token clone grant only AFTER Store.CreateRun persists
// the run row. The counterfactual: with the grant creation folded back into
// wireWorkspaceSource (i.e. run one line BEFORE CreateRun), every CreateGrant
// here is FK-rejected, so no grant exists, WARDYN_GITHUB_GRANT_ID is never set,
// and the sandbox's private-repo clone 403s with no signal at all.
func TestLaunchVerifyRun_CloneGrantCreatedAfterRunRow(t *testing.T) {
	h := newHarness(t)
	wsID := uuid.New()
	fake := newFKGrantStore(wsID)
	cfg := baseTestConfig(h, fake)
	cfg.Runner = &fakeRunner{}
	cfg.Broker = h.broker
	srv := New(cfg)

	_, err := srv.launchVerifyRun(context.Background(), "alice@example.com", fake.ws, fake.ws.SetupCommands)
	if err != nil {
		t.Fatalf("launchVerifyRun on a github repo workspace failed: %v", err)
	}
	kinds := fake.grantKinds()
	if len(kinds) != 1 || kinds[0] != types.GrantGitHubToken {
		t.Fatalf("verify run must persist exactly one github_token clone grant AFTER its run row exists; got %v", kinds)
	}
}

// TestLaunchRecordRun_CloneGrantCreatedAfterRunRow is the record-lane twin of
// TestLaunchVerifyRun_CloneGrantCreatedAfterRunRow — the same inverted order
// lived in both launchers.
func TestLaunchRecordRun_CloneGrantCreatedAfterRunRow(t *testing.T) {
	h := newHarness(t)
	wsID := uuid.New()
	fake := newFKGrantStore(wsID)
	cfg := baseTestConfig(h, fake)
	cfg.Runner = &fakeRunner{}
	cfg.Broker = h.broker
	srv := New(cfg)

	_, _, err := srv.launchRecordRun(context.Background(), "alice@example.com", fake.ws,
		"build", "build", recordModeInteractive, false)
	if err != nil {
		t.Fatalf("launchRecordRun on a github repo workspace failed: %v", err)
	}
	kinds := fake.grantKinds()
	if len(kinds) != 1 || kinds[0] != types.GrantGitHubToken {
		t.Fatalf("record run must persist exactly one github_token clone grant AFTER its run row exists; got %v", kinds)
	}
}

// ─── image build survives a client disconnect ────────────────────────────────

// ctxImageBuilder honors caller cancellation the way the real envbuild builder
// does (runBuildAndFinalize returns on ctx.Done and kills the build container).
type ctxImageBuilder struct{ built string }

func (b *ctxImageBuilder) BuildDevcontainer(ctx context.Context, _, _, _ string) (string, error) {
	return b.BuildFromDevcontainerFiles(ctx, nil, "")
}
func (b *ctxImageBuilder) BuildFromDevcontainerFiles(ctx context.Context, _ map[string]string, _ string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", fmt.Errorf("envbuild: build cancelled or timed out: %w", err)
	}
	return b.built, nil
}
func (b *ctxImageBuilder) FinalizeBase(context.Context, string, string) (string, error) {
	return "", nil
}

// TestLaunchVerifyRun_ImageBuildSurvivesClientDisconnect pins one layer
// higher than dispatch: the launcher's own pre-dispatch work — mint, CreateRun,
// status flip and the multi-minute devcontainer BUILD — must not run on the
// cancellable HTTP request context. resolveWorkspaceImage fails OPEN by
// contract, so a caller that walks away mid-build otherwise silently downgrades
// the run to the Node-only convention image, which then dies on a missing
// toolchain (exit 127) and points the operator at their WARDYN_AGENT_IMAGES
// config for a failure that was really a cancelled build. The counterfactual:
// without launchVerifyRun's context.WithoutCancel, the build sees the cancelled
// ctx and the run dispatches with the convention image.
func TestLaunchVerifyRun_ImageBuildSurvivesClientDisconnect(t *testing.T) {
	h := newHarness(t)
	wsID := uuid.New()
	fake := newFKGrantStore(wsID)
	// A scanned profile is what makes resolveWorkspaceImage generate + build.
	fake.ws.Profile = mustJSON(workspacescan.WorkspaceProfile{Languages: []string{"Go"}})
	builder := &ctxImageBuilder{built: "wardyn-workspace/built:abc123"}
	cfg := baseTestConfig(h, fake)
	cfg.Runner = &fakeRunner{}
	cfg.Broker = h.broker
	cfg.ImageBuilder = builder
	srv := New(cfg)

	// The client hangs up the moment the handler hands off to the launcher.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := srv.launchVerifyRun(ctx, "alice@example.com", fake.ws, fake.ws.SetupCommands); err != nil {
		t.Fatalf("launchVerifyRun after client disconnect: %v", err)
	}
	if got := fake.builtImage(); got != builder.built {
		t.Fatalf("verify run image = %q, want the BUILT image %q — a client disconnect must not cancel the build and silently downgrade the run to the convention image",
			got, builder.built)
	}
}

// TestLaunchVerifyRun_CloneGrantFailureAbortsLaunch pins the other half of the
// same root cause: a CreateGrant failure must never be SWALLOWED. A repo whose
// clone needs a credential has to fail the launch loudly rather than dispatch a
// sandbox whose clone is guaranteed to fail auth. The counterfactual: with
// maybeGitHubReadGrant's `return nil` swallow restored, launchVerifyRun returns
// no error and the run dispatches credential-less.
func TestLaunchVerifyRun_CloneGrantFailureAbortsLaunch(t *testing.T) {
	h := newHarness(t)
	wsID := uuid.New()
	fake := newFKGrantStore(wsID)
	fake.skipRunInsert = true
	cfg := baseTestConfig(h, fake)
	cfg.Runner = &fakeRunner{}
	cfg.Broker = h.broker
	srv := New(cfg)

	if _, err := srv.launchVerifyRun(context.Background(), "alice@example.com", fake.ws, fake.ws.SetupCommands); err == nil {
		t.Fatal("a clone grant that cannot be persisted must FAIL the verify launch, not silently dispatch a sandbox that cannot authenticate its clone")
	}
}

// TestMaybeGitHubReadGrant_ScopeMatchesBrokerKey pins the fix for a grant that
// could never mint. The scan/verify/record clone grant used to carry
// `"repos": []` while the broker allowlist it is reached through was keyed from
// the CLONE URL — two different answers to "which repo is this token for". The
// real minter refuses an empty list outright (GitHub installation tokens are
// per-installation; the owner comes from the first repo), so every GitHub HTTPS
// scan/verify/record clone 502'd at handleGitBroker once a real GitHub App was
// configured. No test saw it because FakeGitHubMinter did not reproduce that
// precondition (it does now — internal/broker.TestMintForGrant_EmptyRepoScopeFails).
//
// The invariant this pins is the one that was broken: the grant's scope.repos
// and the broker map key are THE SAME repo, derived from the same function.
func TestMaybeGitHubReadGrant_ScopeMatchesBrokerKey(t *testing.T) {
	h := newHarness(t)
	wsID := uuid.New()
	fake := newFKGrantStore(wsID)
	srv := New(baseTestConfig(h, fake))

	runID := uuid.New()
	fake.runs[runID] = types.AgentRun{ID: runID} // satisfy the fake's FK check

	const cloneURL = "https://github.com/acme/Private-Thing.git"
	gid, err := srv.maybeGitHubReadGrant(context.Background(), runID, time.Now(), cloneURL)
	if err != nil || gid == nil {
		t.Fatalf("maybeGitHubReadGrant(%q) = (%v, %v), want a grant", cloneURL, gid, err)
	}
	if got := len(fake.grants); got != 1 {
		t.Fatalf("persisted %d grants, want 1", got)
	}
	repos := githubScopeRepos(fake.grants[0].Spec.Scope)
	if len(repos) != 1 {
		t.Fatalf("grant scope.repos = %v — an empty list is unmintable (broker: github token requires at least one repo)", repos)
	}
	// The broker map the proxy serves this grant on, keyed from the clone URL.
	brokerMap := gitBrokerGrant(cloneURL, gid)
	if _, ok := brokerMap[repos[0]]; !ok {
		t.Fatalf("grant scope.repos = %v but the broker route is keyed %v — the token is scoped to a repo the route does not serve",
			repos, brokerMap)
	}

	// A github.com URL with no derivable "<org>/<repo>" yields NO grant: an
	// unmintable grant is worse than none, because it also sets
	// WARDYN_GITHUB_GRANT_ID and points the in-sandbox helper at a mint that can
	// only fail.
	deep, err := srv.maybeGitHubReadGrant(context.Background(), runID, time.Now(), "https://github.com/acme")
	if err != nil || deep != nil {
		t.Fatalf("a github URL with no <org>/<repo> must yield no grant; got (%v, %v)", deep, err)
	}
}
