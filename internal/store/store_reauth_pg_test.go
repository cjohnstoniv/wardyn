// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// The credential_reauth kind, against the database that actually has to accept
// it (migration 0064), and the transaction that moves one to APPROVED with its
// audit row. Guarded by WARDYN_TEST_PG; skipped cleanly when unset.
package store_test

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

func seedReauthRun(t *testing.T, st store.PG) uuid.UUID {
	t.Helper()
	runID := uuid.New()
	if _, err := st.CreateRun(context.Background(), types.AgentRun{
		ID: runID, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
		CreatedBy: "alice@corp.example", Agent: "claude-code", Repo: "test/repo", Task: "t",
		ConfinementClass: types.CC2, State: types.RunPending,
		SPIFFEID: "spiffe://test/agent-run/" + runID.String(), RunnerTarget: "docker",
	}); err != nil {
		t.Fatalf("create run: %v", err)
	}
	return runID
}

// THE ROUND TRIP. The enum-parity guards compare a migration's CHECK against
// the Go set by PARSING; this one writes the value and reads it back, which is
// the only thing that proves the constraint the database is actually enforcing
// admits it.
func TestPG_CredentialReauthKindRoundTrips(t *testing.T) {
	pool := runsPGPool(t)
	ctx := context.Background()
	st := store.NewPG(pool)
	runID := seedReauthRun(t, st)

	created, err := st.CreateApproval(ctx, types.ApprovalRequest{
		ID: uuid.New(), RunID: runID, Kind: types.ApprovalCredentialReauth, State: types.ApprovalPending, RequestedAt: time.Now().UTC(),
		RequestedScope: json.RawMessage(`{"mechanism":"bedrock_sso","credential_source":"per_user","owner":"alice@corp.example"}`),
	})
	if err != nil {
		t.Fatalf("a credential_reauth approval was REFUSED by the database — migration 0064's CHECK does not admit it: %v", err)
	}
	if created.Kind != types.ApprovalCredentialReauth {
		t.Fatalf("read back kind %q, want credential_reauth", created.Kind)
	}
	got, err := st.GetApproval(ctx, created.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Kind != types.ApprovalCredentialReauth || got.State != types.ApprovalPending {
		t.Errorf("round trip = %s/%s, want credential_reauth/PENDING", got.Kind, got.State)
	}
}

// ResolveReauthApproval moves the row and writes its audit row TOGETHER, and is
// a PENDING-only CAS — so a second resolver (two captures, or a capture racing
// the reconcile-on-read) loses cleanly and writes no second audit row.
func TestPG_ResolveReauthApproval_IsAOneWayCASWithItsAuditRow(t *testing.T) {
	pool := runsPGPool(t)
	ctx := context.Background()
	st := store.NewPG(pool)
	runID := seedReauthRun(t, st)

	created, err := st.CreateApproval(ctx, types.ApprovalRequest{
		ID: uuid.New(), RunID: runID, Kind: types.ApprovalCredentialReauth, State: types.ApprovalPending, RequestedAt: time.Now().UTC(),
		RequestedScope: json.RawMessage(`{"owner":"alice@corp.example"}`),
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	ev := func() types.AuditEvent {
		return types.AuditEvent{
			ID: uuid.New(), Time: time.Now().UTC(), RunID: &runID,
			ActorType: types.ActorHuman, Actor: "alice@corp.example",
			Action: "credential.reauth.resolve", Target: created.ID.String(), Outcome: "success",
			Data: json.RawMessage(`{"owner":"alice@corp.example"}`),
		}
	}
	decision := types.ApprovalDecision{State: types.ApprovalApproved, DecidedBy: "alice@corp.example", Reason: "signed in again"}

	out, err := st.ResolveReauthApproval(ctx, created.ID, decision, ev())
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if out.State != types.ApprovalApproved || out.DecidedBy != "alice@corp.example" {
		t.Errorf("resolved row = %+v, want APPROVED by the capturing principal", out)
	}
	rows, err := st.QueryAuditEvents(ctx, runID, 50)
	if err != nil {
		t.Fatalf("audit query: %v", err)
	}
	n := 0
	for _, r := range rows {
		if r.Action == "credential.reauth.resolve" {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("credential.reauth.resolve rows = %d, want exactly 1", n)
	}

	// The SECOND resolver loses, and writes nothing.
	if _, err := st.ResolveReauthApproval(ctx, created.ID, decision, ev()); !errors.Is(err, store.ErrAlreadyDecided) {
		t.Fatalf("a second resolve returned %v, want ErrAlreadyDecided", err)
	}
	rows, _ = st.QueryAuditEvents(ctx, runID, 50)
	n = 0
	for _, r := range rows {
		if r.Action == "credential.reauth.resolve" {
			n++
		}
	}
	if n != 1 {
		t.Errorf("a losing resolver wrote a second audit row (%d total)", n)
	}
}

// It exists for ONE transition: it refuses to be a general
// decide-anything-and-audit-it-however-you-like primitive.
func TestPG_ResolveReauthApproval_RefusesAnotherAuditAction(t *testing.T) {
	pool := runsPGPool(t)
	ctx := context.Background()
	st := store.NewPG(pool)
	runID := seedReauthRun(t, st)
	created, err := st.CreateApproval(ctx, types.ApprovalRequest{
		ID: uuid.New(), RunID: runID, Kind: types.ApprovalCredentialReauth, State: types.ApprovalPending, RequestedAt: time.Now().UTC(),
		RequestedScope: json.RawMessage(`{"owner":"a"}`),
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	_, err = st.ResolveReauthApproval(ctx, created.ID, types.ApprovalDecision{State: types.ApprovalApproved},
		types.AuditEvent{ID: uuid.New(), Time: time.Now().UTC(), Action: "approval.decide", Outcome: "success"})
	if err == nil {
		t.Fatal("ResolveReauthApproval wrote approval.decide — the one action this transition must never write")
	}
	got, _ := st.GetApproval(ctx, created.ID)
	if got.State != types.ApprovalPending {
		t.Errorf("the row moved to %s on a refused call", got.State)
	}
}

// …and it is kind-scoped: an egress_domain row cannot be resolved through the
// re-auth transition, which would put a credential.reauth.resolve row on a
// decision that is a real human decision.
func TestPG_ResolveReauthApproval_RefusesAnotherKind(t *testing.T) {
	pool := runsPGPool(t)
	ctx := context.Background()
	st := store.NewPG(pool)
	runID := seedReauthRun(t, st)
	created, err := st.CreateApproval(ctx, types.ApprovalRequest{
		ID: uuid.New(), RunID: runID, Kind: types.ApprovalEgressDomain, State: types.ApprovalPending, RequestedAt: time.Now().UTC(),
		RequestedScope: json.RawMessage(`{"host":"example.com"}`),
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := st.ResolveReauthApproval(ctx, created.ID, types.ApprovalDecision{State: types.ApprovalApproved},
		types.AuditEvent{
			ID: uuid.New(), Time: time.Now().UTC(), RunID: &runID, ActorType: types.ActorHuman, Actor: "a",
			Action: "credential.reauth.resolve", Target: created.ID.String(), Outcome: "success",
		}); !errors.Is(err, store.ErrAlreadyDecided) {
		t.Fatalf("resolving an egress_domain row through the re-auth transition returned %v, want ErrAlreadyDecided", err)
	}
}

// THE INDEX, UNDER CONCURRENCY (patch-review batch E). Migration 0064's own
// comment claims the 0022 partial unique index "now also covers this kind ...
// One lapsed credential per run is one request, however many of the sandbox's
// concurrent calls discover it" — and nothing proved it. The index is
// `(run_id, kind, requested_scope) WHERE state='PENDING' AND kind <> 'credential'`,
// so the claim rests on `credential_reauth` being a kind OTHER than `credential`,
// which is the kind of thing a reader agrees with and a database decides.
//
// It matters because it is the ONLY thing that makes the property true. The
// application dedup above it is a LIST then an INSERT with a real window between
// them: 16 concurrent resolvers for one run can and do all list an empty table.
// Without this index the honest answer to "how many rows does a lapse raise" is
// "as many as the scheduler allows", and the console would show a person the
// same sign-in request four times.
//
// Eight raises, one row; every loser gets ErrDuplicatePending, which is the
// signal approval.RequestApproval turns back into the winner's row.
func TestPG_ConcurrentCredentialReauthRaisesLandOneRow(t *testing.T) {
	pool := runsPGPool(t)
	ctx := context.Background()
	st := store.NewPG(pool)
	runID := seedReauthRun(t, st)
	scope := json.RawMessage(`{"mechanism":"bedrock_sso","credential_source":"per_user","owner":"alice@corp.example"}`)

	const raisers = 8
	var wg sync.WaitGroup
	errs := make([]error, raisers)
	ids := make([]uuid.UUID, raisers)
	start := make(chan struct{})
	for i := range errs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start // all eight at the index at once
			out, err := st.CreateApproval(ctx, types.ApprovalRequest{
				ID: uuid.New(), RunID: runID, Kind: types.ApprovalCredentialReauth,
				State: types.ApprovalPending, RequestedAt: time.Now().UTC(), RequestedScope: scope,
			})
			errs[i], ids[i] = err, out.ID
		}(i)
	}
	close(start)
	wg.Wait()

	winners, losers := 0, 0
	for i, err := range errs {
		switch {
		case err == nil:
			winners++
		case errors.Is(err, store.ErrDuplicatePending):
			losers++
		default:
			t.Errorf("raiser %d failed with %v, want nil or ErrDuplicatePending — any other error reaches "+
				"the sidecar as a 503 and ends the model call instead of parking it", i, err)
		}
		_ = ids[i]
	}
	if winners != 1 || losers != raisers-1 {
		t.Fatalf("winners=%d losers=%d, want 1 and %d — the partial unique index is not covering "+
			"credential_reauth, so one lapse raises one question per concurrent caller", winners, losers, raisers-1)
	}

	rows, err := st.ListApprovals(ctx, types.ApprovalPending)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	n := 0
	for _, ap := range rows {
		if ap.RunID == runID && ap.Kind == types.ApprovalCredentialReauth {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("pending credential_reauth rows for one run = %d, want exactly 1", n)
	}
}
