// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// A server refusal outside the helper's named cases shows the server's own sentence, and a body with
// no error field keeps the raw status text.
func TestCallMint_ShowsTheServersRefusalSentence(t *testing.T) {
	for _, tc := range []struct {
		name, body, want string
		status           int
	}{
		{"revoked", `{"error":"this run's credentials were revoked, so no credential is minted for it","reason":"grant_run_revoked"}`,
			"mint refused (403): this run's credentials were revoked, so no credential is minted for it", http.StatusForbidden},
		{"no error field", `{"code":"x"}`, `unexpected mint status 403: {"code":"x"}`, http.StatusForbidden},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()
			_, _, _, err := callMint(context.Background(), srv.Client(), srv.URL, "g")
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %v, want %q", err, tc.want)
			}
		})
	}
}
