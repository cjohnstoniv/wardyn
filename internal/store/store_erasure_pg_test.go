// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Integration tests for the reads and writes a person's erasure makes beyond the
// stores that own a scope. Guarded by WARDYN_TEST_PG; skipped cleanly when unset.
package store_test

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

func erasureRun(t *testing.T, st store.PG, owner, task string) uuid.UUID {
	t.Helper()
	id := uuid.New()
	now := time.Now().UTC()
	if _, err := st.CreateRun(context.Background(), types.AgentRun{
		ID: id, CreatedAt: now, UpdatedAt: now, CreatedBy: owner, Agent: "claude-code", Task: task,
		ConfinementClass: types.CC2, State: types.RunStopped, SPIFFEID: "spiffe://wardyn.test/agent-run/" + id.String(), RunnerTarget: "docker",
	}); err != nil {
		t.Fatalf("create run: %v", err)
	}
	return id
}

// Blanking a person's tasks touches that person's runs and nothing else, and
// leaves updated_at alone: the idle reaper reads it.
func TestPG_PersonErasure_BlanksOnlyThatPersonsRunTasks(t *testing.T) {
	pool := runsPGPoolIsolated(t)
	st := store.NewPG(pool)
	ctx := context.Background()
	a1, a2 := erasureRun(t, st, "alice", "secret one"), erasureRun(t, st, "alice", "secret two")
	b1 := erasureRun(t, st, "bob", "bob's task")
	var before time.Time
	if err := pool.QueryRow(ctx, `SELECT updated_at FROM agent_runs WHERE id=$1`, a1).Scan(&before); err != nil {
		t.Fatal(err)
	}

	ids, err := st.RunIDsCreatedBy(ctx, "alice")
	if err != nil || len(ids) != 2 || !slices.Contains(ids, a1) || !slices.Contains(ids, a2) {
		t.Fatalf("RunIDsCreatedBy(alice) = %v, %v, want her two runs", ids, err)
	}
	if n, err := st.BlankRunTasks(ctx, "alice"); err != nil || n != 2 {
		t.Fatalf("BlankRunTasks = %d, %v, want 2", n, err)
	}
	if n, err := st.BlankRunTasks(ctx, "alice"); err != nil || n != 0 {
		t.Fatalf("a second BlankRunTasks = %d, %v, want nothing to change", n, err)
	}
	var task string
	var after time.Time
	if err := pool.QueryRow(ctx, `SELECT task, updated_at FROM agent_runs WHERE id=$1`, a1).Scan(&task, &after); err != nil || task != "" || !after.Equal(before) {
		t.Errorf("alice's run: task %q, updated_at moved %v (%v)", task, !after.Equal(before), err)
	}
	if err := pool.QueryRow(ctx, `SELECT task FROM agent_runs WHERE id=$1`, b1).Scan(&task); err != nil || task != "bob's task" {
		t.Errorf("bob's task = %q (%v), want it untouched", task, err)
	}
}

// The names an audit row carries resolve to the one principal every erasure
// destroys by; a name that resolves to none, or to two, is kept as it is.
func TestPG_PersonErasure_PrincipalForName(t *testing.T) {
	pool := runsPGPoolIsolated(t)
	st := store.NewPG(pool)
	ctx := context.Background()
	now := time.Now().UTC()
	oid := uuid.NewString()
	for _, in := range []store.LoginIdentity{
		{Principal: "entra:" + idTenant + ":" + oid, Issuer: idIssuer, TenantID: idTenant, ObjectID: oid, Email: "Pat@Corp.Example"},
		{Principal: "sub-a", Issuer: "https://dex.example", Email: "shared@corp.example"},
		{Principal: "sub-b", Issuer: "https://dex2.example", Email: "shared@corp.example"},
	} {
		if _, err := st.UpsertLoginIdentity(ctx, in, now); err != nil {
			t.Fatal(err)
		}
	}
	for name, want := range map[string]string{
		"entra:" + idTenant + ":" + oid: "entra:" + idTenant + ":" + oid, // already the principal
		"PAT@corp.example":              "entra:" + idTenant + ":" + oid, // an email alias, any case
		"sub-a":                         "sub-a",
		"shared@corp.example":           "shared@corp.example", // two people: kept as typed
		"nobody@corp.example":           "nobody@corp.example",
		"entra:" + idTenant + ":none":   "entra:" + idTenant + ":none",
		"":                              "",
	} {
		if got, err := st.PrincipalForName(ctx, name); err != nil || got != want {
			t.Errorf("PrincipalForName(%q) = %q, %v, want %q", name, got, err, want)
		}
	}
}

// An erasure destroys the keys of every name the directory holds for the
// person, so a row sealed under a name learned only later is shredded too.
func TestPG_PersonErasure_PrincipalAliases(t *testing.T) {
	pool := runsPGPoolIsolated(t)
	st := store.NewPG(pool)
	ctx := context.Background()
	oid := uuid.NewString()
	principal := "entra:" + idTenant + ":" + oid
	for _, email := range []string{"Pat@Corp.Example", "pat.new@corp.example"} {
		if _, err := st.UpsertLoginIdentity(ctx, store.LoginIdentity{Principal: principal, Issuer: idIssuer, TenantID: idTenant, ObjectID: oid, Email: email}, time.Now().UTC()); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := st.UpsertLoginIdentity(ctx, store.LoginIdentity{Principal: "other", Issuer: "https://dex.example", Email: "other@corp.example"}, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	got, err := st.PrincipalAliases(ctx, principal)
	want := []string{principal, "pat.new@corp.example", "pat@corp.example"}
	slices.Sort(got)
	if err != nil || !slices.Equal(got, want) {
		t.Errorf("PrincipalAliases = %v, %v, want %v", got, err, want)
	}
	if got, err := st.PrincipalAliases(ctx, "nobody"); err != nil || len(got) != 0 {
		t.Errorf("PrincipalAliases(nobody) = %v, %v, want none", got, err)
	}
}

func TestPG_PersonErasure_SubjectKeyDestroyedSince(t *testing.T) {
	pool := runsPGPoolIsolated(t)
	st := store.NewPG(pool)
	ctx := context.Background()
	const ins = `INSERT INTO principal_keys (owner, purpose, version, domain, kek_id, wrapped_key, destroyed_at) VALUES ($1, $2, $3, 'default', 'k', $4, $5)`
	gone := time.Now().Add(-time.Hour)
	if _, err := pool.Exec(ctx, ins, "gone", "audit-seal", 1, nil, gone); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, ins, "live", "audit-seal", 1, []byte{1}, nil); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		owner, purpose string
		since          time.Time
		want           bool
	}{
		{"gone", "audit-seal", gone.Add(-time.Minute), true},
		{"gone", "audit-seal", gone.Add(time.Minute), false},
		{"gone", "cred", gone.Add(-time.Minute), false},
		{"live", "audit-seal", gone.Add(-time.Minute), false},
		{"nobody", "audit-seal", gone.Add(-time.Minute), false},
	} {
		if got, err := st.SubjectKeyDestroyedSince(ctx, tc.owner, tc.purpose, tc.since); err != nil || got != tc.want {
			t.Errorf("SubjectKeyDestroyedSince(%s, %s, %v) = %v, %v, want %v", tc.owner, tc.purpose, tc.since, got, err, tc.want)
		}
	}
}
