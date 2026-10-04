// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"errors"
	"fmt"
	"math"

	"github.com/jackc/pgx/v5"
)

// lockAuditChainSQL serializes appends to the audit_events hash chain. Shared
// verbatim by store.InsertAuditEvent and the broker's insertAuditEventTx.
// Since migration 0056 the trigger takes the same lock, binding writers
// outside this repo too. See db.AuditChainLockKey.
const lockAuditChainSQL = `SELECT pg_advisory_xact_lock($1)`

// AuditChainPageSize is how many rows one page of the verify sweep reads —
// bounds memory to a page, never the table. Large enough that the per-page
// round trip is noise against re-hashing a thousand rows, small enough to
// stay a few hundred kilobytes. A var only so the parity test can shrink it
// and cross page boundaries.
var AuditChainPageSize int32 = 1000

// AuditChainStatus is one verification sweep's verdict (migration 0047). OK is
// the only field an alert should key on; BrokenSeq/Reason name the FIRST row
// that failed, not every row after it, since one edited row makes every later
// link mismatch too and listing them all buries the edit.
type AuditChainStatus struct {
	// OK is true when every chained row re-hashed to its stored row_hash and
	// linked to its predecessor.
	OK bool `json:"ok"`
	// Checked is how many chained rows the sweep walked.
	Checked int64 `json:"checked"`
	// Legacy is how many rows carry NO hash and sit BELOW the first chained
	// row (pre-migration-0047) — expected, not a failure. A hashless row
	// ABOVE that prefix is a break instead (auditChainWalk.step).
	Legacy int64 `json:"legacy"`
	// FirstSeq/HeadSeq bound the chained range (both 0 when Checked is 0).
	FirstSeq int64 `json:"first_seq"`
	HeadSeq  int64 `json:"head_seq"`
	// HeadHash is the row_hash of the newest chained row: the value to compare
	// against what a SIEM recorded off the sink stream.
	HeadHash string `json:"head_hash,omitempty"`
	// AnchorSeq is the last seq of the newest attested retention drop the sweep started from: the first
	// retained row must chain to that drop's recorded tail. 0 when no partition was ever dropped.
	AnchorSeq int64 `json:"anchor_seq,omitempty"`
	// BrokenSeq/Reason are set only when OK is false.
	BrokenSeq int64  `json:"broken_seq,omitempty"`
	Reason    string `json:"reason,omitempty"`
}

// AuditChainVerifier is the OPTIONAL store capability behind
// GET /api/v1/audit/chain/verify: a test fake or non-Postgres store has no
// chain, and the handler answers 501 rather than reporting a chain it never
// wrote as clean.
//
// Operator-invoked, never run at boot (O(whole audit log)). Not a
// substitute for comparing HeadHash against an off-box copy: an actor who
// can rewrite one row can usually rewrite and re-chain the tail, which then
// verifies clean. See docs/OPERATIONS.md.
type AuditChainVerifier interface {
	VerifyAuditChain(ctx context.Context) (AuditChainStatus, error)
}

// Compile-time assertion: PG satisfies AuditChainVerifier.
var _ AuditChainVerifier = PG{}

// auditChainLink is one row as the sweep sees it: what the row CLAIMS
// (prev/row) beside what migration 0047's audit_row_hash recomputes from that
// row's own immutable fields (want).
type auditChainLink struct {
	seq        int64
	prev, row  string
	want       string
	prevIsNull bool
	// unchained is a row with NO row_hash: either a pre-0047 legacy row (only
	// ever BELOW the chain) or a row written while the chain trigger was gone.
	unchained bool
}

// auditChainWalk applies the chain rules to links fed to it oldest-first,
// deliberately separated from the SQL so it is unit-testable with no
// database:
//
//  1. every row must re-hash to its stored row_hash — catches an EDITED row;
//  2. every row's prev_hash must equal the previous row's row_hash, and only
//     the first chained row may have none — catches a DELETED or REORDERED
//     row, which rule 1 alone cannot see. After an attested retention drop
//     the first retained row must instead chain to the drop's recorded tail
//     hash (the anchor): the only removal the chain accepts;
//  3. hashless rows are a prefix: legacy only while no chained row has been
//     seen yet, a break otherwise (and never legacy after a drop, which
//     removed the prefix they could sit in). Without this rule an actor who
//     dropped or disabled migration 0047's trigger (or inserted in replica
//     mode) could append rows the chain neither covers nor reports, while OK
//     stayed true.
//
// Rule 3's blind spot: a seq GAP below the first chained row can still hold a
// hashless forgery indistinguishable from a legacy row; only an off-box copy
// of the log can date it.
//
// step reports false once the chain is broken; every row after an edit
// mismatches by construction, so the caller stops there.
type auditChainWalk struct {
	st   AuditChainStatus
	prev string
	// anchored: the walk starts after an attested drop, and prev began as that drop's tail hash.
	anchored bool
}

func newAuditChainWalk() *auditChainWalk {
	return &auditChainWalk{st: AuditChainStatus{OK: true}}
}

// newAnchoredAuditChainWalk starts the walk after an attested retention drop whose last row hashed to
// tail: the first retained row must carry that hash as its prev_hash.
func newAnchoredAuditChainWalk(tail string) *auditChainWalk {
	// A drop whose tail was hashless (a legacy range) leaves nothing to chain to: the walk starts as
	// the unanchored one does.
	return &auditChainWalk{st: AuditChainStatus{OK: true}, prev: tail, anchored: tail != ""}
}

// removedWithoutDrop is the reason every unattested removal reports, so an operator can search for it.
const removedWithoutDrop = "rows removed without an attested retention drop"

func (w *auditChainWalk) step(l auditChainLink) bool {
	if l.unchained {
		// Rule 3. Before the first chained row this is the legacy prefix; after
		// it, the row was written with the chain trigger off.
		if w.st.Checked == 0 && !w.anchored {
			w.st.Legacy++
			return true
		}
		w.fail(l.seq, "row carries no hash although the chain had already started "+
			"(it was written with the chain trigger dropped, disabled, or bypassed)")
		return false
	}
	if w.st.Checked == 0 {
		w.st.FirstSeq = l.seq
	}
	w.st.Checked++
	w.st.HeadSeq, w.st.HeadHash = l.seq, l.row
	switch {
	case l.row != l.want:
		w.fail(l.seq, "row_hash does not match the row's contents (the row was edited after it was written)")
	case w.prev == "" && !l.prevIsNull:
		w.fail(l.seq, removedWithoutDrop+": the first chained row carries a prev_hash (the row it chained to was deleted)")
	case w.prev != "" && l.prev != w.prev && w.st.Checked == 1 && w.anchored:
		w.fail(l.seq, removedWithoutDrop+": the first retained row does not chain to the newest retention drop's recorded tail")
	case w.prev != "" && l.prev != w.prev:
		w.fail(l.seq, removedWithoutDrop+": prev_hash does not match the preceding row (a row was deleted or reordered)")
	}
	w.prev = l.row
	return w.st.OK
}

func (w *auditChainWalk) fail(seq int64, reason string) {
	w.st.OK, w.st.BrokenSeq, w.st.Reason = false, seq, reason
}

// VerifyAuditChain walks the audit_events hash chain oldest-first,
// re-hashing every row with migration 0047's audit_row_hash — the SAME
// function the insert trigger uses, so there's no second implementation to
// drift.
//
// The sweep is paged as a correctness property, not just a cost one: an
// unbounded `ORDER BY seq` can plan as Seq Scan -> Sort, buffering the whole
// table before returning row one — the tamper-investigation endpoint would
// get slowest over time. Keyset paging (`seq > $1 ORDER BY seq LIMIT n`) is
// an index scan with a bound instead, so memory is one page and an early
// break genuinely stops the work.
//
// Paging does not verify less: since migration 0056 the trigger allocates
// seq while holding the chain lock and releases it at commit, so seq order
// IS commit order, and a row can never appear below a cursor the walk
// already passed. Walk state is carried across pages by one auditChainWalk,
// so a chain longer than a page is one continuous chain.
//
// Everything runs in ONE repeatable-read snapshot, so the chain, the newest
// attested retention drop, the high-water mark and the partition manifest
// are the same instant: an append between two reads cannot read as a
// removed tail. The walk starts from the newest `drop` anchor that dropped
// rows (row_count above 0: an empty partition leaves nothing to chain to; the first
// retained row must chain to its recorded tail) and still inspects every
// retained row. The chain alone cannot see a removed NEWEST tail, so after
// it the newest row must also match audit_partition_meta's high-water mark,
// and every partition the expected manifest names must be present unless an
// anchor accounts for it.
func (s PG) VerifyAuditChain(ctx context.Context) (AuditChainStatus, error) {
	tx, err := s.Pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return AuditChainStatus{}, fmt.Errorf("store: begin audit chain sweep: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // read-only: nothing to undo

	// An audit_events that was never partitioned (a schema from before 0111) has no anchors or
	// high-water mark to check. Once it is partitioned, a missing bookkeeping table is an error, not a
	// reason to skip the checks.
	var partitioned bool
	if err := tx.QueryRow(ctx, `SELECT relkind = 'p' FROM pg_class WHERE oid = 'audit_events'::regclass`).Scan(&partitioned); err != nil {
		return AuditChainStatus{}, fmt.Errorf("store: read audit_events kind: %w", err)
	}
	var anchorTail string
	var anchorSeq int64
	if partitioned {
		if err := tx.QueryRow(ctx, `
			SELECT COALESCE(tail_row_hash, ''), COALESCE(seq_hi, 0)
			  FROM audit_chain_anchors WHERE kind = 'drop' AND row_count > 0 ORDER BY id DESC LIMIT 1`).
			Scan(&anchorTail, &anchorSeq); err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return AuditChainStatus{}, fmt.Errorf("store: read audit chain anchors: %w", err)
		}
	}
	w := newAnchoredAuditChainWalk(anchorTail)
	w.st.AnchorSeq = anchorSeq

	// Every row, hashless ones included, is read in seq order — rule 3 is a
	// statement about where the hashless rows SIT, so the walk must see them
	// in place.
	const q = `
		SELECT seq,
		       row_hash IS NULL,
		       prev_hash IS NULL,
		       COALESCE(prev_hash,''),
		       COALESCE(row_hash,''),
		       COALESCE(audit_row_hash(prev_hash, id, time, run_id, actor_type, actor,
		                               action, target, outcome, source_ip, data), '')
		FROM audit_events
		WHERE seq > $1
		ORDER BY seq
		LIMIT $2`

	// Below every possible seq: the identity starts at 1, but nothing here needs
	// to depend on that.
	after := int64(math.MinInt64)
	lastSeq := int64(0) // the newest row of any kind
	for {
		rows, err := tx.Query(ctx, q, after, AuditChainPageSize)
		if err != nil {
			return AuditChainStatus{}, fmt.Errorf("store: read audit chain: %w", err)
		}
		n, broke := 0, false
		for rows.Next() {
			var l auditChainLink
			if err := rows.Scan(&l.seq, &l.unchained, &l.prevIsNull, &l.prev, &l.row, &l.want); err != nil {
				rows.Close()
				return AuditChainStatus{}, fmt.Errorf("store: scan audit chain row: %w", err)
			}
			n++
			after, lastSeq = l.seq, l.seq
			if !w.step(l) {
				broke = true
				break
			}
		}
		rows.Close()
		// Checked even after an early break: a truncated READ is an error, and
		// answering "tampered" on a mid-sweep disconnect would be a false
		// accusation.
		if err := rows.Err(); err != nil && w.st.OK {
			return AuditChainStatus{}, fmt.Errorf("store: iterate audit chain: %w", err)
		}
		if broke {
			return w.st, nil
		}
		if int32(n) < AuditChainPageSize {
			break
		}
		// Between pages the caller's context is live, so a client going away
		// (or an operator giving up) actually stops the sweep.
		if err := ctx.Err(); err != nil {
			return AuditChainStatus{}, fmt.Errorf("store: audit chain sweep cancelled: %w", err)
		}
	}
	if partitioned {
		if err := verifyAuditPartitionState(ctx, tx, w, lastSeq); err != nil {
			return AuditChainStatus{}, err
		}
	}
	return w.st, nil
}

// verifyAuditPartitionState is what the chain cannot say about itself, checked once the walk is clean.
func verifyAuditPartitionState(ctx context.Context, tx pgx.Tx, w *auditChainWalk, lastSeq int64) error {
	var hwSeq int64
	var hwHash string
	if err := tx.QueryRow(ctx, `SELECT hw_seq, COALESCE(hw_row_hash, '') FROM audit_partition_meta`).
		Scan(&hwSeq, &hwHash); err != nil {
		return fmt.Errorf("store: read audit high-water mark: %w", err)
	}
	if lastSeq != hwSeq || w.st.HeadHash != hwHash {
		w.fail(hwSeq, fmt.Sprintf("the newest retained row (seq %d) is not the newest row ever appended (seq %d): "+
			"the log was truncated at its tail", lastSeq, hwSeq))
		return nil
	}

	// A partition the manifest expects must be present unless a drop anchor names it; a range a split
	// anchor names must be present unless a LATER drop anchor accounts for it.
	var missing string
	if err := tx.QueryRow(ctx, `
		WITH present AS (
		    SELECT c.relname::text AS name
		      FROM pg_inherits i JOIN pg_class c ON c.oid = i.inhrelid
		     WHERE i.inhparent = 'audit_events'::regclass)
		SELECT name FROM (
		    SELECT e->>'name' AS name, e->>'lo' AS ord
		      FROM audit_partition_meta m, jsonb_array_elements(m.manifest) e
		     WHERE NOT EXISTS (SELECT 1 FROM audit_chain_anchors a
		                        WHERE a.kind = 'drop' AND a.partition_name = e->>'name')
		    UNION ALL
		    SELECT a.partition_name, ''
		      FROM audit_chain_anchors a
		     WHERE a.kind = 'split'
		       AND NOT EXISTS (SELECT 1 FROM audit_chain_anchors d
		                        WHERE d.kind = 'drop' AND d.id > a.id AND d.partition_name = a.partition_name)
		) expected
		WHERE name NOT IN (SELECT name FROM present)
		ORDER BY ord, name LIMIT 1`).Scan(&missing); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		return fmt.Errorf("store: read audit partition manifest: %w", err)
	}
	w.fail(0, fmt.Sprintf("partition %s is in the expected manifest but is missing, and no attested retention drop accounts for it", missing))
	return nil
}
