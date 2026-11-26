// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

func identityRow(data string) *types.AuditEvent {
	return &types.AuditEvent{ID: uuid.New(), Time: time.Now().UTC().Truncate(time.Microsecond), ActorType: types.ActorAgent,
		Actor: "spiffe://wardyn.test/agent-run/x", Action: "egress.allow", Outcome: "success", Data: json.RawMessage(data)}
}

// TestAuditChain_RowsWithIdentityKeysVerify: run_owner, run_workload and sponsor are ordinary data to the
// chain; rows carrying them hash, chain and verify like any other.
func TestAuditChain_RowsWithIdentityKeysVerify(t *testing.T) {
	pool := runsPGPoolIsolated(t)
	ctx := context.Background()
	for _, d := range []string{
		`{"run_owner":"alice@example.com","run_workload":"spiffe://wardyn.test/agent-run/x"}`,
		`{"run_owner":"service:ci","sponsor":"boss@example.com","run_workload":"w","owner":"team-a"}`,
	} {
		if err := store.InsertAuditEvent(ctx, pool, identityRow(d)); err != nil {
			t.Fatal(err)
		}
	}
	wantVerifies(t, pool, "after rows with run identity keys")
}

// TestAuditChain_PreItem5RowsStillVerify: rows written before the identity keys existed, and rows written after
// with and without them, share one chain that verifies.
func TestAuditChain_PreItem5RowsStillVerify(t *testing.T) {
	pool := runsPGPoolIsolated(t)
	ctx := context.Background()
	for _, d := range []string{`{"host":"allowed.test"}`, `{"run_owner":"alice@example.com"}`, `{"host":"later.test"}`, ``} {
		ev := identityRow(d)
		if d == "" {
			ev.Data = nil
		}
		if err := store.InsertAuditEvent(ctx, pool, ev); err != nil {
			t.Fatal(err)
		}
	}
	wantVerifies(t, pool, "across rows with and without run identity keys")
}
