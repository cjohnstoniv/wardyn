// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
)

// runPATReaderStore is a store with both halves of the run-token record.
type runPATReaderStore interface {
	store.RunPATStore
	store.RunPATReader
}

// runPATReaderContract is what every RunPATReader must do: a run's tokens,
// revoked or not, oldest first, and a person's newest token on a row.
func runPATReaderContract(t *testing.T, st runPATReaderStore) {
	t.Helper()
	ctx := context.Background()
	run, other := uuid.New(), uuid.New()
	// Authorization ids that sort AGAINST creation order, so an answer ordered
	// by id rather than created_at fails.
	ids := []uuid.UUID{
		uuid.MustParse("ffffffff-0000-4000-8000-000000000003"),
		uuid.MustParse("88888888-0000-4000-8000-000000000002"),
		uuid.MustParse("11111111-0000-4000-8000-000000000001"),
	}
	insert := func(runID, id uuid.UUID, owner, row string) {
		t.Helper()
		if err := st.InsertRunPAT(ctx, store.RunPAT{
			RunID: runID, AuthorizationID: id, Owner: owner, ProviderRowID: row, Org: "contoso",
			Scope: "vso.code", ValidTo: time.Now().Add(time.Hour).UTC().Truncate(time.Microsecond),
		}); err != nil {
			t.Fatalf("InsertRunPAT: %v", err)
		}
		time.Sleep(2 * time.Millisecond) // distinct created_at on every clock
	}
	if _, found, err := st.LastRunPAT(ctx, "oidc:alice", "ado"); err != nil || found {
		t.Fatalf("LastRunPAT on an empty record = found %v, err %v; want none", found, err)
	}
	for _, id := range ids {
		insert(run, id, "oidc:alice", "ado")
	}
	insert(other, uuid.New(), "oidc:bob", "ado")
	insert(other, uuid.New(), "oidc:alice", "ado2")
	if _, err := st.MarkRunPATRevoked(ctx, run, ids[0], "renewal", "HTTP 400"); err != nil {
		t.Fatal(err)
	}

	got, err := st.ListRunPATs(ctx, run)
	if err != nil {
		t.Fatalf("ListRunPATs: %v", err)
	}
	if len(got) != len(ids) {
		t.Fatalf("ListRunPATs = %d tokens, want %d (revoked ones included, other runs' excluded)", len(got), len(ids))
	}
	for i, p := range got {
		if p.AuthorizationID != ids[i] {
			t.Errorf("ListRunPATs[%d] = %s, want %s: not oldest first", i, p.AuthorizationID, ids[i])
		}
	}
	if got[0].RevokedAt == nil || got[0].RevokeReason != "renewal" || got[0].LastError != "HTTP 400" {
		t.Errorf("the revoked token reads %+v, want its revoked_at, reason and last error", got[0])
	}
	if none, err := st.ListRunPATs(ctx, uuid.New()); err != nil || len(none) != 0 || none == nil {
		t.Errorf("ListRunPATs of a run with none = %v, %v; want an empty, non-nil list", none, err)
	}

	last, found, err := st.LastRunPAT(ctx, "oidc:alice", "ado")
	if err != nil || !found || last.AuthorizationID != ids[2] {
		t.Errorf("LastRunPAT(alice, ado) = %s found %v err %v, want %s (newest on that row, not another row's)",
			last.AuthorizationID, found, err, ids[2])
	}
}

func TestMem_RunPATReader(t *testing.T) {
	runPATReaderContract(t, store.NewMemRunPATs())
}

// TestPG_RunPATReader runs the same contract against Postgres. Guarded by
// WARDYN_TEST_PG; skipped cleanly when unset.
func TestPG_RunPATReader(t *testing.T) {
	runPATReaderContract(t, store.NewPG(runsPGPoolIsolated(t)))
}
