// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store_test

// The retention rows the database writes itself (audit.retention.set, audit.retention.partition_dropped)
// reach the SIEM sink with the chain hashes the database computed. Guarded by WARDYN_TEST_PG like every
// *_pg_test.go here.

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

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

// credential.reauth.resolve is written in the resolve's own transaction, past every recorder: the sink gets
// it once, hashes included, and a resolver that lost the CAS sends nothing.
func TestPG_ReauthResolveRowReachesTheSIEM(t *testing.T) {
	pool := runsPGPool(t)
	ctx := context.Background()
	sink := &captureSink{}
	st := store.NewPG(pool)
	st.SIEM = sink
	runID := seedReauthRun(t, st)
	created, err := st.CreateApproval(ctx, types.ApprovalRequest{
		ID: uuid.New(), RunID: runID, Kind: types.ApprovalCredentialReauth, State: types.ApprovalPending, RequestedAt: time.Now().UTC(),
		RequestedScope: json.RawMessage(`{"owner":"alice@corp.example"}`),
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	ev := func() types.AuditEvent {
		return types.AuditEvent{ID: uuid.New(), Time: time.Now().UTC(), RunID: &runID, ActorType: types.ActorHuman, Actor: "alice@corp.example",
			Action: "credential.reauth.resolve", Target: created.ID.String(), Outcome: "success", Data: json.RawMessage(`{"owner":"alice@corp.example"}`)}
	}
	decision := types.ApprovalDecision{State: types.ApprovalApproved, DecidedBy: "alice@corp.example", Reason: "signed in again"}

	if _, err := st.ResolveReauthApproval(ctx, created.ID, decision, ev()); err != nil {
		t.Fatalf("resolve: %v", err)
	}
	got := sink.take()
	if len(got) != 1 || got[0].Action != "credential.reauth.resolve" {
		t.Fatalf("the sink received %+v, want exactly one credential.reauth.resolve", got)
	}
	var prev, hash string
	if err := pool.QueryRow(ctx, `SELECT COALESCE(prev_hash,''), COALESCE(row_hash,'') FROM audit_events WHERE id = $1`, got[0].ID).Scan(&prev, &hash); err != nil {
		t.Fatalf("read the stored row: %v", err)
	}
	if hash == "" || got[0].RowHash != hash || got[0].PrevHash != prev {
		t.Errorf("the sink's hashes %q/%q differ from the stored %q/%q", got[0].PrevHash, got[0].RowHash, prev, hash)
	}

	if _, err := st.ResolveReauthApproval(ctx, created.ID, decision, ev()); !errors.Is(err, store.ErrAlreadyDecided) {
		t.Fatalf("a second resolve returned %v, want ErrAlreadyDecided", err)
	}
	if got := sink.take(); len(got) != 0 {
		t.Fatalf("a losing resolver sent %+v to the sink", got)
	}
}

// An unchanged policy call writes no row, so it must send none, even while other writers append: a row
// another writer appended between its two reads of the high-water mark is that writer's to send.
func TestPG_UnchangedPolicyCallsSendNoForeignRowUnderConcurrentAppends(t *testing.T) {
	c := newRetChain(t)
	c.window(t, 0)
	ctx := context.Background()
	sink := &captureSink{}
	pg := store.NewPG(c.pool)
	pg.SIEM = sink
	if ch, err := pg.SetAuditRetentionPolicy(ctx, 90); err != nil || ch.Outcome != "pending" {
		t.Fatalf("set 90 = %+v, %v, want pending", ch, err)
	}
	sink.take()

	stop := make(chan struct{})
	var wg sync.WaitGroup
	for range 3 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				ev := types.AuditEvent{ID: uuid.New(), Time: time.Now().UTC(), ActorType: types.ActorSystem, Actor: "appender", Action: "test.race", Outcome: "success"}
				if err := store.InsertAuditEvent(ctx, c.pool, &ev); err != nil {
					t.Errorf("append: %v", err)
					return
				}
			}
		}()
	}
	for range 300 {
		if ch, err := pg.SetAuditRetentionPolicy(ctx, 90); err != nil || ch.Outcome != "unchanged" {
			t.Fatalf("set 90 again = %+v, %v, want unchanged", ch, err)
		}
	}
	close(stop)
	wg.Wait()
	if got := sink.take(); len(got) != 0 {
		t.Fatalf("unchanged calls sent %d rows to the sink (first %s), want none", len(got), got[0].Action)
	}
}
