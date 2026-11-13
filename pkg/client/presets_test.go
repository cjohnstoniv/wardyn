// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package client_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/cjohnstoniv/wardyn/pkg/client"
)

// A write that fails after earlier ones stored returns those presets with the error.
func TestApplyPresetsReturnsTheStoredOnesOnAFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		checkAuth(t, r)
		if r.URL.Path == "/api/v1/presets/bad" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "preset_invalid"})
			return
		}
		writeJSON(w, http.StatusOK, client.Preset{Name: "good", Version: 1})
	}))
	defer srv.Close()
	doc := client.PresetsDocument{Presets: []client.Preset{{Name: "good"}, {Name: "bad"}, {Name: "after"}}}
	out, err := newTestClient(srv).ApplyPresets(context.Background(), doc)
	if err == nil || len(out.Presets) != 1 || out.Presets[0].Name != "good" {
		t.Fatalf("ApplyPresets = %+v, %v; want the one stored preset and an error", out, err)
	}
}
