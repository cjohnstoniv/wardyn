// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package client_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/pkg/client"
)

func TestSetAndClearEnvelope(t *testing.T) {
	unit := uuid.New()
	path := "/api/v1/governance/units/" + unit.String() + "/envelope"
	var held bool
	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != path {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		switch {
		case held:
			writeJSON(w, http.StatusAccepted, map[string]any{"pending_change": map[string]any{
				"id": uuid.NewString(), "target_kind": "governance_envelope", "op": r.Method, "state": "pending"}})
		case r.Method == http.MethodPut:
			gotBody, _ = io.ReadAll(r.Body)
			writeJSON(w, http.StatusOK, client.GovernanceEnvelope{UnitID: unit, ApprovedGroups: []string{"plat-a"}, SetBy: "sec"})
		default:
			w.WriteHeader(http.StatusNoContent)
		}
	}))
	defer srv.Close()
	c := newTestClient(srv)
	ctx := context.Background()

	env, pending, err := c.SetEnvelope(ctx, unit, []string{"plat-a"})
	if err != nil || pending != nil || env.UnitID != unit || !slices.Equal(env.ApprovedGroups, []string{"plat-a"}) {
		t.Fatalf("SetEnvelope = %+v, %v, %v", env, pending, err)
	}
	var body map[string][]string
	if json.Unmarshal(gotBody, &body) != nil || !slices.Equal(body["approved_groups"], []string{"plat-a"}) {
		t.Errorf("request body = %s", gotBody)
	}
	if pending, err := c.ClearEnvelope(ctx, unit); err != nil || pending != nil {
		t.Fatalf("ClearEnvelope = %v, %v", pending, err)
	}

	held = true
	if _, pending, err := c.SetEnvelope(ctx, unit, nil); err != nil || pending == nil || pending.TargetKind != "governance_envelope" {
		t.Fatalf("held SetEnvelope = %+v, %v", pending, err)
	}
	if pending, err := c.ClearEnvelope(ctx, unit); err != nil || pending == nil {
		t.Fatalf("held ClearEnvelope = %+v, %v", pending, err)
	}
}
