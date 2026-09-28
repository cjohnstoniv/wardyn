// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"fmt"
	"math"
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
//     row, which rule 1 alone cannot see;
//  3. hashless rows are a prefix: legacy only while no chained row has been
//     seen yet, a break otherwise. Without this rule an actor who dropped or
//     disabled migration 0047's trigger (or inserted in replica mode) could
//     append rows the chain neither covers nor reports, while OK stayed true.
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
func (s PG) VerifyAuditChain(ctx context.Context) (AuditChainStatus, error) {
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
		// Checked even after an early break: a truncated READ is an error, and
		// answering "tampered" on a mid-sweep disconnect would be a false
		// accusation.
		if err := rows.Err(); err != nil && w.st.OK {
			return AuditChainStatus{}, fmt.Errorf("store: iterate audit chain: %w", err)
		}
		if broke || int32(n) < AuditChainPageSize {
			return w.st, nil
		}
		// Between pages the caller's context is live, so a client going away
		// (or an operator giving up) actually stops the sweep.
		if err := ctx.Err(); err != nil {
			return AuditChainStatus{}, fmt.Errorf("store: audit chain sweep cancelled: %w", err)
		}
	}
}
