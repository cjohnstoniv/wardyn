// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/runnerpool"
)

// TestRunnerPoolRoutesAnswerTheStubRefusal pins what every reserved pool route
// says until the pool storage lands: 501, the reason and the sentence.
func TestRunnerPoolRoutesAnswerTheStubRefusal(t *testing.T) {
	fill := strings.NewReplacer("{id}", "11111111-1111-1111-1111-111111111111", "{runner}", "22222222-2222-2222-2222-222222222222", "{executor}", "build-1")
	var routes []string
	for key := range routeMatrix {
		if _, path, _ := strings.Cut(key, " "); strings.HasPrefix(path, "/api/v1/runner-pool") || strings.HasPrefix(path, "/api/v1/me/runner-pool") {
			routes = append(routes, key)
		}
	}
	if len(routes) != 17 {
		t.Fatalf("%d pool routes are classified, want 17", len(routes))
	}
	h := newHarness(t)
	for _, key := range routes {
		method, path, _ := strings.Cut(key, " ")
		w := do(t, h.srv, method, fill.Replace(path), adminToken, "{}")
		var body struct{ Error, Reason string }
		if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
			t.Fatalf("%s: %v", key, err)
		}
		if w.Code != http.StatusNotImplemented || body.Reason != string(runnerpool.ReasonPoolsUnavailable) || body.Error != runnerpool.UnavailableServerMsg() {
			t.Errorf("%s = %d %q %q, want 501 %s %q", key, w.Code, body.Reason, body.Error, runnerpool.ReasonPoolsUnavailable, runnerpool.UnavailableServerMsg())
		}
	}
}
