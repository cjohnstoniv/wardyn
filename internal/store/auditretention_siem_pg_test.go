// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store_test

// The retention rows the database writes itself (audit.retention.set, audit.retention.partition_dropped)
// reach the SIEM sink with the chain hashes the database computed. Guarded by WARDYN_TEST_PG like every
// *_pg_test.go here.

import (
	"context"
	"sync"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

type captureSink struct {
	mu  sync.Mutex
	got []types.AuditEvent
}

func (*captureSink) Name() string { return "capture" }

func (c *captureSink) Emit(_ context.Context, ev types.AuditEvent) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.got = append(c.got, ev)
	return nil
}

func (c *captureSink) take() []types.AuditEvent {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := c.got
	c.got = nil
	return out
}

// wantSent checks that exactly one event of action went to the sink and that it is the stored row, hashes included.
func wantSent(t *testing.T, c *retChain, sink *captureSink, action string) {
	t.Helper()
	got := sink.take()
	if len(got) != 1 || got[0].Action != action {
		t.Fatalf("the sink received %+v, want exactly one %s", got, action)
	}
	var prev, hash string
	if err := c.pool.QueryRow(context.Background(), `SELECT COALESCE(prev_hash,''), COALESCE(row_hash,'') FROM audit_events WHERE id = $1`, got[0].ID).Scan(&prev, &hash); err != nil {
		t.Fatalf("read the stored %s row: %v", action, err)
	}
	if hash == "" || got[0].RowHash != hash || got[0].PrevHash != prev {
		t.Errorf("%s reached the sink with hashes %q/%q, the store holds %q/%q", action, got[0].PrevHash, got[0].RowHash, prev, hash)
	}
	if len(got[0].Data) == 0 {
		t.Errorf("%s reached the sink without its data", action)
	}
}

func TestPG_RetentionRowsReachTheSIEM(t *testing.T) {
	c := newRetChain(t)
	c.window(t, 0)
	ctx := context.Background()
	sink := &captureSink{}
	pg := store.NewPG(c.pool)
	pg.SIEM = sink

	if ch, err := pg.SetAuditRetentionPolicy(ctx, 90); err != nil || ch.Outcome != "pending" {
		t.Fatalf("set 90 = %+v, %v, want pending", ch, err)
	}
	wantSent(t, c, sink, "audit.retention.set")

	// Restating the pending value writes no row, so the sink gets none.
	if ch, err := pg.SetAuditRetentionPolicy(ctx, 90); err != nil || ch.Outcome != "unchanged" {
		t.Fatalf("set 90 again = %+v, %v, want unchanged", ch, err)
	}
	if got := sink.take(); len(got) != 0 {
		t.Fatalf("an unchanged policy sent %+v to the sink", got)
	}

	// A refused drop writes no row either.
	c.window(t, 30)
	if _, err := pg.DropAuditPartition(ctx, retMid, "not-the-digest", "ops@example.com"); err == nil {
		t.Fatal("a drop with a wrong digest was accepted")
	}
	if got := sink.take(); len(got) != 0 {
		t.Fatalf("a refused drop sent %+v to the sink", got)
	}

	d, err := pg.DropAuditPartition(ctx, "audit_events_legacy", c.digest(t, "audit_events_legacy"), "ops@example.com")
	if err != nil {
		t.Fatalf("drop: %v", err)
	}
	wantSent(t, c, sink, "audit.retention.partition_dropped")
	if st := sweep(t, c.pool); !st.OK || st.AnchorSeq != d.SeqHi {
		t.Fatalf("verify after the drop = %+v", st)
	}
}
