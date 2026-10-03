// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store_test

// Live tests for the attested retention drop and the persisted retention policy (migration 0119 and
// store.AuditRetention). Guarded by WARDYN_TEST_PG like every *_pg_test.go here; the aging steps need a
// superuser to bypass the append-only triggers (recorded_at is not hash-covered, so moving a row in time
// leaves the chain intact), and skip without one.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// retChain is a converted chain aged into four partitions, oldest first:
//
//	audit_events_legacy  5 rows (2 hashless, 3 chained), recorded 100+ days ago
//	ar_p_mid             5 rows, 2 of them a RUNNING run's, recorded 80 days ago
//	ar_p_empty           no rows, 60 to 40 days ago
//	the live partition   the newest rows, so the high-water mark is a row that is never dropped
type retChain struct {
	*partChain
	run uuid.UUID
}

const (
	retMid   = "ar_p_mid"
	retEmpty = "ar_p_empty"
)

func newRetChain(t *testing.T) *retChain {
	t.Helper()
	c := &retChain{partChain: newPartChain(t), run: uuid.New()}
	requireTriggerBypass(t, c.pool)
	execMigrationFile(t, c.pool, "0119_audit_retention.sql")
	ctx := context.Background()
	c.exec(t, `INSERT INTO agent_runs (id, created_by, agent, repo, confinement_class, state, spiffe_id, runner_target)
		VALUES ($1, 'ret@example.com', 'claude-code', 'o/r', 'CC1', 'RUNNING', 'spiffe://t/r', 'docker')`, c.run)
	for _, action := range []string{"test.run.1", "test.run.2"} {
		ev := types.AuditEvent{ID: uuid.New(), Time: time.Now().UTC(), RunID: &c.run, ActorType: types.ActorSystem,
			Actor: "ret-probe", Action: action, Outcome: "success"}
		if err := store.InsertAuditEvent(ctx, c.pool, &ev); err != nil {
			t.Fatalf("InsertAuditEvent %s: %v", action, err)
		}
	}
	cutover := scalar[time.Time](t, c.pool, `SELECT cutover FROM audit_partition_meta`)
	lim := cutover.Add(-100 * 24 * time.Hour)
	lit := func(d time.Time) string { return "'" + d.UTC().Format(time.RFC3339Nano) + "'" }
	triggersOff(t, c.pool, func(tx pgx.Tx) error {
		for _, sql := range []string{
			`UPDATE audit_events_legacy SET recorded_at = recorded_at - interval '200 days'`,
			`ALTER TABLE audit_events DETACH PARTITION audit_events_legacy`,
			fmt.Sprintf(`ALTER TABLE audit_events ATTACH PARTITION audit_events_legacy FOR VALUES FROM (MINVALUE) TO (%s)`, lit(lim)),
			`CREATE TABLE ` + retMid + ` (LIKE audit_events INCLUDING DEFAULTS INCLUDING CONSTRAINTS)`,
			`CREATE TRIGGER audit_events_no_truncate BEFORE TRUNCATE ON ` + retMid + ` FOR EACH STATEMENT EXECUTE FUNCTION audit_events_append_only()`,
			fmt.Sprintf(`ALTER TABLE audit_events ATTACH PARTITION %s FOR VALUES FROM (%s) TO (%s)`, retMid, lit(lim), lit(lim.Add(40*24*time.Hour))),
			`CREATE TABLE ` + retEmpty + ` (LIKE audit_events INCLUDING DEFAULTS INCLUDING CONSTRAINTS)`,
			`CREATE TRIGGER audit_events_no_truncate BEFORE TRUNCATE ON ` + retEmpty + ` FOR EACH STATEMENT EXECUTE FUNCTION audit_events_append_only()`,
			fmt.Sprintf(`ALTER TABLE audit_events ATTACH PARTITION %s FOR VALUES FROM (%s) TO (%s)`, retEmpty, lit(lim.Add(40*24*time.Hour)), lit(lim.Add(60*24*time.Hour))),
			// Every row appended since the conversion moves into the mid partition, as seq order demands.
			fmt.Sprintf(`UPDATE audit_events SET recorded_at = %s WHERE seq > (SELECT max(seq) FROM audit_events_legacy)`, lit(lim.Add(20*24*time.Hour))),
		} {
			if _, err := tx.Exec(ctx, sql); err != nil {
				return fmt.Errorf("%s: %w", strings.Join(strings.Fields(sql), " "), err)
			}
		}
		return nil
	})
	for _, action := range []string{"test.tail.1", "test.tail.2"} {
		ev := types.AuditEvent{ID: uuid.New(), Time: time.Now().UTC(), ActorType: types.ActorSystem, Actor: "ret-probe", Action: action, Outcome: "success"}
		if err := store.InsertAuditEvent(ctx, c.pool, &ev); err != nil {
			t.Fatalf("InsertAuditEvent %s: %v", action, err)
		}
	}
	if st := sweep(t, c.pool); !st.OK {
		t.Fatalf("fixture: the aged chain does not verify: %+v", st)
	}
	c.window(t, 30)
	return c
}

// window sets the stored retention window the way an owner could; the policy function is tested separately.
func (c *retChain) window(t *testing.T, days int) {
	t.Helper()
	c.exec(t, `UPDATE audit_partition_meta SET retention_days = $1, pending_days = NULL, pending_effective_at = NULL`, days)
}

func (c *retChain) digest(t *testing.T, name string) string {
	t.Helper()
	return scalar[string](t, c.pool, `SELECT audit_partition_digest($1)`, name)
}

func (c *retChain) drop(name, digest, actor string) (store.AuditRetentionDrop, error) {
	return store.NewPG(c.pool).DropAuditPartition(context.Background(), name, digest, actor)
}

func (c *retChain) hasPartition(t *testing.T, name string) bool {
	t.Helper()
	return scalar[bool](t, c.pool, `SELECT EXISTS (SELECT 1 FROM pg_inherits i JOIN pg_class p ON p.oid = i.inhrelid
		WHERE i.inhparent = 'audit_events'::regclass AND p.relname = $1)`, name)
}

func wantRefused(t *testing.T, err error, reason string) {
	t.Helper()
	var refused *store.AuditRetentionRefused
	if !errors.As(err, &refused) || refused.Reason != reason {
		t.Fatalf("drop error = %v, want a refusal with reason %s", err, reason)
	}
}

// nothingDropped asserts a refused drop changed nothing: every partition is still there, no anchor and no
// drop event was written, and the chain still verifies.
func (c *retChain) nothingDropped(t *testing.T) {
	t.Helper()
	for _, name := range []string{"audit_events_legacy", retMid, retEmpty} {
		if !c.hasPartition(t, name) {
			t.Fatalf("partition %s is gone after a refused drop", name)
		}
	}
	if n := scalar[int](t, c.pool, `SELECT count(*)::int FROM audit_chain_anchors`); n != 0 {
		t.Fatalf("%d anchor(s) after a refused drop", n)
	}
	if n := scalar[int](t, c.pool, `SELECT count(*)::int FROM audit_events WHERE action = 'audit.retention.partition_dropped'`); n != 0 {
		t.Fatalf("%d drop event(s) after a refused drop", n)
	}
	if st := sweep(t, c.pool); !st.OK {
		t.Fatalf("verify after a refused drop: %+v", st)
	}
}

func TestPG_RetentionDrop_Refusals(t *testing.T) {
	c := newRetChain(t)
	good := c.digest(t, "audit_events_legacy")

	t.Run("retention forever refuses everything", func(t *testing.T) {
		c.window(t, 0)
		defer c.window(t, 30)
		_, err := c.drop("audit_events_legacy", good, "ops@example.com")
		wantRefused(t, err, store.RetentionInsideWindow)
		c.nothingDropped(t)
	})
	t.Run("inside the retention window", func(t *testing.T) {
		c.window(t, 120) // the legacy partition ended 100 days ago
		defer c.window(t, 30)
		_, err := c.drop("audit_events_legacy", good, "ops@example.com")
		wantRefused(t, err, store.RetentionInsideWindow)
		c.nothingDropped(t)
	})
	t.Run("not the oldest", func(t *testing.T) {
		_, err := c.drop(retMid, c.digest(t, retMid), "ops@example.com")
		wantRefused(t, err, store.RetentionNotOldest)
		c.nothingDropped(t)
	})
	t.Run("not closed", func(t *testing.T) {
		// Hold the high-water mark inside the legacy partition's range: it can still receive rows.
		c.exec(t, `UPDATE audit_partition_meta SET hw_recorded_at = hw_recorded_at - interval '1000 days'`)
		defer c.exec(t, `UPDATE audit_partition_meta SET hw_recorded_at = hw_recorded_at + interval '1000 days'`)
		_, err := c.drop("audit_events_legacy", good, "ops@example.com")
		wantRefused(t, err, store.RetentionNotClosed)
	})
	t.Run("wrong digest", func(t *testing.T) {
		for _, bad := range []string{strings.Repeat("0", 64), "", good[:63] + "x"} {
			_, err := c.drop("audit_events_legacy", bad, "ops@example.com")
			wantRefused(t, err, store.RetentionDigestMismatch)
		}
		// The digest of another partition is not this one's.
		_, err := c.drop("audit_events_legacy", c.digest(t, retMid), "ops@example.com")
		wantRefused(t, err, store.RetentionDigestMismatch)
		c.nothingDropped(t)
	})
	t.Run("not a partition of the audit log", func(t *testing.T) {
		for _, name := range []string{"nope", "agent_runs", "audit_events", "audit_events_legacy; DROP TABLE agent_runs", `audit_events_legacy"`} {
			if _, err := c.drop(name, good, "ops@example.com"); !errors.Is(err, store.ErrNotFound) {
				t.Errorf("drop(%q) = %v, want ErrNotFound", name, err)
			}
		}
		c.nothingDropped(t)
		_ = scalar[int](t, c.pool, `SELECT count(*)::int FROM agent_runs`)
	})
	t.Run("an anonymous actor", func(t *testing.T) {
		if _, err := c.drop("audit_events_legacy", good, ""); err == nil {
			t.Fatal("a drop with no actor succeeded")
		}
		c.nothingDropped(t)
	})
}

// TestPG_RetentionDrop_AttestedDropsVerifyFromTheirAnchor: the oldest partition goes with the right
// digest, leaving a chained event and an anchor; the next drops (one of them of an empty partition) keep
// verify green and anchored on the newest drop that removed rows.
func TestPG_RetentionDrop_AttestedDropsVerifyFromTheirAnchor(t *testing.T) {
	c := newRetChain(t)

	legacyDigest := c.digest(t, "audit_events_legacy")
	// The same fold the export computes while streaming.
	if _, folded, _ := exportOf(t, c.pool, "audit_events_legacy"); folded != legacyDigest {
		t.Fatalf("export fold %s != database digest %s", folded, legacyDigest)
	}
	d, err := c.drop("audit_events_legacy", strings.ToUpper(legacyDigest), "ops@example.com") // case-insensitive
	if err != nil {
		t.Fatalf("attested drop: %v", err)
	}
	if d.Partition != "audit_events_legacy" || d.Rows != 5 || d.Digest != legacyDigest || d.SeqLo != c.preSeqs[0] || d.SeqHi != c.preSeqs[4] {
		t.Errorf("drop result = %+v", d)
	}
	if c.hasPartition(t, "audit_events_legacy") || scalar[bool](t, c.pool, `SELECT to_regclass('audit_events_legacy') IS NOT NULL`) {
		t.Error("the legacy partition still exists")
	}

	// The anchor.
	var kind, name, tail, actor string
	var rows, ev int64
	if err := c.pool.QueryRow(context.Background(), `SELECT kind, partition_name, row_count, tail_row_hash, actor, event_seq
		FROM audit_chain_anchors`).Scan(&kind, &name, &rows, &tail, &actor, &ev); err != nil {
		t.Fatalf("read the anchor: %v", err)
	}
	if kind != "drop" || name != "audit_events_legacy" || rows != 5 || tail != c.tailHash || actor != "ops@example.com" || ev != d.EventSeq {
		t.Errorf("anchor = %s %s rows=%d tail=%s actor=%s event=%d, want drop audit_events_legacy 5 %s ops@example.com %d",
			kind, name, rows, tail, actor, ev, c.tailHash, d.EventSeq)
	}
	// The chained event, written before the drop and carrying the digest.
	var action, atype, who string
	var data string
	if err := c.pool.QueryRow(context.Background(), `SELECT action, actor_type, actor, data::text FROM audit_events WHERE seq = $1`, ev).
		Scan(&action, &atype, &who, &data); err != nil {
		t.Fatalf("read the drop event: %v", err)
	}
	if action != "audit.retention.partition_dropped" || atype != "human" || who != "ops@example.com" ||
		!strings.Contains(data, legacyDigest) || !strings.Contains(data, `"attested": true`) {
		t.Errorf("drop event = %s %s %s %s", action, atype, who, data)
	}
	st := sweep(t, c.pool)
	if !st.OK || st.AnchorSeq != c.preSeqs[4] || st.Legacy != 0 {
		t.Fatalf("verify after the first drop: %+v, want OK and anchored at seq %d", st, c.preSeqs[4])
	}

	// A live run's rows block the next one; once it ends, it goes too.
	if _, err := c.drop(retMid, c.digest(t, retMid), "ops@example.com"); err == nil {
		t.Fatal("the mid partition holds a RUNNING run's rows and was dropped")
	} else {
		wantRefused(t, err, store.RetentionLiveRun)
	}
	c.exec(t, `UPDATE agent_runs SET state = 'STOPPED' WHERE id = $1`, c.run)
	mid, err := c.drop(retMid, c.digest(t, retMid), "ops@example.com")
	if err != nil {
		t.Fatalf("second drop: %v", err)
	}
	st = sweep(t, c.pool)
	if !st.OK || st.AnchorSeq != mid.SeqHi {
		t.Fatalf("verify after the second drop: %+v, want OK anchored at seq %d", st, mid.SeqHi)
	}

	// An empty partition records row_count 0 and leaves the walk anchored on the drop before it.
	empty, err := c.drop(retEmpty, c.digest(t, retEmpty), "ops@example.com")
	if err != nil {
		t.Fatalf("drop of an empty partition: %v", err)
	}
	if empty.Rows != 0 || scalar[int64](t, c.pool, `SELECT row_count FROM audit_chain_anchors WHERE partition_name = $1`, retEmpty) != 0 {
		t.Errorf("empty drop = %+v, want row_count 0", empty)
	}
	st = sweep(t, c.pool)
	if !st.OK || st.AnchorSeq != mid.SeqHi {
		t.Fatalf("verify after dropping an empty partition: %+v, want OK still anchored at seq %d", st, mid.SeqHi)
	}
	if n := scalar[int](t, c.pool, `SELECT count(*)::int FROM audit_chain_anchors WHERE kind = 'drop'`); n != 3 {
		t.Errorf("%d drop anchor(s), want 3", n)
	}
	// The manifest no longer expects what was dropped.
	if scalar[bool](t, c.pool, `SELECT manifest::text LIKE '%audit_events_legacy%' OR manifest::text LIKE '%`+retMid+`%' FROM audit_partition_meta`) {
		t.Error("the manifest still names a dropped partition")
	}
	// Nothing older than the live partition is left to drop; the live one is open.
	if _, err := c.drop(scalar[string](t, c.pool, `SELECT part_name FROM audit_retention_partitions(NULL, false) LIMIT 1`), good64, "ops@example.com"); err == nil {
		t.Error("the open partition was dropped")
	}
}

const good64 = "0000000000000000000000000000000000000000000000000000000000000000"

// A manual DROP of a partition, with or without the function, fails verify; the function's own drop does not.
func TestPG_RetentionDrop_AManualDropFailsVerify(t *testing.T) {
	c := newRetChain(t)
	c.exec(t, `ALTER TABLE audit_events DETACH PARTITION audit_events_legacy`)
	c.exec(t, `DROP TABLE audit_events_legacy`)
	if st := sweep(t, c.pool); st.OK || !strings.Contains(st.Reason, "rows removed without an attested retention drop") {
		t.Fatalf("verify after a manual drop of the oldest partition: %+v", st)
	}

	c = newRetChain(t)
	c.exec(t, `ALTER TABLE audit_events DETACH PARTITION `+retMid)
	c.exec(t, `DROP TABLE `+retMid)
	if st := sweep(t, c.pool); st.OK {
		t.Fatalf("verify after a manual drop of an interior partition: %+v", st)
	}
}

// GET /audit/retention's per-partition eligibility is the drop function's own answer: the one the status
// calls eligible is dropped, and each the status refuses, the drop refuses with the same reason.
func TestPG_RetentionStatusMatchesTheDropFunction(t *testing.T) {
	c := newRetChain(t)
	pg := store.NewPG(c.pool)
	ctx := context.Background()

	st, err := pg.AuditRetentionStatus(ctx)
	if err != nil {
		t.Fatalf("AuditRetentionStatus: %v", err)
	}
	if st.Policy.Days != 30 || st.Policy.EffectiveDays != 30 || st.Policy.PendingDays != nil {
		t.Errorf("policy = %+v, want 30 effective, nothing pending", st.Policy)
	}
	if st.MonthsAhead < 11 || st.MonthsAhead > 12 {
		t.Errorf("months ahead = %d, want 12 (11 if a month turned over mid-test)", st.MonthsAhead)
	}
	wantRows := map[string]int64{"audit_events_legacy": 5, retMid: 5, retEmpty: 0}
	byName := map[string]store.AuditRetentionPartition{}
	for _, p := range st.Partitions {
		byName[p.Name] = p
	}
	for name, rows := range wantRows {
		if p, ok := byName[name]; !ok || p.Rows != rows || p.State != "closed" {
			t.Errorf("partition %s = %+v, want %d rows, closed", name, p, rows)
		}
	}
	if st.Partitions[0].Name != "audit_events_legacy" || st.Partitions[0].Lo != nil {
		t.Errorf("first partition = %+v, want the legacy one with no lower bound", st.Partitions[0])
	}
	var eligible []string
	open := 0
	for _, p := range st.Partitions {
		if p.Eligible {
			eligible = append(eligible, p.Name)
		}
		if p.State == "open" {
			open++
			if p.Refusal != store.RetentionNotOldest {
				t.Errorf("open partition %s refusal = %q", p.Name, p.Refusal)
			}
		}
	}
	if len(eligible) != 1 || eligible[0] != "audit_events_legacy" {
		t.Fatalf("eligible = %v, want only the oldest, the legacy partition", eligible)
	}
	if open != 1 {
		t.Errorf("%d open partitions, want exactly the current one", open)
	}
	for _, p := range st.Partitions {
		if p.Eligible {
			continue
		}
		_, err := c.drop(p.Name, good64, "ops@example.com")
		wantRefused(t, err, p.Refusal)
	}
	// And the one it calls eligible is the one a correct drop takes.
	if _, err := c.drop("audit_events_legacy", c.digest(t, "audit_events_legacy"), "ops@example.com"); err != nil {
		t.Fatalf("drop of the eligible partition: %v", err)
	}
	st, err = pg.AuditRetentionStatus(ctx)
	if err != nil {
		t.Fatalf("AuditRetentionStatus after the drop: %v", err)
	}
	if st.Partitions[0].Name != retMid || st.Partitions[0].Eligible || st.Partitions[0].Refusal != store.RetentionLiveRun {
		t.Errorf("after the drop the oldest is %+v, want %s refused for its live run", st.Partitions[0], retMid)
	}
}

// With the flag on, the sweeper's drop takes exactly the eligible oldest partition as the system actor.
func TestPG_AutodropDropsOnlyTheEligibleOldest(t *testing.T) {
	c := newRetChain(t)
	pg := store.NewPG(c.pool)
	ctx := context.Background()

	// Inside the window: nothing.
	c.window(t, 120)
	if _, ok, err := pg.AutodropAuditPartition(ctx); err != nil || ok {
		t.Fatalf("autodrop inside the window = ok %v, err %v, want nothing dropped", ok, err)
	}
	c.nothingDropped(t)

	c.window(t, 30)
	d, ok, err := pg.AutodropAuditPartition(ctx)
	if err != nil || !ok {
		t.Fatalf("autodrop = ok %v, err %v, want the legacy partition dropped", ok, err)
	}
	if d.Partition != "audit_events_legacy" || d.Rows != 5 {
		t.Errorf("autodrop dropped %+v, want the legacy partition", d)
	}
	if !c.hasPartition(t, retMid) || !c.hasPartition(t, retEmpty) {
		t.Error("autodrop took more than the oldest partition")
	}
	var atype, who, data string
	if err := c.pool.QueryRow(ctx, `SELECT actor_type, actor, data::text FROM audit_events WHERE seq = $1`, d.EventSeq).Scan(&atype, &who, &data); err != nil {
		t.Fatalf("read the drop event: %v", err)
	}
	if atype != "system" || who != store.SystemAuditActor || !strings.Contains(data, `"attested": false`) {
		t.Errorf("autodrop event = %s %s %s, want the unattested system actor", atype, who, data)
	}
	if st := sweep(t, c.pool); !st.OK || st.AnchorSeq != c.preSeqs[4] {
		t.Fatalf("verify after an autodrop: %+v", st)
	}

	// The next oldest holds a live run's rows: nothing.
	if _, ok, err := pg.AutodropAuditPartition(ctx); err != nil || ok {
		t.Fatalf("autodrop over a live run = ok %v, err %v, want nothing dropped", ok, err)
	}
	if !c.hasPartition(t, retMid) {
		t.Fatal("autodrop dropped a partition holding a live run's rows")
	}
	c.exec(t, `UPDATE agent_runs SET state = 'COMPLETED' WHERE id = $1`, c.run)
	if d, ok, err := pg.AutodropAuditPartition(ctx); err != nil || !ok || d.Partition != retMid {
		t.Fatalf("autodrop after the run ended = %+v ok %v err %v, want %s", d, ok, err, retMid)
	}
	if st := sweep(t, c.pool); !st.OK {
		t.Fatalf("verify after two autodrops: %+v", st)
	}
}

// The policy function: an increase applies at once, a decrease waits 30 days, a repeat never resets the
// date, and none of it depends on the process that asked.
func TestPG_RetentionPolicyCooldown(t *testing.T) {
	c := newRetChain(t)
	c.window(t, 0)
	ctx := context.Background()
	pg := store.NewPG(c.pool)

	// 0 (forever) -> 90 is a decrease: pending, not effective.
	ch, err := pg.SetAuditRetentionPolicy(ctx, 90)
	if err != nil {
		t.Fatalf("set 90: %v", err)
	}
	if ch.Outcome != "pending" || ch.EffectiveDays != 0 || ch.PendingDays == nil || *ch.PendingDays != 90 || ch.PendingEffectiveAt == nil {
		t.Fatalf("0 -> 90 = %+v, want pending, still forever", ch)
	}
	if d := time.Until(*ch.PendingEffectiveAt); d < 29*24*time.Hour+23*time.Hour || d > 30*24*time.Hour {
		t.Errorf("a decrease takes effect in %s, want 30 days", d)
	}
	if n := scalar[int](t, c.pool, `SELECT count(*)::int FROM audit_events WHERE action = 'audit.retention.set'`); n != 1 {
		t.Errorf("%d policy_changed rows after the first change, want 1", n)
	}
	first := *ch.PendingEffectiveAt

	// The same value at every boot for 29 days: still not effective, the date never moves, no more rows.
	for day := 1; day <= 29; day++ {
		c.exec(t, `UPDATE audit_partition_meta SET pending_effective_at = pending_effective_at - interval '1 day'`)
		first = first.Add(-24 * time.Hour)
		ch, err = pg.SetAuditRetentionPolicy(ctx, 90)
		if err != nil {
			t.Fatalf("day %d: %v", day, err)
		}
		if ch.Outcome != "unchanged" || ch.EffectiveDays != 0 || ch.PendingEffectiveAt == nil || !ch.PendingEffectiveAt.Equal(first) {
			t.Fatalf("day %d: %+v, want unchanged, forever, the date unmoved", day, ch)
		}
	}
	if w := scalar[int](t, c.pool, `SELECT audit_retention_window()`); w != 0 {
		t.Errorf("window after 29 days = %d, want forever", w)
	}
	if n := scalar[int](t, c.pool, `SELECT count(*)::int FROM audit_events WHERE action = 'audit.retention.set'`); n != 1 {
		t.Errorf("%d policy_changed rows after 29 repeats, want 1", n)
	}
	// A new store on the same database (a restart) sees the same pending pair.
	st, err := store.NewPG(c.pool).AuditRetentionStatus(ctx)
	if err != nil || st.Policy.PendingDays == nil || *st.Policy.PendingDays != 90 || st.Policy.EffectiveDays != 0 {
		t.Fatalf("status after a restart = %+v, err %v", st.Policy, err)
	}

	// Day 30: the date passes and the window is 90, without anyone asking.
	c.exec(t, `UPDATE audit_partition_meta SET pending_effective_at = clock_timestamp() - interval '1 second'`)
	if w := scalar[int](t, c.pool, `SELECT audit_retention_window()`); w != 90 {
		t.Errorf("window after the date = %d, want 90", w)
	}
	ch, err = pg.SetAuditRetentionPolicy(ctx, 90)
	if err != nil || ch.Outcome != "applied" || ch.EffectiveDays != 90 || ch.PendingDays != nil {
		t.Fatalf("the boot after the date = %+v, err %v, want applied", ch, err)
	}

	// 90 -> 60 is a decrease; 60 -> 120 is an increase and cancels it; 120 -> 0 (forever) applies at once.
	if ch, _ = pg.SetAuditRetentionPolicy(ctx, 60); ch.Outcome != "pending" || ch.EffectiveDays != 90 {
		t.Fatalf("90 -> 60 = %+v, want pending, still 90", ch)
	}
	if ch, _ = pg.SetAuditRetentionPolicy(ctx, 120); ch.Outcome != "applied" || ch.EffectiveDays != 120 || ch.PendingDays != nil {
		t.Fatalf("raising to 120 = %+v, want applied at once, the pending decrease cleared", ch)
	}
	if ch, _ = pg.SetAuditRetentionPolicy(ctx, 0); ch.Outcome != "applied" || ch.EffectiveDays != 0 {
		t.Fatalf("120 -> forever = %+v, want applied at once", ch)
	}
	// Out of range is refused.
	for _, bad := range []int{-1, 36501} {
		if _, err := pg.SetAuditRetentionPolicy(ctx, bad); err == nil {
			t.Errorf("a policy of %d days was accepted", bad)
		}
	}
}

// The SQL spells the live-run states out; they must be the ones types.NonTerminalRunStates names.
func TestRetentionSQLLiveRunStatesAreTheNonTerminalOnes(t *testing.T) {
	b, err := os.ReadFile("../db/migrations/0119_audit_retention.sql")
	if err != nil {
		t.Fatalf("read the migration: %v", err)
	}
	m := regexp.MustCompile(`ar\.state IN \(([^)]*)\)`).FindSubmatch(b)
	if m == nil {
		t.Fatal("0119 has no live-run state list")
	}
	var got []string
	for _, s := range strings.Split(string(m[1]), ",") {
		got = append(got, strings.Trim(strings.TrimSpace(s), "'"))
	}
	var want []string
	for _, s := range types.NonTerminalRunStates {
		want = append(want, string(s))
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("0119 treats %v as live, types.NonTerminalRunStates is %v", got, want)
	}
}
