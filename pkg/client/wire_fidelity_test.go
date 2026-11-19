// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package client_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/pkg/client"
)

// A source's requirements go on the wire as the caller set them: nil is left out, so an existing source
// keeps its contract, and an empty map is sent as {}, which clears it.
func TestCreateSourceSendsAnEmptyRequirementsMap(t *testing.T) {
	var bodies []map[string]json.RawMessage
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]json.RawMessage
		_ = json.NewDecoder(r.Body).Decode(&body)
		bodies = append(bodies, body)
		writeJSON(w, http.StatusOK, types.Source{})
	}))
	t.Cleanup(srv.Close)
	c := newTestClient(srv)
	req := client.SourceRequest{Kind: types.SourceLocalDir, Locator: "/srv/app"}
	for _, reqs := range []map[string]types.WorkspaceRequirement{
		nil, {}, {"secret:deploy-key": {Level: "required", Provenance: "operator_set"}},
	} {
		req.Requirements = reqs
		if _, err := c.CreateSource(context.Background(), req); err != nil {
			t.Fatal(err)
		}
	}
	if _, sent := bodies[0]["requirements"]; sent {
		t.Errorf("nil requirements sent %s, want the key left out", bodies[0]["requirements"])
	}
	if got := string(bodies[1]["requirements"]); got != "{}" {
		t.Errorf("empty requirements sent %q, want {}", got)
	}
	if got := string(bodies[2]["requirements"]); got != `{"secret:deploy-key":{"level":"required","provenance":"operator_set"}}` {
		t.Errorf("one requirement sent %s", got)
	}
}

// An erasure whose Azure DevOps token revoke was refused says so through
// TokensNotRevoked, with the server's sentence; one that revoked every token says nothing.
func TestErasePersonSaysATokenMayStillWork(t *testing.T) {
	for body, want := range map[string]string{
		`{"person":"p","scopes":["credentials"],"outcome":{"credentials":"done"},"detail":{"credentials":{"count":1,"tokens_revocation_failed":true,"detail":"remedy"}}}`: "remedy",
		`{"person":"p","scopes":["credentials"],"outcome":{"credentials":"done"},"detail":{"credentials":{"count":1}}}`:                                                   "",
		`{"person":"p","scopes":["run_tasks"],"outcome":{"run_tasks":"done"},"detail":{"run_tasks":{"runs":0}}}`:                                                          "",
	} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(body)) }))
		res, err := newTestClient(srv).ErasePerson(context.Background(), "p", []string{"credentials"})
		srv.Close()
		if err != nil {
			t.Fatal(err)
		}
		if got, failed := res.TokensNotRevoked(); got != want || failed != (want != "") {
			t.Errorf("TokensNotRevoked() = %q, %v for %s; want %q", got, failed, body, want)
		}
	}
}

// An erasure that left other principals alone says so in its result.
func TestErasePersonKeepsNotErased(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"person":"p","scopes":["credentials"],"outcome":{"credentials":"done"},"detail":{},` +
			`"not_erased":{"subjects":["y"],"note":"not erased: erase by subject"}}`))
	}))
	t.Cleanup(srv.Close)
	res, err := newTestClient(srv).ErasePerson(context.Background(), "p", []string{"credentials"})
	if err != nil {
		t.Fatal(err)
	}
	if res.NotErased == nil || !slices.Equal(res.NotErased.Subjects, []string{"y"}) || res.NotErased.Note != "not erased: erase by subject" {
		t.Fatalf("NotErased = %+v, want subjects [y] and the server's note", res.NotErased)
	}
	out, _ := json.Marshal(res)
	var back map[string]json.RawMessage
	_ = json.Unmarshal(out, &back)
	if _, ok := back["not_erased"]; !ok {
		t.Errorf("encoded result %s lost not_erased", out)
	}
}

// An approval whose role-mapping change could not revoke every token says so on the change it answers;
// one that reached them, and every other read of a change, carries neither field.
func TestApproveGovernanceChangeSaysWhenTokensWereNotRevoked(t *testing.T) {
	id := uuid.New()
	for body, want := range map[string]string{
		`{"id":"` + id.String() + `","state":"applied","tokens_revocation_failed":true,"detail":"remedy"}`: "remedy",
		`{"id":"` + id.String() + `","state":"applied"}`:                                                   "",
	} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(body)) }))
		ch, err := newTestClient(srv).ApproveGovernanceChange(context.Background(), id)
		srv.Close()
		if err != nil {
			t.Fatal(err)
		}
		if ch.ID != id || ch.Detail != want || ch.TokensRevocationFailed != (want != "") {
			t.Errorf("approve = %+v for %s; want detail %q", ch, body, want)
		}
	}
}
