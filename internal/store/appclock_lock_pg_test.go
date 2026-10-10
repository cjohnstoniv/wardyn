// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/google/uuid"
)

// A decision waits on its real approval row; the clock anchor and reauth audit
// transaction must preserve the decision time rather than the eventual unlock.
func TestPG_AppClockDecisionPreservesTimeAcrossRowLock(t *testing.T) {
	for _, reauth := range []bool{false, true} {
		name := "approval"
		if reauth {
			name = "reauth"
		}
		t.Run(name, func(t *testing.T) {
			pool := runsPGPoolIsolated(t)
			st := store.NewPG(pool)
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			runID := seedReauthRun(t, st)
			ap, err := st.CreateApproval(ctx, types.ApprovalRequest{ID: uuid.New(), RunID: runID, Kind: types.ApprovalCredentialReauth, State: types.ApprovalPending, RequestedAt: time.Now().UTC(), RequestedScope: json.RawMessage(`{"owner":"clock"}`)})
			if err != nil {
				t.Fatal(err)
			}
			locker, err := pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = locker.Rollback(context.WithoutCancel(ctx)) }()
			var lockerPID int
			var locked uuid.UUID
			if err := locker.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&lockerPID); err != nil {
				t.Fatal(err)
			}
			if err := locker.QueryRow(ctx, `SELECT id FROM approvals WHERE id=$1 FOR UPDATE`, ap.ID).Scan(&locked); err != nil {
				t.Fatal(err)
			}
			origin := time.Now()
			start := origin.UTC().Add(time.Minute)
			sampled := make(chan struct{})
			var once sync.Once
			st.Now = func() time.Time { once.Do(func() { close(sampled) }); return start.Add(time.Since(origin)) }
			decision := types.ApprovalDecision{State: types.ApprovalApproved, DecidedBy: "clock", Reason: "clock"}
			type response struct {
				approval types.ApprovalRequest
				err      error
			}
			result := make(chan response, 1)
			go func() {
				var got types.ApprovalRequest
				var err error
				if reauth {
					got, err = st.ResolveReauthApproval(ctx, ap.ID, decision, types.AuditEvent{ID: uuid.New(), Time: time.Now().UTC(), RunID: &runID, ActorType: types.ActorHuman, Actor: "clock", Action: "credential.reauth.resolve", Target: ap.ID.String(), Outcome: "success", Data: json.RawMessage(`{}`)})
				} else {
					got, err = st.DecideApproval(ctx, ap.ID, decision)
				}
				result <- response{got, err}
			}()
			select {
			case <-sampled:
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			var updateStarted sql.NullTime
			for {
				if err := pool.QueryRow(ctx, `SELECT min(query_start) FROM pg_stat_activity WHERE $1=ANY(pg_blocking_pids(pid))`, lockerPID).Scan(&updateStarted); err != nil {
					t.Fatal(err)
				}
				if updateStarted.Valid {
					break
				}
				select {
				case <-ctx.Done():
					t.Fatal("decision never waited on row lock")
				case <-time.After(time.Millisecond):
				}
			}
			// Use the UPDATE's own admission clock, frozen before its row wait.
			// A fresh wall-clock sample while blocked can step backward and cannot
			// establish that the eventual timestamp moved forward during the wait.
			cutoff := updateStarted.Time
			if err := locker.Commit(ctx); err != nil {
				t.Fatal(err)
			}
			var got response
			select {
			case got = <-result:
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			if got.err != nil {
				t.Fatal(got.err)
			}
			if got.approval.DecidedAt == nil || got.approval.DecidedAt.After(cutoff) {
				t.Fatalf("lock wait advanced decision: %+v cutoff=%s", got.approval, cutoff)
			}
			if reauth {
				var count int
				if err := pool.QueryRow(ctx, `SELECT count(*) FROM audit_events WHERE action='credential.reauth.resolve' AND target=$1`, ap.ID.String()).Scan(&count); err != nil {
					t.Fatal(err)
				}
				if count != 1 {
					t.Fatalf("reauth decision/audit lost atomicity: count=%d", count)
				}
			}
		})
	}
}
