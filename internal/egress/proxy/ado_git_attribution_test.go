// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/adoscope"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestADOGitRefusalAttributesOnlyPolicyDecisions: the organisation pin is a
// policy decision and names the governing policy; an encoded push body is the
// proxy being unable to read the request, so its refusal names none.
func TestADOGitRefusalAttributesOnlyPolicyDecisions(t *testing.T) {
	const governed = "This run is governed by"
	p, _, _ := newEntraProxy(t, types.RunPolicySpec{}, "127.0.0.1:1", adoscope.CapCodeWrite)
	p.attribution = teamA

	enc := httptest.NewRecorder()
	req := mustLocalReq(t, http.MethodPost, "/wardyn/git/dev.azure.com/acme/proj/_git/app/git-receive-pack", strings.NewReader("gzipped"))
	req.Header.Set("Content-Encoding", "gzip")
	p.ServeHTTP(enc, req)
	if enc.Code != http.StatusForbidden || !strings.Contains(enc.Body.String(), "an encoded push body cannot be checked") {
		t.Fatalf("encoded push = %d %q, want the encoded-body refusal", enc.Code, enc.Body.String())
	}
	if strings.Contains(enc.Body.String(), governed) || enc.Header().Get("X-Wardyn-Policy") != "" {
		t.Errorf("an unreadable push is attributed to a policy: %q %v", enc.Body.String(), enc.Header())
	}

	org := httptest.NewRecorder()
	p.ServeHTTP(org, mustLocalReq(t, http.MethodPost, "/wardyn/git/dev.azure.com/evil/proj/_git/app/git-receive-pack", strings.NewReader("")))
	if !strings.Contains(org.Body.String(), "organisation only") ||
		!strings.Contains(org.Body.String(), governed) || org.Header().Get("X-Wardyn-Policy") == "" {
		t.Errorf("organisation-pin refusal = %d %q %v, want it attributed", org.Code, org.Body.String(), org.Header())
	}
}
