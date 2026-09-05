// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"net/http"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
)

// TestMemberCannotReadOperatorTopologyRoutes is the counterfactual F092 asks for
// and no lane had: a MEMBER session must not reach the three documents that
// carry an upstream-proxy password ref, a local_dir HOST PATH and an internal
// registry image.
//
// The R1 answer to F092 was reclassification rather than projection
// (mountLibraryRoutes' own note: nothing member-facing consumes either route),
// and that answer is only as durable as a test that fails when a route moves
// back. Nothing asserted it: authz_test.go's matrix classifies routes but a
// route re-registered on the wide group is simply reclassified with it.
//
// This is a ROUTE-TIER assertion by design — the leak is reachability, not
// response shape, so the pin has to be the status code a plain member gets.
func TestMemberCannotReadOperatorTopologyRoutes(t *testing.T) {
	h := newHarness(t)
	cfg := baseTestConfig(h, r3PlainStore{})
	cfg.OIDC = &oidc.Authenticator{}
	srv := New(cfg)
	member := ssoSession(t, "sub-plain-member", "member@corp.example", oidc.RoleMember)

	for _, path := range []string{
		"/api/v1/site-config",
		"/api/v1/sources",
		"/api/v1/sources/6b1f0e7a-1f4d-4f1e-9a2c-0d3e5f6a7b8c",
		"/api/v1/base-images",
	} {
		w := doSSO(t, srv, http.MethodGet, path, member, "")
		if w.Code != http.StatusForbidden {
			t.Errorf("member GET %s = %d, want 403: this document carries credential REFS and operator "+
				"topology (upstream_proxy_secret_ref, local_dir host paths, internal registry images), and R1 "+
				"decided the answer is admin-tier reclassification rather than a member projection; body=%s",
				path, w.Code, w.Body.String())
		}
	}
}
