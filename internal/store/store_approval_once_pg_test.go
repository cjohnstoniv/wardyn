// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// The Postgres halves of the Azure DevOps capability hold: a `once` approval is
// spent exactly once under concurrency, and two escalation raises with one
// canonical scope are one PENDING row. Guarded by WARDYN_TEST_PG; skipped
// cleanly when unset.
package store_test

import (
	"context"
	"encoding/json"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/approval"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

func TestPG_SpendApprovalOnce_ExactlyOnceUnderConcurrency(t *testing.T) {
	pool := runsPGPool(t)
	ctx := context.Background()
	pg := store.NewPG(pool)
	st := approvalStore{pg}

	run := newRun(types.RunRunning)
	if _, err := pg.CreateRun(ctx, run); err != nil {
		t.Fatalf("create run: %v", err)
	}
	grantID := uuid.New()
	if _, err := pg.CreateGrant(ctx, types.CredentialGrant{
		ID: grantID, RunID: run.ID, CreatedAt: time.Now().UTC(),
		Spec: types.GrantSpec{Kind: types.GrantAPIKey, Scope: json.RawMessage(`{"host":"dev.azure.com"}`), TTLSeconds: 60},
	}); err != nil {
		t.Fatalf("create grant: %v", err)
	}
	scope := json.RawMessage(`{"lane":"azure_devops","provider_id":"ado-row-1","org":"contoso","grant_id":"` +
		grantID.String() + `","capability":"pr","repo":"app","ref_class":"","tool":"Azure DevOps","cmd":"x"}`)
	raise := func() types.ApprovalRequest {
		t.Helper()
		ap, err := approval.RequestApproval(ctx, st, types.ApprovalRequest{
			RunID: run.ID, GrantID: &grantID, Kind: types.ApprovalToolCall, RequestedScope: scope,
		})
		if err != nil {
			t.Fatalf("raise: %v", err)
		}
		return ap
	}
	a, b := raise(), raise()
	if a.ID != b.ID {
		t.Fatalf("one canonical scope raised two PENDING rows: %s, %s", a.ID, b.ID)
	}

	// Not approved yet: nothing to spend.
	if spent, err := pg.SpendApprovalOnce(ctx, a.ID, "jti-early"); err != nil || spent {
		t.Fatalf("spent a PENDING row: %v %v", spent, err)
	}
	if _, err := pg.DecideApproval(ctx, a.ID, types.ApprovalDecision{
		State: types.ApprovalApproved, DecidedBy: "owner", Scope: types.ScopeOnce,
	}); err != nil {
		t.Fatalf("decide: %v", err)
	}

	const n = 16
	var wg sync.WaitGroup
	wins := make([]bool, n)
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			spent, err := pg.SpendApprovalOnce(ctx, a.ID, "jti-"+strconv.Itoa(i))
			if err != nil {
				t.Errorf("spend %d: %v", i, err)
			}
			wins[i] = spent
		}(i)
	}
	close(start)
	wg.Wait()
	winner := -1
	for i, w := range wins {
		if w {
			if winner >= 0 {
				t.Fatalf("two spends won: %d and %d", winner, i)
			}
			winner = i
		}
	}
	if winner < 0 {
		t.Fatal("no spend won")
	}
	got, err := pg.GetApproval(ctx, a.ID)
	if err != nil || got.MintedJTI != "jti-"+strconv.Itoa(winner) {
		t.Fatalf("minted_jti = %q (%v), want the winner's", got.MintedJTI, err)
	}
}
