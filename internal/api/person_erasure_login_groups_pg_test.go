// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"net/http"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/secretstore/keydomain"
)

// seedLoginGroups gives each of people an IdP group snapshot, and the first a key-domain assignment.
func seedLoginGroups(t *testing.T, q keydomain.Querier, people ...string) {
	t.Helper()
	for _, p := range people {
		if _, err := q.Exec(t.Context(), `INSERT INTO key_domain_login_groups (principal, groups, truncated) VALUES ($1, '["eng"]', false)`, p); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := q.Exec(t.Context(), `INSERT INTO key_domain_assignments (subject_type, subject, domain, set_by) VALUES ('user', $1, 'default', 'admin')`, people[0]); err != nil {
		t.Fatal(err)
	}
}

// loginGroupRows is how many snapshots principal holds, and whether their key-domain assignment stands.
func loginGroupRows(t *testing.T, q keydomain.Querier, principal string) (snapshots int, assigned bool) {
	t.Helper()
	if err := q.QueryRow(t.Context(), `SELECT (SELECT count(*) FROM key_domain_login_groups WHERE principal=$1),
		EXISTS (SELECT 1 FROM key_domain_assignments WHERE subject_type='user' AND subject=$1)`, principal).Scan(&snapshots, &assigned); err != nil {
		t.Fatal(err)
	}
	return snapshots, assigned
}

// Erasing a person's credentials takes their IdP group snapshot with it and leaves their audited
// key-domain assignment, and another person's snapshot, alone.
func TestPG_PersonErasure_DeletesTheLoginGroupSnapshot(t *testing.T) {
	l := newMaskLab(t)
	fail := false
	a := erasureReplica(l, &fail)
	const alice, bob = "alice-sub", "bob-sub"
	seedLoginGroups(t, l.pool, alice, bob)
	if err := a.sec.For(alice).Put(t.Context(), "anthropic-api-key", []byte("alice-credential-value-0000")); err != nil {
		t.Fatal(err)
	}
	sec := ssoSession(t, "sec-sub", "sec@corp.example", oidc.RoleSecurityAdmin)
	if w := doSSO(t, a.srv, http.MethodPost, "/api/v1/people/"+alice+"/erasure", sec, erasureBody("credentials")); w.Code != http.StatusOK {
		t.Fatalf("erase credentials = %d %s, want 200", w.Code, w.Body)
	}
	if n, assigned := loginGroupRows(t, l.pool, alice); n != 0 || !assigned {
		t.Errorf("alice after the erasure: %d snapshots, assignment kept %v; want 0 and true", n, assigned)
	}
	if n, _ := loginGroupRows(t, l.pool, bob); n != 1 {
		t.Errorf("bob's snapshot count = %d, want it untouched", n)
	}
}

// A leaver purge does the same.
func TestSCIMPurgeDeletesTheLoginGroupSnapshot(t *testing.T) {
	e := newSCIMEnv(t)
	f := e.seedPurgeSubject(purgeSub, purgeEmail, purgeOID)
	seedLoginGroups(t, e.pool, purgeSub, "sub-bystander-groups")
	if w := e.del(e.b, f.id); w.Code != http.StatusNoContent {
		t.Fatalf("DELETE = %d %s, want 204", w.Code, w.Body)
	}
	if n, assigned := loginGroupRows(t, e.pool, purgeSub); n != 0 || !assigned {
		t.Errorf("the leaver after the purge: %d snapshots, assignment kept %v; want 0 and true", n, assigned)
	}
	if n, _ := loginGroupRows(t, e.pool, "sub-bystander-groups"); n != 1 {
		t.Errorf("the bystander's snapshot count = %d, want it untouched", n)
	}
}
