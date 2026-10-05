// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/cjohnstoniv/wardyn/internal/authz"
	"github.com/cjohnstoniv/wardyn/internal/secretstore/keydomain"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// principalDirStore is the directory knownPrincipals reads: API tokens and
// workspace owners.
type principalDirStore struct {
	store.Store
	tokens []types.APIToken
}

func (s *principalDirStore) ListAPITokens(context.Context) ([]types.APIToken, error) {
	return s.tokens, nil
}

func (s *principalDirStore) ListWorkspaces(context.Context) ([]types.Workspace, error) {
	return []types.Workspace{{OwnedBy: "sub-owner"}}, nil
}

// decodeVia runs decodeKeyDomainChange behind a real chi route, so the
// {subject_type} and {subject} path parameters resolve as they do in the API.
func decodeVia(t *testing.T, srv *Server, del bool, path, body string) (keyDomainChange, bool, *httptest.ResponseRecorder) {
	t.Helper()
	var (
		got keyDomainChange
		ok  bool
	)
	r := chi.NewRouter()
	r.HandleFunc("/{subject_type}/{subject}", func(w http.ResponseWriter, req *http.Request) {
		got, ok = srv.decodeKeyDomainChange(w, req, del)
	})
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodPut, path, strings.NewReader(body)))
	return got, ok, w
}

func TestDecodeKeyDomainChange(t *testing.T) {
	st := &principalDirStore{tokens: []types.APIToken{
		{Principal: "sub-alice", Email: "alice@corp.example"},
		{Principal: "sub-twin-1", Email: "twin@corp.example"},
		{Principal: "sub-twin-2", Email: "Twin@corp.example"},
	}}
	srv := New(baseTestConfig(newHarness(t), st))

	for _, tc := range []struct {
		name       string
		del        bool
		path, body string
		want       keyDomainChange
		status     int    // 0 = accepted
		reason     string // expected refusal reason
	}{
		{name: "all", path: "/all/all", body: `{"domain":" vault "}`, want: keyDomainChange{SubjectType: "all", Domain: "vault"}},
		{name: "all with a subject", path: "/all/everyone", body: `{"domain":"vault"}`, status: 400, reason: reasonKeyDomainRequestInvalid},
		{name: "group is lower-cased", path: "/group/Eng-Team", body: `{"domain":"vault"}`, want: keyDomainChange{SubjectType: "group", Subject: "eng-team", Domain: "vault"}},
		{name: "group with a control character", path: "/group/a%07b", body: `{"domain":"vault"}`, status: 400, reason: reasonKeyDomainRequestInvalid},
		{name: "group that is not ASCII", path: "/group/%C3%A9ng", body: `{"domain":"vault"}`, status: 400, reason: reasonKeyDomainRequestInvalid},
		{name: "group too long", path: "/group/" + strings.Repeat("g", maxKeyDomainSubjectLen+1), body: `{"domain":"vault"}`, status: 400, reason: reasonKeyDomainRequestInvalid},
		{name: "user by subject", path: "/user/sub-alice", body: `{"domain":"vault"}`, want: keyDomainChange{SubjectType: "user", Subject: "sub-alice", Domain: "vault"}},
		{name: "user by email resolves to the subject", path: "/user/ALICE@corp.example", body: `{"domain":"vault"}`, want: keyDomainChange{SubjectType: "user", Subject: "sub-alice", Domain: "vault"}},
		{name: "user email matching two people is ambiguous", path: "/user/twin@corp.example", body: `{"domain":"vault"}`, status: 422, reason: reasonOwnerAmbiguous},
		{name: "user email matching nobody is unresolved", path: "/user/nobody@corp.example", body: `{"domain":"vault"}`, status: 422, reason: reasonOwnerUnresolved},
		{name: "unknown subject type", path: "/team/x", body: `{"domain":"vault"}`, status: 400, reason: reasonKeyDomainRequestInvalid},
		{name: "missing domain", path: "/all/all", body: `{"domain":"  "}`, status: 400, reason: reasonKeyDomainRequestInvalid},
		{name: "unknown body field", path: "/all/all", body: `{"domain":"vault","x":1}`, status: 400},
		{name: "delete needs no body", del: true, path: "/group/Eng-Team", want: keyDomainChange{Delete: true, SubjectType: "group", Subject: "eng-team"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok, w := decodeVia(t, srv, tc.del, tc.path, tc.body)
			if tc.status != 0 {
				if ok || w.Code != tc.status {
					t.Fatalf("ok=%v %d %s, want refusal %d", ok, w.Code, w.Body.String(), tc.status)
				}
				if tc.reason != "" && reasonOf(t, w) != tc.reason {
					t.Errorf("reason = %q, want %q", reasonOf(t, w), tc.reason)
				}
				return
			}
			if !ok || got != tc.want {
				t.Errorf("ok=%v change=%+v, want %+v (body %s)", ok, got, tc.want, w.Body.String())
			}
		})
	}
}

func TestKeyDomainSetRefusal(t *testing.T) {
	svc := keydomain.NewService(nil, []string{"vault-b", "azure-a"})
	count := func(n int, err error) func(context.Context) (int, error) {
		return func(context.Context) (int, error) { return n, err }
	}
	amb := func(n int, err error) func(context.Context, string, string) (int, error) {
		return func(context.Context, string, string) (int, error) { return n, err }
	}
	boom := errors.New("db down")
	group := keyDomainChange{SubjectType: keydomain.SubjectGroup, Subject: "eng", Domain: "vault-b"}

	for _, tc := range []struct {
		name      string
		c         keyDomainChange
		ambiguous func(context.Context, string, string) (int, error)
		truncated func(context.Context) (int, error)
		wantErr   error
		reason    authz.Reason // "" = allowed
		sentence  string
	}{
		{name: "undeclared domain", c: keyDomainChange{SubjectType: keydomain.SubjectAll, Domain: "vault-c"}, reason: authz.ReasonKeyDomainUnknown,
			sentence: "Declared: default, azure-a, vault-b"},
		{name: "default is always declared", c: keyDomainChange{SubjectType: keydomain.SubjectAll, Domain: keydomain.Default}},
		{name: "a user assignment is never membership-checked", c: keyDomainChange{SubjectType: keydomain.SubjectUser, Subject: "u", Domain: "vault-b"},
			ambiguous: amb(9, boom), truncated: count(9, boom)},
		{name: "group leaving people in two domains", c: group, ambiguous: amb(3, nil), truncated: count(0, nil),
			reason: authz.ReasonKeyDomainAmbiguous, sentence: "3 people last signed in with this group"},
		{name: "ambiguity lookup fails", c: group, ambiguous: amb(0, boom), truncated: count(0, nil), wantErr: boom},
		{name: "group while truncated sign-ins exist", c: group, ambiguous: amb(0, nil), truncated: count(2, nil),
			reason: authz.ReasonKeyDomainAmbiguous, sentence: "2 people last signed in with a group list that was cut short"},
		{name: "truncation lookup fails", c: group, ambiguous: amb(0, nil), truncated: count(0, boom), wantErr: boom},
		{name: "clean group set", c: group, ambiguous: amb(0, nil), truncated: count(0, nil)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d, err := keyDomainSetRefusal(context.Background(), svc, tc.ambiguous, tc.truncated, tc.c)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("err = %v, want %v", err, tc.wantErr)
			}
			if tc.reason == "" {
				if d != nil {
					t.Errorf("refused %+v, want allowed", *d)
				}
				return
			}
			if d == nil || d.Reason != tc.reason || !strings.Contains(d.Sentence, tc.sentence) {
				t.Errorf("decision = %+v, want reason %s containing %q", d, tc.reason, tc.sentence)
			}
		})
	}
}
