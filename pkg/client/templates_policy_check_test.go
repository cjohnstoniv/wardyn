// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package client_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/pkg/client"
)

// checkServer answers the template check route with answer, after holding the request to the
// route's shape: a POST whose body is exactly the candidate.
func checkServer(t *testing.T, id uuid.UUID, answer string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/v1/templates/"+id.String()+"/policy-check" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		checkAuth(t, r)
		body, _ := io.ReadAll(r.Body)
		var sent map[string]json.RawMessage
		var cand client.RunPolicySpec
		if json.Unmarshal(body, &sent) != nil || len(sent) != 1 || json.Unmarshal(sent["candidate"], &cand) != nil {
			t.Errorf("body = %s, want exactly the candidate", body)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(answer))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// The verdict comes back as the server wrote it. An answer that lost "within" reads as not within,
// so a gate fails closed.
func TestCheckTemplatePolicySendsTheCandidateAndReadsTheVerdict(t *testing.T) {
	id := uuid.New()
	cand := client.RunPolicySpec{AllowedDomains: []string{"api.vendor.example"}}
	srv := checkServer(t, id, `{"within":false,"widenings":[{"field":"allowed_domains","entries":["api.vendor.example"]}]}`)
	got, err := newTestClient(srv).CheckTemplatePolicy(context.Background(), id, cand)
	if err != nil {
		t.Fatalf("CheckTemplatePolicy: %v", err)
	}
	if got.Within || len(got.Widenings) != 1 || got.Widenings[0].Field != "allowed_domains" || len(got.Widenings[0].Entries) != 1 {
		t.Errorf("verdict = %+v", got)
	}

	srv = checkServer(t, id, `{"widenings":[]}`)
	if got, err := newTestClient(srv).CheckTemplatePolicy(context.Background(), id, cand); err != nil || got.Within {
		t.Errorf("an answer without within = %+v, %v: want not within", got, err)
	}
}
