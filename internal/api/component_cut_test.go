// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/egress/proxy"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// gatewayWithdrawnStore is withdrawnStore with a run's component snapshot and the units a pin is read
// against, for the gateway mint's MCP arm.
type gatewayWithdrawnStore struct {
	*withdrawnStore
	runComps []types.RunComponent
	units    []types.GovernanceUnit
}

func (s *gatewayWithdrawnStore) ListRunComponents(context.Context, uuid.UUID) ([]types.RunComponent, error) {
	return s.runComps, nil
}

func (s *gatewayWithdrawnStore) ListGovernanceUnits(context.Context) ([]types.GovernanceUnit, error) {
	return s.units, nil
}

// gatewayCutStore is the gateway fixture's store with the component store the checks read, its
// components and the run's snapshot still the fixture's, and no record of any sign-in.
type gatewayCutStore struct {
	*gatewayStore
	fakeComponentStore
}

func (s *gatewayCutStore) GetComponent(ctx context.Context, id uuid.UUID, owner string) (types.Component, error) {
	return s.gatewayStore.GetComponent(ctx, id, owner)
}

func (s *gatewayCutStore) ListRunComponents(ctx context.Context, runID uuid.UUID) ([]types.RunComponent, error) {
	return s.gatewayStore.ListRunComponents(ctx, runID)
}

func (s *gatewayCutStore) GetPrincipalGroups(context.Context, string, string) (store.PrincipalGroups, error) {
	return store.PrincipalGroups{}, store.ErrNotFound
}

var (
	_ store.ComponentStore  = (*gatewayWithdrawnStore)(nil)
	_ principalGroupsReader = (*gatewayWithdrawnStore)(nil)
	_ store.ComponentStore  = (*gatewayCutStore)(nil)
	_ principalGroupsReader = (*gatewayCutStore)(nil)
)

// gatewayWithdrawnFixture is a running run that carries an organisation's MCP server, restricted and
// held by its owner through the group "eng", which the record of the owner's last sign-in names.
func gatewayWithdrawnFixture(t *testing.T) (*Server, *gatewayWithdrawnStore, types.AgentRun, proxy.GatewayIdentityRoute) {
	t.Helper()
	_, ws, run, _ := withdrawnFixture(t)
	c := testGatewayMCPComponent()
	ws.components[c.ID] = c
	ws.capStore.restricted = map[string]map[string]bool{capComponent: {c.ID.String(): true}}
	ws.grants = []types.CapabilityGrant{grant(types.CapabilitySubjectGroup, "eng", capComponent, c.ID.String(), types.CapabilityAllow)}
	ws.records[capSub] = store.PrincipalGroups{Email: capEmail, Groups: []string{"eng"}}
	id := c.ID
	st := &gatewayWithdrawnStore{withdrawnStore: ws,
		runComps: []types.RunComponent{{RunID: run.ID, ComponentID: &id, Name: c.Name, Version: c.Version, Definition: c.Definition}}}
	rt, ok := gatewayMCPRouteOf(c.Definition.MCP.URL, "tools", c.Definition.MCP.GatewayAudience, gatewayMCPUID(c.ID, c.Version))
	if !ok {
		t.Fatalf("no route for %+v", c.Definition.MCP)
	}
	return capServer(st), st, run, rt
}

// An organisation MCP server's gateway token is asked what a credential mint asks of the run's
// components: given while the owner holds the server through a group; refused, with the credential
// doors' reason and text, once the grant is withdrawn, once the server is pinned to a unit the run is
// not in, and once another component the run carries is deleted; the door's own 503 when the owner's
// record cannot be read. A server changed or deleted since dispatch keeps the door's own answer.
func TestGatewayMCPMintAsksTheRunsComponents(t *testing.T) {
	srv, st, run, rt := gatewayWithdrawnFixture(t)
	ctx := t.Context()
	if dest, _, ref := srv.gatewayMintAdmitMCP(ctx, run, rt, ""); ref != nil || dest.mcp != "tools" {
		t.Fatalf("held through a group: %+v, %+v; want the server's token", dest, ref)
	}
	refused := func(step string, status int, reason string) {
		t.Helper()
		_, _, ref := srv.gatewayMintAdmitMCP(ctx, run, rt, "")
		if ref == nil || ref.status != status || ref.reason != reason {
			t.Fatalf("%s: refusal = %+v, want %d %s", step, ref, status, reason)
		}
		if (reason == reasonOwnerCapabilityComponent || reason == reasonOwnerComponentGone) && ref.msg != componentWithdrawnMsgs[reason] {
			t.Fatalf("%s: text %q, want the credential doors' own", step, ref.msg)
		}
	}
	held := st.grants
	st.grants = nil
	refused("grant withdrawn", http.StatusForbidden, reasonOwnerCapabilityComponent)
	st.grants = held

	a, b := uuid.New(), uuid.New()
	st.units = []types.GovernanceUnit{{ID: a, Name: "a"}, {ID: b, Name: "b"}}
	c := st.components[*st.runComps[0].ComponentID]
	c.OwningUnitID = &b
	st.components[c.ID] = c
	pinnedRun := run
	pinnedRun.UnitID = &a
	if _, _, ref := srv.gatewayMintAdmitMCP(ctx, pinnedRun, rt, ""); ref == nil || ref.status != http.StatusForbidden ||
		ref.reason != reasonComponentUnitPinned || ref.msg != componentPinnedMsg {
		t.Fatalf("pinned to another unit: refusal = %+v, want 403 %s", ref, reasonComponentUnitPinned)
	}
	c.OwningUnitID = nil
	st.components[c.ID] = c

	st.recordErr = errors.New("db down")
	refused("record unread", http.StatusServiceUnavailable, reasonGatewayIdentityUnavailable)
	st.recordErr = nil

	gone := uuid.New()
	st.runComps = append(st.runComps, types.RunComponent{RunID: run.ID, ComponentID: &gone, Name: "gone-tool"})
	refused("another component deleted", http.StatusForbidden, reasonOwnerComponentGone)
	st.runComps = st.runComps[:1]

	delete(st.components, c.ID)
	refused("the server itself deleted", http.StatusForbidden, reasonGatewayIdentityChanged)
}

// Every live door words a component's cut the same way. A withdrawal wins over a pin, since a new run
// in the pinned unit would not get a withdrawn component either; only the cut's own reasons count as
// one, so a store fault is never passed over.
func TestComponentCutWithdrawnWinsOverPinned(t *testing.T) {
	for _, c := range []struct {
		pinned          bool
		withdrawn, want string
	}{
		{false, "", ""},
		{true, "", reasonComponentUnitPinned},
		{true, reasonOwnerComponentGone, reasonOwnerComponentGone},
		{false, reasonOwnerCapabilityComponent, reasonOwnerCapabilityComponent},
	} {
		reason, msg := componentCut(c.pinned, c.withdrawn)
		if reason != c.want || (c.want != "" && !strings.HasPrefix(msg, "wardyn: this run is no longer given credentials")) {
			t.Errorf("componentCut(%v, %q) = %q, %q; want %q", c.pinned, c.withdrawn, reason, msg, c.want)
		}
	}
	for _, r := range []string{reasonComponentUnitPinned, reasonOwnerCapabilityComponent, reasonOwnerComponentGone} {
		if !gatewayComponentCutReason(r) {
			t.Errorf("%s is not read as a component's cut", r)
		}
	}
	for _, r := range []string{reasonGatewayIdentityChanged, reasonGatewayIdentityUnavailable, reasonGrantWithheld} {
		if gatewayComponentCutReason(r) {
			t.Errorf("%s is read as a component's cut", r)
		}
	}
}

// At the door, a component's cut refuses an MCP server's token with a gateway_identity.mint denied row
// that names the reason, while the run's model provider token and its client certificate are still
// given: the cut reaches the run's MCP servers, as it reaches its credentials, not its model.
func TestGatewayMCPTokenCutLeavesTheProviderAndCertificate(t *testing.T) {
	f := newGatewayFixture(t)
	testGatewayCA(t, f)
	f.sealMCP(t, testGatewayMCPComponent())
	gone := uuid.New()
	f.st.set(func(s *gatewayStore) {
		s.runComps = append(s.runComps, types.RunComponent{RunID: f.run.ID, ComponentID: &gone, Name: "gone-tool"})
	})
	f.srv.cfg.Store = &gatewayCutStore{gatewayStore: f.st}

	w := f.mintMCP(t, "tools")
	if w.Code != http.StatusForbidden || reasonOf(t, w) != reasonOwnerComponentGone {
		t.Fatalf("MCP mint = %d %s, want 403 %s", w.Code, w.Body, reasonOwnerComponentGone)
	}
	evs := f.mintEvents()
	if len(evs) == 0 || evs[len(evs)-1].Outcome != "denied" || !strings.Contains(string(evs[len(evs)-1].Data), `"reason":"component_gone"`) ||
		!strings.Contains(string(evs[len(evs)-1].Data), `"mcp_server":"tools"`) {
		t.Fatalf("rows = %+v, want a denied row naming the reason and the server", evs)
	}
	if w := f.mint(t); w.Code != http.StatusOK {
		t.Fatalf("provider mint = %d %s, want the model's token", w.Code, w.Body)
	}
	if w := testGatewayCertify(t, f, testGatewayPublicKeyBody(t)); w.Code != http.StatusOK {
		t.Fatalf("certify = %d %s, want the certificate for the provider's route", w.Code, w.Body)
	}
}
