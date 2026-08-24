// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"fmt"
)

// lockAuditChainSQL serializes appends to the audit_events hash chain. It is
// shared by store.InsertAuditEvent and (verbatim, since that package cannot
// import this one) the broker's insertAuditEventTx — the two in-tree paths that
// INSERT into audit_events. See db.AuditChainLockKey for why the lock lives at
// the caller instead of inside migration 0047's trigger.
const lockAuditChainSQL = `SELECT pg_advisory_xact_lock($1)`

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
	// Legacy is how many rows carry NO hash at all — rows that predate
	// migration 0047. They are outside the chain by design and are never a
	// failure; a non-zero value on an upgraded deployment is expected.
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
// Rows are STREAMED, not buffered: an audit log is unbounded, and this is the
// one query in the package that reads all of it.
func (s PG) VerifyAuditChain(ctx context.Context) (AuditChainStatus, error) {
	var legacy int64
	if err := s.Pool.QueryRow(ctx,
		`SELECT count(*) FROM audit_events WHERE row_hash IS NULL`,
	).Scan(&legacy); err != nil {
		return AuditChainStatus{}, fmt.Errorf("store: count pre-chain audit rows: %w", err)
	}

	const q = `
		SELECT seq,
		       prev_hash IS NULL,
		       COALESCE(prev_hash,''),
		       row_hash,
		       audit_row_hash(prev_hash, id, time, run_id, actor_type, actor,
		                      action, target, outcome, source_ip, data)
		FROM audit_events
		WHERE row_hash IS NOT NULL
		ORDER BY seq`
	rows, err := s.Pool.Query(ctx, q)
	if err != nil {
		return AuditChainStatus{}, fmt.Errorf("store: read audit chain: %w", err)
	}
	defer rows.Close()

	w := newAuditChainWalk()
	for rows.Next() {
		var l auditChainLink
		if err := rows.Scan(&l.seq, &l.prevIsNull, &l.prev, &l.row, &l.want); err != nil {
			return AuditChainStatus{}, fmt.Errorf("store: scan audit chain row: %w", err)
		}
		if !w.step(l) {
			break
		}
	}
	// rows.Err() is checked even after an early break: a broken chain is a
	// finding, but a truncated READ is an error, and answering "tampered" when
	// the connection dropped mid-sweep would be a false accusation.
	if err := rows.Err(); err != nil && w.st.OK {
		return AuditChainStatus{}, fmt.Errorf("store: iterate audit chain: %w", err)
	}
	w.st.Legacy = legacy
	return w.st, nil
}
