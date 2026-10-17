// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store_test

// Live tests for the partition digest, the partition export read and the anchor-aware verify
// (migration 0119 and store.VerifyAuditChain). Guarded by WARDYN_TEST_PG like every *_pg_test.go here.
// The tamper steps need a superuser to bypass the append-only triggers; without one they skip.

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// partChain is a converted audit log with a known shape: 2 hashless pre-chain rows and 3 chained rows
// written by 0.8.5 (all in audit_events_legacy), then 3 rows appended through the real writer into the
// first live partition. The high-water mark is past the legacy bound, so legacy is closed.
type partChain struct {
	pool     *pgxpool.Pool
	preSeqs  []int64 // every legacy row, in seq order
	tailHash string  // row_hash of the last legacy row
	post     []types.AuditEvent
}

func newPartChain(t *testing.T) *partChain {
	t.Helper()
	pool := databaseBefore(t, "0111_audit_partitioned.sql")
	ctx := context.Background()
	c := &partChain{pool: pool}
	if _, err := pool.Exec(ctx, `ALTER TABLE audit_events DISABLE TRIGGER audit_events_chain`); err != nil {
		t.Fatalf("disable chain trigger: %v", err)
	}
	for i := 0; i < 2; i++ {
		var seq int64
		if err := pool.QueryRow(ctx, `INSERT INTO audit_events (id, actor_type, actor, action, outcome, data)
			VALUES (gen_random_uuid(), 'human', 'old@example.com', 'legacy.hashless', 'success', '{"n": 1}') RETURNING seq`).Scan(&seq); err != nil {
			t.Fatalf("insert hashless row: %v", err)
		}
		c.preSeqs = append(c.preSeqs, seq)
	}
	if _, err := pool.Exec(ctx, `ALTER TABLE audit_events ENABLE TRIGGER audit_events_chain`); err != nil {
		t.Fatalf("enable chain trigger: %v", err)
	}
	for _, action := range []string{"test.pre.1", "test.pre.2", "test.pre.3"} {
		seq, _, row := appendAuditRow(t, pool, action)
		c.preSeqs = append(c.preSeqs, seq)
		c.tailHash = row
	}
	for _, f := range []string{"0111_audit_partitioned.sql", "0112_audit_chain_partitioned.sql", "0119_audit_partition_digest.sql"} {
		execMigrationFile(t, pool, f)
	}
	for i, action := range []string{"test.post.1", "test.post.2", "test.post.3"} {
		ev := types.AuditEvent{ID: uuid.New(), Time: time.Now().UTC().Add(-time.Duration(i) * time.Hour), ActorType: types.ActorSystem,
			Actor: "part-probe", Action: action, Outcome: "success"}
		if err := store.InsertAuditEvent(ctx, pool, &ev); err != nil {
			t.Fatalf("InsertAuditEvent %s: %v", action, err)
		}
		c.post = append(c.post, ev)
	}
	if st := sweep(t, pool); !st.OK || st.Checked != 6 || st.Legacy != 2 {
		t.Fatalf("fixture: the chain does not verify as built: %+v", st)
	}
	return c
}

func (c *partChain) exec(t *testing.T, sql string, args ...any) {
	t.Helper()
	if _, err := c.pool.Exec(context.Background(), sql, args...); err != nil {
		t.Fatalf("%s: %v", strings.Join(strings.Fields(sql), " "), err)
	}
}

func scalar[T any](t *testing.T, pool *pgxpool.Pool, sql string, args ...any) T {
	t.Helper()
	var v T
	if err := pool.QueryRow(context.Background(), sql, args...).Scan(&v); err != nil {
		t.Fatalf("%s: %v", strings.Join(strings.Fields(sql), " "), err)
	}
	return v
}

// dropLegacy detaches and drops audit_events_legacy, as the retention drop will.
func (c *partChain) dropLegacy(t *testing.T) {
	t.Helper()
	c.exec(t, `ALTER TABLE audit_events DETACH PARTITION audit_events_legacy`)
	c.exec(t, `DROP TABLE audit_events_legacy`)
}

// anchorLegacy records an attested drop of the legacy partition.
func (c *partChain) anchorLegacy(t *testing.T, tail string) {
	t.Helper()
	c.exec(t, `INSERT INTO audit_chain_anchors (kind, partition_name, seq_lo, seq_hi, row_count, tail_row_hash, actor)
		VALUES ('drop', 'audit_events_legacy', $1, $2, $3, $4, 'test')`,
		c.preSeqs[0], c.preSeqs[len(c.preSeqs)-1], len(c.preSeqs), tail)
}

// exportOf reads a partition through the store and returns the manifest, the fold the export computes and
// the rows' fold hashes.
func exportOf(t *testing.T, pool *pgxpool.Pool, name string) (store.PartitionManifest, string, []store.AuditPartitionRow) {
	t.Helper()
	var m store.PartitionManifest
	var d *store.PartitionDigest
	var rows []store.AuditPartitionRow
	err := store.NewPG(pool).ExportAuditPartition(context.Background(), name,
		func(h store.PartitionManifest) error { m, d = h, store.NewPartitionDigest(h); return nil },
		func(r store.AuditPartitionRow) error { d.Add(r.FoldHash); rows = append(rows, r); return nil })
	if err != nil {
		t.Fatalf("ExportAuditPartition(%s): %v", name, err)
	}
	return m, d.Sum(), rows
}

// TestPG_PartitionExportDigestEqualsTheDatabaseDigest: the fold the export computes while streaming is
// the one audit_partition_digest computes in the database, over a partition with hashless and chained
// rows, and over an empty one (d_0).
func TestPG_PartitionExportDigestEqualsTheDatabaseDigest(t *testing.T) {
	c := newPartChain(t)
	m, got, rows := exportOf(t, c.pool, "audit_events_legacy")
	if want := scalar[string](t, c.pool, `SELECT audit_partition_digest('audit_events_legacy')`); got != want {
		t.Errorf("export fold = %s, audit_partition_digest = %s", got, want)
	}
	if int(m.Rows) != len(c.preSeqs) || len(rows) != len(c.preSeqs) || m.SeqLo != c.preSeqs[0] || m.SeqHi != c.preSeqs[len(c.preSeqs)-1] {
		t.Errorf("manifest %+v over %d rows, want %d rows from seq %d to %d", m, len(rows), len(c.preSeqs), c.preSeqs[0], c.preSeqs[len(c.preSeqs)-1])
	}
	var hashless int
	for i, r := range rows {
		if r.Seq != c.preSeqs[i] {
			t.Errorf("row %d is seq %d, want %d: the export is not in seq order", i, r.Seq, c.preSeqs[i])
		}
		if r.RowHash == nil {
			hashless++
			if r.FoldHash == "" || r.PrevHash != nil {
				t.Errorf("hashless row seq %d: fold hash %q prev %v, want a computed fold hash and no prev", r.Seq, r.FoldHash, r.PrevHash)
			}
		} else if r.FoldHash != *r.RowHash {
			t.Errorf("chained row seq %d folds %q, want its row_hash %q", r.Seq, r.FoldHash, *r.RowHash)
		}
		// data is the jsonb's own text, the bytes audit_row_hash embeds (a space after the colon), not the
		// caller's; SQL NULL stays nil.
		if chained := r.RowHash != nil; chained != (r.Data == nil) || (r.Data != nil && *r.Data != `{"n": 1}`) {
			t.Errorf("row seq %d data = %v, want {\"n\": 1} for a hashless row and none for a chained one", r.Seq, r.Data)
		}
	}
	if hashless != 2 {
		t.Errorf("%d hashless rows exported, want 2", hashless)
	}

	// The empty partition: close every month, then the newest one is empty and closed.
	c.exec(t, `UPDATE audit_partition_meta SET hw_recorded_at = '2200-01-01'`)
	empty := scalar[string](t, c.pool, `SELECT m->>'name' FROM audit_partition_meta, jsonb_array_elements(manifest) m ORDER BY m->>'hi' DESC LIMIT 1`)
	em, edigest, erows := exportOf(t, c.pool, empty)
	if em.Rows != 0 || len(erows) != 0 {
		t.Fatalf("%s: manifest %+v, %d rows; want an empty partition", empty, em, len(erows))
	}
	if want := scalar[string](t, c.pool, `SELECT audit_partition_digest($1)`, empty); edigest != want {
		t.Errorf("empty partition: export fold = %s, audit_partition_digest = %s", edigest, want)
	}
	if edigest != store.NewPartitionDigest(store.PartitionManifest{Partition: empty}).Sum() {
		t.Errorf("an empty partition's digest is not d_0")
	}
}

// TestPG_PartitionExportRefusesBeforeReadingAnything: an unknown name and an open partition are refused
// before either callback runs.
func TestPG_PartitionExportRefusesBeforeReadingAnything(t *testing.T) {
	c := newPartChain(t)
	live := scalar[string](t, c.pool, `SELECT m->>'name' FROM audit_partition_meta, jsonb_array_elements(manifest) m WHERE m->>'name' <> 'audit_events_legacy' ORDER BY m->>'lo' LIMIT 1`)
	for name, tc := range map[string]struct {
		partition string
		want      error
	}{
		"open":            {live, store.ErrAuditPartitionOpen},
		"unknown":         {"audit_events_p199001", store.ErrNotFound},
		"not a partition": {"agent_runs", store.ErrNotFound},
		"empty":           {"", store.ErrNotFound},
		"sql":             {"audit_events_legacy\"; DROP TABLE agent_runs; --", store.ErrNotFound},
	} {
		t.Run(name, func(t *testing.T) {
			called := false
			err := store.NewPG(c.pool).ExportAuditPartition(context.Background(), tc.partition,
				func(store.PartitionManifest) error { called = true; return nil },
				func(store.AuditPartitionRow) error { called = true; return nil })
			if !errors.Is(err, tc.want) || called {
				t.Errorf("ExportAuditPartition(%q) = %v (callback ran: %v), want %v and no callback", tc.partition, err, called, tc.want)
			}
		})
	}
}

// TestPG_VerifyAuditChain_StartsFromTheDropAnchor: dropping the oldest partition with an attested anchor
// verifies from the anchor, over every retained row; the same drop with no anchor, or an anchor for a
// different tail, does not.
func TestPG_VerifyAuditChain_StartsFromTheDropAnchor(t *testing.T) {
	t.Run("anchored drop verifies", func(t *testing.T) {
		c := newPartChain(t)
		c.anchorLegacy(t, c.tailHash)
		c.dropLegacy(t)
		st := sweep(t, c.pool)
		if !st.OK || st.Checked != 3 || st.AnchorSeq != c.preSeqs[len(c.preSeqs)-1] || st.Legacy != 0 {
			t.Fatalf("verify after an attested drop: %+v, want OK, the 3 retained rows, anchored at seq %d", st, c.preSeqs[len(c.preSeqs)-1])
		}
	})
	t.Run("the same drop without an anchor fails", func(t *testing.T) {
		c := newPartChain(t)
		c.dropLegacy(t)
		st := sweep(t, c.pool)
		if st.OK || !strings.Contains(st.Reason, "rows removed without an attested retention drop") {
			t.Fatalf("verify after an unattested drop: %+v, want a break naming the missing drop", st)
		}
	})
	t.Run("an anchor for another tail fails", func(t *testing.T) {
		c := newPartChain(t)
		c.anchorLegacy(t, strings.Repeat("0", 64))
		c.dropLegacy(t)
		if st := sweep(t, c.pool); st.OK || st.BrokenSeq != c.seqOf(t, c.post[0].ID) {
			t.Fatalf("verify against a wrong anchor tail: %+v, want a break at the first retained row", st)
		}
	})
	t.Run("an anchor with the rows still present fails", func(t *testing.T) {
		c := newPartChain(t)
		c.anchorLegacy(t, c.tailHash) // the partition was NOT dropped: the retained rows start before the anchor
		if st := sweep(t, c.pool); st.OK {
			t.Fatalf("verify with an anchor whose rows were never dropped: %+v, want a break", st)
		}
	})
	t.Run("every retained row is inspected, from the anchor", func(t *testing.T) {
		c := newPartChain(t)
		requireTriggerBypass(t, c.pool)
		c.anchorLegacy(t, c.tailHash)
		c.dropLegacy(t)
		victim := c.post[2]
		triggersOff(t, c.pool, func(tx pgx.Tx) error {
			_, err := tx.Exec(context.Background(), `UPDATE audit_events SET data = '{"edited": true}' WHERE id = $1`, victim.ID)
			return err
		})
		want := scalar[int64](t, c.pool, `SELECT seq FROM audit_events WHERE id = $1`, victim.ID)
		if st := sweep(t, c.pool); st.OK || st.BrokenSeq != want {
			t.Fatalf("verify after editing the newest row: %+v, want a break at seq %d", st, want)
		}
	})
	t.Run("pages across the anchor", func(t *testing.T) {
		c := newPartChain(t)
		c.anchorLegacy(t, c.tailHash)
		c.dropLegacy(t)
		defer func(old int32) { store.AuditChainPageSize = old }(store.AuditChainPageSize)
		store.AuditChainPageSize = 2
		if st := sweep(t, c.pool); !st.OK || st.Checked != 3 {
			t.Fatalf("paged verify from an anchor: %+v, want OK over 3 rows", st)
		}
	})
}

// seqOf is the seq of an event the store returned: InsertAuditEvent fills the hashes, not the seq.
func (c *partChain) seqOf(t *testing.T, id uuid.UUID) int64 {
	t.Helper()
	return scalar[int64](t, c.pool, `SELECT seq FROM audit_events WHERE id = $1`, id)
}

// TestPG_VerifyAuditChain_DetectsARemovedTail: the chain alone cannot see the newest rows removed (what is
// left is a valid, shorter chain); the high-water mark in audit_partition_meta can.
func TestPG_VerifyAuditChain_DetectsARemovedTail(t *testing.T) {
	for name, where := range map[string]string{
		"the newest row":             `id = (SELECT id FROM audit_events ORDER BY seq DESC LIMIT 1)`,
		"every row of the live part": `tableoid <> 'audit_events_legacy'::regclass`,
	} {
		t.Run(name, func(t *testing.T) {
			c := newPartChain(t)
			requireTriggerBypass(t, c.pool)
			triggersOff(t, c.pool, func(tx pgx.Tx) error {
				_, err := tx.Exec(context.Background(), `DELETE FROM audit_events WHERE `+where)
				return err
			})
			st := sweep(t, c.pool)
			if st.OK || !strings.Contains(st.Reason, "truncated at its tail") {
				t.Fatalf("verify after removing %s: %+v, want a truncated-tail break", name, st)
			}
		})
	}
}

// A partitioned audit log whose bookkeeping tables are gone is not a log with nothing to check: verify
// fails (an error, the 5xx "the sweep could not run"), never a clean verdict that skipped the anchors.
func TestPG_VerifyAuditChain_MissingBookkeepingIsAnErrorNotASkip(t *testing.T) {
	for _, table := range []string{"audit_chain_anchors", "audit_partition_meta"} {
		t.Run(table, func(t *testing.T) {
			c := newPartChain(t)
			c.exec(t, `DROP TABLE `+table+` CASCADE`)
			if st, err := store.NewPG(c.pool).VerifyAuditChain(context.Background()); err == nil {
				t.Fatalf("verify with %s dropped: %+v, want an error", table, st)
			}
		})
	}
}

// TestPG_VerifyAuditChain_DetectsAMissingPartition: every partition in the expected manifest must be
// present unless an anchor accounts for it; a split anchor's range must be present or later dropped.
func TestPG_VerifyAuditChain_DetectsAMissingPartition(t *testing.T) {
	newest := func(c *partChain) string {
		return scalar[string](t, c.pool, `SELECT m->>'name' FROM audit_partition_meta, jsonb_array_elements(manifest) m ORDER BY m->>'hi' DESC LIMIT 1`)
	}
	dropPart := func(c *partChain, name string) {
		c.exec(t, `ALTER TABLE audit_events DETACH PARTITION `+name)
		c.exec(t, `DROP TABLE `+name)
	}
	t.Run("a missing expected partition fails", func(t *testing.T) {
		c := newPartChain(t)
		name := newest(c)
		dropPart(c, name)
		st := sweep(t, c.pool)
		if st.OK || !strings.Contains(st.Reason, name) || !strings.Contains(st.Reason, "missing") {
			t.Fatalf("verify with %s missing: %+v, want a break naming it", name, st)
		}
	})
	t.Run("a drop anchor accounts for it", func(t *testing.T) {
		c := newPartChain(t)
		name := newest(c)
		dropPart(c, name)
		c.exec(t, `INSERT INTO audit_chain_anchors (kind, partition_name, actor) VALUES ('drop', $1, 'test')`, name)
		// An anchor with no rows (tail NULL) is the newest drop: the walk still starts unanchored.
		if st := sweep(t, c.pool); !st.OK {
			t.Fatalf("verify with the missing partition accounted for: %+v, want OK", st)
		}
	})
	t.Run("a split range that is neither present nor dropped fails", func(t *testing.T) {
		c := newPartChain(t)
		c.exec(t, `INSERT INTO audit_chain_anchors (kind, partition_name, actor) VALUES ('split', 'audit_events_legacy_r1', 'test')`)
		if st := sweep(t, c.pool); st.OK || !strings.Contains(st.Reason, "audit_events_legacy_r1") {
			t.Fatalf("verify with a split range missing: %+v, want a break naming it", st)
		}
		c.exec(t, `INSERT INTO audit_chain_anchors (kind, partition_name, actor) VALUES ('drop', 'audit_events_legacy_r1', 'test')`)
		if st := sweep(t, c.pool); !st.OK {
			t.Fatalf("verify with the split range later dropped: %+v, want OK", st)
		}
	})
}
