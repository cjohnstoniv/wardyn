// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
)

// TestAuditSignInDenied_EmailPolicy: the three email-policy refusals the
// callback reports (#155) are auth.fail rows from the callback's own boundary,
// each under its own closed-enum reason.
func TestAuditSignInDenied_EmailPolicy(t *testing.T) {
	h := newHarness(t)
	srv := New(baseTestConfig(h, &roleMapStore{}))
	reasons := []string{oidc.DenialEmailVerifiedAbsent, oidc.DenialEmailUnverified, oidc.DenialEmailDomain}
	for _, reason := range reasons {
		srv.auditSignInDenied(httptest.NewRequest(http.MethodGet, "/auth/callback", nil), reason)
	}
	var got []string
	for _, ev := range h.audit.snapshot() {
		if ev.Action != "auth.fail" {
			continue
		}
		if ev.Actor != oidcCallbackActor {
			t.Errorf("actor = %q, want %q", ev.Actor, oidcCallbackActor)
		}
		var data struct {
			Reason string `json:"reason"`
		}
		_ = json.Unmarshal(ev.Data, &data)
		got = append(got, data.Reason)
	}
	if !slices.Equal(got, []string{"email_verified_absent", "email_unverified", "email_domain"}) {
		t.Errorf("auth.fail reasons = %v, want the three email-policy refusals", got)
	}
}
