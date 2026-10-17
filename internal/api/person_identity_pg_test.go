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
	"net/url"
	"strings"
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

// crossNameRig seeds victims known every way a person is (a people row created by object id, a person
// signed in under a pairwise subject, a person an admin created under an email-shaped subject, another
// such person) and an attacker whose claims spell every victim's name: sign-in email claims (the login
// stores one lowercased and never checks it for an @), the email an API token stamps from the sign-in
// claim, and an email a SCIM push recorded. The attacker also claims the signed-in victim's own email,
// which leaves that email shared.
func crossNameRig(t *testing.T) (srv *Server, pg store.PG, sec secretstore.Store, attacker string, victims []string) {
	t.Helper()
	_, srv, pg, sec = identityRig(t)
	ctx := t.Context()
	victimOid := uuid.NewString()
	victimEntra := "entra:" + identTenant + ":" + victimOid
	if _, _, err := pg.CreatePerson(ctx, types.Person{Principal: victimEntra, Email: "victim@corp.example", Issuer: identIssuer, TenantID: identTenant, ObjectID: victimOid, CreatedBy: "admin"}); err != nil {
		t.Fatal(err)
	}
	signedOid := uuid.NewString()
	victimSubject := "pairwise-" + signedOid
	seedIdentity(t, pg, victimSubject, signedOid, crossSharedEmail)
	attOid := uuid.NewString()
	attacker = "pairwise-" + attOid
	// People an admin created by an email-shaped subject, not yet signed in: only people knows them.
	victimEarly, victimSCIM := "early@corp.example", "scim-victim@corp.example"
	for _, p := range []string{victimEarly, victimSCIM} {
		if _, _, err := pg.CreatePerson(ctx, types.Person{Principal: p, CreatedBy: "admin"}); err != nil {
			t.Fatal(err)
		}
	}
	for _, claim := range []string{victimEntra, victimSubject, victimEarly, crossSharedEmail} {
		seedIdentity(t, pg, attacker, attOid, claim)
		if _, err := pg.CreateAPIToken(ctx, types.APIToken{ID: uuid.New(), Principal: attacker, Email: claim, Role: oidc.RoleUser, UserType: types.UserTypeStandard, Name: "t"}, uuid.NewString()); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, err := pg.CreateScimIdentity(ctx, identIssuer, identTenant, attOid, store.IdentityUpdate{Emails: []string{victimSCIM}}, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	return srv, pg, sec, attacker, []string{victimEntra, victimSubject, victimEarly, victimSCIM}
}

// crossSharedEmail is the signed-in victim's email, which the attacker also claims.
const crossSharedEmail = "signed@corp.example"

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

// crossRoute is one route that erases or deletes a person's records by a name an admin types, with the
// record it removes: seed gives owner one, gone reports whether owner's is gone.
type crossRoute struct {
	name string
	call func(t *testing.T, srv *Server, typed string) *httptest.ResponseRecorder
	ok   int
	seed func(t *testing.T, srv *Server, pg store.PG, sec secretstore.Store, owner string)
	gone func(t *testing.T, srv *Server, pg store.PG, sec secretstore.Store, owner string, since time.Time) bool
	// ownerFirst: the route takes a name that holds its records exactly before it resolves one, so only a
	// name that holds none reaches the resolver.
	ownerFirst bool
}

func crossRoutes() []crossRoute {
	sa := func(t *testing.T) *http.Cookie {
		return ssoSession(t, "sec-1", "sec@corp.example", oidc.RoleSecurityAdmin)
	}
	erasure := func(scope string) func(*testing.T, *Server, string) *httptest.ResponseRecorder {
		return func(t *testing.T, srv *Server, typed string) *httptest.ResponseRecorder {
			return doSSO(t, srv, http.MethodPost, "/api/v1/people/"+typed+"/erasure", sa(t), erasureBody(scope))
		}
	}
	putCred := func(t *testing.T, _ *Server, _ store.PG, sec secretstore.Store, owner string) {
		if err := sec.For(owner).Put(t.Context(), "anthropic-api-key", []byte("credential-of-"+owner)); err != nil {
			t.Fatal(err)
		}
	}
	credGone := func(t *testing.T, _ *Server, _ store.PG, sec secretstore.Store, owner string, _ time.Time) bool {
		return len(namesOf(t, sec, owner)) == 0
	}
	return []crossRoute{
		{name: "DELETE credentials", ok: http.StatusOK, seed: putCred, gone: credGone,
			call: func(t *testing.T, srv *Server, typed string) *httptest.ResponseRecorder {
				return doSSO(t, srv, http.MethodDelete, "/api/v1/people/"+typed+"/credentials", sa(t), "")
			}},
		{name: "erasure credentials", ok: http.StatusOK, seed: putCred, gone: credGone, call: erasure("credentials")},
		{name: "DELETE secret ?owner=", ok: http.StatusNoContent, seed: putCred, gone: credGone,
			call: func(t *testing.T, srv *Server, typed string) *httptest.ResponseRecorder {
				admin := ssoSession(t, "admin-1", "admin@corp.example", oidc.RoleAdmin)
				return doSSO(t, srv, http.MethodDelete, "/api/v1/secrets/anthropic-api-key?owner="+url.QueryEscape(typed), admin, "")
			}},
		{name: "erasure audit_personal_fields", ok: http.StatusOK, call: erasure("audit_personal_fields"),
			seed: func(t *testing.T, srv *Server, _ store.PG, _ secretstore.Store, owner string) {
				if _, _, err := srv.cfg.SubjectKeys.Current(t.Context(), owner, subjectkey.PurposeAuditSeal); err != nil {
					t.Fatal(err)
				}
			},
			gone: func(t *testing.T, _ *Server, pg store.PG, _ secretstore.Store, owner string, since time.Time) bool {
				g, err := pg.SubjectKeyDestroyedSince(t.Context(), owner, subjectkey.PurposeAuditSeal, since)
				if err != nil {
					t.Fatal(err)
				}
				return g
			}},
		{name: "erasure run_tasks", ok: http.StatusOK, call: erasure("run_tasks"),
			seed: func(t *testing.T, _ *Server, pg store.PG, _ secretstore.Store, owner string) {
				now, id := time.Now().UTC(), uuid.New()
				if _, err := pg.CreateRun(t.Context(), types.AgentRun{ID: id, CreatedAt: now, UpdatedAt: now, CreatedBy: owner, Agent: "claude-code",
					Task: "task of " + owner, ConfinementClass: types.CC2, State: types.RunStopped, SPIFFEID: "spiffe://wardyn.test/agent-run/" + id.String(), RunnerTarget: "docker"}); err != nil {
					t.Fatal(err)
				}
			},
			gone: func(t *testing.T, _ *Server, pg store.PG, _ secretstore.Store, owner string, _ time.Time) bool {
				var n int
				if err := pg.Pool.QueryRow(t.Context(), `SELECT count(*) FROM agent_runs WHERE created_by = $1 AND task <> ''`, owner).Scan(&n); err != nil {
					t.Fatal(err)
				}
				return n == 0
			}},
		{name: "DELETE ssh-keys", ok: http.StatusOK, ownerFirst: true,
			call: func(t *testing.T, srv *Server, typed string) *httptest.ResponseRecorder {
				return doSSO(t, srv, http.MethodDelete, "/api/v1/people/"+typed+"/ssh-keys", sa(t), "")
			},
			seed: func(t *testing.T, _ *Server, pg store.PG, _ secretstore.Store, owner string) {
				if _, err := pg.AddSSHKey(t.Context(), types.SSHPublicKey{Fingerprint: "SHA256:" + uuid.NewString(), Principal: owner, Name: "k",
					PublicKey: "ssh-ed25519 AAAA " + owner, Role: oidc.RoleUser, CreatedAt: time.Now().UTC()}); err != nil {
					t.Fatal(err)
				}
			},
			gone: func(t *testing.T, _ *Server, pg store.PG, _ secretstore.Store, owner string, _ time.Time) bool {
				keys, err := pg.ListSSHKeysByPrincipal(t.Context(), owner)
				if err != nil {
					t.Fatal(err)
				}
				return len(keys) == 0
			}},
	}
}

// dbNow is the database's clock. A key's destroyed_at is stamped by it, so an instant compared against
// destroyed_at is read from it too: the app's clock can sit behind the database's under load or a step.
func dbNow(t *testing.T, pg store.PG) time.Time {
	t.Helper()
	var at time.Time
	if err := pg.Pool.QueryRow(t.Context(), `SELECT clock_timestamp()`).Scan(&at); err != nil {
		t.Fatalf("read the database clock: %v", err)
	}
	return at.UTC()
}

// Every route that erases or deletes by a typed name resolves it once, the same way: a victim's name,
// however the attacker's claims spell it (a sign-in email, an API token's email, a SCIM email, in any
// case), reaches the victim and never the attacker, and an email both claim is refused, not guessed.
func TestPG_ErasureRoutes_AVictimsNameNeverReachesTheClaimant(t *testing.T) {
	for _, rt := range crossRoutes() {
		t.Run(rt.name, func(t *testing.T) {
			srv, pg, sec, attacker, victims := crossNameRig(t)
			victimEntra, victimSubject, victimEarly, victimSCIM := victims[0], victims[1], victims[2], victims[3]
			for _, tc := range []struct{ typed, victim string }{
				{victimEarly, victimEarly},
				{strings.ToUpper(victimEarly), victimEarly},
				{victimSubject, victimSubject},
				{victimEntra, victimEntra},
				{victimSCIM, victimSCIM},
				{strings.ToUpper(victimSCIM), victimSCIM},
				{crossSharedEmail, ""}, // the signed-in victim's and the attacker's: refused
			} {
				rt.seed(t, srv, pg, sec, attacker)
				if tc.victim != "" && !rt.ownerFirst {
					rt.seed(t, srv, pg, sec, tc.victim)
				}
				if tc.victim == "" {
					rt.seed(t, srv, pg, sec, victimSubject)
				}
				claimed := crossClaimedChange(t, pg, attacker, tc.typed)
				since := dbNow(t, pg) // after an earlier name's erasure, on the clock that stamped it
				w := rt.call(t, srv, tc.typed)
				switch {
				case tc.victim == "":
					if w.Code != http.StatusUnprocessableEntity || errorReason(w) != reasonOwnerAmbiguous {
						t.Errorf("%s: an email two people claim = %d %s, want 422 %s", tc.typed, w.Code, w.Body, reasonOwnerAmbiguous)
					}
					if rt.gone(t, srv, pg, sec, victimSubject, since) {
						t.Errorf("%s: a refused name reached %q", tc.typed, victimSubject)
					}
				case w.Code != rt.ok:
					t.Errorf("%s = %d %s, want %d", tc.typed, w.Code, w.Body, rt.ok)
				case !rt.ownerFirst && !rt.gone(t, srv, pg, sec, tc.victim, since):
					t.Errorf("%s: the victim %q kept the record", tc.typed, tc.victim)
				}
				if rt.gone(t, srv, pg, sec, attacker, since) {
					t.Errorf("%s: erasing the victim's name reached the attacker %q", tc.typed, attacker)
				}
				if by := crossChangeProposer(t, pg, claimed); by != attacker {
					t.Errorf("%s: the attacker's governance change lost its proposer (now %q)", tc.typed, by)
				}
			}
		})
	}
}

// Erasing the attacker by their principal reaches the attacker alone on every route, whichever victims'
// names their claims spell; a governance change the signed-in victim proposed under the email both claim
// keeps its proposer.
func TestPG_ErasureRoutes_TheClaimantsErasureNeverReachesAVictim(t *testing.T) {
	for _, rt := range crossRoutes() {
		t.Run(rt.name, func(t *testing.T) {
			srv, pg, sec, attacker, victims := crossNameRig(t)
			for _, owner := range append([]string{attacker}, victims...) {
				rt.seed(t, srv, pg, sec, owner)
			}
			victimsChange := crossProposedChange(t, pg, victims[1], crossSharedEmail)
			since := time.Now().UTC().Add(-time.Second)
			if w := rt.call(t, srv, attacker); w.Code != rt.ok {
				t.Fatalf("erasing %q = %d %s, want %d", attacker, w.Code, w.Body, rt.ok)
			}
			if !rt.gone(t, srv, pg, sec, attacker, since) {
				t.Errorf("the attacker %q kept the record", attacker)
			}
			for _, v := range victims {
				if rt.gone(t, srv, pg, sec, v, since) {
					t.Errorf("erasing %q reached the victim %q", attacker, v)
				}
			}
			if by := crossChangeProposer(t, pg, victimsChange); by != victims[1] {
				t.Errorf("the victim's governance change lost its proposer (now %q)", by)
			}
		})
	}
}

// Three principals under one tenant and object id: an identity bound to entra:<tenant>:<object>, a
// sign-in of the same ids under another issuer, and an admin-created person (people row) with a
// sign-in under another issuer only. Every route that acts by a typed name (administration by
// ?owner= and the credentials and ssh-keys routes, erasure by every scope) reaches the bound
// principal by its canonical name and never the other issuer's, the audit sealer keys the bound name
// as itself, and the unlinked people row is still refused as ambiguous.
func TestPG_ErasureRoutes_ABoundEntraNameIsNotAmbiguousAcrossIssuers(t *testing.T) {
	for _, rt := range crossRoutes() {
		t.Run(rt.name, func(t *testing.T) {
			_, srv, pg, sec := identityRig(t)
			boundOID, unlinkedOID := uuid.NewString(), uuid.NewString()
			bound, unlinked := "entra:"+identTenant+":"+boundOID, "entra:"+identTenant+":"+unlinkedOID
			other := "https://login.microsoftonline.com/other/v2.0"
			if _, _, err := pg.CreatePerson(t.Context(), types.Person{Principal: unlinked, Email: "unlinked-x@corp.example", Issuer: identIssuer,
				TenantID: identTenant, ObjectID: unlinkedOID, CreatedBy: "admin"}); err != nil {
				t.Fatal(err)
			}
			for _, in := range []store.LoginIdentity{
				{Principal: bound, Issuer: identIssuer, TenantID: identTenant, ObjectID: boundOID},
				{Principal: "pairwise-y-" + boundOID, Issuer: other, TenantID: identTenant, ObjectID: boundOID},
				{Principal: "pairwise-unlinked-" + unlinkedOID, Issuer: other, TenantID: identTenant, ObjectID: unlinkedOID},
			} {
				if _, err := pg.UpsertLoginIdentity(t.Context(), in, time.Now().UTC()); err != nil {
					t.Fatal(err)
				}
			}
			if got, err := pg.PrincipalForName(t.Context(), bound); err != nil || got != bound {
				t.Fatalf("the sealer keys %q as %q, %v, want itself", bound, got, err)
			}
			y := "pairwise-y-" + boundOID
			rt.seed(t, srv, pg, sec, bound)
			rt.seed(t, srv, pg, sec, y)
			since := time.Now().UTC().Add(-time.Second)
			if w := rt.call(t, srv, bound); w.Code != rt.ok {
				t.Fatalf("%s by the bound name = %d %s, want %d", rt.name, w.Code, w.Body, rt.ok)
			}
			if !rt.gone(t, srv, pg, sec, bound, since) {
				t.Errorf("the bound principal %q kept the record", bound)
			}
			if rt.gone(t, srv, pg, sec, y, since) {
				t.Errorf("erasing %q reached the other issuer's %q", bound, y)
			}
			w := rt.call(t, srv, unlinked)
			if w.Code != http.StatusUnprocessableEntity || errorReason(w) != reasonOwnerAmbiguous {
				t.Errorf("%s by the unlinked name = %d %s, want 422 %s", rt.name, w.Code, w.Body, reasonOwnerAmbiguous)
			}
		})
	}
}

// crossClaimedChange is a pending governance change the attacker proposed while signed in with the
// email claim claimed.
func crossClaimedChange(t *testing.T, pg store.PG, attacker, claimed string) uuid.UUID {
	t.Helper()
	return crossProposedChange(t, pg, attacker, strings.ToLower(claimed))
}

func crossProposedChange(t *testing.T, pg store.PG, by, email string) uuid.UUID {
	t.Helper()
	ch := types.GovernanceChange{TargetKind: "governance_profile", Op: "update", TargetKey: uuid.NewString(),
		Payload: json.RawMessage(`{"name":"p"}`), Diff: json.RawMessage(`{"changed":["name"]}`), ProposedBy: by, ProposedByEmail: email}
	saved, _, err := pg.ProposeGovernanceChange(t.Context(), ch, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	return saved.ID
}

func crossChangeProposer(t *testing.T, pg store.PG, id uuid.UUID) (by string) {
	t.Helper()
	if err := pg.Pool.QueryRow(t.Context(), `SELECT proposed_by FROM governance_changes WHERE id = $1`, id).Scan(&by); err != nil {
		t.Fatal(err)
	}
	return by
}
