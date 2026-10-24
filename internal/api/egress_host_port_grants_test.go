// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// With WARDYN_EGRESS_HOST_PORT_GRANTS_INERT on (EgressHostPortGrantsMatch false), a stored egress_host allow row
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
		srv.cfg.EgressHostPortGrantsMatch = !on
		views := srv.markInertGrants(r, []types.CapabilityGrant{portAllow, portDeny, hostAllow})
		if views[0].Inert != on || views[1].Inert || views[2].Inert {
			t.Errorf("switch %v: inert = %v %v %v; want only the port allow row, and only with the switch on",
				on, views[0].Inert, views[1].Inert, views[2].Inert)
		}
		if got, err := srv.capAllowed(memberCtx(nil), capEgressHost, "pypi.org"); err != nil || got != !on {
			t.Errorf("switch %v: a port allow row grants %v, %v; want %v", on, got, err, !on)
		}
		srv = capServer(&capStore{grants: []types.CapabilityGrant{hostAllow, portDeny}, enf: enf})
		srv.cfg.EgressHostPortGrantsMatch = !on
		if got, err := srv.capAllowed(memberCtx(nil), capEgressHost, "pypi.org"); err != nil || got {
			t.Errorf("switch %v: a port deny row lets the host through (%v, %v)", on, got, err)
		}
	}
}

// The access step's decider follows the same switch: with it on (EgressHostPortGrantsMatch false), a
// member whose only egress_host allow row carries a port decides nothing for themselves (admin); with
// it off that row still makes them a decider (you_or_admin). A host allow row keeps them one either way.
func TestEgressHostPortAllowRows_DeciderExcludesThemOnlyWithTheSwitch(t *testing.T) {
	enf := map[string]bool{capEgressHost: true}
	portAllow := grant(types.CapabilitySubjectUser, capSub, capEgressHost, "pypi.org:443", types.CapabilityAllow)
	hostAllow := grant(types.CapabilitySubjectUser, capSub, capEgressHost, "pypi.org", types.CapabilityAllow)
	for _, tc := range []struct {
		grants []types.CapabilityGrant
		on     bool
		want   string
	}{
		{[]types.CapabilityGrant{portAllow}, false, deciderYouOrAdmin},
		{[]types.CapabilityGrant{portAllow}, true, deciderAdmin},
		{[]types.CapabilityGrant{portAllow, hostAllow}, true, deciderYouOrAdmin},
	} {
		srv := capServer(&capStore{grants: tc.grants, enf: enf})
		srv.cfg.EgressHostPortGrantsMatch = !tc.on
		if got, err := srv.accessDecider(memberCtx(nil)); err != nil || got != tc.want {
			t.Errorf("switch %v, %d row(s): decider = %q, %v; want %q", tc.on, len(tc.grants), got, err, tc.want)
		}
	}
}

// WARDYN_EGRESS_HOST_PORT_GRANTS_INERT unset is on: with EgressHostPortGrantsMatch at its zero value a
// stored egress_host allow row carrying a port is marked inert and grants nothing.
func TestEgressHostPortAllowRows_DefaultInert(t *testing.T) {
	portAllow := grant(types.CapabilitySubjectUser, capSub, capEgressHost, "pypi.org:443", types.CapabilityAllow)
	srv := capServer(&capStore{grants: []types.CapabilityGrant{portAllow}, enf: map[string]bool{capEgressHost: true}})
	r := httptest.NewRequest(http.MethodGet, "/api/v1/permissions", nil)
	if views := srv.markInertGrants(r, []types.CapabilityGrant{portAllow}); !views[0].Inert {
		t.Error("default: a port allow row is not marked inert")
	}
	if got, err := srv.capAllowed(memberCtx(nil), capEgressHost, "pypi.org"); err != nil || got {
		t.Errorf("default: a port allow row grants %v, %v; want false", got, err)
	}
}

// Boot logs both default-on grant switches and how many stored egress_host allow rows carry a port and
// so went inert: all of them by default, none with the inert switch off.
func TestLogGrantSwitches_StatesAndInertCount(t *testing.T) {
	grants := []types.CapabilityGrant{
		grant(types.CapabilitySubjectUser, capSub, capEgressHost, "pypi.org:443", types.CapabilityAllow),
		grant(types.CapabilitySubjectUser, capSub, capEgressHost, "files.example.com:8443", types.CapabilityAllow),
		grant(types.CapabilitySubjectUser, capSub, capEgressHost, "pypi.org:443", types.CapabilityDeny),
		grant(types.CapabilitySubjectUser, capSub, capEgressHost, "pypi.org", types.CapabilityAllow),
	}
	for _, tc := range []struct {
		off  bool
		want []string
	}{
		{false, []string{"WARDYN_GOVERNANCE_UNIT_PUT_EXISTING_ONLY=true", "WARDYN_EGRESS_HOST_PORT_GRANTS_INERT=true", "egress_host_port_rows=2", "inert_rows=2"}},
		{true, []string{"WARDYN_GOVERNANCE_UNIT_PUT_EXISTING_ONLY=false", "WARDYN_EGRESS_HOST_PORT_GRANTS_INERT=false", "egress_host_port_rows=2", "inert_rows=0"}},
	} {
		srv := capServer(&capStore{grants: grants})
		srv.cfg.GovernanceUnitPutCreates, srv.cfg.EgressHostPortGrantsMatch = tc.off, tc.off
		var logged lockedBuffer
		prev := slog.Default()
		slog.SetDefault(slog.New(slog.NewTextHandler(&logged, nil)))
		srv.LogGrantSwitches(context.Background())
		slog.SetDefault(prev)
		out := logged.String()
		if strings.Count(out, "grant switches") != 1 {
			t.Errorf("switches off=%v: logged %q, want one grant switches line", tc.off, out)
		}
		for _, w := range tc.want {
			if !strings.Contains(out, w) {
				t.Errorf("switches off=%v: log %q lacks %s", tc.off, out, w)
			}
		}
	}
}
