// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store_test

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Every timestamp writer must preserve admission across a real pool wait.
func TestPG_AppClockWritesPreserveAdmissionAcrossPoolWait(t *testing.T) {
	for _, kind := range []string{"token", "delegated", "run", "capped run", "approval"} {
		t.Run(kind, func(t *testing.T) {
			backing := runsPGPoolIsolated(t)
			cfg := backing.Config().Copy()
			cfg.MaxConns = 1
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			pool, err := pgxpool.NewWithConfig(ctx, cfg)
			if err != nil {
				t.Fatal(err)
			}
			defer pool.Close()
			st := store.NewPG(pool)
			write := clockWaitingWrite(t, ctx, st, kind)
			admitted := time.Now()
			held, err := pool.Acquire(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer held.Release()
			baseline := pool.Stat().EmptyAcquireCount()
			type response struct {
				at  time.Time
				err error
			}
			started := make(chan struct{})
			result := make(chan response, 1)
			go func() { close(started); at, err := write(admitted); result <- response{at, err} }()
			<-started
			time.Sleep(25 * time.Millisecond)
			var cutoff time.Time
			if err := backing.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&cutoff); err != nil {
				t.Fatal(err)
			}
			time.Sleep(175 * time.Millisecond)
			held.Release()
			var got response
			select {
			case got = <-result:
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			if got.err != nil {
				t.Fatal(got.err)
			}
			if pool.Stat().EmptyAcquireCount() <= baseline {
				t.Fatal("writer did not wait for the held pool connection")
			}
			if got.at.After(cutoff) {
				t.Fatalf("%s admitted before cutoff acquired a later timestamp after pool wait: admitted=%s persisted=%s cutoff=%s", kind, admitted, got.at, cutoff)
			}
		})
	}
}

func clockWaitingWrite(t *testing.T, ctx context.Context, st store.PG, kind string) func(time.Time) (time.Time, error) {
	t.Helper()
	principal := "clock-" + uuid.NewString()
	switch kind {
	case "token":
		return func(at time.Time) (time.Time, error) {
			expires := at.Add(time.Hour)
			got, err := st.CreateAPIToken(ctx, types.APIToken{ID: uuid.New(), Principal: principal, Role: "user", UserType: types.UserTypeStandard, Name: "clock", CreatedAt: at, ExpiresAt: &expires}, "wdn_"+uuid.NewString())
			if err == nil && (got.ExpiresAt == nil || got.ExpiresAt.Sub(got.CreatedAt) != time.Hour) {
				return time.Time{}, fmt.Errorf("token lifetime changed")
			}
			return got.CreatedAt, err
		}
	case "delegated":
		d, err := st.CreateDelegate(ctx, types.Delegate{ID: uuid.New(), Name: principal, IdPClientID: principal, Group: "g"}, "wdp_"+uuid.NewString())
		if err != nil {
			t.Fatal(err)
		}
		return func(at time.Time) (time.Time, error) {
			got, err := st.MintDelegatedToken(ctx, types.DelegatedToken{ID: uuid.New(), DelegateID: d.ID, Principal: principal, UserType: types.UserTypeStandard, CreatedAt: at, ExpiresAt: at.Add(10 * time.Minute)}, "wdg_"+uuid.NewString(), at)
			return got.CreatedAt, err
		}
	case "run", "capped run":
		return func(at time.Time) (time.Time, error) {
			run := newRun(types.RunPending)
			run.UpdatedAt = at
			var got types.AgentRun
			var err error
			if kind == "capped run" {
				got, err = st.CreateRunUnderCap(ctx, run, 10)
			} else {
				got, err = st.CreateRun(ctx, run)
			}
			return got.UpdatedAt, err
		}
	case "approval":
		runID := seedReauthRun(t, st)
		ap, err := st.CreateApproval(ctx, types.ApprovalRequest{ID: uuid.New(), RunID: runID, Kind: types.ApprovalCredentialReauth, State: types.ApprovalPending, RequestedAt: time.Now().UTC(), RequestedScope: json.RawMessage(`{"owner":"clock"}`)})
		if err != nil {
			t.Fatal(err)
		}
		return func(at time.Time) (time.Time, error) {
			// Decisions capture their own authority time when the method starts.
			st.Now = func() time.Time { return at.Add(time.Since(at)) }
			decision := types.ApprovalDecision{State: types.ApprovalApproved, DecidedBy: principal, Reason: "clock"}
			got, err := st.DecideApproval(ctx, ap.ID, decision)
			if got.DecidedAt == nil {
				return time.Time{}, fmt.Errorf("decision timestamp missing: %w", err)
			}
			return *got.DecidedAt, err
		}
	default:
		t.Fatalf("unknown writer %s", kind)
		return nil
	}
}
