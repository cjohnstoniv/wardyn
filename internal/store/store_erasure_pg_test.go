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
// destroys by; a name that resolves to none, or to two, is kept as it is. A
// person an admin created is that name's owner, whoever signs in claiming it.
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
		{Principal: "sub-c", Issuer: "https://dex.example", Email: "early@corp.example"},
	} {
		if _, err := st.UpsertLoginIdentity(ctx, in, now); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, err := st.CreatePerson(ctx, types.Person{Principal: "early@corp.example", CreatedBy: "admin"}); err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]string{
		"early@corp.example":            "early@corp.example",            // a created person, not whoever claims the email
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

// The identity directory names who a typed value is, for a person with no token
// or workspace: an exact principal, the entra form, an email in any case (from a
// sign-in alias or a pre-created person); two people sharing an email are both
// returned, and a stranger is none.
func TestPG_PersonErasure_PrincipalsNamed(t *testing.T) {
	pool := runsPGPoolIsolated(t)
	st := store.NewPG(pool)
	ctx := context.Background()
	oid := uuid.NewString()
	for _, in := range []store.LoginIdentity{
		{Principal: "pairwise-sub", Issuer: idIssuer, TenantID: idTenant, ObjectID: oid, Email: "Pat@Corp.Example"},
		{Principal: "a@b.example", Issuer: "https://dex.example"},
		{Principal: "sub-a", Issuer: "https://dex.example", Email: "shared@corp.example"},
		{Principal: "sub-b", Issuer: "https://dex2.example", Email: "shared@corp.example"},
	} {
		if _, err := st.UpsertLoginIdentity(ctx, in, time.Now().UTC()); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, err := st.CreatePerson(ctx, types.Person{Principal: "precreated", Email: "Kit@Corp.Example", CreatedBy: "admin"}); err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string][]string{
		"pairwise-sub":                       {"pairwise-sub"},
		"a@b.example":                        {"a@b.example"}, // an email-shaped subject is its own principal
		"entra:" + idTenant + ":" + oid:      {"pairwise-sub"},
		"PAT@corp.example":                   {"pairwise-sub"},
		"kit@corp.example":                   {"precreated"},
		"precreated":                         {"precreated"},
		"shared@corp.example":                {"sub-a", "sub-b"},
		"nobody@corp.example":                nil,
		"entra:" + idTenant + ":no-such-oid": nil,
		"":                                   nil,
	} {
		got, err := st.PrincipalsNamed(ctx, name)
		if err != nil || !slices.Equal(got, want) {
			t.Errorf("PrincipalsNamed(%q) = %v, %v, want %v", name, got, err, want)
		}
	}
}

// Every place a principal is recorded answers, and a principal always wins over an email claim that
// spells it: a person an admin created, an API token's or a workspace owner's principal, exact or
// case-folded, beats a sign-in alias, a token's email or a SCIM email naming someone else; an email is
// matched only for a name with an @; an email two people carry is both.
func TestPG_PersonErasure_PrincipalsNamedPrefersAPrincipalToAnyEmailClaim(t *testing.T) {
	pool := runsPGPoolIsolated(t)
	st := store.NewPG(pool)
	ctx := context.Background()
	now := time.Now().UTC()
	if _, _, err := st.CreatePerson(ctx, types.Person{Principal: "early@corp.example", CreatedBy: "admin"}); err != nil {
		t.Fatal(err)
	}
	oid := uuid.NewString()
	for _, claim := range []string{"early@corp.example", "tok-only", "ws-only", "shared@corp.example"} {
		if _, err := st.UpsertLoginIdentity(ctx, store.LoginIdentity{Principal: "claimant", Issuer: idIssuer, TenantID: idTenant, ObjectID: oid, Email: claim}, now); err != nil {
			t.Fatal(err)
		}
		if _, err := st.CreateAPIToken(ctx, types.APIToken{ID: uuid.New(), Principal: "claimant", Email: claim, Role: "user", UserType: types.UserTypeStandard, Name: "t"}, uuid.NewString()); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, err := st.CreateScimIdentity(ctx, idIssuer, idTenant, oid, store.IdentityUpdate{Emails: []string{"scim@corp.example"}}, now); err != nil {
		t.Fatal(err)
	}
	if _, err := st.UpsertLoginIdentity(ctx, store.LoginIdentity{Principal: "holder", Issuer: "https://dex.example", Email: "shared@corp.example"}, now); err != nil {
		t.Fatal(err)
	}
	// Two subjects that differ only by case are legal: the exact one wins wherever it is recorded.
	for _, p := range []string{"casetok", "casews"} {
		if _, err := st.UpsertLoginIdentity(ctx, store.LoginIdentity{Principal: p, Issuer: "https://dex.example"}, now); err != nil {
			t.Fatal(err)
		}
	}
	for p, email := range map[string]string{"tok-only": "tok@corp.example", "CaseTok": ""} {
		if _, err := st.CreateAPIToken(ctx, types.APIToken{ID: uuid.New(), Principal: p, Email: email, Role: "user", UserType: types.UserTypeStandard, Name: "t"}, uuid.NewString()); err != nil {
			t.Fatal(err)
		}
	}
	for _, owner := range []string{"ws-only", "CaseWs"} {
		ws, err := st.CreateWorkspace(ctx, types.Workspace{ID: uuid.New(), Name: "ws-" + uuid.NewString(), OwnedBy: owner,
			Sources: []types.WorkspaceSource{{Type: types.WorkspaceSourceTypeEphemeral, Target: "/workspace"}}, Status: types.WorkspaceScanned, CreatedAt: now, UpdatedAt: now})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = st.DeleteWorkspace(context.Background(), ws.ID) })
	}
	for name, want := range map[string][]string{
		"early@corp.example":  {"early@corp.example"}, // a created person, not the claimant
		"EARLY@Corp.Example":  {"early@corp.example"}, // in any case
		"tok-only":            {"tok-only"},           // a token's principal, which the claimant's email spells
		"TOK-ONLY":            {"tok-only"},
		"ws-only":             {"ws-only"},            // a workspace owner's principal
		"tok@corp.example":    {"tok-only"},           // a token's email names its holder
		"scim@corp.example":   {"claimant"},           // a SCIM email names its identity
		"shared@corp.example": {"claimant", "holder"}, // two people carry it
		"claimant":            {"claimant"},
		"CaseTok":             {"CaseTok"}, // exact, from a token, over the case-folded "casetok"
		"CaseWs":              {"CaseWs"},  // exact, from a workspace owner
		"CASETOK":             {"CaseTok", "casetok"},
	} {
		got, err := st.PrincipalsNamed(ctx, name)
		slices.Sort(got) // the database's collation orders case variants its own way
		if err != nil || !slices.Equal(got, want) {
			t.Errorf("PrincipalsNamed(%q) = %v, %v, want %v", name, got, err, want)
		}
	}
}

// A person an admin created by object id (people row entra:<tenant>:<object>) who signed in under a
// pairwise subject is that subject, whether the admin types the entra form, the email or the subject:
// the people row's own namespace is empty, so resolving to it would erase nothing and report success.
func TestPG_PersonErasure_PrincipalsNamedFollowsTheSignIn(t *testing.T) {
	pool := runsPGPoolIsolated(t)
	st := store.NewPG(pool)
	ctx := context.Background()
	oid, other := uuid.NewString(), uuid.NewString()
	entra := "entra:" + idTenant + ":" + oid
	for _, p := range []types.Person{
		{Principal: entra, Email: "Kit@Corp.Example", Issuer: idIssuer, TenantID: idTenant, ObjectID: oid, CreatedBy: "admin"},
		{Principal: "entra:" + idTenant + ":" + other, Email: "never@corp.example", Issuer: idIssuer, TenantID: idTenant, ObjectID: other, CreatedBy: "admin"},
	} {
		if _, _, err := st.CreatePerson(ctx, p); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := st.UpsertLoginIdentity(ctx, store.LoginIdentity{Principal: "pairwise-kit", Issuer: idIssuer, TenantID: idTenant, ObjectID: oid, Email: "kit@corp.example"}, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string][]string{
		entra:                             {"pairwise-kit"},
		"kit@corp.example":                {"pairwise-kit"},
		"pairwise-kit":                    {"pairwise-kit"},
		"entra:" + idTenant + ":" + other: {"entra:" + idTenant + ":" + other}, // never signed in: still the people row
		"never@corp.example":              {"entra:" + idTenant + ":" + other},
	} {
		got, err := st.PrincipalsNamed(ctx, name)
		if err != nil || !slices.Equal(got, want) {
			t.Errorf("PrincipalsNamed(%q) = %v, %v, want %v", name, got, err, want)
		}
	}
}

// An admin-created person under one issuer and a sign-in under another with the same tenant and object id
// are not linked (the sign-in's own link needs the same issuer), so the typed entra form must not resolve
// to the empty people namespace alone: it names both, which an erase refuses as ambiguous.
func TestPG_PersonErasure_PrincipalsNamedEntraFormIsAmbiguousAcrossIssuers(t *testing.T) {
	pool := runsPGPoolIsolated(t)
	st := store.NewPG(pool)
	ctx := context.Background()
	oid := uuid.NewString()
	entra := "entra:" + idTenant + ":" + oid
	if _, _, err := st.CreatePerson(ctx, types.Person{Principal: entra, Email: "kit@corp.example", Issuer: idIssuer, TenantID: idTenant, ObjectID: oid, CreatedBy: "admin"}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.UpsertLoginIdentity(ctx, store.LoginIdentity{Principal: "pairwise-kit", Issuer: "https://other-issuer.example", TenantID: idTenant, ObjectID: oid}, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	want := []string{entra, "pairwise-kit"}
	if got, err := st.PrincipalsNamed(ctx, entra); err != nil || !slices.Equal(got, want) {
		t.Errorf("PrincipalsNamed(%q) = %v, %v, want both %v", entra, got, err, want)
	}
	if got, err := st.PrincipalForName(ctx, entra); err != nil || got != entra {
		t.Errorf("PrincipalForName(%q) = %q, %v, want the name kept as it is", entra, got, err)
	}
	if got, err := st.PrincipalsNamed(ctx, "pairwise-kit"); err != nil || !slices.Equal(got, []string{"pairwise-kit"}) {
		t.Errorf("PrincipalsNamed(pairwise-kit) = %v, %v, want itself", got, err)
	}
}

// An identity bound to entra:<tenant>:<object> is a principal, so the typed name is it alone: a later
// sign-in of the same tenant and object id under another issuer does not make the canonical name
// ambiguous. A people row that a sign-in of its own issuer followed is the sign-in alone as well. Only an
// unlinked people row with an empty namespace (the case above) reads the other issuer's sign-ins.
func TestPG_PersonErasure_PrincipalsNamedBoundEntraFormStaysAlone(t *testing.T) {
	pool := runsPGPoolIsolated(t)
	st := store.NewPG(pool)
	ctx := context.Background()
	otherIssuer := "https://other-issuer.example"
	boundOID, linkedOID, unlinkedOID := uuid.NewString(), uuid.NewString(), uuid.NewString()
	bound, linked, unlinked := "entra:"+idTenant+":"+boundOID, "entra:"+idTenant+":"+linkedOID, "entra:"+idTenant+":"+unlinkedOID
	for _, p := range []types.Person{
		{Principal: linked, Email: "linked@corp.example", Issuer: idIssuer, TenantID: idTenant, ObjectID: linkedOID, CreatedBy: "admin"},
		{Principal: unlinked, Email: "unlinked@corp.example", Issuer: idIssuer, TenantID: idTenant, ObjectID: unlinkedOID, CreatedBy: "admin"},
	} {
		if _, _, err := st.CreatePerson(ctx, p); err != nil {
			t.Fatal(err)
		}
	}
	for _, in := range []store.LoginIdentity{
		{Principal: bound, Issuer: idIssuer, TenantID: idTenant, ObjectID: boundOID},
		{Principal: "pairwise-bound-y", Issuer: otherIssuer, TenantID: idTenant, ObjectID: boundOID},
		{Principal: "pairwise-linked", Issuer: idIssuer, TenantID: idTenant, ObjectID: linkedOID},
		{Principal: "pairwise-linked-y", Issuer: otherIssuer, TenantID: idTenant, ObjectID: linkedOID},
		{Principal: "pairwise-unlinked-y", Issuer: otherIssuer, TenantID: idTenant, ObjectID: unlinkedOID},
	} {
		if _, err := st.UpsertLoginIdentity(ctx, in, time.Now().UTC()); err != nil {
			t.Fatal(err)
		}
	}
	for name, want := range map[string][]string{
		bound:               {bound},
		linked:              {"pairwise-linked"},
		unlinked:            {unlinked, "pairwise-unlinked-y"}, // unchanged: still refused as ambiguous
		"pairwise-bound-y":  {"pairwise-bound-y"},
		"pairwise-linked-y": {"pairwise-linked-y"},
	} {
		if got, err := st.PrincipalsNamed(ctx, name); err != nil || !slices.Equal(got, want) {
			t.Errorf("PrincipalsNamed(%q) = %v, %v, want %v", name, got, err, want)
		}
	}
	// The audit sealer keys a row by the same answer: the bound name is its own principal.
	if got, err := st.PrincipalForName(ctx, bound); err != nil || got != bound {
		t.Errorf("PrincipalForName(%q) = %q, %v, want itself", bound, got, err)
	}
	if got, err := st.PrincipalForName(ctx, linked); err != nil || got != "pairwise-linked" {
		t.Errorf("PrincipalForName(%q) = %q, %v, want the sign-in its issuer linked", linked, got, err)
	}
	if got, err := st.PrincipalForName(ctx, unlinked); err != nil || got != unlinked {
		t.Errorf("PrincipalForName(%q) = %q, %v, want the ambiguous name kept as it is", unlinked, got, err)
	}
}

// A people row keyed by subject (no issuer, tenant or object id) is not the sign-in of whoever else has
// none of those either: only an object-id row follows a sign-in.
func TestPG_PersonErasure_PrincipalsNamedKeepsASubjectKeyedPerson(t *testing.T) {
	pool := runsPGPoolIsolated(t)
	st := store.NewPG(pool)
	ctx := context.Background()
	if _, _, err := st.CreatePerson(ctx, types.Person{Principal: "sub-keyed-person", Email: "subkeyed@corp.example", CreatedBy: "admin"}); err != nil {
		t.Fatal(err)
	}
	// An identity row with no issuer, tenant or object id is not one a sign-in writes; it is what a
	// join on those three columns alone would take for the subject-keyed person's own.
	if _, err := pool.Exec(ctx, `INSERT INTO principal_identities (principal, issuer, email_lower) VALUES ('someone-else', '', 'else@corp.example')`); err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string][]string{
		"sub-keyed-person":      {"sub-keyed-person"},
		"subkeyed@corp.example": {"sub-keyed-person"},
		"someone-else":          {"someone-else"},
	} {
		got, err := st.PrincipalsNamed(ctx, name)
		if err != nil || !slices.Equal(got, want) {
			t.Errorf("PrincipalsNamed(%q) = %v, %v, want %v", name, got, err, want)
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

// The entra names come from the person's own tenant and object ids, never from an email claim that
// spells another person's entra:<tenant>:<object> form.
func TestPG_PersonErasure_PrincipalEntraNamesIgnoreAliasValues(t *testing.T) {
	pool := runsPGPoolIsolated(t)
	st := store.NewPG(pool)
	ctx := context.Background()
	oid := uuid.NewString()
	principal := "pairwise-" + oid
	crafted := "entra:" + idTenant + ":" + uuid.NewString()
	if _, err := st.UpsertLoginIdentity(ctx, store.LoginIdentity{Principal: principal, Issuer: idIssuer, TenantID: idTenant, ObjectID: oid, Email: crafted}, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if aliases, err := st.PrincipalAliases(ctx, principal); err != nil || !slices.Contains(aliases, crafted) {
		t.Fatalf("PrincipalAliases = %v, %v, want the crafted email among them", aliases, err)
	}
	got, err := st.PrincipalEntraNames(ctx, principal)
	if want := []string{"entra:" + idTenant + ":" + oid}; err != nil || !slices.Equal(got, want) {
		t.Errorf("PrincipalEntraNames = %v, %v, want %v", got, err, want)
	}
	if got, err := st.PrincipalEntraNames(ctx, "nobody"); err != nil || len(got) != 0 {
		t.Errorf("PrincipalEntraNames(nobody) = %v, %v, want none", got, err)
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

// A stored sign-in address is found by any casing of it through the alias
// tier alone: the identity's current email is another address, so neither the
// principal tier nor the email column can answer.
func TestPG_PersonErasure_PrincipalsNamedAliasTierFoldsCase(t *testing.T) {
	pool := runsPGPoolIsolated(t)
	st := store.NewPG(pool)
	ctx := context.Background()
	for _, email := range []string{"Alias@Example.ORG", "current@corp.example"} {
		if _, err := st.UpsertLoginIdentity(ctx, store.LoginIdentity{Principal: "sub-alias", Issuer: "https://dex.example", Email: email}, time.Now().UTC()); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{"alias@example.org", "ALIAS@Example.Org", "Alias@Example.ORG"} {
		if got, err := st.PrincipalsNamed(ctx, name); err != nil || !slices.Equal(got, []string{"sub-alias"}) {
			t.Errorf("PrincipalsNamed(%q) = %v, %v, want [sub-alias]", name, got, err)
		}
	}
}

// A people row follows only a sign-in of its own tenant: the same issuer and
// object id under another tenant is a different person's identity.
func TestPG_PersonErasure_PrincipalsNamedIgnoresASignInOfAnotherTenant(t *testing.T) {
	pool := runsPGPoolIsolated(t)
	st := store.NewPG(pool)
	ctx := context.Background()
	oid := uuid.NewString()
	entra := "entra:" + idTenant + ":" + oid
	if _, _, err := st.CreatePerson(ctx, types.Person{Principal: entra, Email: "kit@corp.example", Issuer: idIssuer, TenantID: idTenant, ObjectID: oid, CreatedBy: "admin"}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.UpsertLoginIdentity(ctx, store.LoginIdentity{Principal: "pairwise-elsewhere", Issuer: idIssuer, TenantID: uuid.NewString(), ObjectID: oid}, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if got, err := st.PrincipalsNamed(ctx, entra); err != nil || !slices.Equal(got, []string{entra}) {
		t.Errorf("PrincipalsNamed(%q) = %v, %v, want the people row alone", entra, got, err)
	}
}
