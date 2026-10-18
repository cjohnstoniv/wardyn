// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Which AWS SSO grants the background renewer renews for: only a live run's,
// and only one the broker would still mint. Guarded by WARDYN_TEST_PG; skipped
// cleanly when unset.
package store_test

import (
	"context"
	"encoding/json"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

func TestPG_ListLiveAWSSSOGrants_OnlyALiveRunsMintableGrant(t *testing.T) {
	pool := runsPGPoolIsolated(t)
	ctx := context.Background()
	pg := store.NewPG(pool)
	scope := json.RawMessage(`{"host":"portal.sso.eu-west-2.amazonaws.com","secret_name":"` + types.AWSSSOAccessTokenSecret + `"}`)

	grant := func(state types.RunState, spec types.GrantSpec, after func(run types.AgentRun, grantID uuid.UUID)) uuid.UUID {
		t.Helper()
		run := newRun(state)
		if _, err := pg.CreateRun(ctx, run); err != nil {
			t.Fatalf("create run: %v", err)
		}
		id := uuid.New()
		if _, err := pg.CreateGrant(ctx, types.CredentialGrant{ID: id, RunID: run.ID, CreatedAt: time.Now().UTC(), Spec: spec}); err != nil {
			t.Fatalf("create grant: %v", err)
		}
		if after != nil {
			after(run, id)
		}
		return id
	}
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, q, args...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	approve := func(state types.ApprovalState, apScope json.RawMessage, runID *uuid.UUID) func(types.AgentRun, uuid.UUID) {
		return func(run types.AgentRun, grantID uuid.UUID) {
			t.Helper()
			on := run.ID
			if runID != nil {
				on = *runID
			}
			if _, err := pg.CreateApproval(ctx, types.ApprovalRequest{ID: uuid.New(), RunID: on, GrantID: &grantID,
				Kind: types.ApprovalCredential, RequestedScope: apScope, State: state, RequestedAt: time.Now().UTC()}); err != nil {
				t.Fatalf("create approval: %v", err)
			}
		}
	}
	sso := types.GrantSpec{Kind: types.GrantAPIKey, Scope: scope}
	gated := types.GrantSpec{Kind: types.GrantAPIKey, Scope: scope, RequiresApproval: true}
	otherRun := newRun(types.RunStopped)
	if _, err := pg.CreateRun(ctx, otherRun); err != nil {
		t.Fatalf("create run: %v", err)
	}
	other := otherRun.ID

	want := []uuid.UUID{
		grant(types.RunRunning, sso, nil),
		grant(types.RunStarting, sso, nil),
		grant(types.RunWaiting, sso, nil),
		grant(types.RunRunning, gated, approve(types.ApprovalApproved, scope, nil)),
	}
	refused := map[string]uuid.UUID{
		"another secret": grant(types.RunRunning, types.GrantSpec{Kind: types.GrantAPIKey,
			Scope: json.RawMessage(`{"host":"api.example.com","secret_name":"some-key"}`)}, nil),
		"ended run":  grant(types.RunCompleted, sso, nil),
		"queued run": grant(types.RunQueued, sso, nil),
		"paused run": grant(types.RunRunning, sso, func(r types.AgentRun, _ uuid.UUID) {
			exec(`UPDATE agent_runs SET paused_at = now() WHERE id = $1`, r.ID)
		}),
		"lost run": grant(types.RunRunning, sso, func(r types.AgentRun, _ uuid.UUID) {
			exec(`UPDATE agent_runs SET lost_at = now(), lost_reason = $2 WHERE id = $1`, r.ID, string(types.LostEnded))
		}),
		"revoked run": grant(types.RunRunning, sso, func(r types.AgentRun, _ uuid.UUID) {
			exec(`INSERT INTO identity_revocations (jti, run_id) VALUES ($1, $2)`, "run:"+r.ID.String(), r.ID)
		}),
		"gated, never approved":    grant(types.RunRunning, gated, nil),
		"gated, pending":           grant(types.RunRunning, gated, approve(types.ApprovalPending, scope, nil)),
		"denied approval":          grant(types.RunRunning, sso, approve(types.ApprovalDenied, scope, nil)),
		"approved for another run": grant(types.RunRunning, gated, approve(types.ApprovalApproved, scope, &other)),
		"approved a wider scope": grant(types.RunRunning, gated, approve(types.ApprovalApproved,
			json.RawMessage(`{"host":"*.amazonaws.com","secret_name":"`+types.AWSSSOAccessTokenSecret+`"}`), nil)),
	}

	got, err := pg.ListLiveAWSSSOGrants(ctx, types.AWSSSOAccessTokenSecret)
	if err != nil {
		t.Fatal(err)
	}
	ids := make([]uuid.UUID, len(got))
	for i, g := range got {
		ids[i] = g.ID
		if g.Spec.Kind != types.GrantAPIKey || len(g.Spec.Scope) == 0 {
			t.Errorf("grant %s came back without its spec: %+v", g.ID, g.Spec)
		}
	}
	for _, id := range want {
		if !slices.Contains(ids, id) {
			t.Errorf("live grant %s is missing", id)
		}
	}
	for name, id := range refused {
		if slices.Contains(ids, id) {
			t.Errorf("%s: grant %s is listed; the renewer would renew for it", name, id)
		}
	}
	if len(ids) != len(want) {
		t.Errorf("listed %d grants, want %d", len(ids), len(want))
	}
}
