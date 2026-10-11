// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

// A person who never minted a token or created a workspace is still a person
// the identity directory knows, and a name the directory resolves to
// the caller is the caller. Guarded by WARDYN_TEST_PG.

import (
	"net/http"
	"testing"
	"time"

	"filippo.io/age"
	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/secretstore"
	secretspg "github.com/cjohnstoniv/wardyn/internal/secretstore/pg"
	"github.com/cjohnstoniv/wardyn/internal/store"
)

const (
	identTenant = "11111111-1111-1111-1111-111111111111"
	identIssuer = "https://login.microsoftonline.com/11111111-1111-1111-1111-111111111111/v2.0"
)

// identityRig is a server over a real store and secret store, with audit
// sealing keys on, so person erasure reaches the identity directory.
func identityRig(t *testing.T) (*harness, *Server, store.PG, secretstore.Store) {
	t.Helper()
	pool := throwawayPGPool(t)
	id, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	sec, err := secretspg.New(pool, id)
	if err != nil {
		t.Fatal(err)
	}
	pg := store.NewPG(pool)
	h := newHarness(t)
	cfg := baseTestConfig(h, pg)
	cfg.OIDC = &oidc.Authenticator{}
	cfg.Secrets = sec
	cfg.SubjectKeys = sec.SubjectKeys()
	return h, New(cfg), pg, sec
}

func seedIdentity(t *testing.T, pg store.PG, principal, oid, email string) {
	t.Helper()
	in := store.LoginIdentity{Principal: principal, Issuer: identIssuer, TenantID: identTenant, ObjectID: oid, Email: email}
	if _, err := pg.UpsertLoginIdentity(t.Context(), in, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
}

// A security admin signed in under a pairwise subject names their own
// entra:<tenant>:<object> form. The audit step resolves that name to their
// principal, so the self check must see the same person.
func TestPG_ErasePerson_RefusesTheCallersOwnEntraAlias(t *testing.T) {
	h, srv, pg, _ := identityRig(t)
	oid := uuid.NewString()
	seedIdentity(t, pg, "pairwise-"+oid, oid, "sec-self@corp.example")
	sa := ssoSession(t, "pairwise-"+oid, "sec-self@corp.example", oidc.RoleSecurityAdmin)
	other := ssoSession(t, "sec-other", "sec-other@corp.example", oidc.RoleSecurityAdmin)
	body := erasureBody("audit_personal_fields")

	for _, name := range []string{"entra:" + identTenant + ":" + oid, "sec-self@corp.example", "SEC-SELF@corp.example", "pairwise-" + oid} {
		w := doSSO(t, srv, http.MethodPost, "/api/v1/people/"+name+"/erasure", sa, body)
		if w.Code != http.StatusForbidden || errorReason(w) != reasonErasureSelfRefused {
			t.Errorf("self erasure by %q = %d %s, want 403 %s", name, w.Code, w.Body, reasonErasureSelfRefused)
		}
	}
	for _, ev := range erasureRows(h) {
		if ev.Outcome == "success" {
			t.Fatalf("a refused self erasure left a success row: %+v", ev)
		}
	}
	// Another security admin may erase that person by the same name.
	w := doSSO(t, srv, http.MethodPost, "/api/v1/people/entra:"+identTenant+":"+oid+"/erasure", other, body)
	if w.Code != http.StatusOK {
		t.Fatalf("a second security admin erasing the alias = %d %s, want 200", w.Code, w.Body)
	}
}

// The erasure route: Alice signed in and stored a credential but
// never minted a token or created a workspace. The erase by her email reaches
// her namespace, and two people sharing an email stay ambiguous.
func TestPG_ErasePersonCredentials_ResolvesAnIdentityWithNoTokenOrWorkspace(t *testing.T) {
	_, srv, pg, sec := identityRig(t)
	alice := "alice-" + uuid.NewString()
	seedIdentity(t, pg, alice, "", "alice-dir@corp.example")
	seedIdentity(t, pg, "twin-a-"+alice, "", "twin-dir@corp.example")
	seedIdentity(t, pg, "twin-b-"+alice, "", "twin-dir@corp.example")
	if err := sec.For(alice).Put(t.Context(), "anthropic-api-key", []byte("alice-credential-0000000")); err != nil {
		t.Fatal(err)
	}
	sa := ssoSession(t, "sec-1", "sec@corp.example", oidc.RoleSecurityAdmin)

	w := doSSO(t, srv, http.MethodDelete, "/api/v1/people/twin-dir@corp.example/credentials", sa, "")
	if w.Code != http.StatusUnprocessableEntity || errorReason(w) != reasonOwnerAmbiguous {
		t.Fatalf("an email two identities share = %d %s, want 422 %s", w.Code, w.Body, reasonOwnerAmbiguous)
	}
	w = doSSO(t, srv, http.MethodDelete, "/api/v1/people/ALICE-DIR@corp.example/credentials", sa, "")
	if w.Code != http.StatusOK {
		t.Fatalf("erase by the email of a person with no token or workspace = %d %s, want 200", w.Code, w.Body)
	}
	if got := namesOf(t, sec, alice); len(got) != 0 {
		t.Fatalf("alice still holds %v", got)
	}
	w = doSSO(t, srv, http.MethodDelete, "/api/v1/people/nobody-dir@corp.example/credentials", sa, "")
	if w.Code != http.StatusUnprocessableEntity || errorReason(w) != reasonOwnerUnresolved {
		t.Fatalf("an email nobody holds = %d %s, want 422 %s", w.Code, w.Body, reasonOwnerUnresolved)
	}
}

// The key-domain route: a user assignment by email resolves through
// the same directory.
func TestPG_KeyDomainUserAssignment_ResolvesAnIdentityWithNoTokenOrWorkspace(t *testing.T) {
	_, srv, pg, _ := identityRig(t)
	bob := "bob-" + uuid.NewString()
	seedIdentity(t, pg, bob, "", "bob-dir@corp.example")
	got, ok, w := decodeVia(t, srv, false, "/user/Bob-Dir@corp.example", `{"domain":"vault"}`)
	if !ok || got.Subject != bob || got.SubjectType != "user" {
		t.Fatalf("ok=%v change=%+v (%d %s), want the subject %q", ok, got, w.Code, w.Body, bob)
	}
	_, ok, w = decodeVia(t, srv, false, "/user/nobody-dir@corp.example", `{"domain":"vault"}`)
	if ok || w.Code != http.StatusUnprocessableEntity || reasonOf(t, w) != reasonOwnerUnresolved {
		t.Fatalf("an email nobody holds: ok=%v %d %s, want 422 %s", ok, w.Code, w.Body, reasonOwnerUnresolved)
	}
}
