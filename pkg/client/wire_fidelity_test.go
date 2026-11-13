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
