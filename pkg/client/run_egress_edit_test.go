// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package client_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/pkg/client"
)

func TestEditRunEgress(t *testing.T) {
	id := uuid.New()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPatch || r.URL.Path != "/api/v1/runs/"+id.String()+"/egress" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		checkAuth(t, r)
		var body map[string][]string
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if len(body) != 2 || !slices.Equal(body["remove_hosts"], []string{"a.example"}) || !slices.Equal(body["deny_hosts"], []string{"b.example"}) {
			t.Errorf("body = %v, want only remove_hosts and deny_hosts", body)
		}
		writeJSON(w, http.StatusOK, map[string]any{"policy_generation": 4})
	}))
	defer srv.Close()

	resp, err := newTestClient(srv).EditRunEgress(context.Background(), id,
		client.EditRunEgressRequest{RemoveHosts: []string{"a.example"}, DenyHosts: []string{"b.example"}})
	if err != nil || resp.PolicyGeneration != 4 {
		t.Fatalf("EditRunEgress = %+v, %v; want generation 4", resp, err)
	}
}

func TestEditRunEgress_Unconfirmed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "not confirmed", "reason": "egress_edit_unconfirmed"})
	}))
	defer srv.Close()

	_, err := newTestClient(srv).EditRunEgress(context.Background(), uuid.New(), client.EditRunEgressRequest{DenyHosts: []string{"b.example"}})
	var apiErr *client.APIError
	if !errors.As(err, &apiErr) || apiErr.Status != http.StatusServiceUnavailable || apiErr.Reason != "egress_edit_unconfirmed" {
		t.Fatalf("err = %v, want the 503 egress_edit_unconfirmed APIError", err)
	}
}
