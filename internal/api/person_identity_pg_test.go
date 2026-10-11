// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

// A person who never minted a token or created a workspace is still a person
// the identity directory knows, and a name the directory resolves to
// the caller is the caller. Guarded by WARDYN_TEST_PG.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"filippo.io/age"
	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/secretstore"
	secretspg "github.com/cjohnstoniv/wardyn/internal/secretstore/pg"
	"github.com/cjohnstoniv/wardyn/internal/secretstore/subjectkey"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
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

// An object-id person (a people row entra:<tenant>:<object>) who signed in under a pairwise subject
// keeps their credentials under the subject. Typing the entra form or the email must erase that
// namespace, not the people row's empty one.
func TestPG_ErasePersonCredentials_AnObjectIDPersonWhoSignedInIsTheirSubject(t *testing.T) {
	_, srv, pg, sec := identityRig(t)
	oid := uuid.NewString()
	entra := "entra:" + identTenant + ":" + oid
	subject := "pairwise-" + oid
	if _, _, err := pg.CreatePerson(t.Context(), types.Person{Principal: entra, Email: "kit-dir@corp.example", Issuer: identIssuer, TenantID: identTenant, ObjectID: oid, CreatedBy: "admin"}); err != nil {
		t.Fatal(err)
	}
	seedIdentity(t, pg, subject, oid, "kit-dir@corp.example")
	sa := ssoSession(t, "sec-1", "sec@corp.example", oidc.RoleSecurityAdmin)
	for _, typed := range []string{entra, "kit-dir@corp.example", subject} {
		if err := sec.For(subject).Put(t.Context(), "anthropic-api-key", []byte("kit-credential-00000000")); err != nil {
			t.Fatal(err)
		}
		w := doSSO(t, srv, http.MethodDelete, "/api/v1/people/"+typed+"/credentials", sa, "")
		if w.Code != http.StatusOK {
			t.Fatalf("erase by %q = %d %s, want 200", typed, w.Code, w.Body)
		}
		if got := namesOf(t, sec, subject); len(got) != 0 {
			t.Fatalf("erase by %q reported success and left %v under the subject", typed, got)
		}
	}
}

// The same person can hold credentials in two namespaces: the subject they sign in under, and the
// entra:<tenant>:<object> one an install upgraded from 0.8.9 may still hold. An erase by any name
// reaches both, and reports both in its count.
func TestPG_ErasePersonCredentials_ErasesEveryNamespaceOfThePerson(t *testing.T) {
	_, srv, pg, sec := identityRig(t)
	oid := uuid.NewString()
	entra := "entra:" + identTenant + ":" + oid
	subject := "pairwise-" + oid
	if _, _, err := pg.CreatePerson(t.Context(), types.Person{Principal: entra, Email: "kit-two@corp.example", Issuer: identIssuer, TenantID: identTenant, ObjectID: oid, CreatedBy: "admin"}); err != nil {
		t.Fatal(err)
	}
	seedIdentity(t, pg, subject, oid, "kit-two@corp.example")
	other := "other-" + oid
	if err := sec.For(other).Put(t.Context(), "anthropic-api-key", []byte("another-persons-credential")); err != nil {
		t.Fatal(err)
	}
	sa := ssoSession(t, "sec-1", "sec@corp.example", oidc.RoleSecurityAdmin)
	for _, typed := range []string{entra, "kit-two@corp.example", subject} {
		for _, owner := range []string{entra, subject} {
			if err := sec.For(owner).Put(t.Context(), "anthropic-api-key", []byte("kit-credential-00000000")); err != nil {
				t.Fatal(err)
			}
		}
		w := doSSO(t, srv, http.MethodDelete, "/api/v1/people/"+typed+"/credentials", sa, "")
		var body struct{ Count int }
		_ = json.Unmarshal(w.Body.Bytes(), &body)
		if w.Code != http.StatusOK || body.Count != 2 {
			t.Fatalf("erase by %q = %d %s, want 200 with count 2", typed, w.Code, w.Body)
		}
		for _, owner := range []string{entra, subject} {
			if got := namesOf(t, sec, owner); len(got) != 0 {
				t.Fatalf("erase by %q reported success and left %v under %q", typed, got, owner)
			}
		}
	}
	if got := namesOf(t, sec, other); len(got) != 1 {
		t.Fatalf("another person's namespace holds %v", got)
	}
}

// crossNameRig seeds a victim known two ways (a people row created by object id, and a person signed in
// under a pairwise subject) and an attacker whose email claims spell both victims' names. The login stores
// an email claim lowercased and never checks it for an @, so these are aliases the directory holds.
func crossNameRig(t *testing.T) (srv *Server, pg store.PG, sec secretstore.Store, attacker string, victims []string) {
	t.Helper()
	_, srv, pg, sec = identityRig(t)
	victimOid := uuid.NewString()
	victimEntra := "entra:" + identTenant + ":" + victimOid
	if _, _, err := pg.CreatePerson(t.Context(), types.Person{Principal: victimEntra, Email: "victim@corp.example", Issuer: identIssuer, TenantID: identTenant, ObjectID: victimOid, CreatedBy: "admin"}); err != nil {
		t.Fatal(err)
	}
	signedOid := uuid.NewString()
	victimSubject := "pairwise-" + signedOid
	seedIdentity(t, pg, victimSubject, signedOid, "signed@corp.example")
	attOid := uuid.NewString()
	attacker = "pairwise-" + attOid
	// A person an admin created by an email-shaped subject, not yet signed in: only people knows them.
	victimEarly := "early@corp.example"
	if _, _, err := pg.CreatePerson(t.Context(), types.Person{Principal: victimEarly, CreatedBy: "admin"}); err != nil {
		t.Fatal(err)
	}
	seedIdentity(t, pg, attacker, attOid, victimEntra)
	seedIdentity(t, pg, attacker, attOid, victimSubject)
	seedIdentity(t, pg, attacker, attOid, victimEarly)
	return srv, pg, sec, attacker, []string{victimEntra, victimSubject, victimEarly}
}

// Erasing a person an admin created under an email-shaped subject never reaches whoever signed in
// claiming that email: the name resolves to the created person, not to the alias holder.
func TestPG_EraseAuditSealKeys_ACreatedPersonsErasureNeverReachesTheAliasHolder(t *testing.T) {
	srv, pg, _, attacker, victims := crossNameRig(t)
	early := victims[2]
	for _, owner := range []string{attacker, early} {
		if _, _, err := srv.cfg.SubjectKeys.Current(t.Context(), owner, subjectkey.PurposeAuditSeal); err != nil {
			t.Fatal(err)
		}
	}
	before := time.Now().UTC().Add(-time.Minute)
	sa := ssoSession(t, "sec-1", "sec@corp.example", oidc.RoleSecurityAdmin)
	if w := doSSO(t, srv, http.MethodPost, "/api/v1/people/"+early+"/erasure", sa, erasureBody("audit_personal_fields")); w.Code != http.StatusOK {
		t.Fatalf("erasure = %d %s, want 200", w.Code, w.Body)
	}
	for owner, want := range map[string]bool{early: true, attacker: false} {
		if got, err := pg.SubjectKeyDestroyedSince(t.Context(), owner, subjectkey.PurposeAuditSeal, before); err != nil || got != want {
			t.Errorf("erasing %q: %q audit-seal key destroyed = %v (%v), want %v", early, owner, got, err, want)
		}
	}
}

// Erasing a person whose email alias spells another person's name never reaches that person's credential
// namespace, on either erase route: the namespaces come from the person's own tenant and object ids.
func TestPG_EraseCredentials_AnEmailAliasNeverNamesAnotherPersonsNamespace(t *testing.T) {
	for _, route := range []string{"delete", "erasure"} {
		t.Run(route, func(t *testing.T) {
			srv, _, sec, attacker, victims := crossNameRig(t)
			for _, owner := range append([]string{attacker}, victims...) {
				if err := sec.For(owner).Put(t.Context(), "anthropic-api-key", []byte("credential-of-"+owner)); err != nil {
					t.Fatal(err)
				}
			}
			sa := ssoSession(t, "sec-1", "sec@corp.example", oidc.RoleSecurityAdmin)
			var w *httptest.ResponseRecorder
			if route == "delete" {
				w = doSSO(t, srv, http.MethodDelete, "/api/v1/people/"+attacker+"/credentials", sa, "")
				var body struct{ Count int }
				_ = json.Unmarshal(w.Body.Bytes(), &body)
				if body.Count != 1 {
					t.Errorf("erase count = %d (%s), want 1: the attacker's own credential alone", body.Count, w.Body)
				}
			} else {
				w = doSSO(t, srv, http.MethodPost, "/api/v1/people/"+attacker+"/erasure", sa, erasureBody("credentials"))
			}
			if w.Code != http.StatusOK {
				t.Fatalf("erase = %d %s, want 200", w.Code, w.Body)
			}
			if got := namesOf(t, sec, attacker); len(got) != 0 {
				t.Fatalf("the attacker's own namespace still holds %v", got)
			}
			for _, v := range victims {
				if got := namesOf(t, sec, v); len(got) != 1 {
					t.Errorf("erasing %q left %v under another person's namespace %q, want it untouched", attacker, got, v)
				}
			}
		})
	}
}

// The audit_personal_fields scope destroys an alias's key only when the directory resolves that alias to
// the person being erased: an email alias that spells another person's name never reaches their key.
func TestPG_EraseAuditSealKeys_AnEmailAliasNeverReachesAnotherPersonsKey(t *testing.T) {
	srv, pg, _, attacker, victims := crossNameRig(t)
	for _, owner := range append([]string{attacker}, victims...) {
		if _, _, err := srv.cfg.SubjectKeys.Current(t.Context(), owner, subjectkey.PurposeAuditSeal); err != nil {
			t.Fatal(err)
		}
	}
	before := time.Now().UTC().Add(-time.Minute)
	sa := ssoSession(t, "sec-1", "sec@corp.example", oidc.RoleSecurityAdmin)
	if w := doSSO(t, srv, http.MethodPost, "/api/v1/people/"+attacker+"/erasure", sa, erasureBody("audit_personal_fields")); w.Code != http.StatusOK {
		t.Fatalf("erasure = %d %s, want 200", w.Code, w.Body)
	}
	gone := func(owner string) bool {
		g, err := pg.SubjectKeyDestroyedSince(t.Context(), owner, subjectkey.PurposeAuditSeal, before)
		if err != nil {
			t.Fatal(err)
		}
		return g
	}
	if !gone(attacker) {
		t.Fatal("the attacker's own audit-seal key survived the erasure")
	}
	for _, v := range victims {
		if gone(v) {
			t.Errorf("erasing %q destroyed another person's audit-seal key %q", attacker, v)
		}
	}
}
