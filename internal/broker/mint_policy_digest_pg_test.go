// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package broker

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestBrokerMint_StampsPolicyDigest: the mint row is written on the mint's own transaction, past the
// audit recorder, so it reads the run's digest itself. A run with a digest stamps it, overwriting any
// value the row carried; a run without one drops any value the row carried.
func TestBrokerMint_StampsPolicyDigest(t *testing.T) {
	pool := pgPool(t)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	st := NewPgxStore(pool)
	for name, tc := range map[string]struct{ stored, want string }{
		"run with a digest": {"d1", "d1"},
		"run without one":   {"", ""},
	} {
		runID := uuid.New()
		seedRun(ctx, t, pool, runID)
		if tc.stored != "" {
			if _, err := pool.Exec(ctx, `UPDATE agent_runs SET policy_digest=$2 WHERE id=$1`, runID, tc.stored); err != nil {
				t.Fatal(err)
			}
		}
		tx, err := st.BeginReadCommitted(ctx)
		if err != nil {
			t.Fatal(err)
		}
		ev := types.AuditEvent{ID: uuid.New(), Time: time.Now().UTC(), RunID: &runID, ActorType: types.ActorSystem,
			Actor: "broker", Action: "credential.mint", Outcome: "success", Data: json.RawMessage(`{"jti":"j","policy_digest":"forged"}`)}
		if err := insertAuditEventTx(ctx, tx, ev); err != nil {
			_ = tx.Rollback(ctx)
			t.Fatalf("%s: insertAuditEventTx: %v", name, err)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
		var got *string
		if err := pool.QueryRow(ctx, `SELECT data->>'policy_digest' FROM audit_events WHERE id=$1`, ev.ID).Scan(&got); err != nil {
			t.Fatal(err)
		}
		switch {
		case tc.want == "" && got != nil:
			t.Errorf("%s: policy_digest = %q, want the key absent", name, *got)
		case tc.want != "" && (got == nil || *got != tc.want):
			t.Errorf("%s: policy_digest = %v, want %q", name, got, tc.want)
		}
	}
}
