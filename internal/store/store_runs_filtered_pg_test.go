// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// #1197 L1a: ListRunsFiltered/CountHiddenRuns — the landing page's SQL-level
// predicates (status, ended_within, include_killed/killedVisibleFor,
// workspace, q, ordering) and F4's lease-ended end-time correction. Guarded
// by WARDYN_TEST_PG, same as every other store_*_pg_test.go file. Every test
// scopes RunFilter.Owner to its own unique creator string, so it is isolated
// within the shared DB regardless of what else is in agent_runs.
package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// newFilterRun is newRun parametrized on the fields ListRunsFiltered's
// predicates read: CreatedBy (the isolation key every test below uses),
// Title/Task/Repo (q's ILIKE targets) and WorkspacePath (the workspace match's
// fallback half).
func newFilterRun(owner string, state types.RunState) types.AgentRun {
	now := time.Now().UTC()
	id := uuid.New()
	return types.AgentRun{
		ID:               id,
		CreatedAt:        now,
		UpdatedAt:        now,
		CreatedBy:        owner,
		Agent:            "claude-code",
		Repo:             "octocat/Hello-World",
		Task:             "filter test",
		Title:            "filter test run",
		ConfinementClass: types.CC2,
		State:            state,
		SPIFFEID:         "spiffe://wardyn.test/agent-run/" + id.String(),
		RunnerTarget:     "docker",
	}
}

// stampEnded sets state/ended_at/lost_at/lost_reason directly (bypassing the
// state-writer CAS methods, which is fine here: these tests are about the
// READ side, and the row shapes they need — an old KILLED row, a lease-ended
// RUNNING row with both lost_at and ended_at set — are not reachable through
// the writers alone in one step).
func stampEnded(t *testing.T, ctx context.Context, pool *pgxpool.Pool, id uuid.UUID, state types.RunState, endedAt *time.Time, lostAt *time.Time, lostReason types.LostReason) {
	t.Helper()
	if _, err := pool.Exec(ctx,
		`UPDATE agent_runs SET state=$2, ended_at=$3, lost_at=$4, lost_reason=$5 WHERE id=$1`,
		id, string(state), endedAt, lostAt, string(lostReason)); err != nil {
		t.Fatalf("stamp ended shape: %v", err)
	}
}

func timePtr(t time.Time) *time.Time { return &t }

// TestPG_ListRunsFiltered_StatusFilters pins active/ended/failed/killed
// against one owner's four differently-stated rows: each status value
// selects exactly the row(s) its definition names, "active" includes a
// lost-but-not-ended (reboot) row, and no filter at all returns everything.
func TestPG_ListRunsFiltered_StatusFilters(t *testing.T) {
	pool := runsPGPool(t)
	ctx := context.Background()
	pg := store.NewPG(pool)
	owner := "status-filter-" + uuid.NewString()

	running := persistRun(t, ctx, pool, newFilterRun(owner, types.RunRunning))
	rebooted := persistRun(t, ctx, pool, newFilterRun(owner, types.RunRunning))
	stampEnded(t, ctx, pool, rebooted.ID, types.RunRunning, nil, timePtr(time.Now().UTC().Add(-time.Minute)), types.LostReboot)
	completed := persistRun(t, ctx, pool, newFilterRun(owner, types.RunCompleted))
	stampEnded(t, ctx, pool, completed.ID, types.RunCompleted, timePtr(time.Now().UTC()), nil, "")
	failed := persistRun(t, ctx, pool, newFilterRun(owner, types.RunFailed))
	stampEnded(t, ctx, pool, failed.ID, types.RunFailed, timePtr(time.Now().UTC()), nil, "")
	killed := persistRun(t, ctx, pool, newFilterRun(owner, types.RunKilled))
	stampEnded(t, ctx, pool, killed.ID, types.RunKilled, timePtr(time.Now().UTC()), nil, "")

	idsOf := func(runs []types.AgentRun) map[uuid.UUID]bool {
		out := map[uuid.UUID]bool{}
		for _, r := range runs {
			out[r.ID] = true
		}
		return out
	}

	cases := []struct {
		name   string
		status []string
		want   []uuid.UUID
	}{
		{"active_includes_lost_not_ended", []string{"active"}, []uuid.UUID{running.ID, rebooted.ID}},
		{"ended_is_terminal_or_lost_ended", []string{"ended"}, []uuid.UUID{completed.ID, failed.ID, killed.ID}},
		{"failed", []string{"failed"}, []uuid.UUID{failed.ID}},
		{"killed", []string{"killed"}, []uuid.UUID{killed.ID}},
		{"none_is_everything", nil, []uuid.UUID{running.ID, rebooted.ID, completed.ID, failed.ID, killed.ID}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := pg.ListRunsFiltered(ctx, store.RunFilter{Owner: owner, Statuses: c.status, IncludeKilled: true}, store.Page{})
			if err != nil {
				t.Fatalf("ListRunsFiltered: %v", err)
			}
			gotIDs := idsOf(got)
			if len(gotIDs) != len(c.want) {
				t.Fatalf("got %d row(s) %v, want %d", len(gotIDs), gotIDs, len(c.want))
			}
			for _, want := range c.want {
				if !gotIDs[want] {
					t.Errorf("status=%v: missing expected row %s", c.status, want)
				}
			}
		})
	}
}

// TestPG_ListRunsFiltered_EndedWithinNeverWindowsLiveRows is H-2: a live
// (RUNNING, not lease-ended) row is never hidden by ended_within, however
// short, while an ended row outside that same window is.
func TestPG_ListRunsFiltered_EndedWithinNeverWindowsLiveRows(t *testing.T) {
	pool := runsPGPool(t)
	ctx := context.Background()
	pg := store.NewPG(pool)
	owner := "ended-within-" + uuid.NewString()

	live := persistRun(t, ctx, pool, newFilterRun(owner, types.RunRunning))
	old := persistRun(t, ctx, pool, newFilterRun(owner, types.RunCompleted))
	stampEnded(t, ctx, pool, old.ID, types.RunCompleted, timePtr(time.Now().UTC().Add(-30*24*time.Hour)), nil, "")

	got, err := pg.ListRunsFiltered(ctx, store.RunFilter{Owner: owner, EndedWithin: time.Hour}, store.Page{})
	if err != nil {
		t.Fatalf("ListRunsFiltered: %v", err)
	}
	if len(got) != 1 || got[0].ID != live.ID {
		t.Fatalf("ended_within=1h: got %+v, want only the live row %s (the 30-day-old completed row must be hidden, never the live one)", got, live.ID)
	}

	all, err := pg.ListRunsFiltered(ctx, store.RunFilter{Owner: owner, EndedWithin: 0}, store.Page{})
	if err != nil {
		t.Fatalf("ListRunsFiltered: %v", err)
	}
	if len(all) != 2 {
		t.Errorf("EndedWithin=0 (\"all\"): got %d row(s), want 2", len(all))
	}
}

// TestPG_ListRunsFiltered_KilledVisibilityDefault is H-2's killedVisibleFor
// rule: a KILLED row older than 24h is hidden unless IncludeKilled is set,
// and CountHiddenRuns reports it under killedHidden specifically (not
// olderHidden, since it is inside an "all" ended_within window).
func TestPG_ListRunsFiltered_KilledVisibilityDefault(t *testing.T) {
	pool := runsPGPool(t)
	ctx := context.Background()
	pg := store.NewPG(pool)
	owner := "killed-vis-" + uuid.NewString()

	oldKilled := persistRun(t, ctx, pool, newFilterRun(owner, types.RunKilled))
	stampEnded(t, ctx, pool, oldKilled.ID, types.RunKilled, timePtr(time.Now().UTC().Add(-25*time.Hour)), nil, "")
	recentKilled := persistRun(t, ctx, pool, newFilterRun(owner, types.RunKilled))
	stampEnded(t, ctx, pool, recentKilled.ID, types.RunKilled, timePtr(time.Now().UTC().Add(-time.Hour)), nil, "")

	hidden, err := pg.ListRunsFiltered(ctx, store.RunFilter{Owner: owner}, store.Page{})
	if err != nil {
		t.Fatalf("ListRunsFiltered: %v", err)
	}
	if len(hidden) != 1 || hidden[0].ID != recentKilled.ID {
		t.Fatalf("default (IncludeKilled=false): got %+v, want only the recent KILLED row", hidden)
	}

	shown, err := pg.ListRunsFiltered(ctx, store.RunFilter{Owner: owner, IncludeKilled: true}, store.Page{})
	if err != nil {
		t.Fatalf("ListRunsFiltered: %v", err)
	}
	if len(shown) != 2 {
		t.Errorf("IncludeKilled=true: got %d row(s), want 2 (both KILLED rows)", len(shown))
	}

	olderHidden, killedHidden, err := pg.CountHiddenRuns(ctx, store.RunFilter{Owner: owner})
	if err != nil {
		t.Fatalf("CountHiddenRuns: %v", err)
	}
	if olderHidden != 0 || killedHidden != 1 {
		t.Errorf("CountHiddenRuns = (older=%d, killed=%d), want (0, 1)", olderHidden, killedHidden)
	}
}

// TestPG_CountHiddenRuns_OlderVsKilled distinguishes the two hidden counts on
// one owner: a completed row outside a 1h ended_within window counts as
// olderHidden (any state), while a KILLED row INSIDE that same window still
// counts as killedHidden via the 24h default — the two headers' disjoint
// definitions (attention-decision.md's AGED note).
func TestPG_CountHiddenRuns_OlderVsKilled(t *testing.T) {
	pool := runsPGPool(t)
	ctx := context.Background()
	pg := store.NewPG(pool)
	owner := "hidden-counts-" + uuid.NewString()

	outsideWindow := persistRun(t, ctx, pool, newFilterRun(owner, types.RunCompleted))
	stampEnded(t, ctx, pool, outsideWindow.ID, types.RunCompleted, timePtr(time.Now().UTC().Add(-2*time.Hour)), nil, "")
	killedButOld := persistRun(t, ctx, pool, newFilterRun(owner, types.RunKilled))
	stampEnded(t, ctx, pool, killedButOld.ID, types.RunKilled, timePtr(time.Now().UTC().Add(-25*time.Hour)), nil, "")

	olderHidden, killedHidden, err := pg.CountHiddenRuns(ctx, store.RunFilter{Owner: owner, EndedWithin: time.Hour})
	if err != nil {
		t.Fatalf("CountHiddenRuns: %v", err)
	}
	// killedButOld is ALSO outside the 1h window, so it counts under
	// olderHidden (the window predicate applies to every state) and NOT under
	// killedHidden (which is specifically "inside the window, hidden only by
	// the killed rule").
	if olderHidden != 2 {
		t.Errorf("olderHidden = %d, want 2 (both rows are outside the 1h window)", olderHidden)
	}
	if killedHidden != 0 {
		t.Errorf("killedHidden = %d, want 0 (the only KILLED row is already counted under olderHidden)", killedHidden)
	}
}

// TestPG_ListRunsFiltered_Workspace pins the exact-match fallback:
// COALESCE(NULLIF(repo,”), workspace_path).
func TestPG_ListRunsFiltered_Workspace(t *testing.T) {
	pool := runsPGPool(t)
	ctx := context.Background()
	pg := store.NewPG(pool)
	owner := "workspace-" + uuid.NewString()

	withRepo := newFilterRun(owner, types.RunRunning)
	withRepo.Repo = "acme/widgets"
	withRepo = persistRun(t, ctx, pool, withRepo)

	noRepo := newFilterRun(owner, types.RunRunning)
	noRepo.Repo = ""
	noRepo.WorkspacePath = "/home/alice/widgets"
	noRepo = persistRun(t, ctx, pool, noRepo)

	got, err := pg.ListRunsFiltered(ctx, store.RunFilter{Owner: owner, Workspace: "acme/widgets"}, store.Page{})
	if err != nil {
		t.Fatalf("ListRunsFiltered: %v", err)
	}
	if len(got) != 1 || got[0].ID != withRepo.ID {
		t.Fatalf("workspace=acme/widgets: got %+v, want only the repo-labelled row", got)
	}

	got, err = pg.ListRunsFiltered(ctx, store.RunFilter{Owner: owner, Workspace: "/home/alice/widgets"}, store.Page{})
	if err != nil {
		t.Fatalf("ListRunsFiltered: %v", err)
	}
	if len(got) != 1 || got[0].ID != noRepo.ID {
		t.Fatalf("workspace=/home/alice/widgets: got %+v, want only the path-labelled row", got)
	}
}

// TestPG_ListRunsFiltered_QueryEscaping pins that a literal '%'/'_' in ?q=
// matches literally rather than as a LIKE wildcard, and that the match is a
// case-insensitive substring over title/task/repo/created_by.
func TestPG_ListRunsFiltered_QueryEscaping(t *testing.T) {
	pool := runsPGPool(t)
	ctx := context.Background()
	pg := store.NewPG(pool)
	owner := "query-escape-" + uuid.NewString()

	literalPercent := newFilterRun(owner, types.RunRunning)
	literalPercent.Title = "refund 50% flow"
	literalPercent = persistRun(t, ctx, pool, literalPercent)

	unrelated := newFilterRun(owner, types.RunRunning)
	unrelated.Title = "refundA50Xflow" // would match a WRONGLY-unescaped "50%" (% as wildcard) or "_" patterns
	unrelated = persistRun(t, ctx, pool, unrelated)

	got, err := pg.ListRunsFiltered(ctx, store.RunFilter{Owner: owner, Query: "50%"}, store.Page{})
	if err != nil {
		t.Fatalf("ListRunsFiltered: %v", err)
	}
	if len(got) != 1 || got[0].ID != literalPercent.ID {
		t.Fatalf("q=50%%: got %+v, want only the literal-%% row (a bare LIKE wildcard would also match %q)", got, unrelated.Title)
	}

	// Case-insensitive substring, unrelated to escaping.
	got, err = pg.ListRunsFiltered(ctx, store.RunFilter{Owner: owner, Query: "REFUND"}, store.Page{})
	if err != nil {
		t.Fatalf("ListRunsFiltered: %v", err)
	}
	if len(got) != 2 {
		t.Errorf("q=REFUND (case-insensitive): got %d row(s), want 2", len(got))
	}
}

// TestPG_ListRunsFiltered_OrderingAndLeaseEndedEndTime pins the landing-page
// order (live rows first by created_at DESC, then ended rows by end time
// DESC) AND F1197-L1a-F4: a lease-ended row's end time is lost_at, not
// ended_at, even when both columns are set on the row (the verify finding's
// exact repro shape).
func TestPG_ListRunsFiltered_OrderingAndLeaseEndedEndTime(t *testing.T) {
	pool := runsPGPool(t)
	ctx := context.Background()
	pg := store.NewPG(pool)
	owner := "ordering-" + uuid.NewString()

	older := persistRun(t, ctx, pool, newFilterRun(owner, types.RunRunning))
	time.Sleep(10 * time.Millisecond)
	newer := persistRun(t, ctx, pool, newFilterRun(owner, types.RunRunning))

	// A lease-ended run: lost_reason='ended', BOTH lost_at and ended_at set,
	// to DISAGREE with each other — lost_at is recent (the lease end), ended_at
	// is old (a later grace-stop timestamp the F4 fix must NOT read for this
	// row's end time or its ordering).
	leaseEnded := persistRun(t, ctx, pool, newFilterRun(owner, types.RunRunning))
	recentLostAt := time.Now().UTC().Add(-time.Minute)
	oldEndedAt := time.Now().UTC().Add(-30 * 24 * time.Hour)
	stampEnded(t, ctx, pool, leaseEnded.ID, types.RunRunning, &oldEndedAt, &recentLostAt, types.LostEnded)

	got, err := pg.ListRunsFiltered(ctx, store.RunFilter{Owner: owner, IncludeKilled: true}, store.Page{})
	if err != nil {
		t.Fatalf("ListRunsFiltered: %v", err)
	}
	wantOrder := []uuid.UUID{newer.ID, older.ID, leaseEnded.ID}
	if len(got) != len(wantOrder) {
		t.Fatalf("got %d row(s), want %d", len(got), len(wantOrder))
	}
	for i, id := range wantOrder {
		if got[i].ID != id {
			t.Errorf("position %d: got %s, want %s (order = %v)", i, got[i].ID, id, idList(got))
		}
	}

	// F4 in the window predicate too: an ended_within window that EXCLUDES
	// ended_at (30 days ago) but INCLUDES lost_at (1 minute ago) must still show
	// the row — proving the window reads lost_at, not ended_at, for this row.
	windowed, err := pg.ListRunsFiltered(ctx, store.RunFilter{Owner: owner, Statuses: []string{"ended"}, EndedWithin: time.Hour}, store.Page{})
	if err != nil {
		t.Fatalf("ListRunsFiltered: %v", err)
	}
	if len(windowed) != 1 || windowed[0].ID != leaseEnded.ID {
		t.Fatalf("ended_within=1h, status=ended: got %+v, want only the lease-ended row (F4: window must read lost_at)", windowed)
	}
}

func idList(runs []types.AgentRun) []uuid.UUID {
	out := make([]uuid.UUID, len(runs))
	for i, r := range runs {
		out[i] = r.ID
	}
	return out
}
