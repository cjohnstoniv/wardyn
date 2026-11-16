// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// A refusal for a component's cut is named to the sandbox by a sentence of the proxy's own, and the
// holder asks again later, as for any refusal but the portal's cut; another reason, or a body that only
// looks like one, keeps the refusal's own text.
func TestGatewayIdentity_ComponentCutNamed(t *testing.T) {
	for body, named := range map[string]bool{
		`{"reason":"capability_component"}`:  true,
		`{"reason":"component_gone"}`:        true,
		`{"reason":"component_unit_pinned"}`: true,
		`{"reason":"identity_deactivated"}`:  false,
		`{"reason":"Component_Gone"}`:        false,
		`{"message":"component_gone"}`:       false,
	} {
		g := newGwidRig(t, gwidRoutes(), false)
		g.answerBody(http.StatusForbidden, body)
		_, _, drop, err := g.p.gatewayHolders["prov-1"].mint(t.Context(), g.p)
		if err == nil || !drop || errors.Is(err, errGatewayAccessCut) {
			t.Fatalf("%s: mint = drop %v, %v; want a definite refusal that leaves the holder asking", body, drop, err)
		}
		if got := strings.Contains(err.Error(), gatewayComponentCutSentence); got != named {
			t.Errorf("%s: names the cut = %v, want %v: %v", body, got, named, err)
		}
	}
}

// A component's cut is a 403 to the sandbox on every lane, with the proxy's sentence, not a 502 an SDK
// retries at once; the holder still asks again after its interval, so a grant given back admits a later mint.
func TestGatewayIdentity_ComponentCutIsA403(t *testing.T) {
	for lane := range gwidLanes(newGwidRig(t, gwidRoutes(), false), t) {
		t.Run(lane, func(t *testing.T) {
			g := newGwidRig(t, gwidRoutes(), false)
			g.answerBody(http.StatusForbidden, `{"reason":"component_gone"}`)
			send := gwidLanes(g, t)[lane]
			rec := send()
			if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), gatewayComponentCutSentence) {
				t.Fatalf("status %d, want 403 with the cut sentence: %s %v", rec.Code, rec.Body, rec.Header())
			}
			g.advance(lastGoodRetry + time.Second)
			if rec := send(); rec.Code != http.StatusForbidden {
				t.Fatalf("after the interval: status %d, want 403: %s", rec.Code, rec.Body)
			}
			if n := g.mint.hits.Load(); n != 2 {
				t.Errorf("mints = %d, want 2: the holder asks again after its interval", n)
			}
		})
	}
}

// On an MCP server's own route a component's cut is the same 403 with the proxy's reason and sentence, and the
// server is sent nothing.
func TestMCPBroker_GatewayComponentCutIsA403(t *testing.T) {
	f := newMCPFixture(t, types.RunPolicySpec{AllowedDomains: []string{"mcp.linear.test", "mcp.github.test"}}, nil, nil)
	t.Cleanup(func() { procRegistry.Evict(f.p.maskKey) })
	withMCPGatewayRoute(f, "linear", "/mcp")
	f.up.mintStatus, f.up.mintRefusal = http.StatusForbidden, `{"reason":"component_gone"}`
	rec := f.post(t, mcpToken("claude-code"), "linear", mcpInit)
	if rec.Code != http.StatusForbidden || rec.Header().Get(egressHeaderWardynReason) != reasonGatewayComponentCut ||
		!strings.Contains(rec.Body.String(), gatewayComponentCutSentence) || f.up.hitCount() != 0 {
		t.Fatalf("status %d, reason %q, hits %d: %s; want 403 %s with the cut sentence and nothing sent",
			rec.Code, rec.Header().Get(egressHeaderWardynReason), f.up.hitCount(), rec.Body, reasonGatewayComponentCut)
	}
}
