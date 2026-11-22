// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package client_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/pkg/client"
)

// AuditFilter.Order reaches the server as ?order=, and stays out of the query when empty.
func TestAuditEventsPageSendsOrderOnlyWhenSet(t *testing.T) {
	for _, tc := range []struct {
		order, want string
		present     bool
	}{{"desc", "desc", true}, {"", "", false}} {
		var got []string
		var has bool
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, has = r.URL.Query()["order"]
			got = r.URL.Query()["order"]
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode([]any{})
		}))
		_, _, err := client.New(srv.URL, "t").AuditEventsPage(context.Background(), uuid.New(), client.AuditFilter{Order: tc.order})
		srv.Close()
		if err != nil {
			t.Fatalf("order %q: %v", tc.order, err)
		}
		if has != tc.present || (has && (len(got) != 1 || got[0] != tc.want)) {
			t.Errorf("order %q: query order = %v (present %v), want %q (present %v)", tc.order, got, has, tc.want, tc.present)
		}
	}
}
