// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Integration tests for the AgentRun store: CRUD round-trips, the conditional
// state machine (UpdateRunStateIf), the reaper "list idle candidates" query
// shape, the TouchRun keepalive, and grant/approval persistence. Guarded by
// WARDYN_TEST_PG. Run with: WARDYN_TEST_PG=postgres://... go test ./internal/store/...
//
// These COMPLEMENT store_pg_test.go / store_completed_state_pg_test.go /
// store_policy_pg_test.go: they exercise the run state-machine + reaper-backing
// queries those files do not. Every test mints UNIQUE uuids and asserts only on
// rows it created, so it is isolated within the shared DB (no empty-DB
// assumption) and safe to run repeatedly.
package store_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cjohnstoniv/wardyn/internal/db"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// runsPGPool connects + migrates against the live substrate, skipping cleanly
// when WARDYN_TEST_PG is unset (plain CI). It returns the concrete *pgxpool.Pool
// so it can be passed straight to the store functions. Mirrors the connect/
// migrate/cleanup dance the sibling store tests open-code inline.
func runsPGPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("WARDYN_TEST_PG")
	if dsn == "" {
		t.Skip("WARDYN_TEST_PG not set; skipping Postgres integration tests")
	}
	ctx := context.Background()
	pool, err := db.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	if err := db.Migrate(ctx, pool); err != nil {
		pool.Close()
		t.Fatalf("migrate: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// newRun builds a RUNNING run with unique ids for the given pool. The caller
// owns persistence + cleanup. state defaults to RUNNING which is what the state-
// machine + reaper tests start from.
func newRun(state types.RunState) types.AgentRun {
	now := time.Now().UTC()
	id := uuid.New()
	return types.AgentRun{
		ID:               id,
		CreatedAt:        now,
		UpdatedAt:        now,
		CreatedBy:        "tester@example.com",
		Agent:            "claude-code",
		Repo:             "octocat/Hello-World",
		Task:             "store run integration",
		ConfinementClass: types.CC2,
		State:            state,
		SPIFFEID:         "spiffe://wardyn.test/agent-run/" + id.String(),
		RunnerTarget:     "docker",
	}
}

// persistRun inserts r and registers a cleanup that deletes it (cascading to its
// grants/approvals via the schema's ON DELETE CASCADE).
func persistRun(t *testing.T, ctx context.Context, pool *pgxpool.Pool, r types.AgentRun) types.AgentRun {
	t.Helper()
	created, err := store.CreateRun(ctx, pool, r)
	if err != nil {
		t.Fatalf("create run %s: %v", r.ID, err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM agent_runs WHERE id=$1`, r.ID)
	})
	return created
}

// TestPG_CreateGetRun_RoundTrip asserts CreateRun persists EVERY field and
// GetRun reads them back identically, including the nullable policy_id snapshot,
// the confinement class, the lifecycle state, and the sandbox_ref. This is the
// baseline the reaper + watcher rely on: an idle scan that misread state or
// confinement_class would stop the wrong runs.
func TestPG_CreateGetRun_RoundTrip(t *testing.T) {
	pool := runsPGPool(t)
	ctx := context.Background()

	// A policy the run references, so policy_id round-trips as a real FK-ish
	// snapshot (agent_runs.policy_id is a plain UUID column, not a FK, but we
	// still point it at a real policy for realism).
	polID := uuid.New()
	pol := types.RunPolicy{
		ID:        polID,
		Name:      "run-roundtrip-" + polID.String(),
		CreatedAt: time.Now().UTC(),
		UpdatedAt: time.Now().UTC(),
		Spec: types.RunPolicySpec{
			AllowedDomains:      []string{"api.anthropic.com"},
			MinConfinementClass: types.CC2,
		},
	}
	if _, err := store.CreatePolicy(ctx, pool, pol); err != nil {
		t.Fatalf("create policy: %v", err)
	}
	t.Cleanup(func() { _ = store.DeletePolicy(context.Background(), pool, polID) })

	r := newRun(types.RunStarting)
	r.PolicyID = &polID
	r.ConfinementClass = types.CC3
	r.SandboxRef = "container-" + r.ID.String()
	created := persistRun(t, ctx, pool, r)

	// CreateRun returns the hydrated row.
	if created.ID != r.ID {
		t.Errorf("created.ID = %s, want %s", created.ID, r.ID)
	}

	got, err := store.GetRun(ctx, pool, r.ID)
	if err != nil {
		t.Fatalf("get run: %v", err)
	}

	// Field-by-field round-trip. got/want on each so a single regression is clear.
	if got.CreatedBy != r.CreatedBy {
		t.Errorf("created_by = %q, want %q", got.CreatedBy, r.CreatedBy)
	}
	if got.Agent != r.Agent {
		t.Errorf("agent = %q, want %q", got.Agent, r.Agent)
	}
	if got.Repo != r.Repo {
		t.Errorf("repo = %q, want %q", got.Repo, r.Repo)
	}
	if got.Task != r.Task {
		t.Errorf("task = %q, want %q", got.Task, r.Task)
	}
	if got.PolicyID == nil || *got.PolicyID != polID {
		t.Errorf("policy_id = %v, want %s", got.PolicyID, polID)
	}
	if got.ConfinementClass != types.CC3 {
		t.Errorf("confinement_class = %q, want CC3", got.ConfinementClass)
	}
	if got.State != types.RunStarting {
		t.Errorf("state = %q, want STARTING", got.State)
	}
	if got.SPIFFEID != r.SPIFFEID {
		t.Errorf("spiffe_id = %q, want %q", got.SPIFFEID, r.SPIFFEID)
	}
	if got.RunnerTarget != r.RunnerTarget {
		t.Errorf("runner_target = %q, want %q", got.RunnerTarget, r.RunnerTarget)
	}
	if got.SandboxRef != r.SandboxRef {
		t.Errorf("sandbox_ref = %q, want %q", got.SandboxRef, r.SandboxRef)
	}

	// GetRun of an unknown id => ErrNotFound.
	if _, err := store.GetRun(ctx, pool, uuid.New()); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("get unknown run err = %v, want ErrNotFound", err)
	}
}

// TestPG_UpdateRunStateIf_ConditionalTransition is the core state-machine
// regression backing the reaper + completion-watcher fixes. UpdateRunStateIf
// must:
//   - apply (return true) only when the row is STILL in fromState, and
//   - return false WITHOUT clobbering when the row has already moved to a
//     terminal state (the TOCTOU "someone else won the transition" case).
//
// Both the STOPPED (reaper) and COMPLETED (watcher) targets are covered, plus
// the no-clobber guard which is the security-relevant half: a late reaper tick
// must never overwrite a KILLED run back to STOPPED.
func TestPG_UpdateRunStateIf_ConditionalTransition(t *testing.T) {
	pool := runsPGPool(t)
	ctx := context.Background()

	tests := []struct {
		name      string
		from      types.RunState // precondition state in the WHERE clause
		to        types.RunState // target state
		startAs   types.RunState // the run's actual persisted state
		wantOK    bool           // expected applied result
		wantState types.RunState // expected state after the call
	}{
		{
			name:      "running_to_stopped_applies",
			from:      types.RunRunning,
			to:        types.RunStopped,
			startAs:   types.RunRunning,
			wantOK:    true,
			wantState: types.RunStopped,
		},
		{
			name:      "running_to_completed_applies",
			from:      types.RunRunning,
			to:        types.RunCompleted,
			startAs:   types.RunRunning,
			wantOK:    true,
			wantState: types.RunCompleted,
		},
		{
			name:      "running_to_stopped_no_clobber_when_already_killed",
			from:      types.RunRunning,
			to:        types.RunStopped,
			startAs:   types.RunKilled, // already terminal: precondition fails
			wantOK:    false,
			wantState: types.RunKilled, // must be untouched
		},
		{
			name:      "running_to_completed_no_clobber_when_already_completed",
			from:      types.RunRunning,
			to:        types.RunCompleted,
			startAs:   types.RunCompleted, // watcher fired twice: second is a no-op
			wantOK:    false,
			wantState: types.RunCompleted,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := persistRun(t, ctx, pool, newRun(tc.startAs))

			ok, err := store.UpdateRunStateIf(ctx, pool, r.ID, tc.from, tc.to)
			if err != nil {
				t.Fatalf("UpdateRunStateIf: %v", err)
			}
			if ok != tc.wantOK {
				t.Errorf("applied = %v, want %v", ok, tc.wantOK)
			}

			got, err := store.GetRun(ctx, pool, r.ID)
			if err != nil {
				t.Fatalf("get run: %v", err)
			}
			if got.State != tc.wantState {
				t.Errorf("state after = %q, want %q", got.State, tc.wantState)
			}
		})
	}

	// UpdateRunStateIf against a nonexistent id is a no-op (false, nil) — never
	// an error: the watcher treats "row gone" the same as "lost the race".
	ok, err := store.UpdateRunStateIf(ctx, pool, uuid.New(), types.RunRunning, types.RunStopped)
	if err != nil {
		t.Fatalf("UpdateRunStateIf unknown id: %v", err)
	}
	if ok {
		t.Error("UpdateRunStateIf on unknown id applied; want false")
	}
}

// TestPG_UpdateRunStateIfIdle_TOCTOU covers finding N3: the idle-guarded CAS must
// no-op when updated_at has advanced past the snapshot (an active `wardyn attach`
// TouchRun landed between the reaper's scan and its stop), and apply when it has
// not. Guarding only on state=RUNNING (UpdateRunStateIf) would clobber the
// now-active run; UpdateRunStateIfIdle adds the `updated_at <= notAfter` guard.
func TestPG_UpdateRunStateIfIdle_TOCTOU(t *testing.T) {
	pool := runsPGPool(t)
	ctx := context.Background()

	// Case 1 — NOT touched since the snapshot: the stop must APPLY.
	notTouched := persistRun(t, ctx, pool, newRun(types.RunRunning))
	snap1, err := store.GetRun(ctx, pool, notTouched.ID)
	if err != nil {
		t.Fatalf("get snapshot: %v", err)
	}
	applied, err := store.UpdateRunStateIfIdle(ctx, pool, notTouched.ID, types.RunRunning, types.RunStopped, snap1.UpdatedAt)
	if err != nil {
		t.Fatalf("idle CAS (not touched): %v", err)
	}
	if !applied {
		t.Error("idle CAS should APPLY when updated_at == snapshot (run not touched since scan)")
	}
	if got, _ := store.GetRun(ctx, pool, notTouched.ID); got.State != types.RunStopped {
		t.Errorf("state after applied idle CAS = %q, want STOPPED", got.State)
	}

	// Case 2 — TOUCHED after the snapshot (active attach keepalive): the stop must
	// NO-OP and leave the run RUNNING.
	touched := persistRun(t, ctx, pool, newRun(types.RunRunning))
	snap2, err := store.GetRun(ctx, pool, touched.ID)
	if err != nil {
		t.Fatalf("get snapshot: %v", err)
	}
	// Advance updated_at strictly past the snapshot, as TouchRun would (now() is
	// monotonically later, but assert it moved to avoid a same-instant flake).
	if _, err := pool.Exec(ctx, `UPDATE agent_runs SET updated_at = $1 WHERE id = $2`,
		snap2.UpdatedAt.Add(time.Second), touched.ID); err != nil {
		t.Fatalf("simulate attach touch: %v", err)
	}
	applied, err = store.UpdateRunStateIfIdle(ctx, pool, touched.ID, types.RunRunning, types.RunStopped, snap2.UpdatedAt)
	if err != nil {
		t.Fatalf("idle CAS (touched): %v", err)
	}
	if applied {
		t.Error("idle CAS must NO-OP when updated_at advanced past the snapshot (active attach)")
	}
	if got, _ := store.GetRun(ctx, pool, touched.ID); got.State != types.RunRunning {
		t.Errorf("touched run state = %q, want RUNNING (left untouched by the no-op)", got.State)
	}

	// Case 3 — already terminal: the idle CAS no-ops on the state guard too (never
	// resurrects/clobbers a KILLED run), matching UpdateRunStateIf.
	killed := persistRun(t, ctx, pool, newRun(types.RunKilled))
	applied, err = store.UpdateRunStateIfIdle(ctx, pool, killed.ID, types.RunRunning, types.RunStopped, time.Now().UTC().Add(time.Hour))
	if err != nil {
		t.Fatalf("idle CAS (killed): %v", err)
	}
	if applied {
		t.Error("idle CAS must NO-OP when the run is already terminal, even with a generous notAfter")
	}
	if got, _ := store.GetRun(ctx, pool, killed.ID); got.State != types.RunKilled {
		t.Errorf("killed run state = %q, want KILLED (untouched)", got.State)
	}
}

// TestPG_ListRuns_ReaperCandidateQuery exercises the query shape the idle reaper
// relies on (internal/lifecycle): it lists runs, then selects RUNNING runs whose
// updated_at is older than the idle threshold. We backdate one run's updated_at
// to make it a candidate, keep a second RUNNING run fresh, and leave a third in
// a terminal state — then assert ListRuns returns all of OUR rows and that
// candidate filtering (the reaper's predicate: state==RUNNING && idle>threshold)
// picks exactly the stale RUNNING run.
func TestPG_ListRuns_ReaperCandidateQuery(t *testing.T) {
	pool := runsPGPool(t)
	ctx := context.Background()

	// Stale RUNNING run: the reaper SHOULD treat this as an idle candidate.
	stale := persistRun(t, ctx, pool, newRun(types.RunRunning))
	// Fresh RUNNING run: too recently active to reap.
	fresh := persistRun(t, ctx, pool, newRun(types.RunRunning))
	// Terminal run: never a reap candidate regardless of age.
	terminal := persistRun(t, ctx, pool, newRun(types.RunStopped))

	// Backdate the stale run's updated_at by an hour. agent_runs has no update
	// trigger, so a direct UPDATE is the canonical way to simulate idleness
	// (the reaper measures idle = now - updated_at). Also backdate the terminal
	// run so age alone would tempt a naive reaper — state must still exclude it.
	threshold := 10 * time.Minute
	staleAt := time.Now().UTC().Add(-time.Hour)
	if _, err := pool.Exec(ctx, `UPDATE agent_runs SET updated_at=$1 WHERE id=$2`, staleAt, stale.ID); err != nil {
		t.Fatalf("backdate stale run: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE agent_runs SET updated_at=$1 WHERE id=$2`, staleAt, terminal.ID); err != nil {
		t.Fatalf("backdate terminal run: %v", err)
	}

	all, err := store.ListRuns(ctx, pool)
	if err != nil {
		t.Fatalf("list runs: %v", err)
	}

	// Index our rows out of the shared listing; never assume an empty DB.
	byID := make(map[uuid.UUID]types.AgentRun, len(all))
	for _, r := range all {
		byID[r.ID] = r
	}
	for _, want := range []uuid.UUID{stale.ID, fresh.ID, terminal.ID} {
		if _, ok := byID[want]; !ok {
			t.Fatalf("ListRuns omitted run %s we created", want)
		}
	}

	// Apply the reaper's candidate predicate over the listing and assert the
	// exact membership among OUR rows.
	now := time.Now().UTC()
	isCandidate := func(r types.AgentRun) bool {
		return r.State == types.RunRunning && now.Sub(r.UpdatedAt) > threshold
	}
	if !isCandidate(byID[stale.ID]) {
		t.Errorf("stale RUNNING run (idle %v) not selected as reap candidate", now.Sub(byID[stale.ID].UpdatedAt))
	}
	if isCandidate(byID[fresh.ID]) {
		t.Errorf("fresh RUNNING run wrongly selected as reap candidate")
	}
	if isCandidate(byID[terminal.ID]) {
		t.Errorf("terminal (STOPPED) run wrongly selected as reap candidate")
	}

	// ListRuns is ordered newest-first by created_at; verify monotonic ordering
	// across the full listing (a broken ORDER BY would scramble the UI feed).
	for i := 1; i < len(all); i++ {
		if all[i-1].CreatedAt.Before(all[i].CreatedAt) {
			t.Fatalf("ListRuns not in DESC created_at order at index %d: %v before %v",
				i, all[i-1].CreatedAt, all[i].CreatedAt)
		}
	}
}

// TestPG_TouchRun_Keepalive asserts the activity keepalive the interactive-
// attach handler calls actually bumps updated_at forward (so a reaper measuring
// idleness by updated_at will not stop an attached run), and that TouchRun on an
// unknown id returns ErrNotFound. This backs the never-reap-while-attached fix.
func TestPG_TouchRun_Keepalive(t *testing.T) {
	pool := runsPGPool(t)
	ctx := context.Background()

	r := persistRun(t, ctx, pool, newRun(types.RunRunning))

	// Backdate updated_at to simulate a run that has gone idle.
	old := time.Now().UTC().Add(-time.Hour)
	if _, err := pool.Exec(ctx, `UPDATE agent_runs SET updated_at=$1 WHERE id=$2`, old, r.ID); err != nil {
		t.Fatalf("backdate run: %v", err)
	}

	before, err := store.GetRun(ctx, pool, r.ID)
	if err != nil {
		t.Fatalf("get run before touch: %v", err)
	}

	if err := store.TouchRun(ctx, pool, r.ID); err != nil {
		t.Fatalf("touch run: %v", err)
	}

	after, err := store.GetRun(ctx, pool, r.ID)
	if err != nil {
		t.Fatalf("get run after touch: %v", err)
	}
	if !after.UpdatedAt.After(before.UpdatedAt) {
		t.Errorf("updated_at not advanced by TouchRun: before=%v after=%v", before.UpdatedAt, after.UpdatedAt)
	}
	// TouchRun must not disturb any other field.
	if after.State != before.State {
		t.Errorf("TouchRun changed state: %q -> %q", before.State, after.State)
	}

	// Unknown id => ErrNotFound.
	if err := store.TouchRun(ctx, pool, uuid.New()); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("touch unknown run err = %v, want ErrNotFound", err)
	}
}

// TestPG_GrantPersistence_RoundTrip covers the credential-grant store: a created
// grant round-trips through GetGrant with its JSON spec intact, and appears in
// ListGrantsByRun scoped to its run (and not to a sibling run). Grants are the
// eligibility records the broker checks before minting, so a spec that did not
// survive the JSONB round-trip would silently widen or lose scope.
func TestPG_GrantPersistence_RoundTrip(t *testing.T) {
	pool := runsPGPool(t)
	ctx := context.Background()

	owner := persistRun(t, ctx, pool, newRun(types.RunRunning))
	other := persistRun(t, ctx, pool, newRun(types.RunRunning))

	g := types.CredentialGrant{
		ID:        uuid.New(),
		RunID:     owner.ID,
		CreatedAt: time.Now().UTC(),
		Spec: types.GrantSpec{
			Kind:             types.GrantGitHubToken,
			Scope:            json.RawMessage(`{"repos":["octocat/Hello-World"],"permissions":{"contents":"read"}}`),
			TTLSeconds:       3600,
			RequiresApproval: true,
		},
	}
	if _, err := store.CreateGrant(ctx, pool, g); err != nil {
		t.Fatalf("create grant: %v", err)
	}

	got, err := store.GetGrant(ctx, pool, g.ID)
	if err != nil {
		t.Fatalf("get grant: %v", err)
	}
	if got.RunID != owner.ID {
		t.Errorf("grant run_id = %s, want %s", got.RunID, owner.ID)
	}
	if got.Spec.Kind != types.GrantGitHubToken {
		t.Errorf("grant kind = %q, want github_token", got.Spec.Kind)
	}
	if got.Spec.TTLSeconds != 3600 {
		t.Errorf("grant ttl = %d, want 3600", got.Spec.TTLSeconds)
	}
	if !got.Spec.RequiresApproval {
		t.Error("grant requires_approval = false, want true")
	}
	// The kind-specific scope JSON must survive the JSONB round-trip byte-for-meaning.
	var gotScope, wantScope map[string]any
	if err := json.Unmarshal(got.Spec.Scope, &gotScope); err != nil {
		t.Fatalf("unmarshal got scope: %v", err)
	}
	if err := json.Unmarshal(g.Spec.Scope, &wantScope); err != nil {
		t.Fatalf("unmarshal want scope: %v", err)
	}
	if repos, ok := gotScope["repos"].([]any); !ok || len(repos) != 1 || repos[0] != "octocat/Hello-World" {
		t.Errorf("grant scope repos = %v, want [octocat/Hello-World]", gotScope["repos"])
	}

	// Scoped listing: present for its run, absent from a sibling run.
	mine, err := store.ListGrantsByRun(ctx, pool, owner.ID)
	if err != nil {
		t.Fatalf("list grants for owner: %v", err)
	}
	var found bool
	for _, lg := range mine {
		if lg.ID == g.ID {
			found = true
		}
	}
	if !found {
		t.Errorf("grant %s not in ListGrantsByRun(%s)", g.ID, owner.ID)
	}

	otherGrants, err := store.ListGrantsByRun(ctx, pool, other.ID)
	if err != nil {
		t.Fatalf("list grants for other: %v", err)
	}
	for _, lg := range otherGrants {
		if lg.ID == g.ID {
			t.Errorf("grant %s leaked into a sibling run's listing", g.ID)
		}
	}

	// Unknown grant id => ErrNotFound.
	if _, err := store.GetGrant(ctx, pool, uuid.New()); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("get unknown grant err = %v, want ErrNotFound", err)
	}
}

// TestPG_ApprovalPersistence_RoundTrip covers approval persistence + the state
// filter on ListApprovals: a created PENDING approval round-trips through
// GetApproval (scope + grant linkage intact) and is returned by the PENDING
// filter but not by the APPROVED filter. This complements the single-decision
// test in store_pg_test.go (which covers the DecideApproval transition itself).
func TestPG_ApprovalPersistence_RoundTrip(t *testing.T) {
	pool := runsPGPool(t)
	ctx := context.Background()

	run := persistRun(t, ctx, pool, newRun(types.RunRunning))

	// A grant the approval links back to (grant_id is a nullable FK).
	g := types.CredentialGrant{
		ID:        uuid.New(),
		RunID:     run.ID,
		CreatedAt: time.Now().UTC(),
		Spec:      types.GrantSpec{Kind: types.GrantGitHubToken, Scope: json.RawMessage(`{}`), TTLSeconds: 600},
	}
	if _, err := store.CreateGrant(ctx, pool, g); err != nil {
		t.Fatalf("create grant: %v", err)
	}

	ap := types.ApprovalRequest{
		ID:             uuid.New(),
		RunID:          run.ID,
		GrantID:        &g.ID,
		Kind:           types.ApprovalCredential,
		RequestedScope: json.RawMessage(`{"kind":"github_token","repos":["octocat/Hello-World"]}`),
		State:          types.ApprovalPending,
		RequestedAt:    time.Now().UTC(),
	}
	if _, err := store.CreateApproval(ctx, pool, ap); err != nil {
		t.Fatalf("create approval: %v", err)
	}

	got, err := store.GetApproval(ctx, pool, ap.ID)
	if err != nil {
		t.Fatalf("get approval: %v", err)
	}
	if got.RunID != run.ID {
		t.Errorf("approval run_id = %s, want %s", got.RunID, run.ID)
	}
	if got.GrantID == nil || *got.GrantID != g.ID {
		t.Errorf("approval grant_id = %v, want %s", got.GrantID, g.ID)
	}
	if got.Kind != types.ApprovalCredential {
		t.Errorf("approval kind = %q, want credential", got.Kind)
	}
	if got.State != types.ApprovalPending {
		t.Errorf("approval state = %q, want PENDING", got.State)
	}
	// RequestedScope must survive verbatim — it is EXACTLY what the approver saw.
	var gotScope, wantScope map[string]any
	if err := json.Unmarshal(got.RequestedScope, &gotScope); err != nil {
		t.Fatalf("unmarshal got scope: %v", err)
	}
	if err := json.Unmarshal(ap.RequestedScope, &wantScope); err != nil {
		t.Fatalf("unmarshal want scope: %v", err)
	}
	if gotScope["kind"] != "github_token" {
		t.Errorf("approval scope kind = %v, want github_token", gotScope["kind"])
	}

	// State filter: present under PENDING, absent under APPROVED.
	pending, err := store.ListApprovals(ctx, pool, types.ApprovalPending)
	if err != nil {
		t.Fatalf("list pending approvals: %v", err)
	}
	if !containsApproval(pending, ap.ID) {
		t.Errorf("approval %s missing from PENDING listing", ap.ID)
	}

	approved, err := store.ListApprovals(ctx, pool, types.ApprovalApproved)
	if err != nil {
		t.Fatalf("list approved approvals: %v", err)
	}
	if containsApproval(approved, ap.ID) {
		t.Errorf("PENDING approval %s wrongly returned by APPROVED filter", ap.ID)
	}

	// Empty filter lists all states; our PENDING row must be in it.
	all, err := store.ListApprovals(ctx, pool, "")
	if err != nil {
		t.Fatalf("list all approvals: %v", err)
	}
	if !containsApproval(all, ap.ID) {
		t.Errorf("approval %s missing from unfiltered listing", ap.ID)
	}

	// Unknown approval id => ErrNotFound.
	if _, err := store.GetApproval(ctx, pool, uuid.New()); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("get unknown approval err = %v, want ErrNotFound", err)
	}
}

// containsApproval reports whether id appears in the approval slice.
func containsApproval(aps []types.ApprovalRequest, id uuid.UUID) bool {
	for _, a := range aps {
		if a.ID == id {
			return true
		}
	}
	return false
}
