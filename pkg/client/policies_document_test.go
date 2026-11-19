// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package client_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// A stored policy can hold a field a newer server knows and this build does not. GetPolicyDocument
// hands it back as written, so a strict reader can refuse it; GetPolicy drops it.
func TestGetPolicyDocumentKeepsAFieldThisBuildDoesNotKnow(t *testing.T) {
	id := uuid.New()
	body := `{"id":"` + id.String() + `","name":"p","spec":{"allowed_domains":["a.example"],"field_from_a_newer_server":true}}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/api/v1/policies/"+id.String() {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		checkAuth(t, r)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()

	doc, err := newTestClient(srv).GetPolicyDocument(context.Background(), id)
	if err != nil {
		t.Fatalf("GetPolicyDocument: %v", err)
	}
	if !strings.Contains(string(doc), `"field_from_a_newer_server":true`) {
		t.Errorf("the document lost the unknown field: %s", doc)
	}
	if typed, err := newTestClient(srv).GetPolicy(context.Background(), id); err != nil || len(typed.Spec.AllowedDomains) != 1 {
		t.Errorf("GetPolicy on the same document = %+v, %v: want it decoded, the unknown field dropped", typed, err)
	}
}
