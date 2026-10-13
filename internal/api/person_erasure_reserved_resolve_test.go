// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"net/http"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
)

// An ordinary-looking name that the identity directory resolves onto a
// reserved principal is refused on both erase routes after the resolve, not
// only when the typed name is itself reserved.
func TestErase_RefusesANameThatResolvesToAReservedPrincipal(t *testing.T) {
	for _, rt := range []struct{ name, method, path, body string }{
		{"person erasure", http.MethodPost, "/api/v1/people/bob/erasure", erasureBody("run_tasks", "credentials")},
		{"credential erase", http.MethodDelete, "/api/v1/people/bob/credentials", ""},
	} {
		t.Run(rt.name, func(t *testing.T) {
			_, srv, st, sec := erasureRig(t)
			srv.cfg.Store = &aliasingStore{erasureTestStore: st, forName: map[string]string{"bob": "admin-token"}}
			sa := ssoSession(t, "sec-2", "sec2@corp.example", oidc.RoleSecurityAdmin)
			before := len(namesOf(t, sec, "bob"))
			w := doSSO(t, srv, rt.method, rt.path, sa, rt.body)
			if w.Code != http.StatusUnprocessableEntity || errorReason(w) != reasonReservedPrincipal {
				t.Fatalf("%s = %d %s, want 422 %s", rt.name, w.Code, w.Body, reasonReservedPrincipal)
			}
			if len(st.blanked) != 0 || len(namesOf(t, sec, "bob")) != before {
				t.Fatalf("a refused request erased something: blanked %v", st.blanked)
			}
		})
	}
}
