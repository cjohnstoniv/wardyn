// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/cjohnstoniv/wardyn/internal/db"
)

// The refusal reasons of SplitLegacyAudit. Each is a state the split must not run in; nothing was changed.
const (
	SplitNotOwner       = "audit_split_not_migrator"
	SplitNotPartitioned = "audit_split_not_partitioned"
	SplitNoLegacy       = "audit_split_no_legacy_partition"
	SplitEmpty          = "audit_split_legacy_empty"
	SplitChainBroken    = "audit_split_chain_broken"
	SplitNameTaken      = "audit_split_name_taken"
)

// AuditSplitRefused is a legacy split that was not attempted, or was undone, for a named reason.
type AuditSplitRefused struct {
	Reason string
	Detail string
}

func (e *AuditSplitRefused) Error() string {
	return fmt.Sprintf("store: audit legacy split refused: %s: %s", e.Reason, e.Detail)
}

func splitRefused(reason, format string, args ...any) error {
	return &AuditSplitRefused{Reason: reason, Detail: fmt.Sprintf(format, args...)}
}

// AuditSplitRange is one seq-contiguous range the legacy partition was split into.
type AuditSplitRange struct {
	Name  string
	Rows  int64
	SeqLo int64
	SeqHi int64
	// RecordedLo and RecordedHi are the smallest and largest recorded_at the range's rows now carry.
	RecordedLo time.Time
	RecordedHi time.Time
	// Digest is the range's partition digest (audit_partition_digest), equal to the fold over its source rows.
	Digest string
}

// AuditSplitResult is what one split did. Checked and Legacy are the chain verifier's counts, which were
// the same before the split and after it: the split removed no row from the chain.
type AuditSplitResult struct {
	Rows    int64
	Checked int64
	Legacy  int64
	Ranges  []AuditSplitRange
}

// legacyPartition is the name migration 0111 gave the table that held every pre-0.8.6 row.
const legacyPartition = "audit_events_legacy"

// SplitLegacyAudit splits the legacy partition, which holds all of the audit history from before 0.8.6
// under one recorded_at, into seq-contiguous ranges that ordinary retention can drop one at a time. It is
// the one-shot offline tool behind `wardynd -audit-split-legacy`.
//
// It runs on tx, a read-write transaction on the migrator's connection that the caller commits only when
// this returns nil. The caller has already made sure no writer is running; this takes
// db.AuditPartitionLockKey and then db.AuditChainLockKey for the transaction (the order the retention drop
// takes them), so a writer that slipped in is held out, not interleaved.
//
// A range is not a month of the rows' own time. The boundaries are where the running maximum of "time"
// first crosses a month, walking the rows in seq order, and each row's new recorded_at is that running
// maximum (never past the cutover). A spool replay writes an old "time" late, so splitting by "time" would
// scatter a month through the chain and leave an interior link behind when its range was dropped; the
// running maximum cannot go backwards, so every range is a contiguous prefix of chain order, and its
// largest recorded_at is the largest "time" it holds. row_hash does not cover recorded_at, so no hash
// changes.
//
// Every range is copied to a new table, attached, and proved against its source (the same fold
// audit_partition_digest computes, and a row-for-row comparison), the legacy table is dropped, the ranges
// are written to the expected manifest and a 'split' anchor each, and the chain is verified again. The
// verification must see exactly the rows it saw before.
func SplitLegacyAudit(ctx context.Context, tx pgx.Tx) (AuditSplitResult, error) {
	s := &legacySplit{ctx: ctx, tx: tx}
	for _, step := range []func() error{
		s.lock, s.preconditions, s.verifyBefore, s.plan, s.checkPlan, s.catalog, s.build, s.swap, s.prove, s.record, s.verifyAfter,
	} {
		if err := step(); err != nil {
			return s.res, err
		}
	}
	return s.res, nil
}

// splitPlan is one range as planned: what it holds and the recorded_at it will carry.
type splitPlan struct {
	AuditSplitRange
	recLoUS, recHiUS int64
	bound            time.Time // the exclusive upper bound: the largest recorded_at plus a microsecond
	tail             *string   // row_hash of the range's last row; nil for a hashless one
}

// legacySplit is one SplitLegacyAudit run; each step reads what the earlier ones found.
type legacySplit struct {
	ctx context.Context
	tx  pgx.Tx
	res AuditSplitResult

	ns      string // the schema audit_events lives in
	cutover time.Time
	before  AuditChainStatus
	plans   []splitPlan

	// insertCols and selectCols copy a legacy row with its new recorded_at; sameCols is every other column.
	insertCols, selectCols, sameCols string
	readers                          []string
	truncAlways                      bool
}

func (s *legacySplit) q(name string) string { return pgx.Identifier{s.ns, name}.Sanitize() }

// fail wraps err with what the step was doing, leaving a refusal as it is.
func (s *legacySplit) fail(what string, err error) error {
	if err == nil {
		return nil
	}
	var refused *AuditSplitRefused
	if errors.As(err, &refused) {
		return err
	}
	return fmt.Errorf("store: audit legacy split: %s: %w", what, err)
}

func (s *legacySplit) exec(what, sql string, args ...any) error {
	if _, err := s.tx.Exec(s.ctx, sql, args...); err != nil {
		return s.fail(what, err)
	}
	return nil
}

func (s *legacySplit) one(what, sql string, args []any, dest ...any) error {
	if err := s.tx.QueryRow(s.ctx, sql, args...).Scan(dest...); err != nil {
		return s.fail(what, err)
	}
	return nil
}

// column reads a one-column result into a slice.
func (s *legacySplit) column(what, sql string, args ...any) ([]string, error) {
	rows, err := s.tx.Query(s.ctx, sql, args...)
	if err != nil {
		return nil, s.fail(what, err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			return nil, s.fail(what, err)
		}
		out = append(out, v)
	}
	return out, s.fail(what, rows.Err())
}

func (s *legacySplit) lock() error {
	// 30 s is long enough for a lock another statement is about to release and short enough that a
	// reader left on the table is a failure the operator sees, not a hang.
	if err := s.exec("set lock_timeout", `SET LOCAL lock_timeout = '30s'`); err != nil {
		return err
	}
	for _, key := range []int64{db.AuditPartitionLockKey, db.AuditChainLockKey} {
		if err := s.exec("take the audit locks", `SELECT pg_advisory_xact_lock($1)`, key); err != nil {
			return err
		}
	}
	return nil
}

func (s *legacySplit) preconditions() error {
	var kind string
	if err := s.one("read audit_events", `SELECT n.nspname::text, c.relkind::text
		FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace WHERE c.oid = 'audit_events'::regclass`, nil, &s.ns, &kind); err != nil {
		return err
	}
	if kind != "p" {
		return splitRefused(SplitNotPartitioned, "audit_events is not partitioned: run wardynd -migrate-only first")
	}

	var owns, anchors, meta bool
	var role string
	if err := s.one("read the connected role's privileges", `SELECT current_user::text,
			pg_has_role(current_user, (SELECT relowner FROM pg_class WHERE oid = 'audit_events'::regclass), 'USAGE'),
			has_table_privilege(current_user, $1::text::regclass, 'INSERT'),
			has_table_privilege(current_user, $2::text::regclass, 'UPDATE')`,
		[]any{s.q("audit_chain_anchors"), s.q("audit_partition_meta")}, &role, &owns, &anchors, &meta); err != nil {
		return err
	}
	// The app role has no INSERT on the anchors and cannot DETACH or ATTACH a partition: refuse, never
	// fall back to a different connection.
	if !owns || !anchors || !meta {
		return splitRefused(SplitNotOwner, "the connected role %q does not own the audit tables: run this with WARDYN_PG_MIGRATE_DSN set to the migrator role", role)
	}

	var isLegacy bool
	if err := s.one("look for the legacy partition", `SELECT EXISTS (SELECT 1 FROM pg_inherits i JOIN pg_class c ON c.oid = i.inhrelid
			WHERE i.inhparent = 'audit_events'::regclass AND c.oid = to_regclass($1))`, []any{s.q(legacyPartition)}, &isLegacy); err != nil {
		return err
	}
	if !isLegacy {
		return splitRefused(SplitNoLegacy, "%s is not a partition of audit_events: the history was already split, or already dropped", legacyPartition)
	}
	if err := s.one("count the legacy rows", `SELECT count(*) FROM `+s.q(legacyPartition), nil, &s.res.Rows); err != nil {
		return err
	}
	if s.res.Rows == 0 {
		return splitRefused(SplitEmpty, "%s holds no rows: there is nothing to split", legacyPartition)
	}
	return s.one("read the cutover", `SELECT cutover FROM `+s.q("audit_partition_meta"), nil, &s.cutover)
}

// verifyBefore: the chain must verify before. A split must never be what hides a break, nor what a break
// is blamed on.
func (s *legacySplit) verifyBefore() error {
	st, err := verifyAuditChain(s.ctx, s.tx)
	if err != nil {
		return s.fail("verify the chain before the split", err)
	}
	if !st.OK {
		return splitRefused(SplitChainBroken, "the chain does not verify before the split (seq %d: %s)", st.BrokenSeq, st.Reason)
	}
	s.before = st
	return nil
}

// plan works out every row's new recorded_at in one pass (the running maximum of "time" in seq order,
// never past the instant before the cutover, so a row stamped in the future is still placeable) and groups
// the rows into ranges by the UTC month of that value.
func (s *legacySplit) plan() error {
	if err := s.exec("plan the ranges", `CREATE TEMP TABLE audit_split_plan ON COMMIT DROP AS
		SELECT seq, least(max("time") OVER (ORDER BY seq),
		                  (SELECT cutover - interval '1 microsecond' FROM `+s.q("audit_partition_meta")+`)) AS rec
		  FROM `+s.q(legacyPartition)); err != nil {
		return err
	}
	if err := s.exec("index the plan", `ALTER TABLE pg_temp.audit_split_plan ADD PRIMARY KEY (seq)`); err != nil {
		return err
	}
	rows, err := s.tx.Query(s.ctx, `
		SELECT to_char(date_trunc('month', rec AT TIME ZONE 'UTC'), 'YYYYMM'), min(seq), max(seq), count(*), min(rec), max(rec),
		       (extract(epoch FROM min(rec)) * 1000000)::bigint, (extract(epoch FROM max(rec)) * 1000000)::bigint
		  FROM pg_temp.audit_split_plan GROUP BY 1 ORDER BY min(seq)`)
	if err != nil {
		return s.fail("read the ranges", err)
	}
	defer rows.Close()
	for rows.Next() {
		var p splitPlan
		var ym string
		if err := rows.Scan(&ym, &p.SeqLo, &p.SeqHi, &p.Rows, &p.RecordedLo, &p.RecordedHi, &p.recLoUS, &p.recHiUS); err != nil {
			return s.fail("scan a range", err)
		}
		p.Name = legacyPartition + "_" + ym
		p.bound = p.RecordedHi.Add(time.Microsecond)
		s.plans = append(s.plans, p)
	}
	return s.fail("read the ranges", rows.Err())
}

// checkPlan states the properties that make a range removable. Matching digests prove a copy; these prove
// the ranges are disjoint prefixes of chain order that sit wholly below the cutover, and that no name is taken.
func (s *legacySplit) checkPlan() error {
	var total int64
	for i, p := range s.plans {
		total += p.Rows
		if i > 0 && (p.SeqLo <= s.plans[i-1].SeqHi || !p.RecordedLo.After(s.plans[i-1].RecordedHi)) {
			return fmt.Errorf("store: audit legacy split: ranges %s and %s overlap in seq or recorded_at (internal error; nothing was changed)", s.plans[i-1].Name, p.Name)
		}
		if p.bound.After(s.cutover) {
			return fmt.Errorf("store: audit legacy split: range %s reaches past the cutover (internal error; nothing was changed)", p.Name)
		}
	}
	if total != s.res.Rows {
		return fmt.Errorf("store: audit legacy split: the ranges hold %d rows, the legacy partition %d (internal error; nothing was changed)", total, s.res.Rows)
	}
	for _, p := range s.plans {
		var taken bool
		if err := s.one("look for an existing "+p.Name, `SELECT to_regclass($1) IS NOT NULL`, []any{s.q(p.Name)}, &taken); err != nil {
			return err
		}
		if taken {
			return splitRefused(SplitNameTaken, "%s already exists", p.Name)
		}
	}
	return nil
}

// catalog reads what the copies must reproduce. Columns come from the catalog, so a column a later release
// adds is copied too; recorded_at is the one that changes.
func (s *legacySplit) catalog() error {
	cols, err := s.column("read the legacy columns", `SELECT a.attname::text FROM pg_attribute a
		WHERE a.attrelid = $1::text::regclass AND a.attnum > 0 AND NOT a.attisdropped ORDER BY a.attnum`, s.q(legacyPartition))
	if err != nil {
		return err
	}
	var insert, sel, same []string
	for _, c := range cols {
		id := pgx.Identifier{c}.Sanitize()
		insert = append(insert, id)
		if c == "recorded_at" {
			sel = append(sel, "p.rec")
			continue
		}
		sel = append(sel, "l."+id)
		same = append(same, id)
	}
	s.insertCols, s.selectCols, s.sameCols = strings.Join(insert, ", "), strings.Join(sel, ", "), strings.Join(same, ", ")

	if s.readers, err = s.column("read the log's readers", `SELECT DISTINCT ro.rolname::text
		FROM pg_class c CROSS JOIN LATERAL pg_catalog.aclexplode(c.relacl) a JOIN pg_roles ro ON ro.oid = a.grantee
		WHERE c.oid = 'audit_events'::regclass AND a.privilege_type = 'SELECT' AND a.grantee <> c.relowner AND a.grantee <> 0`); err != nil {
		return err
	}
	return s.one("read the truncate guard", `SELECT COALESCE((SELECT tgenabled = 'A' FROM pg_trigger
		WHERE tgrelid = 'audit_events'::regclass AND tgname = 'audit_events_no_truncate'), false)`, nil, &s.truncAlways)
}

// timeLit is a timestamptz literal that round-trips a microsecond exactly.
func timeLit(t time.Time) string { return "'" + t.UTC().Format("2006-01-02 15:04:05.000000") + "+00'" }

// build copies every range into a table of its own, not yet attached to the log.
func (s *legacySplit) build() error {
	for _, p := range s.plans {
		tbl := s.q(p.Name)
		for _, st := range []struct{ what, sql string }{
			{"create " + p.Name, `CREATE TABLE ` + tbl + ` (LIKE ` + s.q("audit_events") + ` INCLUDING DEFAULTS INCLUDING CONSTRAINTS)`},
			// The CHECK lets ATTACH skip its own validation scan.
			{"bound " + p.Name, `ALTER TABLE ` + tbl + ` ADD CONSTRAINT ` + pgx.Identifier{p.Name + "_range"}.Sanitize() +
				` CHECK (recorded_at >= ` + timeLit(p.RecordedLo) + ` AND recorded_at < ` + timeLit(p.bound) + `)`},
			{"copy " + p.Name, `INSERT INTO ` + tbl + ` (` + s.insertCols + `) SELECT ` + s.selectCols + ` FROM ` + s.q(legacyPartition) +
				` l JOIN pg_temp.audit_split_plan p ON p.seq = l.seq WHERE l.seq BETWEEN ` + fmt.Sprint(p.SeqLo) + ` AND ` + fmt.Sprint(p.SeqHi)},
		} {
			if err := s.exec(st.what, st.sql); err != nil {
				return err
			}
		}
	}
	return nil
}

// swap detaches the legacy partition (its bound covers every range's, so it leaves first) and attaches the
// ranges, each armed like any partition the log creates.
func (s *legacySplit) swap() error {
	parent := s.q("audit_events")
	if err := s.exec("detach the legacy partition", `ALTER TABLE `+parent+` DETACH PARTITION `+s.q(legacyPartition)); err != nil {
		return err
	}
	for _, p := range s.plans {
		tbl := s.q(p.Name)
		if err := s.exec("arm the truncate guard on "+p.Name, `CREATE TRIGGER audit_events_no_truncate BEFORE TRUNCATE ON `+tbl+
			` FOR EACH STATEMENT EXECUTE FUNCTION `+s.q("audit_events_append_only")+`()`); err != nil {
			return err
		}
		if s.truncAlways {
			if err := s.exec("carry ENABLE ALWAYS to "+p.Name, `ALTER TABLE `+tbl+` ENABLE ALWAYS TRIGGER audit_events_no_truncate`); err != nil {
				return err
			}
		}
		if err := s.exec("harden "+p.Name, `SELECT `+s.q("audit_harden_relation")+`($1::text::regclass, $2::text[])`, tbl, s.readers); err != nil {
			return err
		}
		// ATTACH clones the parent's row triggers (the chain guard and the append-only guard) onto it.
		if err := s.exec("attach "+p.Name, `ALTER TABLE `+parent+` ATTACH PARTITION `+tbl+
			` FOR VALUES FROM (`+timeLit(p.RecordedLo)+`) TO (`+timeLit(p.bound)+`)`); err != nil {
			return err
		}
	}
	return nil
}

// prove checks every range against its source while the source still exists, then drops the source.
func (s *legacySplit) prove() error {
	for i := range s.plans {
		if err := s.proveRange(&s.plans[i]); err != nil {
			return err
		}
	}
	return s.exec("drop the legacy table", `DROP TABLE `+s.q(legacyPartition))
}

func (s *legacySplit) proveRange(p *splitPlan) error {
	legacy := s.q(legacyPartition)
	var dbDigest string
	if err := s.one("digest "+p.Name, `SELECT `+s.q("audit_partition_digest")+`($1)`, []any{p.Name}, &dbDigest); err != nil {
		return err
	}
	fold := NewPartitionDigest(PartitionManifest{Partition: p.Name, Rows: p.Rows, SeqLo: p.SeqLo, SeqHi: p.SeqHi,
		RecordedLoUS: p.recLoUS, RecordedHiUS: p.recHiUS})
	hashes, err := s.column("read the source rows of "+p.Name, `SELECT COALESCE(row_hash, `+s.q("audit_row_hash")+`(NULL, id, "time", run_id,
			actor_type, actor, action, target, outcome, source_ip, data)) FROM `+legacy+` WHERE seq BETWEEN $1 AND $2 ORDER BY seq`, p.SeqLo, p.SeqHi)
	if err != nil {
		return err
	}
	for _, h := range hashes {
		fold.Add(h)
	}
	if got := fold.Sum(); got != dbDigest {
		return fmt.Errorf("store: audit legacy split: range %s digests to %s, its source rows to %s (nothing was changed)", p.Name, dbDigest, got)
	}
	// The digest folds hashes; this compares every other column, so a copy that changed a payload under an
	// unchanged hash cannot pass.
	var diff int64
	if err := s.one("compare "+p.Name+" with its source", `SELECT count(*) FROM (
			SELECT `+s.sameCols+` FROM `+legacy+` WHERE seq BETWEEN $1 AND $2
			EXCEPT ALL SELECT `+s.sameCols+` FROM `+s.q(p.Name)+`) d`, []any{p.SeqLo, p.SeqHi}, &diff); err != nil {
		return err
	}
	if diff != 0 {
		return fmt.Errorf("store: audit legacy split: %d row(s) of range %s differ from the source (nothing was changed)", diff, p.Name)
	}
	p.Digest = dbDigest
	return s.one("read the tail of "+p.Name, `SELECT row_hash FROM `+legacy+` WHERE seq = $1`, []any{p.SeqHi}, &p.tail)
}

// record writes the ranges to the expected partition manifest and a 'split' anchor each.
func (s *legacySplit) record() error {
	names, los, his := make([]string, len(s.plans)), make([]time.Time, len(s.plans)), make([]time.Time, len(s.plans))
	for i, p := range s.plans {
		names[i], los[i], his[i] = p.Name, p.RecordedLo, p.bound
	}
	if err := s.exec("update the expected partition manifest", `UPDATE `+s.q("audit_partition_meta")+` SET manifest = (
			SELECT COALESCE(jsonb_agg(x.e ORDER BY x.e->>'lo'), '[]'::jsonb) FROM (
				SELECT e FROM jsonb_array_elements(manifest) e
				UNION ALL
				SELECT jsonb_build_object('name', u.n,
				       'lo', to_char(u.lo AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS.US"Z"'),
				       'hi', to_char(u.hi AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS.US"Z"'))
				  FROM unnest($1::text[], $2::timestamptz[], $3::timestamptz[]) AS u(n, lo, hi)) x(e))`,
		names, los, his); err != nil {
		return err
	}
	for _, p := range s.plans {
		if err := s.exec("write the split anchor of "+p.Name, `INSERT INTO `+s.q("audit_chain_anchors")+`
				(kind, partition_name, seq_lo, seq_hi, recorded_lo, recorded_hi, row_count, digest, tail_row_hash, actor)
			VALUES ('split', $1, $2, $3, $4, $5, $6, $7, $8, current_user::text)`,
			p.Name, p.SeqLo, p.SeqHi, p.RecordedLo, p.RecordedHi, p.Rows, p.Digest, p.tail); err != nil {
			return err
		}
		s.res.Ranges = append(s.res.Ranges, p.AuditSplitRange)
	}
	return nil
}

// verifyAfter: the chain must verify over exactly the rows it saw before. Nothing was lost, nothing added.
func (s *legacySplit) verifyAfter() error {
	after, err := verifyAuditChain(s.ctx, s.tx)
	if err != nil {
		return s.fail("verify the chain after the split", err)
	}
	if !after.OK || after.Checked != s.before.Checked || after.Legacy != s.before.Legacy {
		return fmt.Errorf("store: audit legacy split: the chain after the split is %+v, before it %+v (nothing was changed)", after, s.before)
	}
	s.res.Checked, s.res.Legacy = after.Checked, after.Legacy
	return nil
}
