// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestPG_AuditChainVerifiesAcrossThePartitionConversion: rows chained by the 0.8.5 trigger, the
// conversion to a partitioned table (0111, 0112), and more rows appended through the real writer
// (store.InsertAuditEvent, which calls audit_append) are ONE chain to the verifier.
func TestPG_AuditChainVerifiesAcrossThePartitionConversion(t *testing.T) {
	pool := databaseBefore(t, "0111_audit_partitioned.sql") // a 0.8.5-shaped chain trigger, not yet converted
	ctx := context.Background()

	var preHead string
	for _, action := range []string{"test.pre.1", "test.pre.2", "test.pre.3"} {
		_, _, row := appendAuditRow(t, pool, action)
		preHead = row
	}

	execMigrationFile(t, pool, "0111_audit_partitioned.sql")
	execMigrationFile(t, pool, "0112_audit_chain_partitioned.sql")

	var first types.AuditEvent
	for i, action := range []string{"test.post.1", "test.post.2", "test.post.3"} {
		ev := types.AuditEvent{
			ID: uuid.New(), Time: time.Now().UTC().Add(-time.Duration(i) * 36 * time.Hour), // a replayed event keeps its old time
			ActorType: types.ActorSystem, Actor: "upgrade-probe", Action: action, Outcome: "success",
		}
		if err := store.InsertAuditEvent(ctx, pool, &ev); err != nil {
			t.Fatalf("InsertAuditEvent %s: %v", action, err)
		}
		if i == 0 {
			first = ev
		}
	}
	if first.PrevHash != preHead {
		t.Errorf("the first row appended after the conversion chains to %q, want the 0.8.5 head %q", first.PrevHash, preHead)
	}

	st, err := store.NewPG(pool).VerifyAuditChain(ctx)
	if err != nil {
		t.Fatalf("VerifyAuditChain: %v", err)
	}
	if !st.OK {
		t.Fatalf("chain verify across the conversion: OK=false reason=%q broken_seq=%d", st.Reason, st.BrokenSeq)
	}
	if st.Checked != 6 || st.Legacy != 0 {
		t.Errorf("checked=%d legacy=%d, want the 6 chained rows either side of the conversion and none legacy", st.Checked, st.Legacy)
	}
	if st.HeadHash == "" {
		t.Error("head_hash is empty")
	}
}
