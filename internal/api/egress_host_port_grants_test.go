// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// With WARDYN_EGRESS_HOST_PORT_GRANTS_INERT on, a stored egress_host allow row
// carrying a port is inert: marked so and never matched. A deny row with a port
// keeps binding, and the switch off changes nothing.
func TestEgressHostPortAllowRows_InertOnlyWithTheSwitch(t *testing.T) {
	allow, deny := types.CapabilityAllow, types.CapabilityDeny
	enf := map[string]bool{capEgressHost: true}
	portAllow := grant(types.CapabilitySubjectUser, capSub, capEgressHost, "pypi.org:443", allow)
	portDeny := grant(types.CapabilitySubjectUser, capSub, capEgressHost, "pypi.org:443", deny)
	hostAllow := grant(types.CapabilitySubjectUser, capSub, capEgressHost, "pypi.org", allow)
	r := httptest.NewRequest(http.MethodGet, "/api/v1/permissions", nil)
	for _, on := range []bool{false, true} {
		srv := capServer(&capStore{grants: []types.CapabilityGrant{portAllow}, enf: enf})
		srv.cfg.EgressHostPortGrantsInert = on
		views := srv.markInertGrants(r, []types.CapabilityGrant{portAllow, portDeny, hostAllow})
		if views[0].Inert != on || views[1].Inert || views[2].Inert {
			t.Errorf("switch %v: inert = %v %v %v; want only the port allow row, and only with the switch on",
				on, views[0].Inert, views[1].Inert, views[2].Inert)
		}
		if got, err := srv.capAllowed(memberCtx(nil), capEgressHost, "pypi.org"); err != nil || got != !on {
			t.Errorf("switch %v: a port allow row grants %v, %v; want %v", on, got, err, !on)
		}
		srv = capServer(&capStore{grants: []types.CapabilityGrant{hostAllow, portDeny}, enf: enf})
		srv.cfg.EgressHostPortGrantsInert = on
		if got, err := srv.capAllowed(memberCtx(nil), capEgressHost, "pypi.org"); err != nil || got {
			t.Errorf("switch %v: a port deny row lets the host through (%v, %v)", on, got, err)
		}
	}
}
