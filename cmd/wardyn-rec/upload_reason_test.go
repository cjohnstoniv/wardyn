// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestPutCast_RefusalNamesTheOrgReason: a refused upload's error, which wardyn-rec logs, carries the
// organisation's reason token, so a laptop's log says the organisation keeps no recordings.
func TestPutCast_RefusalNamesTheOrgReason(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"error":"this organisation keeps no session recordings","reason":"recording_off"}`))
	}))
	defer srv.Close()
	err := putCast([]byte("{}\n"), srv.URL, "")
	if err == nil || !strings.Contains(err.Error(), "409") || !strings.Contains(err.Error(), `"reason":"recording_off"`) {
		t.Fatalf("err = %v, want the 409 and its recording_off reason", err)
	}
}
