// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cjohnstoniv/wardyn/internal/notify"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

func insertOutbox(t *testing.T, pool *pgxpool.Pool, approvalID uuid.UUID, tier int, channel string, due time.Time, state, lastErr string, attempt *time.Time, sent *time.Time) {
	t.Helper()
	_, err := pool.Exec(context.Background(), `
		INSERT INTO approval_notifications (id, approval_id, tier, channel, due_at, state, next_attempt_at, last_error, last_attempt_at, sent_at)
		VALUES ($1, $2, $3, $4, $5, $6, $5, $7, $8, $9)`,
		uuid.New(), approvalID, tier, channel, due, state, lastErr, attempt, sent)
	if err != nil {
		t.Fatalf("insert outbox row: %v", err)
	}
}

// TestPG_ApprovalEscalations_MatchesOutboxRows: the tier is the highest one already due and the next
// time is the earliest one still ahead, read for a whole page in one call.
func TestPG_ApprovalEscalations_MatchesOutboxRows(t *testing.T) {
	pool := runsPGPoolIsolated(t)
	ctx := context.Background()
	notify.SetActive(nil, nil) // seed the approvals without letting the store write its own outbox rows
	run := persistRun(t, ctx, pool, newRun(types.RunRunning))
	st := store.NewPG(pool)
	mk := func(scope string) uuid.UUID {
		a, err := st.CreateApproval(ctx, pendingApproval(run.ID, types.ApprovalEgressDomain, scope))
		if err != nil {
			t.Fatalf("CreateApproval: %v", err)
		}
		return a.ID
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	noTierYet, tierOne, lastTier, noRows := mk(`{"host":"a.example"}`), mk(`{"host":"b.example"}`), mk(`{"host":"c.example"}`), mk(`{"host":"d.example"}`)

	next := now.Add(40 * time.Minute)
	insertOutbox(t, pool, noTierYet, 0, "hook-a", now.Add(5*time.Minute), "pending", "", nil, nil)
	insertOutbox(t, pool, tierOne, 0, "hook-a", now.Add(-30*time.Minute), "sent", "", nil, nil)
	insertOutbox(t, pool, tierOne, 1, "hook-a", now.Add(-5*time.Minute), "pending", "", nil, nil)
	insertOutbox(t, pool, tierOne, 1, "hook-b", now.Add(-5*time.Minute), "pending", "", nil, nil)
	insertOutbox(t, pool, tierOne, 2, "hook-a", next, "pending", "", nil, nil)
	insertOutbox(t, pool, tierOne, 3, "hook-a", next.Add(time.Hour), "pending", "", nil, nil)
	insertOutbox(t, pool, lastTier, 0, "hook-a", now.Add(-2*time.Hour), "sent", "", nil, nil)
	insertOutbox(t, pool, lastTier, 1, "hook-a", now.Add(-time.Hour), "dead", "timeout", nil, nil)

	got, err := st.ApprovalEscalations(ctx, []uuid.UUID{noTierYet, tierOne, lastTier, noRows}, now)
	if err != nil {
		t.Fatalf("ApprovalEscalations: %v", err)
	}
	if e := got[noTierYet]; e.Tier != 0 || e.NextAt == nil || !e.NextAt.Equal(now.Add(5*time.Minute)) {
		t.Errorf("no tier yet = %+v, want tier 0 and next at +5m", e)
	}
	if e := got[tierOne]; e.Tier != 1 || e.NextAt == nil || !e.NextAt.Equal(next) {
		t.Errorf("tier 1 in force = %+v, want tier 1 and next at the tier-2 due time (earliest ahead, not tier 3)", e)
	}
	if e := got[lastTier]; e.Tier != 1 || e.NextAt != nil {
		t.Errorf("no further tier = %+v, want tier 1 and no next time", e)
	}
	if _, ok := got[noRows]; ok {
		t.Errorf("an approval with no outbox rows must be absent, got %+v", got[noRows])
	}
	if empty, err := st.ApprovalEscalations(ctx, nil, now); err != nil || len(empty) != 0 {
		t.Errorf("empty ids = %v, %v; want an empty map and no query", empty, err)
	}
}

// TestPG_ApprovalNotifyChannelStats: last success, the newest error class and its time, and the dead
// rows in the last hour, per channel.
func TestPG_ApprovalNotifyChannelStats(t *testing.T) {
	pool := runsPGPoolIsolated(t)
	ctx := context.Background()
	notify.SetActive(nil, nil)
	run := persistRun(t, ctx, pool, newRun(types.RunRunning))
	st := store.NewPG(pool)
	mk := func(scope string) uuid.UUID {
		a, err := st.CreateApproval(ctx, pendingApproval(run.ID, types.ApprovalEgressDomain, scope))
		if err != nil {
			t.Fatalf("CreateApproval: %v", err)
		}
		return a.ID
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	ago := func(d time.Duration) *time.Time { v := now.Add(-d); return &v }
	a1, a2, a3, a4 := mk(`{"host":"1.example"}`), mk(`{"host":"2.example"}`), mk(`{"host":"3.example"}`), mk(`{"host":"4.example"}`)

	// good: one delivered, nothing failed.
	insertOutbox(t, pool, a1, 0, "good", now.Add(-time.Hour), "sent", "", ago(4*time.Minute), ago(4*time.Minute))
	// bad: two dead in the last hour, one dead two hours ago, one retrying with an older class.
	insertOutbox(t, pool, a1, 0, "bad", now.Add(-time.Hour), "dead", "http_status:503", ago(6*time.Minute), nil)
	insertOutbox(t, pool, a2, 0, "bad", now.Add(-time.Hour), "dead", "timeout", ago(20*time.Minute), nil)
	insertOutbox(t, pool, a3, 0, "bad", now.Add(-4*time.Hour), "dead", "dial", ago(2*time.Hour), nil)
	insertOutbox(t, pool, a4, 0, "bad", now.Add(-time.Hour), "pending", "tls_verify", ago(40*time.Minute), nil)

	stats, err := st.ApprovalNotifyChannelStats(ctx, now)
	if err != nil {
		t.Fatalf("ApprovalNotifyChannelStats: %v", err)
	}
	by := map[string]types.ApprovalNotifyChannelStat{}
	for _, s := range stats {
		by[s.Channel] = s
	}
	if g := by["good"]; g.LastSentAt == nil || g.LastError != "" || g.LastErrorAt != nil || g.FailedLastHour != 0 {
		t.Errorf("good = %+v, want a last send and no error", g)
	}
	b := by["bad"]
	if b.LastSentAt != nil {
		t.Errorf("bad delivered nothing, LastSentAt = %v", b.LastSentAt)
	}
	if b.LastError != "http_status:503" || b.LastErrorAt == nil || !b.LastErrorAt.Equal(now.Add(-6*time.Minute)) {
		t.Errorf("bad last error = %q at %v, want http_status:503 at -6m (the newest attempt that left a class)", b.LastError, b.LastErrorAt)
	}
	if b.FailedLastHour != 2 {
		t.Errorf("bad failed in the last hour = %d, want 2 (the dead row from two hours ago and the retrying row do not count)", b.FailedLastHour)
	}
}
