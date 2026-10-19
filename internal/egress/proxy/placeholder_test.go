// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// A placeholder rule swaps the sandbox's placeholder for the secret in each
// request header to its host and strips nothing; a rule without one replaces
// the sandbox's Authorization with the header the control plane resolved
// (mintServer: Authorization), as before.
func TestPlaceholderRuleFillsTheSandboxsHeader(t *testing.T) {
	cp := mintServer(t, "tok")
	defer cp.Close()
	pol := CompilePolicy(types.RunPolicySpec{AllowedDomains: []string{"api.test"}})
	gid := uuid.New()
	value := "Bearer minted-" + gid.String()[:8] // what mintServer resolves
	const ph = "wardyn-placeholder:abc"
	for _, placeholder := range []string{"", ph} {
		inj, err := buildInjector(context.Background(), cp.URL, newTokenSource("tok"), pol, []InjectionConfig{{
			InjectionRule: egress.InjectionRule{Host: "api.test", Header: "Authorization", Format: "%s", Placeholder: placeholder},
			GrantID:       gid,
		}}, cp.Client())
		if err != nil {
			t.Fatalf("buildInjector: %v", err)
		}
		for _, lane := range []string{"plain", "intercepted"} {
			h := http.Header{"Authorization": {"Token " + ph}, "X-Other": {ph + "," + ph}}
			if lane == "plain" {
				req := httptest.NewRequest(http.MethodGet, "http://api.test/v1", nil)
				req.Header = h
				inj.apply(req, "api.test", 80)
			} else {
				hdr, _, _ := inj.resolve("api.test")
				applyCredential(h, hdr.name, &hdr)
			}
			if placeholder == "" {
				if h.Get("Authorization") != value || h.Get("X-Other") != ph+","+ph {
					t.Errorf("%s, no placeholder: headers %v, want Authorization replaced and X-Other untouched", lane, h)
				}
				continue
			}
			if h.Get("Authorization") != "Token "+value || h.Get("X-Other") != value+","+value {
				t.Errorf("%s, placeholder: headers %v, want the placeholder filled in place and nothing set", lane, h)
			}
		}
	}
}
