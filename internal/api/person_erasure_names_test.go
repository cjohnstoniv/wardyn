// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
)

// aliasingStore is the person-erasure store with an identity directory that
// maps chosen names onto principals and lists chosen aliases.
type aliasingStore struct {
	*erasureTestStore
	forName map[string]string
	aliases map[string][]string
	err     error
}

func (s *aliasingStore) PrincipalForName(_ context.Context, name string) (string, error) {
	if s.err != nil {
		return "", s.err
	}
	if p, ok := s.forName[name]; ok {
		return p, nil
	}
	return name, nil
}

func (s *aliasingStore) PrincipalAliases(_ context.Context, principal string) ([]string, error) {
	return s.aliases[principal], nil
}

// The erasure's steps resolve the name through the identity directory,
// so the self check compares the caller against that same name set.
func TestErasePerson_SelfCheckUsesTheNamesTheStepsAct(t *testing.T) {
	for _, tc := range []struct {
		name    string
		typed   string
		forName map[string]string
		aliases map[string][]string
	}{
		{name: "a name the directory maps to the caller", typed: "legacy-alias", forName: map[string]string{"legacy-alias": "sec-1"}},
		{name: "an alias of the target that is the caller's email", typed: "bob", aliases: map[string][]string{"bob": {"sec@corp.example"}}},
		{name: "an alias of the target that is the caller's own alias", typed: "bob",
			aliases: map[string][]string{"bob": {"entra:t:o"}, "sec-1": {"ENTRA:t:o"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, srv, st, _ := erasureRig(t)
			srv.cfg.Store = &aliasingStore{erasureTestStore: st, forName: tc.forName, aliases: tc.aliases}
			sa := ssoSession(t, "sec-1", "sec@corp.example", oidc.RoleSecurityAdmin)
			w := doSSO(t, srv, http.MethodPost, "/api/v1/people/"+tc.typed+"/erasure", sa, erasureBody("run_tasks"))
			if w.Code != http.StatusForbidden || errorReason(w) != reasonErasureSelfRefused {
				t.Fatalf("erasure = %d %s, want 403 %s", w.Code, w.Body, reasonErasureSelfRefused)
			}
			if len(st.blanked) != 0 {
				t.Fatalf("a refused self erasure blanked %v", st.blanked)
			}
		})
	}
}

func TestErasePerson_SelfCheckFailsClosedWhenTheDirectoryCannotBeRead(t *testing.T) {
	_, srv, st, _ := erasureRig(t)
	srv.cfg.Store = &aliasingStore{erasureTestStore: st, err: errors.New("directory down")}
	sa := ssoSession(t, "sec-2", "sec2@corp.example", oidc.RoleSecurityAdmin)
	w := doSSO(t, srv, http.MethodPost, "/api/v1/people/bob/erasure", sa, erasureBody("run_tasks"))
	if w.Code != http.StatusInternalServerError || len(st.blanked) != 0 {
		t.Fatalf("erasure with an unreadable directory = %d %s (blanked %v), want 500 and nothing erased", w.Code, w.Body, st.blanked)
	}
}

// The names that are no person are refused on both erase routes
// before anything runs.
func TestErasePerson_RefusesReservedNonPersonPrincipals(t *testing.T) {
	h, srv, st, sec := erasureRig(t)
	sa := ssoSession(t, "sec-2", "sec2@corp.example", oidc.RoleSecurityAdmin)
	for _, name := range []string{"admin-token", "ADMIN-TOKEN", "local:alice", "device:laptop-1"} {
		for _, rt := range []struct{ method, path, body string }{
			{http.MethodPost, "/api/v1/people/" + name + "/erasure", erasureBody("run_tasks", "run_outputs", "recordings", "credentials")},
			{http.MethodDelete, "/api/v1/people/" + name + "/credentials", ""},
		} {
			w := doSSO(t, srv, rt.method, rt.path, sa, rt.body)
			if w.Code != http.StatusUnprocessableEntity || errorReason(w) != reasonReservedPrincipal {
				t.Errorf("%s %s = %d %s, want 422 %s", rt.method, rt.path, w.Code, w.Body, reasonReservedPrincipal)
			}
		}
	}
	if len(st.blanked) != 0 || len(namesOf(t, sec, "bob")) != 2 {
		t.Fatalf("a refused request erased something: blanked %v, bob holds %v", st.blanked, namesOf(t, sec, "bob"))
	}
	for _, ev := range erasureRows(h) {
		if ev.Outcome == "success" {
			t.Fatalf("a refused request left a success row: %+v", ev)
		}
	}
	denied := 0
	for _, ev := range h.audit.events {
		if ev.Action == "credential.erase" && ev.Outcome == "denied" {
			denied++
		}
	}
	if denied != 4 {
		t.Errorf("%d denied credential.erase rows, want one per refused name", denied)
	}
}
