// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package vaultkv

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// A response of exactly maxVaultResponse bytes decodes; one byte more is an
// explicit error and never a decode of a truncated body.
func TestClient_ResponseSizeBoundary(t *testing.T) {
	body := func(n int) string { // {"v":"aaa…"} of exactly n bytes
		const wrap = len(`{"v":""}`)
		return `{"v":"` + strings.Repeat("a", n-wrap) + `"}`
	}
	for name, tc := range map[string]struct {
		size    int
		wantErr bool
	}{
		"1 MiB, the old cap": {1 << 20, false},
		"just over 1 MiB":    {1<<20 + 1, false},
		"at 16 MiB":          {16 << 20, false},
		"16 MiB plus a byte": {16<<20 + 1, true},
	} {
		t.Run(name, func(t *testing.T) {
			payload := body(tc.size)
			vault := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(payload)) }))
			t.Cleanup(vault.Close)
			c := redirectClient(t, vault.URL)
			var out struct {
				V string `json:"v"`
			}
			_, err := c.call(t.Context(), http.MethodGet, "wardyn/data/k", nil, &out)
			if tc.wantErr {
				if err == nil || !strings.Contains(err.Error(), "response is over") {
					t.Fatalf("err = %v, want the explicit oversize error", err)
				}
				return
			}
			if err != nil || len(out.V) != tc.size-len(`{"v":""}`) {
				t.Fatalf("err = %v, decoded %d bytes; want the whole body", err, len(out.V))
			}
		})
	}
}
