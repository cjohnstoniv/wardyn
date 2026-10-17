// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package client_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/cjohnstoniv/wardyn/pkg/client"
)

func TestListPeople_SendsOptionsAndDecodes(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/api/v1/people" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		checkAuth(t, r)
		q := r.URL.Query()
		if q.Get("limit") != "25" || q.Get("cursor") != "abc" || q.Get("q") != "pat" || q.Get("state") != "active" {
			t.Errorf("query = %q, want limit, cursor, q and state", r.URL.RawQuery)
		}
		writeJSON(w, http.StatusOK, client.PersonList{
			People:     []client.PersonSummary{{Principal: "pat-sub", Email: "pat@corp.example", IssuerKind: "oidc", Role: "user", APITokens: 2}},
			NextCursor: "next",
		})
	}))
	defer srv.Close()

	got, err := newTestClient(srv).ListPeople(context.Background(), client.PeopleListOpts{Limit: 25, Cursor: "abc", Query: "pat", State: "active"})
	if err != nil {
		t.Fatalf("ListPeople: %v", err)
	}
	if len(got.People) != 1 || got.People[0].Principal != "pat-sub" || got.People[0].APITokens != 2 || got.NextCursor != "next" {
		t.Errorf("got %+v, want the decoded page", got)
	}
}

func TestListPeople_NoOptionsSendsNoQuery(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.RawQuery != "" {
			t.Errorf("query = %q, want none", r.URL.RawQuery)
		}
		writeJSON(w, http.StatusOK, client.PersonList{})
	}))
	defer srv.Close()
	if _, err := newTestClient(srv).ListPeople(context.Background()); err != nil {
		t.Fatalf("ListPeople: %v", err)
	}
}
