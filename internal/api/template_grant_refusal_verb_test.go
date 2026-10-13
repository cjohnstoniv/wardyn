// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"net/http"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
)

// A refused revoke says no grant was removed; a refused grant says none was written.
func TestTemplateGrantRefusalNamesTheVerb(t *testing.T) {
	e := newTemplateEnv(t)
	root := templateCookie(t, "root", oidc.RoleAdmin, false, []string{}, false)
	e.signIn("bob", "shared@corp.example")
	e.signIn("mallory", "shared@corp.example")
	call := func(method, person string) string {
		status, body := e.call(root, method, "/api/v1/admin/template-group-admins", `{"group_id":"eng","person":"`+person+`"}`)
		if status != http.StatusUnprocessableEntity {
			t.Fatalf("%s %s = %d %s, want 422", method, person, status, body)
		}
		return body
	}
	for _, person := range []string{"shared@corp.example", "nobody@corp.example"} {
		if body := call(http.MethodDelete, person); !strings.Contains(body, "no grant was removed") || strings.Contains(body, "no grant was written") {
			t.Errorf("revoke of %s refused with %s, want \"no grant was removed\"", person, body)
		}
		if body := call(http.MethodPut, person); !strings.Contains(body, "no grant was written") || strings.Contains(body, "no grant was removed") {
			t.Errorf("grant of %s refused with %s, want \"no grant was written\"", person, body)
		}
	}
	if body := call(http.MethodDelete, "nobody@corp.example"); strings.Contains(body, "grant after they first sign in") {
		t.Errorf("a revoke was told to grant after sign-in: %s", body)
	}
}
