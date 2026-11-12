// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package client_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

func TestPutUnitDeciders(t *testing.T) {
	unit := uuid.New()
	rules := []types.DeciderRule{{Kinds: []string{types.DeciderKindEgressDomain}, Who: []string{types.DeciderOwner}}}

	t.Run("stored", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodPut || r.URL.Path != "/api/v1/governance/units/"+unit.String()+"/deciders" {
				t.Errorf("request = %s %s", r.Method, r.URL.Path)
			}
			var body struct {
				Rules []types.DeciderRule `json:"rules"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil || len(body.Rules) != 1 || body.Rules[0].Kinds[0] != "egress_domain" {
				t.Errorf("body = %+v, err=%v", body, err)
			}
			writeJSON(w, http.StatusOK, types.UnitDeciders{UnitID: unit, Rules: body.Rules})
		}))
		defer srv.Close()
		got, pending, err := newTestClient(srv).PutUnitDeciders(context.Background(), unit, rules)
		if err != nil || pending != nil || got.UnitID != unit || len(got.Rules) != 1 {
			t.Errorf("got %+v, pending %v, err %v", got, pending, err)
		}
	})

	t.Run("held", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			writeJSON(w, http.StatusAccepted, map[string]any{"pending_change": map[string]any{
				"id": uuid.New(), "target_kind": "governance_unit_deciders", "state": "pending",
				"proposed_at": time.Now().UTC().Format(time.RFC3339),
			}})
		}))
		defer srv.Close()
		got, pending, err := newTestClient(srv).PutUnitDeciders(context.Background(), unit, rules)
		if err != nil || pending == nil || pending.TargetKind != "governance_unit_deciders" || len(got.Rules) != 0 {
			t.Errorf("got %+v, pending %+v, err %v", got, pending, err)
		}
	})
}
