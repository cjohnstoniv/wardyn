// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
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

func (s *fkGrantStore) SetWorkspaceImportState(ctx context.Context, id uuid.UUID, status types.WorkspaceStatus, active *uuid.UUID, expectedActive *uuid.UUID) (types.Workspace, bool, error) {
	return s.importStateFake.SetWorkspaceImportState(ctx, id, status, active, expectedActive)
}
func (s *fkGrantStore) ClaimWorkspaceActiveRun(_ context.Context, _ uuid.UUID, runID uuid.UUID, _ *uuid.UUID) (types.Workspace, bool, error) {
	ws := s.ws
	ws.ActiveRunID = &runID
	return ws, true, nil
}

// GetSiteConfig is a no-op stub: launchRecordRun now folds the run's model
// access unconditionally (W20-W20-llm-transport-matrix-1), reaching
// defaultAgentRunsIntegration's GetSiteConfig read on every call — the
// embedded nil store.Store would otherwise panic here.
func (s *fkGrantStore) GetSiteConfig(context.Context) (types.SiteConfig, error) {
	return types.SiteConfig{}, nil
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
			ID:      wsID,
			Sources: []types.WorkspaceSource{{Type: types.WorkspaceSourceTypeRepo, Source: "acme/private-thing"}},
			Status:  types.WorkspaceScanned,
		}},
	}
}

// TestLaunchRecordRun_CloneGrantCreatedAfterRunRow pins the ORDERING that
// credential_grants' FK to agent_runs(id) requires: a repo workspace's record run
// must create its github_token clone grant only AFTER Store.CreateRun persists
// the run row. The counterfactual: with the grant creation folded back into
// wireWorkspaceSource (i.e. run one line BEFORE CreateRun), every CreateGrant
// here is FK-rejected, so no grant exists, WARDYN_GITHUB_GRANT_ID is never set,
// and the sandbox's private-repo clone 403s with no signal at all.
func TestLaunchRecordRun_CloneGrantCreatedAfterRunRow(t *testing.T) {
	h := newHarness(t)
	wsID := uuid.New()
	fake := newFKGrantStore(wsID)
	cfg := baseTestConfig(h, fake)
	cfg.Runner = &fakeRunner{}
	cfg.Broker = h.broker
	srv := New(cfg)

	_, _, err := srv.launchRecordRun(context.Background(), "alice@example.com", fake.ws,
		"build", "build", false)
	if err != nil {
		t.Fatalf("launchRecordRun on a github repo workspace failed: %v", err)
	}
	kinds := fake.grantKinds()
	if len(kinds) != 1 || kinds[0] != types.GrantGitHubToken {
		t.Fatalf("record run must persist exactly one github_token clone grant AFTER its run row exists; got %v", kinds)
	}
}

// TestLaunchRecordRun_RequiredSecretRowRidesAlong is the W8-S1-2 regression: a
// workspace's REQUIRED secret: contract row must ride a record/verify session
// the same way it rides a real run — the Verify carry card (step-requirements.tsx)
// promises "N required secrets ride proxy-side", but launchRecordRun used to
// hand-roll only the integration: case (requiredIntegrationIDs), silently
// dropping every secret: row. Local-dir workspace (no repo source) isolates
// the assertion to exactly the one grant the required secret produces — a repo
// source would also add its own github_token clone grant.
func TestLaunchRecordRun_RequiredSecretRowRidesAlong(t *testing.T) {
	h := newHarness(t)
	wsID := uuid.New()
	ws := types.Workspace{
		ID:      wsID,
		Sources: []types.WorkspaceSource{{Type: types.WorkspaceSourceTypeLocalDir, Path: "/work/acme"}},
		Status:  types.WorkspaceScanned,
		Requirements: map[string]types.WorkspaceRequirement{
			"secret:acme-deploy-key": {Level: "required", Provenance: "operator_set"},
		},
	}
	fake := &fkGrantStore{runs: map[uuid.UUID]types.AgentRun{}, importStateFake: importStateFake{ws: ws}}
	cfg := baseTestConfig(h, fake)
	cfg.Runner = &fakeRunner{}
	cfg.Broker = h.broker
	cfg.Secrets = &memSecrets{m: map[string][]byte{"acme-deploy-key": []byte("v")}}
	srv := New(cfg)

	_, _, err := srv.launchRecordRun(context.Background(), "alice@example.com", ws, "build", "build", false)
	if err != nil {
		t.Fatalf("launchRecordRun on a workspace with a required secret row failed: %v", err)
	}
	kinds := fake.grantKinds()
	if len(kinds) != 1 || kinds[0] != types.GrantAPIKey {
		t.Fatalf("record run must persist exactly one api_key grant for the required secret row; got %v", kinds)
	}
	var scope struct {
		SecretName string `json:"secret_name"`
	}
	if err := json.Unmarshal(fake.grants[0].Spec.Scope, &scope); err != nil {
		t.Fatalf("decode grant scope: %v", err)
	}
	if scope.SecretName != "acme-deploy-key" {
		t.Errorf("grant scope.secret_name = %q, want the required row's own secret (acme-deploy-key)", scope.SecretName)
	}
}

// TestMaybeGitHubReadGrant_ScopeMatchesBrokerKey pins the fix for a grant that
// could never mint. The scan/record clone grant used to carry `"repos": []`
// while the broker allowlist it is reached through was keyed from the CLONE
// URL — two different answers to "which repo is this token for". The real
// minter refuses an empty list outright (GitHub installation tokens are
// per-installation; the owner comes from the first repo), so every GitHub HTTPS
// scan/record clone 502'd at handleGitBroker once a real GitHub App was
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

// ─── ephemeral workspace targets (WARDYN_EPHEMERAL_DIRS) ─────────────────────

// TestWireWorkspaceSource_EphemeralTargetReturnedForDispatch pins audit row
// 56's fix at its source: an ephemeral workspace source has no mount/clone —
// wireWorkspaceSource must return its Target in ephemeralDirs (for the caller
// to thread into dispatchParams.EphemeralDirs) rather than silently dropping
// it, which is what the ephemeral case used to do.
func TestWireWorkspaceSource_EphemeralTargetReturnedForDispatch(t *testing.T) {
	var run types.AgentRun
	var policy types.RunPolicySpec
	ws := types.Workspace{
		ID: uuid.New(),
		Sources: []types.WorkspaceSource{
			{Type: types.WorkspaceSourceTypeEphemeral, Target: "/home/agent/scratch"},
			{Type: types.WorkspaceSourceTypeEphemeral}, // no target: nothing to surface
		},
	}

	_, ephemeralDirs, err := wireWorkspaceSource(&run, &policy, ws)
	if err != nil {
		t.Fatalf("wireWorkspaceSource: %v", err)
	}

	if want := []string{"/home/agent/scratch"}; !slices.Equal(ephemeralDirs, want) {
		t.Errorf("ephemeralDirs = %v, want %v", ephemeralDirs, want)
	}
	if len(policy.WorkspaceMounts) != 0 || len(policy.WorkspaceRepos) != 0 {
		t.Errorf("an ephemeral source must add no mount/clone policy entry; got mounts=%v repos=%v",
			policy.WorkspaceMounts, policy.WorkspaceRepos)
	}
}

// TestWireWorkspaceSource_EphemeralTargetMustPassValidateTarget pins the
// reconcile-wave1 fix: an ephemeral source never passes through ValidateMount
// (there is no mount, just WARDYN_EPHEMERAL_DIRS -> mkdir -p inside the
// sandbox), so wireWorkspaceSource is the only gate standing between a legacy
// row's target and the sandbox.
//
// STRENGTHENED (R1): it used to assert the bad target was DROPPED and the good
// one surfaced. Dropping is now a REFUSAL, and the change is deliberate — the
// guard moved above the type switch so it covers repo and local_dir too, and
// for those a drop is not an option: seedRequestWorkspace, the create path's
// sibling over the same stored rows, 422s the identical workspace, and the repo
// half's late drop (buildRepoRecords) was already silent enough to start a
// session with a repo that never cloned. A workspace cannot be legal on one run
// door and quietly broken on the other, so both now refuse and say which source.
func TestWireWorkspaceSource_EphemeralTargetMustPassValidateTarget(t *testing.T) {
	var run types.AgentRun
	var policy types.RunPolicySpec
	ws := types.Workspace{
		ID: uuid.New(),
		Sources: []types.WorkspaceSource{
			{Type: types.WorkspaceSourceTypeEphemeral, Target: "/root/.ssh"},          // outside allowedTargetPrefixes
			{Type: types.WorkspaceSourceTypeEphemeral, Target: "/home/agent/scratch"}, // valid
		},
	}

	_, ephemeralDirs, err := wireWorkspaceSource(&run, &policy, ws)
	if err == nil {
		t.Fatalf("an out-of-allowlist ephemeral target was accepted; ephemeralDirs=%v", ephemeralDirs)
	}
	if !strings.Contains(err.Error(), "/root/.ssh") && !strings.Contains(err.Error(), "target") {
		t.Errorf("refusal = %q, want it to name the offending target", err)
	}
	// Nothing is surfaced for dispatch on a refusal — not even the valid sibling.
	if len(ephemeralDirs) != 0 {
		t.Errorf("ephemeralDirs = %v on a refused workspace, want none", ephemeralDirs)
	}
}

// TestWireWorkspaceSource_ReservedDriveTargetRefusedOnEveryBranch is the
// reserved-drive rule stated as requirement 11 says it: /home/agent/drive is
// refused as an authored target EVERYWHERE.
//
// It was refused on the create-run path (seedRequestWorkspace re-validates every
// stored source and 422s) and on the ONBOARDING write, but the record/verify
// composition re-validated only its EPHEMERAL branch — the one branch whose
// target is a mkdir rather than a mount. So a stored local_dir source targeting
// the reserved path became a real operator bind mount at exactly the path
// driveMountFor calls "the path validateWorkspaceSources/validatePolicySpec
// refuse to let anyone else name", and a repo source targeting it was dropped
// LATE and SILENTLY by buildRepoRecords, starting a session with a repo that
// never cloned.
//
// Nothing downstream re-imposed it, which is the load-bearing half: the composed
// record policy never passes through validatePolicySpec, and the driver's own
// gate is runner.ValidateMount -> ValidateTarget, which does NOT carry the
// reserved rule (targetReservedForDrive is reached only from
// ValidateAuthoredTarget). The control below asserts exactly that, so this test
// fails if someone "fixes" it by trusting the driver.
func TestWireWorkspaceSource_ReservedDriveTargetRefusedOnEveryBranch(t *testing.T) {
	// The control first: the driver's gate accepts the reserved path, so the
	// refusal has to come from here.
	if err := runner.ValidateTarget(runner.DriveTarget); err != nil {
		t.Fatalf("premise changed: runner.ValidateTarget now refuses %q (%v) — if the driver carries the "+
			"reserved rule, re-argue where this guard belongs", runner.DriveTarget, err)
	}
	if err := runner.ValidateAuthoredTarget(runner.DriveTarget); err == nil {
		t.Fatal("premise changed: ValidateAuthoredTarget no longer refuses the reserved drive target")
	}

	for _, c := range []struct {
		name string
		src  types.WorkspaceSource
	}{
		{"local_dir at the reserved path", types.WorkspaceSource{
			Type: types.WorkspaceSourceTypeLocalDir, Path: "/home/operator/legacy",
			Target: runner.DriveTarget, Writable: true,
		}},
		{"repo UNDER the reserved subtree", types.WorkspaceSource{
			Type: types.WorkspaceSourceTypeRepo, Source: "octocat/Hello-World",
			Target: runner.DriveTarget + "/repo",
		}},
		{"ephemeral under the reserved subtree", types.WorkspaceSource{
			Type: types.WorkspaceSourceTypeEphemeral, Target: runner.DriveTarget + "/scratch",
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			var run types.AgentRun
			var policy types.RunPolicySpec
			ws := types.Workspace{ID: uuid.New(), Sources: []types.WorkspaceSource{c.src}}
			_, eph, err := wireWorkspaceSource(&run, &policy, ws)
			if err == nil {
				t.Fatalf("composed unrefused: mounts=%+v repos=%+v ephemeral=%v", policy.WorkspaceMounts, policy.WorkspaceRepos, eph)
			}
			if !strings.Contains(err.Error(), "reserved for the user drive") {
				t.Errorf("refusal = %q, want the reserved-drive wording a reader can match against the create path's", err)
			}
			// Nothing composed on a refusal — a mount that reached the policy
			// would be the bind this test is about, error or not.
			if len(policy.WorkspaceMounts) != 0 || len(policy.WorkspaceRepos) != 0 || len(eph) != 0 {
				t.Errorf("a refused source still composed mounts=%+v repos=%+v ephemeral=%v",
					policy.WorkspaceMounts, policy.WorkspaceRepos, eph)
			}
		})
	}

	// The other direction: an ORDINARY /home/agent target still composes, so the
	// guard is scoped to the reserved subtree rather than to the prefix.
	t.Run("an ordinary /home/agent target still composes", func(t *testing.T) {
		var run types.AgentRun
		var policy types.RunPolicySpec
		ws := types.Workspace{ID: uuid.New(), Sources: []types.WorkspaceSource{
			{Type: types.WorkspaceSourceTypeLocalDir, Path: "/home/operator/app", Target: "/home/agent/work"},
		}}
		if _, _, err := wireWorkspaceSource(&run, &policy, ws); err != nil {
			t.Fatalf("an ordinary target was refused: %v", err)
		}
		if len(policy.WorkspaceMounts) != 1 {
			t.Fatalf("mounts = %+v, want the one ordinary bind", policy.WorkspaceMounts)
		}
	})
}
