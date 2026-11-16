// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"errors"
	"net/http"
	"strings"
	"testing"
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
