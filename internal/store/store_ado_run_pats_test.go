// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
)

// runPATContract is what every RunPATStore must do, so the in-memory double the
// other lanes test against cannot drift from Postgres.
func runPATContract(t *testing.T, st store.RunPATStore) {
	t.Helper()
	ctx := context.Background()
	runA, runB := uuid.New(), uuid.New()
	pat := func(run uuid.UUID, owner, row string) store.RunPAT {
		return store.RunPAT{
			RunID: run, AuthorizationID: uuid.New(), Owner: owner, ProviderRowID: row, Org: "contoso",
			Scope: "vso.code vso.project", ValidTo: time.Now().Add(time.Hour).UTC().Truncate(time.Microsecond),
		}
	}
	a1, a2, b1 := pat(runA, "oidc:alice", "ado"), pat(runA, "oidc:alice", "ado"), pat(runB, "oidc:bob", "ado2")
	for _, p := range []store.RunPAT{a1, a2, b1} {
		if err := st.InsertRunPAT(ctx, p); err != nil {
			t.Fatalf("InsertRunPAT: %v", err)
		}
	}
	if err := st.InsertRunPAT(ctx, a1); !errors.Is(err, store.ErrConflict) {
		t.Errorf("InsertRunPAT of a recorded token = %v, want ErrConflict", err)
	}
	for name, bad := range map[string]store.RunPAT{
		"no run":      {AuthorizationID: uuid.New(), Owner: "o", ProviderRowID: "r", Org: "c", Scope: "s", ValidTo: a1.ValidTo},
		"no owner":    {RunID: runA, AuthorizationID: uuid.New(), ProviderRowID: "r", Org: "c", Scope: "s", ValidTo: a1.ValidTo},
		"no scope":    {RunID: runA, AuthorizationID: uuid.New(), Owner: "o", ProviderRowID: "r", Org: "c", ValidTo: a1.ValidTo},
		"no valid_to": {RunID: runA, AuthorizationID: uuid.New(), Owner: "o", ProviderRowID: "r", Org: "c", Scope: "s"},
	} {
		if err := st.InsertRunPAT(ctx, bad); err == nil {
			t.Errorf("InsertRunPAT(%s) = nil, want a refusal", name)
		}
	}

	ids := func(f store.RunPATFilter) []uuid.UUID {
		t.Helper()
		got, err := st.ListUnrevokedRunPATs(ctx, f)
		if err != nil {
			t.Fatalf("ListUnrevokedRunPATs(%+v): %v", f, err)
		}
		out := make([]uuid.UUID, len(got))
		for i, p := range got {
			out[i] = p.AuthorizationID
			if p.RevokedAt != nil || p.CreatedAt.IsZero() {
				t.Errorf("listed %+v: want unrevoked with a created_at", p)
			}
		}
		return out
	}
	same := func(name string, got []uuid.UUID, want ...store.RunPAT) {
		t.Helper()
		if len(got) != len(want) {
			t.Fatalf("%s = %v, want %d tokens", name, got, len(want))
		}
		seen := map[uuid.UUID]bool{}
		for _, g := range got {
			seen[g] = true
		}
		for _, w := range want {
			if !seen[w.AuthorizationID] {
				t.Errorf("%s = %v, missing %s", name, got, w.AuthorizationID)
			}
		}
	}
	same("everything", ids(store.RunPATFilter{}), a1, a2, b1)
	same("by run", ids(store.RunPATFilter{RunID: runA}), a1, a2)
	same("by owner", ids(store.RunPATFilter{Owner: "oidc:bob"}), b1)
	same("by provider row", ids(store.RunPATFilter{ProviderRowID: "ado"}), a1, a2)
	same("by run and owner that disagree", ids(store.RunPATFilter{RunID: runA, Owner: "oidc:bob"}))

	if ok, err := st.MarkRunPATRevoked(ctx, runA, a1.AuthorizationID, "pause", ""); err != nil || !ok {
		t.Fatalf("MarkRunPATRevoked = %v, %v; want true", ok, err)
	}
	if ok, err := st.MarkRunPATRevoked(ctx, runA, a1.AuthorizationID, "sweep", ""); err != nil || ok {
		t.Errorf("a second MarkRunPATRevoked = %v, %v; want false (already closed)", ok, err)
	}
	if ok, err := st.MarkRunPATRevoked(ctx, runB, a2.AuthorizationID, "sweep", ""); err != nil || ok {
		t.Errorf("MarkRunPATRevoked with another run's key = %v, %v; want false", ok, err)
	}
	if ok, err := st.MarkRunPATRevoked(ctx, runA, a2.AuthorizationID, "", ""); err == nil || ok {
		t.Errorf("MarkRunPATRevoked with no reason = %v, %v; want a refusal", ok, err)
	}

	// A failed revoke is noted and the row stays listed, with the error.
	if err := st.NoteRunPATRevokeFailed(ctx, runB, b1.AuthorizationID, "dead refresh token"); err != nil {
		t.Fatalf("NoteRunPATRevokeFailed: %v", err)
	}
	failed, err := st.ListUnrevokedRunPATs(ctx, store.RunPATFilter{RunID: runB})
	if err != nil || len(failed) != 1 || failed[0].AuthorizationID != b1.AuthorizationID || failed[0].LastError != "dead refresh token" {
		t.Fatalf("after NoteRunPATRevokeFailed = %+v, %v; want b1 still listed with its last error", failed, err)
	}
	// Noting a closed or unknown row is harmless and does not reopen or create it.
	if err := st.NoteRunPATRevokeFailed(ctx, runA, a1.AuthorizationID, "late"); err != nil {
		t.Errorf("NoteRunPATRevokeFailed on a closed row: %v", err)
	}
	if err := st.NoteRunPATRevokeFailed(ctx, uuid.New(), uuid.New(), "nobody"); err != nil {
		t.Errorf("NoteRunPATRevokeFailed on an unknown row: %v", err)
	}
	same("after the notes", ids(store.RunPATFilter{}), a2, b1)

	// Closing keeps the last error beside the reason.
	if ok, err := st.MarkRunPATRevoked(ctx, runB, b1.AuthorizationID, "expired", "dead refresh token"); err != nil || !ok {
		t.Fatalf("MarkRunPATRevoked after a failed attempt = %v, %v; want true", ok, err)
	}
	same("after revokes", ids(store.RunPATFilter{}), a2)
}

func TestMemRunPATs(t *testing.T) { runPATContract(t, store.NewMemRunPATs()) }
