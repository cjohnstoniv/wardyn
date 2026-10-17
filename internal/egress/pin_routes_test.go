// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package egress

import (
	"net/http"
	"testing"
)

// A rule with PinRoutes admits exactly its method-and-path pairs (full path, exact, query ignored) and
// nothing else; with PinPath as well, a request must satisfy both.
func TestInjectionRuleAllowsInjection_PinRoutes(t *testing.T) {
	r := InjectionRule{PinRoutes: []PinRoute{
		{Method: http.MethodPost, Path: "/anthropic/v1/messages"},
		{Method: http.MethodPost, Path: "/anthropic/v1/messages/count_tokens"},
	}}
	if !r.Pinned() {
		t.Fatal("a rule with PinRoutes is not Pinned")
	}
	for _, c := range []struct {
		method, path, query string
		want                bool
	}{
		{http.MethodPost, "/anthropic/v1/messages", "", true},
		{http.MethodPost, "/anthropic/v1/messages", "a=b;c", true},
		{http.MethodPost, "/anthropic/v1/messages/count_tokens", "", true},
		{http.MethodGet, "/anthropic/v1/messages", "", false},
		{http.MethodPost, "/anthropic/v1/messages/", "", false},
		{http.MethodPost, "/anthropic/v1/message", "", false},
		{http.MethodPost, "/openai/files", "", false},
	} {
		if got := r.AllowsInjection(c.method, c.path, c.query); got != c.want {
			t.Errorf("AllowsInjection(%s %s ?%s) = %v, want %v", c.method, c.path, c.query, got, c.want)
		}
	}
	both := InjectionRule{PinPath: "/p", PinRoutes: []PinRoute{{Method: http.MethodGet, Path: "/p"}, {Method: http.MethodPost, Path: "/q"}}}
	if !both.AllowsInjection(http.MethodGet, "/p", "") || both.AllowsInjection(http.MethodPost, "/q", "") {
		t.Error("with PinPath and PinRoutes both set, a request must satisfy both")
	}
}
