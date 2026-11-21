// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/audit"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// pinFixtureStore answers the two reads the pin makes (the portal's row and the unit tree) over the matrix
// store, so a delegated request can be walked without Postgres.
type pinFixtureStore struct {
	*authzStore
	units []types.GovernanceUnit
}

func (s *pinFixtureStore) ListGovernanceUnits(context.Context) ([]types.GovernanceUnit, error) {
	return s.units, nil
}

func (s *pinFixtureStore) GetDelegateByID(_ context.Context, id uuid.UUID) (types.Delegate, error) {
	s.authzStore.fakeDelegateStore.mu.Lock()
	defer s.authzStore.fakeDelegateStore.mu.Unlock()
	d, ok := s.authzStore.fakeDelegateStore.delegates[id]
	if !ok {
		return types.Delegate{}, store.ErrNotFound
	}
	return d, nil
}

// portalPinFixture is a server whose delegated token belongs to an issuer portal pinned to unit pin. The tree is the
// Default unit, the pin with a child, and an unrelated unit.
type portalPinFixture struct {
	srv                 *Server
	ast                 *authzStore
	tok, person         string
	via                 types.DelegationVia
	pin, child, outside types.GovernanceUnit
	delegate            types.Delegate
}

func newPinFixture(t *testing.T) *portalPinFixture {
	t.Helper()
	f := &portalPinFixture{person: "delegate:person:aaaaaaaaaaaaaaaaaaaaaaaaaa:mfrggzdf"}
	f.pin = types.GovernanceUnit{ID: uuid.New(), Name: "pin"}
	f.child = types.GovernanceUnit{ID: uuid.New(), Name: "child", ParentID: &f.pin.ID}
	f.outside = types.GovernanceUnit{ID: uuid.New(), Name: "outside"}
	srv, ast, _, _ := newAuthzMatrixServer(t)
	f.srv, f.ast = srv, ast
	f.tok, f.via = seedDelegation(t, ast.fakeDelegateStore, f.person)
	ast.fakeDelegateStore.mu.Lock()
	d := ast.fakeDelegateStore.delegates[f.via.Delegate]
	d.Issuer, d.IssuerKey, d.RunInUnder = "https://idp.example.com", "aaaaaaaaaaaaaaaaaaaaaaaaaa", &f.pin.ID
	ast.fakeDelegateStore.delegates[f.via.Delegate] = d
	ast.fakeDelegateStore.mu.Unlock()
	f.delegate = d
	srv.cfg.Store = &pinFixtureStore{authzStore: ast, units: []types.GovernanceUnit{{ID: store.DefaultUnitID, Name: "Default"}, f.pin, f.child, f.outside}}
	srv.router = srv.routes()
	return f
}

func (f *portalPinFixture) seed(owner string, unit *uuid.UUID) uuid.UUID {
	id := uuid.New()
	f.ast.mu.Lock()
	f.ast.runs[id] = types.AgentRun{ID: id, CreatedBy: owner, State: types.RunRunning, Agent: "claude-code", UnitID: unit}
	f.ast.mu.Unlock()
	return id
}

func idRoutes() []string {
	var out []string
	for key := range delegationAllowed {
		if strings.Contains(key, "{id}") {
			out = append(out, key)
		}
	}
	return out
}

func (f *portalPinFixture) request(t *testing.T, key string, id uuid.UUID) (code int, reason, body string) {
	t.Helper()
	method, pattern, _ := strings.Cut(key, " ")
	w := do(t, f.srv, method, buildPath(pattern, id.String()), f.tok, bodyFor(method, routeMatrix[key]))
	return w.Code, errorReason(w), w.Body.String()
}

// Every allow-listed run route that names {id} answers 403 portal_unit_scope for the person's own run outside
// the pin. It iterates delegationAllowed itself, so a route added to the list cannot skip the check.
func TestPortalPin_EveryAllowListedRunRoute(t *testing.T) {
	f := newPinFixture(t)
	routes := idRoutes()
	if len(routes) < 7 {
		t.Fatalf("delegationAllowed names %d {id} routes, want at least the seven run routes", len(routes))
	}
	outside := f.seed(f.person, &f.outside.ID)
	for _, key := range routes {
		t.Run(key, func(t *testing.T) {
			if code, reason, body := f.request(t, key, outside); code != http.StatusForbidden || reason != "portal_unit_scope" {
				t.Fatalf("own run outside the pin = %d %q %s, want 403 portal_unit_scope", code, reason, body)
			}
		})
	}
	inside := f.seed(f.person, &f.child.ID)
	if code, reason, _ := f.request(t, "GET /api/v1/runs/{id}", inside); code != http.StatusOK {
		t.Errorf("own run under the pin = %d %q, want 200", code, reason)
	}
}

// A missing run and another person's run answer the same 404 with byte-identical bodies on every {id} route,
// whatever their unit; only the person's own run outside the pin is a 403.
func TestPortalPin_MissingAndForeignRunSame404(t *testing.T) {
	f := newPinFixture(t)
	foreign := f.seed("delegate:person:aaaaaaaaaaaaaaaaaaaaaaaaaa:onxw2zq", &f.outside.ID)
	for _, key := range idRoutes() {
		t.Run(key, func(t *testing.T) {
			mc, mr, mb := f.request(t, key, uuid.New())
			fc, fr, fb := f.request(t, key, foreign)
			if mc != http.StatusNotFound || fc != mc || fr != mr || fb != mb {
				t.Fatalf("missing = %d %q %s, foreign = %d %q %s; want one identical 404", mc, mr, mb, fc, fr, fb)
			}
		})
	}
}

// A launch through a pinned portal that resolves outside the pin is refused 403 portal_unit_scope, and one
// under it passes; a queued start's own re-check is the queue tests'.
func TestPortalPin_CreateOutsidePinRefused(t *testing.T) {
	f := newPinFixture(t)
	ctx := audit.WithDelegation(context.Background(), f.via)
	outside := governanceCeiling{Unit: &ResolvedUnit{ID: f.outside.ID, Name: f.outside.Name}}
	if r := f.srv.portalUnitRefusal(ctx, outside, false); r == nil || r.status != http.StatusForbidden || r.body.Reason != "portal_unit_scope" {
		t.Fatalf("launch outside the pin = %+v, want 403 portal_unit_scope", r)
	}
	if r := f.srv.portalUnitRefusal(ctx, governanceCeiling{Unit: &ResolvedUnit{ID: f.child.ID, Name: f.child.Name}}, false); r != nil {
		t.Fatalf("launch under the pin = %+v, want none", r)
	}
}
