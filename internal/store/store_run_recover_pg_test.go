// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// srExec runs one statement and fails the test when it errors.
func srExec(t *testing.T, pool *pgxpool.Pool, sql string, args ...any) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), sql, args...); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
}

// srRun stores a RUNNING server run whose token was last renewed renewedAgo ago, so its token expired
// renewedAgo - 1 h ago, and whose lineage is the single token jti.
func srRun(t *testing.T, pool *pgxpool.Pool, renewedAgo time.Duration, jti string) types.AgentRun {
	t.Helper()
	run := persistRun(t, context.Background(), pool, newRun(types.RunRunning))
	srExec(t, pool, `UPDATE agent_runs SET token_renewed_at = now() - $2::interval, token_jti = NULLIF($3, ''), token_prev_jti = NULL WHERE id = $1`,
		run.ID, renewedAgo.String(), jti)
	return run
}

func srLineage(t *testing.T, pg store.PG, id uuid.UUID) store.TokenLineage {
	t.Helper()
	l, err := pg.RunRecoveryLineage(context.Background(), id)
	if err != nil {
		t.Fatalf("RunRecoveryLineage: %v", err)
	}
	return l
}

// A lineage cut at a stopped pause is returned as its sentinel, never as "": an empty lineage accepts whoever
// presents a token, so reading the sentinel as empty would let a cut run be recovered.
func TestRecoverLineage_SentinelNotEmpty_PG(t *testing.T) {
	pool := runsPGPool(t)
	pg := store.NewPG(pool)
	run := srRun(t, pool, 70*time.Minute, "jti-1")
	if l := srLineage(t, pg, run.ID); l.JTI != "jti-1" || l.PrevJTI != "" || l.RenewedAt.IsZero() {
		t.Fatalf("lineage = %+v, want jti-1 with no previous jti and a renewal stamp", l)
	}

	const sentinel = store.TokenCutPrefix + "3"
	srExec(t, pool, `UPDATE agent_runs SET token_jti = $2, token_prev_jti = NULL WHERE id = $1`, run.ID, sentinel)
	l := srLineage(t, pg, run.ID)
	if l.JTI != sentinel || !strings.HasPrefix(l.JTI, store.TokenCutPrefix) {
		t.Fatalf("lineage of a cut run = %q, want the sentinel %q verbatim", l.JTI, sentinel)
	}
	if l.PrevJTI != "" {
		t.Fatalf("previous jti of a cut run = %q, want none", l.PrevJTI)
	}

	// A run with no token recorded is the one empty lineage.
	srExec(t, pool, `UPDATE agent_runs SET token_jti = NULL, token_prev_jti = NULL WHERE id = $1`, run.ID)
	if l := srLineage(t, pg, run.ID); l.JTI != "" || l.PrevJTI != "" {
		t.Fatalf("lineage with no token recorded = %+v, want empty", l)
	}
	if _, err := pg.RunRecoveryLineage(context.Background(), uuid.New()); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("RunRecoveryLineage of an unknown run = %v, want ErrNotFound", err)
	}
}

// A new proxy start begins as "does not pause": it clears run_outage_terms.proxy_pauses until its own loaded report
// says otherwise. It touches only that column, and writes no row for a run that has none.
func TestBeginProxyStart_ClearsPauses_PG(t *testing.T) {
	ctx := context.Background()
	pool := runsPGPool(t)
	pg := store.NewPG(pool)
	pauses := func(id uuid.UUID) bool {
		t.Helper()
		terms, err := pg.RunOutageTerms(ctx, id)
		if err != nil {
			t.Fatalf("RunOutageTerms: %v", err)
		}
		return terms.ProxyPauses
	}

	run := persistRun(t, ctx, pool, newRun(types.RunRunning))
	want := types.OutageTerms{AllowanceSec: 7200, Strict: true, EndUnattended: true}
	if _, err := pg.SetRunOutageTerms(ctx, run.ID, want); err != nil {
		t.Fatal(err)
	}
	if err := pg.SetRunProxyPauses(ctx, run.ID, true); err != nil {
		t.Fatal(err)
	}
	if !pauses(run.ID) {
		t.Fatal("SetRunProxyPauses(true) left the flag false")
	}
	if _, err := pg.BeginProxyStart(ctx, run.ID, 1, "J1", store.ProxyStartExpected, false); err != nil {
		t.Fatalf("BeginProxyStart: %v", err)
	}
	got, err := pg.RunOutageTerms(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.ProxyPauses {
		t.Fatal("a new proxy start left proxy_pauses true")
	}
	if got.AllowanceSec != want.AllowanceSec || got.Strict != want.Strict || got.EndUnattended != want.EndUnattended {
		t.Fatalf("terms after the start = %+v, want %+v with the flag cleared", got, want)
	}

	// The same generation begun again (a retry) clears it too.
	if err := pg.SetRunProxyPauses(ctx, run.ID, true); err != nil {
		t.Fatal(err)
	}
	if _, err := pg.BeginProxyStart(ctx, run.ID, 1, "J1b", store.ProxyStartExpected, false); err != nil {
		t.Fatal(err)
	}
	if pauses(run.ID) {
		t.Fatal("a repeated begin of the same generation left proxy_pauses true")
	}

	// A run with no terms row gets none from a start.
	bare := persistRun(t, ctx, pool, newRun(types.RunRunning))
	if _, err := pg.BeginProxyStart(ctx, bare.ID, 1, "J1", store.ProxyStartExpected, false); err != nil {
		t.Fatal(err)
	}
	var rows int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM run_outage_terms WHERE run_id = $1`, bare.ID).Scan(&rows); err != nil || rows != 0 {
		t.Fatalf("terms rows of a run with none after a start = %d, %v; want 0", rows, err)
	}
}

// A recovery stamps only for the run's current token (or a run with none recorded), and the presented token leaves
// the lineage: token_prev_jti is cleared. A previous jti, a cut lineage, a lost run and a run that is not RUNNING
// are each refused with false and no error, and change nothing.
func TestStampRunTokenRecovered_PG(t *testing.T) {
	ctx := context.Background()
	pool := runsPGPool(t)
	pg := store.NewPG(pool)
	const fresh = "jti-fresh"
	for _, c := range []struct {
		name      string
		setup     string // run on the run after it has the lineage jti "jti-cur" and previous "jti-old"
		presented string
		want      bool
	}{
		{"the current token", ``, "jti-cur", true},
		{"no token recorded", `UPDATE agent_runs SET token_jti = NULL, token_prev_jti = NULL WHERE id = $1`, "jti-any", true},
		{"the previous token", ``, "jti-old", false},
		{"another token", ``, "jti-other", false},
		{"a cut lineage, by a real token", `UPDATE agent_runs SET token_jti = 'wardyn-cut:3', token_prev_jti = NULL WHERE id = $1`, "jti-cur", false},
		{"a cut lineage, by the sentinel itself", `UPDATE agent_runs SET token_jti = 'wardyn-cut:3', token_prev_jti = NULL WHERE id = $1`, "wardyn-cut:3", false},
		{"a lost run", `UPDATE agent_runs SET lost_at = now(), lost_reason = 'outage' WHERE id = $1`, "jti-cur", false},
		{"a run that is starting", `UPDATE agent_runs SET state = 'STARTING' WHERE id = $1`, "jti-cur", false},
		{"a killed run", `UPDATE agent_runs SET state = 'KILLED' WHERE id = $1`, "jti-cur", false},
	} {
		t.Run(c.name, func(t *testing.T) {
			run := srRun(t, pool, 70*time.Minute, "jti-cur")
			srExec(t, pool, `UPDATE agent_runs SET token_prev_jti = 'jti-old' WHERE id = $1`, run.ID)
			if c.setup != "" {
				srExec(t, pool, c.setup, run.ID)
			}
			before := srLineage(t, pg, run.ID)
			got, err := pg.StampRunTokenRecovered(ctx, run.ID, fresh, c.presented)
			if err != nil || got != c.want {
				t.Fatalf("StampRunTokenRecovered(%q) = %v, %v; want %v", c.presented, got, err, c.want)
			}
			after := srLineage(t, pg, run.ID)
			if !c.want {
				if after != before {
					t.Fatalf("a refused stamp changed the lineage: %+v -> %+v", before, after)
				}
				return
			}
			if after.JTI != fresh || after.PrevJTI != "" {
				t.Fatalf("lineage after the stamp = %+v, want %q with no previous jti (the presented token is cut)", after, fresh)
			}
			if !after.RenewedAt.After(before.RenewedAt) {
				t.Fatalf("token_renewed_at %v did not move past %v", after.RenewedAt, before.RenewedAt)
			}
			// The presented token left the lineage: it can never be stamped again.
			if again, err := pg.StampRunTokenRecovered(ctx, run.ID, "jti-third", c.presented); err != nil || again {
				t.Fatalf("a second stamp with %q = %v, %v; want false", c.presented, again, err)
			}
		})
	}
}

// srWindow is one outage window relative to the database's now: it started startedAgo ago and ended endedAgo ago,
// or is still open when endedAgo is nil.
type srWindow struct {
	kind       string
	runner     *uuid.UUID
	startedAgo time.Duration
	endedAgo   *time.Duration
}

// RunRecoveryCheck over a few runs: Held (the one hold predicate), Covered (a recorded window covers the token's
// expiry, however long ago it ended), StrictSpent, EndUnattended and Cut. Windows are global, so the database is the
// test's own and each case writes the windows it needs.
func TestRunRecoveryCheck_PG(t *testing.T) {
	ctx := context.Background()
	pool := runsPGPoolIsolated(t)
	pg := store.NewPG(pool)
	other := uuid.New()
	for _, c := range []struct {
		name       string
		renewedAgo time.Duration
		jti        string
		terms      *types.OutageTerms
		pauses     bool
		recovering bool // the store health tracker is recovering (the argument)
		windows    []srWindow
		want       store.RecoveryCheck
	}{
		{name: "held inside an open window", renewedAgo: 70 * time.Minute, pauses: true,
			windows: []srWindow{{kind: store.OutageKindServer, startedAgo: time.Hour}},
			want:    store.RecoveryCheck{Held: true, Covered: true}},
		{name: "a proxy that does not pause is not held", renewedAgo: 70 * time.Minute,
			windows: []srWindow{{kind: store.OutageKindServer, startedAgo: time.Hour}},
			want:    store.RecoveryCheck{Covered: true}},
		{name: "no window and the store not recovering", renewedAgo: 70 * time.Minute, pauses: true,
			want: store.RecoveryCheck{}},
		{name: "no window and the store recovering", renewedAgo: 70 * time.Minute, pauses: true, recovering: true,
			want: store.RecoveryCheck{Held: true}},
		{name: "strict, allowance spent", renewedAgo: 70 * time.Minute, pauses: true, terms: &types.OutageTerms{AllowanceSec: 1800, Strict: true},
			windows: []srWindow{{kind: store.OutageKindServer, startedAgo: time.Hour}},
			want:    store.RecoveryCheck{Covered: true, StrictSpent: true}},
		{name: "strict, inside its allowance", renewedAgo: 70 * time.Minute, pauses: true, terms: &types.OutageTerms{AllowanceSec: 7200, Strict: true},
			windows: []srWindow{{kind: store.OutageKindServer, startedAgo: time.Hour}},
			want:    store.RecoveryCheck{Held: true, Covered: true}},
		{name: "ends an unattended run", renewedAgo: 70 * time.Minute, pauses: true, terms: &types.OutageTerms{AllowanceSec: 3600, EndUnattended: true},
			windows: []srWindow{{kind: store.OutageKindServer, startedAgo: time.Hour}},
			want:    store.RecoveryCheck{Held: true, Covered: true, EndUnattended: true}},
		{name: "a cut lineage is covered but never held", renewedAgo: 70 * time.Minute, jti: store.TokenCutPrefix + "2", pauses: true,
			windows: []srWindow{{kind: store.OutageKindServer, startedAgo: time.Hour}},
			want:    store.RecoveryCheck{Covered: true, Cut: true}},
		{name: "a token expired over 24 hours ago is not held", renewedAgo: 26 * time.Hour, pauses: true,
			windows: []srWindow{{kind: store.OutageKindServer, startedAgo: 30 * time.Hour}},
			want:    store.RecoveryCheck{Covered: true}},
		{name: "a window that ended long ago covers but does not hold", renewedAgo: 3 * time.Hour, pauses: true,
			windows: []srWindow{{kind: store.OutageKindServer, startedAgo: 4 * time.Hour, endedAgo: srDur(100 * time.Minute)}},
			want:    store.RecoveryCheck{Covered: true}},
		{name: "a window that ended inside the grace holds", renewedAgo: 3 * time.Hour, pauses: true,
			windows: []srWindow{{kind: store.OutageKindServer, startedAgo: 4 * time.Hour, endedAgo: srDur(10 * time.Minute)}},
			want:    store.RecoveryCheck{Held: true, Covered: true}},
		{name: "a control-plane window holds", renewedAgo: 70 * time.Minute, pauses: true,
			windows: []srWindow{{kind: store.OutageKindControlPlane, startedAgo: time.Hour, endedAgo: srDur(5 * time.Minute)}},
			want:    store.RecoveryCheck{Held: true, Covered: true}},
		{name: "another runner's link window covers a server run nowhere", renewedAgo: 70 * time.Minute, pauses: true,
			windows: []srWindow{{kind: store.OutageKindRunnerLink, runner: &other, startedAgo: time.Hour}},
			want:    store.RecoveryCheck{}},
	} {
		t.Run(c.name, func(t *testing.T) {
			srExec(t, pool, `DELETE FROM outage_windows`)
			srExec(t, pool, `UPDATE cp_heartbeat SET beat_at = now()`)
			now := outageDBNow(t, pool)
			for _, w := range c.windows {
				var ended *time.Time
				if w.endedAgo != nil {
					e := now.Add(-*w.endedAgo)
					ended = &e
				}
				outageInsert(t, pool, w.kind, w.runner, now.Add(-w.startedAgo), ended)
			}
			run := srRun(t, pool, c.renewedAgo, c.jti)
			if c.terms != nil {
				if _, err := pg.SetRunOutageTerms(ctx, run.ID, *c.terms); err != nil {
					t.Fatal(err)
				}
			}
			if c.pauses {
				if err := pg.SetRunProxyPauses(ctx, run.ID, true); err != nil {
					t.Fatal(err)
				}
			}
			got, err := pg.RunRecoveryCheck(ctx, run.ID, c.recovering)
			if err != nil || got != c.want {
				t.Fatalf("RunRecoveryCheck = %+v, %v; want %+v", got, err, c.want)
			}
		})
	}
	if _, err := pg.RunRecoveryCheck(ctx, uuid.New(), false); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("RunRecoveryCheck of an unknown run = %v, want ErrNotFound", err)
	}
}

func srDur(d time.Duration) *time.Duration { return &d }
