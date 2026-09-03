// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"fmt"
	"math"
)

// lockAuditChainSQL serializes appends to the audit_events hash chain. It is
// shared by store.InsertAuditEvent and (verbatim, since that package cannot
// import this one) the broker's insertAuditEventTx — the two in-tree paths that
// INSERT into audit_events. Since migration 0056 the trigger takes the same
// lock (that is what binds writers outside this repo); these callers keep
// taking it first because advisory locks are re-entrant within a transaction
// and holding it across the whole statement costs nothing. See
// db.AuditChainLockKey.
const lockAuditChainSQL = `SELECT pg_advisory_xact_lock($1)`

// AuditChainPageSize is how many rows one page of the verify sweep reads. It
// bounds the sweep's memory: a page, never the table. Large enough that the
// per-page round trip is noise against re-hashing a thousand rows, small enough
// that the page itself is a few hundred kilobytes. A var only so the parity test
// can shrink it and actually cross page boundaries.
var AuditChainPageSize int32 = 1000

// AuditChainStatus is one verification sweep's verdict (migration 0047).
//
// OK is the only field an alert should key on. Everything else is context for
// the operator reading the result: HeadHash is what to compare against the last
// head an off-box SIEM recorded (the check the chain structurally cannot
// perform on its own — see AuditChainVerifier), and BrokenSeq/Reason name the
// FIRST row that failed rather than every row after it, because one edited row
// makes every later link mismatch too and listing them all buries the edit.
type AuditChainStatus struct {
	// OK is true when every chained row re-hashed to its stored row_hash and
	// linked to its predecessor.
	OK bool `json:"ok"`
	// Checked is how many chained rows the sweep walked.
	Checked int64 `json:"checked"`
	// Legacy is how many rows carry NO hash at all AND sit BELOW the first
	// chained row — the pre-migration-0047 prefix. They are outside the chain
	// by design and are never a failure; a non-zero value on an upgraded
	// deployment is expected. A hashless row ABOVE that prefix is NOT legacy
	// and is not counted here: it is a break (see auditChainWalk.step).
	Legacy int64 `json:"legacy"`
	// FirstSeq/HeadSeq bound the chained range (both 0 when Checked is 0).
	FirstSeq int64 `json:"first_seq"`
	HeadSeq  int64 `json:"head_seq"`
	// HeadHash is the row_hash of the newest chained row: the value to compare
	// against what a SIEM recorded off the sink stream.
	HeadHash string `json:"head_hash,omitempty"`
	// BrokenSeq/Reason are set only when OK is false.
	BrokenSeq int64  `json:"broken_seq,omitempty"`
	Reason    string `json:"reason,omitempty"`
}

// AuditChainVerifier is the OPTIONAL store capability behind
// GET /api/v1/audit/chain/verify. Optional for the same reason Pager is: a test
// fake or a non-Postgres store has no chain, and the handler answers 501 rather
// than reporting that a chain it never wrote verified clean.
//
// The sweep is OPERATOR-INVOKED and never runs at boot. It reads and re-hashes
// every chained row, which is O(whole audit log) — paying that on every wardynd
// start would tax the common case (no tamper) for a result nobody is watching
// at that moment. It is also NOT a substitute for comparing HeadHash against an
// off-box copy: an actor who can rewrite one row can usually rewrite the tail
// and re-chain it, and a re-chained tail verifies clean. See
// docs/OPERATIONS.md.
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

// auditChainWalk applies the two chain rules to links fed to it oldest-first.
// It is the whole verification decision, deliberately separated from the SQL so
// it is unit-testable with no database:
//
//  1. every row must re-hash to its stored row_hash — catches an EDITED row;
//  2. every row's prev_hash must equal the previous row's row_hash, and only
//     the very first chained row may have none — catches a DELETED or REORDERED
//     row, which rule 1 alone cannot see, since splicing one row out leaves
//     both neighbours internally consistent.
//  3. HASHLESS ROWS ARE A PREFIX. A row with no row_hash is legacy only while
//     no chained row has been seen yet; one that appears AFTER the chain has
//     started is a break naming that row's seq. Without this rule the sweep
//     stepped over every hashless row (the old query filtered them out and the
//     count bucketed them as "legacy"), so an actor who DROPPED or DISABLED
//     migration 0047's trigger — or inserted in replica mode — could append
//     rows that the chain neither covers nor reports, while ok stayed true.
//     The trigger's own head lookup skips them too, so the chain simply
//     stepped over the forged row and closed back up behind it.
//
// Rule 3's blind spot, stated: seq GAPS below the first chained row (burned by
// rolled-back inserts, 0047's "seq is not hashed") can still hold a hashless
// forgery that is indistinguishable from a legacy row. Nothing in the row
// itself dates it; only an off-box copy of the log can.
//
// step reports false once the chain is broken; the caller stops there, because
// every row after an edit mismatches by construction.
type auditChainWalk struct {
	st   AuditChainStatus
	prev string
}

func newAuditChainWalk() *auditChainWalk {
	return &auditChainWalk{st: AuditChainStatus{OK: true}}
}

func (w *auditChainWalk) step(l auditChainLink) bool {
	if l.unchained {
		// Rule 3. Before the first chained row this is the legacy prefix; after
		// it, the row was written with the chain trigger off.
		if w.st.Checked == 0 {
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
		w.fail(l.seq, "the first chained row carries a prev_hash (the row it chained to was deleted)")
	case w.prev != "" && l.prev != w.prev:
		w.fail(l.seq, "prev_hash does not match the preceding row (a row was deleted or reordered)")
	}
	w.prev = l.row
	return w.st.OK
}

func (w *auditChainWalk) fail(seq int64, reason string) {
	w.st.OK, w.st.BrokenSeq, w.st.Reason = false, seq, reason
}

// VerifyAuditChain walks the audit_events hash chain oldest-first, re-hashing
// every row with migration 0047's audit_row_hash — the SAME function the insert
// trigger used to write it, so there is no second implementation to drift out
// of agreement with the first.
//
// THE SWEEP IS PAGED, and that is a correctness property before it is a cost
// one. The comment here used to say "rows are STREAMED, not buffered", which
// described the Go side (pgx iteration with an early break) and not the
// database side: whether an unbounded `ORDER BY seq` streams or materializes is
// a PLANNER decision, and review observed it planning as Seq Scan -> Sort on a
// real audit_events, which buffers the entire table before returning row one.
// Under that plan the early break below saves nothing (the sort has already
// finished), the memory is the whole table, and an audit log cannot be pruned —
// the append-only triggers see to that — so the endpoint an operator reaches for
// during a suspected tamper incident is the one that gets slowest and hungriest
// over time.
//
// Walking it in keyset pages over the primary key removes the planner's choice
// instead of hoping for it: `seq > $1 ORDER BY seq LIMIT n` is an index scan
// with a bound, so memory is one page whatever the table's statistics say, and
// the early break genuinely stops the work.
//
// PAGING DOES NOT VERIFY LESS, which is the only thing that would make this a
// bad trade. Each page is its own snapshot, so the question is whether a row can
// appear BELOW the cursor after the walk has passed it. It cannot: since 0056
// the trigger allocates seq while holding the chain lock and releases it at
// commit, so seq order IS commit order — a row is never assigned a lower seq
// than one that has already committed. For a writer that bypasses the trigger
// entirely (session_replication_role = replica, the bypass rule 3 exists to
// catch after the fact) the comparison is with what the single-statement version
// did, and it is never worse: a row committing DURING the sweep was invisible to
// the one snapshot the old query took, while the paged walk sees it whenever its
// seq is still ahead of the cursor. The walk state — prev, Checked, FirstSeq —
// is carried across pages by one auditChainWalk, so a chain longer than a page
// is one continuous chain and not a sequence of independent ones.
func (s PG) VerifyAuditChain(ctx context.Context) (AuditChainStatus, error) {
	// EVERY row, hashless ones included, in seq order — rule 3 is a statement
	// about where the hashless rows SIT, so the walk has to see them in place.
	// (This also retires the separate `count(*) WHERE row_hash IS NULL` query:
	// Legacy is now counted by the same pass that decides the verdict, so the
	// two can no longer describe different instants under concurrent writes.)
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

	w := newAuditChainWalk()
	// Below every possible seq: the identity starts at 1, but nothing here needs
	// to depend on that.
	after := int64(math.MinInt64)
	for {
		rows, err := s.Pool.Query(ctx, q, after, AuditChainPageSize)
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
			after = l.seq
			if !w.step(l) {
				broke = true
				break
			}
		}
		rows.Close()
		// rows.Err() is checked even after an early break: a broken chain is a
		// finding, but a truncated READ is an error, and answering "tampered"
		// when the connection dropped mid-sweep would be a false accusation.
		if err := rows.Err(); err != nil && w.st.OK {
			return AuditChainStatus{}, fmt.Errorf("store: iterate audit chain: %w", err)
		}
		if broke || int32(n) < AuditChainPageSize {
			return w.st, nil
		}
		// A short-circuit the single-statement version could not offer: between
		// pages the caller's context is live, so a client that goes away (or an
		// operator who gives up) actually stops the sweep.
		if err := ctx.Err(); err != nil {
			return AuditChainStatus{}, fmt.Errorf("store: audit chain sweep cancelled: %w", err)
		}
	}
}
