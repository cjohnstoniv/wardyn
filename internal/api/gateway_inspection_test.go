// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// Inspection is "" (Wardyn checks prompts) or "gateway"; any other spelling is
// refused at the field, so no near miss is stored and read as either.
func TestGatewayInspectionValues(t *testing.T) {
	for _, v := range []string{"", types.GatewayInspectionGateway} {
		p := testGatewayProvider()
		p.GatewayIdentity.Inspection = v
		if err := validateProviderGatewayIdentity(p); err != nil {
			t.Errorf("%q refused: %v", v, err)
		}
	}
	for _, v := range []string{"wardyn", "Gateway", " gateway", "gateway ", "off", "none"} {
		p := testGatewayProvider()
		p.GatewayIdentity.Inspection = v
		err := validateProviderGatewayIdentity(p)
		if err == nil || !strings.Contains(err.Error(), "gateway_identity: inspection must be empty") {
			t.Errorf("%q: err = %v", v, err)
		}
	}
}

// The switch is sealed on the route, and only its exact value defers: a stored row
// that somehow holds another spelling is dispatched with Wardyn inspecting.
func TestGatewayInspectionRoute(t *testing.T) {
	for v, want := range map[string]bool{"": false, types.GatewayInspectionGateway: true, "GATEWAY": false, "gateway ": false} {
		p := testGatewayProvider()
		p.GatewayIdentity.Inspection = v
		rt, ok := gatewayRouteOf(p)
		if !ok || rt.InspectionDeferred != want {
			t.Errorf("inspection %q: route %+v, ok %v; want deferred %v", v, rt, ok, want)
		}
	}
}

// Turning the switch either way after dispatch is a route change: the next mint is
// refused, as for any other edit to the sealed route, and the switch set back passes.
func TestGatewayInspectionEditRefusesTheNextMint(t *testing.T) {
	for name, sealed := range map[string]string{"turned on": "", "turned off": types.GatewayInspectionGateway} {
		t.Run(name, func(t *testing.T) {
			f := newGatewayFixture(t)
			testGatewayIdentityOn(f.srv)
			at := testGatewayProvider()
			at.GatewayIdentity.Inspection = sealed
			f.seal(t, f.run.ID, at)
			f.st.set(func(s *gatewayStore) { s.site = testGatewaySite(at) })
			if w := f.mint(t); w.Code != http.StatusOK {
				t.Fatalf("unchanged: mint = %d %s", w.Code, w.Body)
			}
			edited := testGatewayProvider()
			edited.GatewayIdentity.Inspection = map[string]string{"": types.GatewayInspectionGateway, types.GatewayInspectionGateway: ""}[sealed]
			f.st.set(func(s *gatewayStore) { s.site = testGatewaySite(edited) })
			if w := f.mint(t); w.Code != http.StatusForbidden || reasonOf(t, w) != reasonGatewayIdentityChanged {
				t.Fatalf("edited: mint = %d %s", w.Code, w.Body)
			}
			f.st.set(func(s *gatewayStore) { s.site = testGatewaySite(at) })
			if w := f.mint(t); w.Code != http.StatusOK {
				t.Fatalf("set back: mint = %d %s", w.Code, w.Body)
			}
		})
	}
}

// The run's coverage line names each provider whose sealed route defers, by its name
// now, or its id once it is gone; a run with no sealed config names none.
func TestGatewayInspectionLeftTo(t *testing.T) {
	f := newGatewayFixture(t)
	ctx := context.Background()
	if got := f.srv.inspectionLeftTo(ctx, f.run.ID, testGatewaySite(testGatewayProvider())); got != nil {
		t.Fatalf("switch off: %v", got)
	}
	p := testGatewayProvider()
	p.Name, p.GatewayIdentity.Inspection = "Corp gateway", types.GatewayInspectionGateway
	f.seal(t, f.run.ID, p)
	if got := f.srv.inspectionLeftTo(ctx, f.run.ID, testGatewaySite(p)); !slices.Equal(got, []string{"Corp gateway"}) {
		t.Fatalf("switch on: %v", got)
	}
	if got := f.srv.inspectionLeftTo(ctx, f.run.ID, testGatewaySite()); !slices.Equal(got, []string{"corp-gw"}) {
		t.Fatalf("provider gone: %v", got)
	}
	if got := f.srv.inspectionLeftTo(ctx, uuid.New(), testGatewaySite(p)); got != nil {
		t.Fatalf("no sealed config: %v", got)
	}
}

// A Bedrock key row's calls are scanned by the generic forward check, which no route
// defers: the switch is refused when saved, never sealed, and so never named on the run.
func TestGatewayInspectionNotForBedrock(t *testing.T) {
	p := testGatewayBedrockBearer()
	p.GatewayIdentity.Inspection = types.GatewayInspectionGateway
	if err := validateProviderGatewayIdentity(p); err == nil || !strings.Contains(err.Error(), "inspection is not available for a Bedrock provider") {
		t.Fatalf("err = %v", err)
	}
	rt, ok := gatewayRouteOf(p)
	if !ok || rt.InspectionDeferred {
		t.Fatalf("route %+v, ok %v: a Bedrock route must not defer", rt, ok)
	}
	f := newGatewayFixture(t)
	f.seal(t, f.run.ID, p)
	if got := f.srv.inspectionLeftTo(context.Background(), f.run.ID, testGatewaySite(p)); got != nil {
		t.Fatalf("a Bedrock run claims its prompts were left to the gateway: %v", got)
	}
}
