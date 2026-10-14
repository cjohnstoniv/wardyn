// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// aliasingStore is the person-erasure store with an identity directory that
// maps chosen names onto principals and lists chosen aliases.
type aliasingStore struct {
	*erasureTestStore
	forName map[string]string
	aliases map[string][]string
	err     error
}

var _ store.PrincipalDirectoryStore = (*aliasingStore)(nil)

func (s *aliasingStore) PrincipalsNamed(_ context.Context, name string) ([]string, error) {
	if s.err != nil {
		return nil, s.err
	}
	if p, ok := s.forName[name]; ok {
		return []string{p}, nil
	}
	return nil, nil
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

// On a store with no identity directory the same order holds: an email is claim text, so a name with no
// @ is never matched against one, and a claimant whose token email spells another person's subject is not
// that person.
func TestErasePersonCredentials_ATokenEmailNeverNamesABareSubject(t *testing.T) {
	_, srv, st, sec := erasureRig(t)
	st.toks = append(st.toks, types.APIToken{ID: uuid.New(), Principal: "claimant", Email: "victim-sub"})
	for _, owner := range []string{"claimant", "victim-sub"} {
		if err := sec.For(owner).Put(t.Context(), "anthropic-api-key", []byte("credential-of-"+owner)); err != nil {
			t.Fatal(err)
		}
	}
	sa := ssoSession(t, "sec-2", "sec2@corp.example", oidc.RoleSecurityAdmin)
	if w := doSSO(t, srv, http.MethodDelete, "/api/v1/people/victim-sub/credentials", sa, ""); w.Code != http.StatusOK {
		t.Fatalf("erase = %d %s, want 200", w.Code, w.Body)
	}
	if got := namesOf(t, sec, "victim-sub"); len(got) != 0 {
		t.Errorf("the named subject still holds %v", got)
	}
	if got := namesOf(t, sec, "claimant"); len(got) != 1 {
		t.Errorf("erasing a subject reached the claimant whose token email spells it: %v left", got)
	}
}

// tokensDownStore is the erasure store whose API token list cannot be read.
type tokensDownStore struct{ *erasureTestStore }

func (tokensDownStore) ListAPITokens(context.Context) ([]types.APIToken, error) {
	return nil, errors.New("tokens down")
}

// A store with no identity directory resolves against its token and workspace lists; one it cannot read
// stops the erase, because a partial list could make an ambiguous name look unique.
func TestErasePersonCredentials_AnUnreadableListErasesNothing(t *testing.T) {
	_, srv, st, sec := erasureRig(t)
	srv.cfg.Store = tokensDownStore{st}
	sa := ssoSession(t, "sec-2", "sec2@corp.example", oidc.RoleSecurityAdmin)
	if w := doSSO(t, srv, http.MethodDelete, "/api/v1/people/bob/credentials", sa, ""); w.Code != http.StatusInternalServerError {
		t.Fatalf("erase with an unreadable token list = %d %s, want 500", w.Code, w.Body)
	}
	if got := namesOf(t, sec, "bob"); len(got) != 2 {
		t.Fatalf("bob holds %v after a refused erase, want both credentials", got)
	}
}

// unknownEntraStore derives an entra:<tenant>:<object> name for bob that the directory resolves to no one.
type unknownEntraStore struct{ *erasureTestStore }

func (unknownEntraStore) PrincipalEntraNames(context.Context, string) ([]string, error) {
	return []string{"entra:t:o"}, nil
}

func (unknownEntraStore) PrincipalsNamed(_ context.Context, name string) ([]string, error) {
	if name == "bob" {
		return []string{"bob"}, nil
	}
	return nil, nil
}

// A derived name nobody is known by is not proved the person's, so both erase routes fail closed and
// erase nothing; the POST answer does not tell the operator a retry finishes it, and the credential.erase
// failure row carries the cause naming the derived name.
func TestErasePerson_AnUnknownDerivedNameFailsClosed(t *testing.T) {
	h, srv, st, sec := erasureRig(t)
	srv.cfg.Store = unknownEntraStore{st}
	sa := ssoSession(t, "sec-1", "sec@corp.example", oidc.RoleSecurityAdmin)
	if w := doSSO(t, srv, http.MethodPost, "/api/v1/people/bob/erasure", sa, erasureBody("credentials")); w.Code != http.StatusInternalServerError ||
		errorReason(w) != reasonErasureIncomplete || strings.Contains(w.Body.String(), "retry") {
		t.Fatalf("erasure = %d %s, want 500 %s with no retry advice", w.Code, w.Body, reasonErasureIncomplete)
	}
	if w := doSSO(t, srv, http.MethodDelete, "/api/v1/people/bob/credentials", sa, ""); w.Code != http.StatusInternalServerError {
		t.Fatalf("credential erase = %d %s, want 500", w.Code, w.Body)
	}
	if got := namesOf(t, sec, "bob"); len(got) != 2 {
		t.Fatalf("a refused erasure left bob %v, want both credentials", got)
	}
	var cause bool
	for _, ev := range h.audit.events {
		cause = cause || ev.Action == "credential.erase" && ev.Outcome == "failure" && strings.Contains(string(ev.Data), "entra:t:o")
	}
	if !cause {
		t.Errorf("no credential.erase failure row names the unproved entra:t:o")
	}
}
