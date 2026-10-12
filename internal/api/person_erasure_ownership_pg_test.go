// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

// An erasure acts on an entra:<tenant>:<object> form derived from the person's sign-ins only when the
// identity directory proves the form is theirs alone. Guarded by WARDYN_TEST_PG.

import (
	"errors"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/erasure"
	"github.com/cjohnstoniv/wardyn/internal/secretstore"
	"github.com/cjohnstoniv/wardyn/internal/secretstore/subjectkey"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

const ownIssuerB = "https://sts.windows.net/" + identTenant + "/"

// ownRecords are what one name holds: a credential, a live audit-seal key and a pending governance change.
type ownRecords struct{ change uuid.UUID }

func ownSeed(t *testing.T, srv *Server, pg store.PG, sec secretstore.Store, owner string) ownRecords {
	t.Helper()
	if err := sec.For(owner).Put(t.Context(), "anthropic-api-key", []byte("credential-of-"+owner)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := srv.cfg.SubjectKeys.Current(t.Context(), owner, subjectkey.PurposeAuditSeal); err != nil {
		t.Fatal(err)
	}
	return ownRecords{change: crossProposedChange(t, pg, owner, "")}
}

// ownKept reports which of owner's records survive, as "credential", "key" and "change" flags.
func ownKept(t *testing.T, pg store.PG, sec secretstore.Store, owner string, r ownRecords, since time.Time) (cred, key, change bool) {
	t.Helper()
	destroyed, err := pg.SubjectKeyDestroyedSince(t.Context(), owner, subjectkey.PurposeAuditSeal, since)
	if err != nil {
		t.Fatal(err)
	}
	return len(namesOf(t, sec, owner)) > 0, !destroyed, crossChangeProposer(t, pg, r.change) == owner
}

func ownBind(t *testing.T, pg store.PG, principal, issuer, oid string) store.PrincipalIdentity {
	t.Helper()
	id, err := pg.UpsertLoginIdentity(t.Context(), store.LoginIdentity{Principal: principal, Issuer: issuer, TenantID: identTenant, ObjectID: oid}, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func ownPerson(t *testing.T, pg store.PG, principal, issuer, oid string) {
	t.Helper()
	if _, _, err := pg.CreatePerson(t.Context(), types.Person{Principal: principal, Issuer: issuer, TenantID: identTenant, ObjectID: oid, CreatedBy: "admin"}); err != nil {
		t.Fatal(err)
	}
}

func ownDeactivate(t *testing.T, pg store.PG, oid string) {
	t.Helper()
	if _, err := pg.Pool.Exec(t.Context(), `UPDATE principal_identities SET deactivated_at = now() WHERE object_id = $1`, oid); err != nil {
		t.Fatal(err)
	}
	if _, err := pg.Pool.Exec(t.Context(), `UPDATE people SET deactivated_at = now() WHERE object_id = $1`, oid); err != nil {
		t.Fatal(err)
	}
}

func ownErase(t *testing.T, srv *Server, method, name string) (code int, reason string) {
	t.Helper()
	sa := ssoSession(t, "sec-1", "sec@corp.example", oidc.RoleSecurityAdmin)
	path, body := "/api/v1/people/"+name+"/erasure", erasureBody("audit_personal_fields", "credentials")
	if method == http.MethodDelete {
		path, body = "/api/v1/people/"+name+"/credentials", ""
	}
	w := doSSO(t, srv, method, path, sa, body)
	return w.Code, errorReason(w)
}

// P is bound to entra:<tenant>:<object> under one issuer, Y is a pairwise subject with the same tenant and
// object id under another. Erasing Y never reaches P's credential, audit key or governance change, and
// erasing P never reaches Y's, active or deactivated.
func TestPG_Erasure_AnotherIssuersEntraNameIsNeverErased(t *testing.T) {
	for _, deactivated := range []bool{false, true} {
		for _, erasingY := range []bool{true, false} {
			for _, method := range []string{http.MethodPost, http.MethodDelete} {
				t.Run(fmt.Sprintf("deactivated=%v/erasingY=%v/%s", deactivated, erasingY, method), func(t *testing.T) {
					_, srv, pg, sec := identityRig(t)
					oid := uuid.NewString()
					p, y := "entra:"+identTenant+":"+oid, "pairwise-y-"+oid
					ownPerson(t, pg, p, identIssuer, oid)
					ownBind(t, pg, p, identIssuer, oid)
					ownBind(t, pg, y, ownIssuerB, oid)
					if deactivated {
						ownDeactivate(t, pg, oid)
					}
					target, other := y, p
					if !erasingY {
						target, other = p, y
					}
					since := time.Now().UTC().Add(-time.Second)
					targetRecs, otherRecs := ownSeed(t, srv, pg, sec, target), ownSeed(t, srv, pg, sec, other)
					if code, reason := ownErase(t, srv, method, target); code != http.StatusOK {
						t.Fatalf("erasing %q = %d %s, want 200", target, code, reason)
					}
					if cred, _, _ := ownKept(t, pg, sec, target, targetRecs, since); cred {
						t.Errorf("erasing %q left its credential", target)
					}
					if cred, key, change := ownKept(t, pg, sec, other, otherRecs, since); !cred || !key || !change {
						t.Errorf("erasing %q reached %q: credential kept %v, key kept %v, change kept %v", target, other, cred, key, change)
					}
				})
			}
		}
	}
}

// A person an admin created by object id who signed in under a pairwise subject of the same issuer may
// still hold records under the entra:<tenant>:<object> name. Erasing the subject erases them, active or
// deactivated.
func TestPG_Erasure_SameIssuerHistoryUnderTheEntraNameIsErased(t *testing.T) {
	for _, deactivated := range []bool{false, true} {
		t.Run(map[bool]string{false: "active", true: "deactivated"}[deactivated], func(t *testing.T) {
			_, srv, pg, sec := identityRig(t)
			oid := uuid.NewString()
			c, y := "entra:"+identTenant+":"+oid, "pairwise-"+oid
			ownPerson(t, pg, c, identIssuer, oid)
			ownBind(t, pg, y, identIssuer, oid)
			if deactivated {
				ownDeactivate(t, pg, oid)
			}
			since := time.Now().UTC().Add(-time.Second)
			history, own := ownSeed(t, srv, pg, sec, c), ownSeed(t, srv, pg, sec, y)
			if code, reason := ownErase(t, srv, http.MethodPost, y); code != http.StatusOK {
				t.Fatalf("erasing %q = %d %s, want 200", y, code, reason)
			}
			for name, r := range map[string]ownRecords{c: history, y: own} {
				if cred, key, change := ownKept(t, pg, sec, name, r, since); cred || key || change {
					t.Errorf("erasing %q left %q: credential %v, key %v, change %v", y, name, cred, key, change)
				}
			}
		})
	}
}

// Y and Q are pairwise subjects with one tenant and object id under two issuers, and nobody is bound to
// the entra:<tenant>:<object> name: what is under it may be either's. Erasing Y fails closed on both
// routes, and nothing is erased.
func TestPG_Erasure_AnAmbiguousEntraNameFailsClosed(t *testing.T) {
	_, srv, pg, sec := identityRig(t)
	oid := uuid.NewString()
	c, y := "entra:"+identTenant+":"+oid, "pairwise-y-"+oid
	ownBind(t, pg, y, ownIssuerB, oid)
	ownBind(t, pg, "pairwise-q-"+oid, identIssuer, oid)
	since := time.Now().UTC().Add(-time.Second)
	history, own := ownSeed(t, srv, pg, sec, c), ownSeed(t, srv, pg, sec, y)
	for _, tc := range []struct {
		method string
		code   int
		reason string
	}{{http.MethodPost, http.StatusInternalServerError, reasonErasureIncomplete}, {http.MethodDelete, http.StatusInternalServerError, reasonInternalError}} {
		if code, reason := ownErase(t, srv, tc.method, y); code != tc.code || reason != tc.reason {
			t.Fatalf("%s erasing %q = %d %s, want %d %s", tc.method, y, code, reason, tc.code, tc.reason)
		}
	}
	for name, r := range map[string]ownRecords{c: history, y: own} {
		if cred, key, change := ownKept(t, pg, sec, name, r, since); !cred || !key || !change {
			t.Errorf("a refused erasure reached %q: credential kept %v, key kept %v, change kept %v", name, cred, key, change)
		}
	}
}

// The names are proved once, before anything runs; a binding that changes before a destructive step fails
// that step and erases nothing under the name it changed.
func TestPG_Erasure_ABindingThatChangesAfterTheProofFailsTheStep(t *testing.T) {
	for _, tc := range []struct {
		name   string
		before func(t *testing.T, pg store.PG, oid string) string // binds Y, returns Y
		change func(t *testing.T, pg store.PG, oid, y string)
	}{
		{name: "another issuer's sign-in binds the entra name",
			before: func(t *testing.T, pg store.PG, oid string) string {
				ownBind(t, pg, "pairwise-y-"+oid, ownIssuerB, oid)
				return "pairwise-y-" + oid
			},
			change: func(t *testing.T, pg store.PG, oid, _ string) {
				c := "entra:" + identTenant + ":" + oid
				ownPerson(t, pg, c, identIssuer, oid)
				ownBind(t, pg, c, identIssuer, oid)
			}},
		{name: "the person's sign-in is released",
			before: func(t *testing.T, pg store.PG, oid string) string {
				ownPerson(t, pg, "entra:"+identTenant+":"+oid, identIssuer, oid)
				ownBind(t, pg, "pairwise-"+oid, identIssuer, oid)
				return "pairwise-" + oid
			},
			change: func(t *testing.T, pg store.PG, oid, y string) {
				id, err := pg.GetIdentityByObject(t.Context(), identIssuer, identTenant, oid)
				if err != nil {
					t.Fatal(err)
				}
				if err := pg.UnbindIdentity(t.Context(), id.ID, y); err != nil {
					t.Fatal(err)
				}
			}},
	} {
		for _, sc := range []erasure.Scope{erasure.AuditPersonalFields, erasure.Credentials} {
			t.Run(tc.name+"/"+string(sc), func(t *testing.T) {
				_, srv, pg, sec := identityRig(t)
				oid := uuid.NewString()
				c, y := "entra:"+identTenant+":"+oid, tc.before(t, pg, oid)
				want, err := srv.erasureNamesOf(t.Context(), y)
				if err != nil || len(want.entra) != 1 || want.entra[0] != c {
					t.Fatalf("proved names of %q = %+v, %v, want the entra name %q", y, want, err, c)
				}
				tc.change(t, pg, oid, y)
				since := time.Now().UTC().Add(-time.Second)
				history := ownSeed(t, srv, pg, sec, c)
				_, err = srv.erasureOrchestrator(nil, &want).Orchestrate(t.Context(), y, []erasure.Scope{sc})
				if !errors.Is(err, errErasureNamesChanged) {
					t.Fatalf("erasing %q after the change = %v, want %v", y, err, errErasureNamesChanged)
				}
				if cred, key, _ := ownKept(t, pg, sec, c, history, since); !cred || !key {
					t.Errorf("a step that failed on the change reached %q: credential kept %v, key kept %v", c, cred, key)
				}
			})
		}
	}
}
