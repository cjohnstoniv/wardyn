// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Postgres-gated coverage for the two paths the decision-scope work added that
// no other suite reaches: AddWorkspaceEgressDecision's single atomic UPDATE, and
// the agent_runs.workspace_ids round trip. Guarded by WARDYN_TEST_PG.
//
// Both are invisible to `go build` and to `make ci` — a wrong jsonb operator or
// a mis-ordered column list compiles perfectly and fails only against a real
// server, which is exactly why they are here rather than in a fake.
package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// newEgressWorkspace creates a minimal onboarded workspace to decide against.
func newEgressWorkspace(t *testing.T, pg store.PG, ctx context.Context) types.Workspace {
	t.Helper()
	now := time.Now().UTC()
	ws, err := pg.CreateWorkspace(ctx, types.Workspace{
		ID:        uuid.New(),
		Name:      "ws-egress-" + uuid.NewString(),
		Sources:   []types.WorkspaceSource{{Type: types.WorkspaceSourceTypeEphemeral, Target: "/home/agent/work"}},
		Status:    types.WorkspaceScanned,
		CreatedAt: now, UpdatedAt: now,
	})
	if err != nil {
		t.Fatalf("create workspace: %v", err)
	}
	return ws
}

func has(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

// TestPG_AddWorkspaceEgressDecision covers every branch of the one statement an
// `always` decision runs. The cross-list move is the load-bearing one: deny beats
// allow everywhere the proxy evaluates policy, so a host left on BOTH lists makes
// one direction a silent no-op — the UI says approved and the next run still
// blocks.
func TestPG_AddWorkspaceEgressDecision(t *testing.T) {
	pool := runsPGPool(t)
	ctx := context.Background()
	pg := store.NewPG(pool)

	t.Run("approve on a NULL column starts the list", func(t *testing.T) {
		ws := newEgressWorkspace(t, pg, ctx)
		// Both columns are nullable with no DEFAULT, so this is the state every
		// brand-new workspace is in. Without COALESCE the cap guard evaluates
		// NULL < 64 => NULL, zero rows update, and the caller reports "cap
		// reached" on the very first use.
		got, err := pg.AddWorkspaceEgressDecision(ctx, ws.ID, "registry.npmjs.org", true, 64)
		if err != nil {
			t.Fatalf("approve on NULL column: %v", err)
		}
		if !has(got.ApprovedEgress, "registry.npmjs.org") {
			t.Fatalf("approved_egress = %v, want it to contain the host", got.ApprovedEgress)
		}
	})

	t.Run("deny moves the host across lists atomically", func(t *testing.T) {
		ws := newEgressWorkspace(t, pg, ctx)
		if _, err := pg.AddWorkspaceEgressDecision(ctx, ws.ID, "evil.example.com", true, 64); err != nil {
			t.Fatalf("seed approve: %v", err)
		}
		got, err := pg.AddWorkspaceEgressDecision(ctx, ws.ID, "evil.example.com", false, 64)
		if err != nil {
			t.Fatalf("deny: %v", err)
		}
		if !has(got.DeniedEgress, "evil.example.com") {
			t.Fatalf("denied_egress = %v, want the host", got.DeniedEgress)
		}
		if has(got.ApprovedEgress, "evil.example.com") {
			t.Fatal("host stayed on approved_egress after a deny — deny beats allow, so the approve would be a silent no-op")
		}
	})

	t.Run("re-deciding the same host is idempotent, not a duplicate", func(t *testing.T) {
		ws := newEgressWorkspace(t, pg, ctx)
		for i := 0; i < 3; i++ {
			if _, err := pg.AddWorkspaceEgressDecision(ctx, ws.ID, "pypi.org", true, 64); err != nil {
				t.Fatalf("approve #%d: %v", i+1, err)
			}
		}
		got, err := pg.GetWorkspace(ctx, ws.ID)
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		n := 0
		for _, h := range got.ApprovedEgress {
			if h == "pypi.org" {
				n++
			}
		}
		if n != 1 {
			t.Fatalf("host appears %d times after 3 approves, want 1 (dedupe must not grow the array or burn cap)", n)
		}
	})

	t.Run("cap refuses a NEW host but allows an idempotent re-decide", func(t *testing.T) {
		ws := newEgressWorkspace(t, pg, ctx)
		const cap = 3
		for i, h := range []string{"a.example.com", "b.example.com", "c.example.com"} {
			if _, err := pg.AddWorkspaceEgressDecision(ctx, ws.ID, h, true, cap); err != nil {
				t.Fatalf("fill #%d: %v", i+1, err)
			}
		}
		if _, err := pg.AddWorkspaceEgressDecision(ctx, ws.ID, "d.example.com", true, cap); err == nil {
			t.Fatal("a NEW host at cap was accepted, want a cap error")
		}
		// The already-listed host must still succeed: with dedupe the write is a
		// no-op, so reporting "cap reached" for something that changed nothing
		// would be a lie.
		if _, err := pg.AddWorkspaceEgressDecision(ctx, ws.ID, "a.example.com", true, cap); err != nil {
			t.Fatalf("idempotent re-decide at cap: %v, want success", err)
		}
	})

	t.Run("the cap follows the direction being written", func(t *testing.T) {
		ws := newEgressWorkspace(t, pg, ctx)
		const cap = 2
		for _, h := range []string{"x.example.com", "y.example.com"} {
			if _, err := pg.AddWorkspaceEgressDecision(ctx, ws.ID, h, true, cap); err != nil {
				t.Fatalf("fill approved: %v", err)
			}
		}
		// approved_egress is now full, but denied_egress is empty — guarding the
		// wrong column would refuse this.
		if _, err := pg.AddWorkspaceEgressDecision(ctx, ws.ID, "z.example.com", false, cap); err != nil {
			t.Fatalf("deny with a FULL approved list: %v, want success (the cap must guard the target list only)", err)
		}
	})
}

// TestPG_SetWorkspaceDeniedEgress covers the revocation route's store call — the
// only cure for a workspace whose permanent deny broke its own credential
// injection, so it has to actually clear.
func TestPG_SetWorkspaceDeniedEgress(t *testing.T) {
	pool := runsPGPool(t)
	ctx := context.Background()
	pg := store.NewPG(pool)
	ws := newEgressWorkspace(t, pg, ctx)

	if _, err := pg.AddWorkspaceEgressDecision(ctx, ws.ID, "blocked.example.com", false, 64); err != nil {
		t.Fatalf("seed deny: %v", err)
	}
	got, err := pg.SetWorkspaceDeniedEgress(ctx, ws.ID, nil)
	if err != nil {
		t.Fatalf("clear denied egress: %v", err)
	}
	if len(got.DeniedEgress) != 0 {
		t.Fatalf("denied_egress = %v after a full-replace with nil, want empty", got.DeniedEgress)
	}
}

// TestPG_RunWorkspaceIDsRoundTrip pins the agent_runs.workspace_ids column
// through EVERY reader that shares scanRun's positional column list. A missed
// list compiles fine and fails only on the path that uses it — and one of those
// paths (ClaimStaleRunWatchers) only runs on a control-plane restart with a live
// sandbox, which is the least-exercised code path in the product.
func TestPG_RunWorkspaceIDsRoundTrip(t *testing.T) {
	pool := runsPGPool(t)
	ctx := context.Background()
	pg := store.NewPG(pool)

	want := []uuid.UUID{uuid.New(), uuid.New()}
	now := time.Now().UTC()
	created, err := pg.CreateRun(ctx, types.AgentRun{
		ID: uuid.New(), CreatedAt: now, UpdatedAt: now,
		CreatedBy: "op@example.com", Agent: "claude-code", Task: "scope round trip",
		ConfinementClass: types.CC1, State: types.RunPending,
		SPIFFEID:         "spiffe://test/agent-run/" + uuid.NewString(),
		RunnerTarget:     "docker",
		WorkspaceIDs:     want,
	})
	if err != nil {
		t.Fatalf("create run: %v", err)
	}
	if len(created.WorkspaceIDs) != len(want) || created.WorkspaceIDs[0] != want[0] || created.WorkspaceIDs[1] != want[1] {
		t.Fatalf("CreateRun RETURNING workspace_ids = %v, want %v", created.WorkspaceIDs, want)
	}

	got, err := pg.GetRun(ctx, created.ID)
	if err != nil {
		t.Fatalf("get run: %v", err)
	}
	if len(got.WorkspaceIDs) != len(want) {
		t.Fatalf("GetRun workspace_ids = %v, want %v", got.WorkspaceIDs, want)
	}

	// The list readers share the same column list; a missed one surfaces here as
	// a scan error rather than as wrong data.
	if _, err := pg.ListRunsPage(ctx, store.Page{Limit: 50}); err != nil {
		t.Fatalf("ListRunsPage: %v", err)
	}
	if _, err := pg.ListRunsPageByCreator(ctx, "op@example.com", store.Page{Limit: 50}); err != nil {
		t.Fatalf("ListRunsPageByCreator: %v", err)
	}
}

// TestPG_RunWorkspaceIDsNilRoundTrip: a run that references no onboarded
// workspace must store NULL and read back as an empty slice, because that is the
// value rule 5 keys on to refuse an `always` decision. A non-nil empty slice
// would write '{}' instead, which is a different value on the wire.
func TestPG_RunWorkspaceIDsNilRoundTrip(t *testing.T) {
	pool := runsPGPool(t)
	ctx := context.Background()
	pg := store.NewPG(pool)

	now := time.Now().UTC()
	created, err := pg.CreateRun(ctx, types.AgentRun{
		ID: uuid.New(), CreatedAt: now, UpdatedAt: now,
		CreatedBy: "op@example.com", Agent: "claude-code", Task: "no workspace",
		ConfinementClass: types.CC1, State: types.RunPending,
		SPIFFEID:         "spiffe://test/agent-run/" + uuid.NewString(),
		RunnerTarget:     "docker",
	})
	if err != nil {
		t.Fatalf("create run: %v", err)
	}
	if len(created.WorkspaceIDs) != 0 {
		t.Fatalf("workspace_ids = %v for a workspace-free run, want empty", created.WorkspaceIDs)
	}
}

// TestPG_DecideApprovalPersistsScope is the guard for the sweep item that "reads
// as done while doing nothing": DecideApproval's SET clause is a SEPARATE list
// from its RETURNING, so updating only the RETURNING echoes the un-updated row
// back and the scope silently never persists — taking the `always` write-back
// with it, since that is gated on the round-tripped value.
func TestPG_DecideApprovalPersistsScope(t *testing.T) {
	pool := runsPGPool(t)
	ctx := context.Background()
	pg := store.NewPG(pool)

	now := time.Now().UTC()
	run, err := pg.CreateRun(ctx, types.AgentRun{
		ID: uuid.New(), CreatedAt: now, UpdatedAt: now,
		CreatedBy: "op@example.com", Agent: "claude-code", Task: "scope persist",
		ConfinementClass: types.CC1, State: types.RunPending,
		SPIFFEID:         "spiffe://test/agent-run/" + uuid.NewString(),
		RunnerTarget:     "docker",
	})
	if err != nil {
		t.Fatalf("create run: %v", err)
	}
	ap, err := pg.CreateApproval(ctx, types.ApprovalRequest{
		ID: uuid.New(), RunID: run.ID, Kind: types.ApprovalEgressDomain,
		RequestedScope: []byte(`{"host":"registry.npmjs.org"}`),
		State:          types.ApprovalPending, RequestedAt: now,
	})
	if err != nil {
		t.Fatalf("create approval: %v", err)
	}
	// A PENDING row carries no decision, so its scope must be empty — DEFAULT
	// 'run' here would assert a decision nobody made.
	if ap.DecisionScope != "" {
		t.Fatalf("PENDING decision_scope = %q, want empty", ap.DecisionScope)
	}

	until := now.Add(2 * time.Hour)
	decided, err := pg.DecideApproval(ctx, ap.ID, types.ApprovalDecision{
		State: types.ApprovalApproved, DecidedBy: "op@example.com", Reason: "ok",
		Scope: types.ScopeUntil, ExpiresAt: &until,
	})
	if err != nil {
		t.Fatalf("decide: %v", err)
	}
	if decided.DecisionScope != types.ScopeUntil {
		t.Fatalf("returned decision_scope = %q, want %q", decided.DecisionScope, types.ScopeUntil)
	}
	// Re-read from the DB: the RETURNING can echo a value the SET never wrote.
	reread, err := pg.GetApproval(ctx, ap.ID)
	if err != nil {
		t.Fatalf("re-read: %v", err)
	}
	if reread.DecisionScope != types.ScopeUntil {
		t.Fatalf("PERSISTED decision_scope = %q, want %q — the SET clause did not carry the scope", reread.DecisionScope, types.ScopeUntil)
	}
	// Compare at MICROSECOND precision: TIMESTAMPTZ stores microseconds, so a
	// Go time.Time carrying nanoseconds never round-trips Equal. Truncating is
	// the honest assertion — and it is worth pinning, because an `until` deadline
	// silently losing its sub-microsecond tail is fine while a test that demands
	// exact equality is a permanent flake.
	if reread.DecisionExpiresAt == nil ||
		!reread.DecisionExpiresAt.Truncate(time.Microsecond).Equal(until.UTC().Truncate(time.Microsecond)) {
		t.Fatalf("persisted decision_expires_at = %v, want %v", reread.DecisionExpiresAt, until.UTC())
	}
}
