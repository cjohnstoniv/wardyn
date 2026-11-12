// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

// Run identity on an organisation MCP server's route: the same token as a model
// provider's (internal_gateway_identity.go), named by the server's key instead of a
// provider's id, with the audience on the component. The route is sealed at dispatch
// from the component the run attached; a mint checks the run still carries it at the
// sealed version, and that it would be dispatched the same today.

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/db"
	"github.com/cjohnstoniv/wardyn/internal/egress/domainmatch"
	"github.com/cjohnstoniv/wardyn/internal/egress/proxy"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

const (
	gatewayMCPChangedMsg = "This run's MCP server was changed or turned off after the run started, so the run gets no new " +
		"gateway identity for it. Create a new run to use the server as it is now."
	gatewayMCPAudienceDup = "mcp.gateway_audience: %q is used by %s and %s at different addresses; use a different one for each"
	gatewayMCPAddress     = "mcp.url: this address can't carry a signed identity: its path may not hold a dot segment, a backslash, a query or a fragment"
	gatewayMCPNeedsOn     = "mcp.gateway_audience: run identity is turned on per model provider, which also makes the signing key; turn it on for a provider first"
	gatewayMCPRunAddress  = "MCP server %q sends a signed identity to its gateway, but its address can't carry one, so nothing was started. " +
		"Ask an administrator to correct the component's url."
	gatewayMCPRunClash = "MCP server %q and %s both send a signed identity to one gateway address and path, so nothing was started. " +
		"Ask an administrator to give them addresses or paths apart."
	gatewayMCPRunAudienceDup = "MCP server %q and %s send a signed identity with one audience to different addresses, so nothing was started. " +
		"Ask an administrator to give each its own audience."
)

// gatewayMCPKeyPattern is an MCP server key as the proxy's broker accepts one.
var gatewayMCPKeyPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,31}$`)

// gatewayMCPComponents is what the MCP arm reads of the component store.
type gatewayMCPComponents interface {
	GetComponent(ctx context.Context, id uuid.UUID, owner string) (types.Component, error)
	ListRunComponents(ctx context.Context, runID uuid.UUID) ([]types.RunComponent, error)
}

func gatewayMCPChanged() *ownerRefusal {
	return &ownerRefusal{status: http.StatusForbidden, reason: reasonGatewayIdentityChanged, msg: gatewayMCPChangedMsg}
}

// gatewayMCPUID is what an MCP route seals in place of a provider's UID: the component and its version.
func gatewayMCPUID(id uuid.UUID, version int) string {
	return id.String() + "/v" + strconv.Itoa(version)
}

// gatewayMCPUIDParts reads a sealed MCP route's UID.
func gatewayMCPUIDParts(uid string) (id uuid.UUID, version int, ok bool) {
	raw, v, cut := strings.Cut(uid, "/v")
	if !cut {
		return uuid.Nil, 0, false
	}
	id, err := uuid.Parse(raw)
	version, verr := strconv.Atoi(v)
	return id, version, err == nil && verr == nil && version > 0
}

// gatewayMCPRouteOf is the route a remote MCP server would be dispatched with. ok is false for an
// address no route can hold, or one that names no audience or component.
func gatewayMCPRouteOf(rawURL, key, audience, uid string) (_ proxy.GatewayIdentityRoute, ok bool) {
	d, ok := gatewayDestinationOf(rawURL)
	if !ok || key == "" || audience == "" || uid == "" {
		return proxy.GatewayIdentityRoute{}, false
	}
	return proxy.GatewayIdentityRoute{
		Host: d.host, Port: d.port, PathPrefix: d.path, MCPKey: key, ProviderUID: uid, Audience: audience,
	}, true
}

// gatewayMCPDispatchRoute is the route for one attached MCP server, or ok false when it sends no identity.
func gatewayMCPDispatchRoute(m componentMCP) (rt proxy.GatewayIdentityRoute, sends, ok bool) {
	if m.GatewayAudience == "" {
		return proxy.GatewayIdentityRoute{}, false, false
	}
	rt, ok = gatewayMCPRouteOf(m.URL, m.Key, m.GatewayAudience, m.GatewayUID)
	return rt, true, ok
}

// gatewayMCPBindingHolds is the MCP twin of gatewayBindingHolds, shared by a mint and a revive:
// the run still carries the organisation's component at the sealed version, the live row is
// still the organisation's remote server at that version, and it would be dispatched today with
// the sealed route. A failed read is an error, never a yes.
func (s *Server) gatewayMCPBindingHolds(ctx context.Context, runID uuid.UUID, sealed proxy.GatewayIdentityRoute) (bool, error) {
	cs, ok := s.cfg.Store.(gatewayMCPComponents)
	if !ok {
		return false, errors.New("the store cannot read components")
	}
	id, version, ok := gatewayMCPUIDParts(sealed.ProviderUID)
	if !ok || sealed.MCPKey == "" {
		return false, nil
	}
	attached, err := cs.ListRunComponents(ctx, runID)
	if err != nil {
		return false, err
	}
	if !slices.ContainsFunc(attached, func(rc types.RunComponent) bool {
		return !rc.SelfDefined && !rc.Erased && rc.Owner == "" && rc.ComponentID != nil && *rc.ComponentID == id && rc.Version == version
	}) {
		return false, nil
	}
	live, err := cs.GetComponent(ctx, id, "")
	if errors.Is(err, store.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if live.Owner != "" || live.Version != version || !live.Definition.RemoteMCP() {
		return false, nil
	}
	now, ok := gatewayMCPRouteOf(live.Definition.MCP.URL, sealed.MCPKey, live.Definition.MCP.GatewayAudience, gatewayMCPUID(live.ID, live.Version))
	return ok && now == sealed, nil
}

// gatewayMintAdmitMCP is the MCP arm of gatewayMintAdmit, once the sealed route for the server is found.
func (s *Server) gatewayMintAdmitMCP(ctx context.Context, run types.AgentRun, sealed proxy.GatewayIdentityRoute, sponsor string) (gatewayDest, string, *ownerRefusal) {
	held, err := s.gatewayMCPBindingHolds(ctx, run.ID, sealed)
	switch {
	case err != nil:
		slog.ErrorContext(ctx, "gateway identity: read the MCP server", slog.String("run_id", run.ID.String()), slog.Any("err", err))
		return gatewayDest{}, "", gatewayUnavailable()
	case !held:
		return gatewayDest{}, "", gatewayMCPChanged()
	}
	// The audience is the sealed one, which the binding has just shown equals the live one; the lifetime is the cap.
	return gatewayDest{audience: sealed.Audience, mcp: sealed.MCPKey}, sponsor, nil
}

// gatewayMCPRoutesOf is the dispatch's MCP routes added to the providers', or the refusal for a run that
// cannot carry them: an address no route can hold, a route a provider's or another server's could also match,
// or an audience another route has at a different address. A provider save and a component save each check
// audiences against the other table, but nothing spans both, so two saves at once can still leave one audience
// at two addresses; the run is refused here rather than sent a token that one gateway would take for the other.
func gatewayMCPRoutesOf(providers []proxy.GatewayIdentityRoute, mcp mcpDispatch) ([]proxy.GatewayIdentityRoute, string) {
	if len(mcp.gatewayBad) > 0 {
		return nil, fmt.Sprintf(gatewayMCPRunAddress, mcp.gatewayBad[0])
	}
	routes := slices.Clone(providers)
	for _, rt := range mcp.gatewayRoutes {
		if i := slices.IndexFunc(routes, rt.Clashes); i >= 0 {
			return nil, fmt.Sprintf(gatewayMCPRunClash, rt.MCPKey, gatewayRouteLabel(routes[i]))
		}
		if i := slices.IndexFunc(routes, func(o proxy.GatewayIdentityRoute) bool {
			return o.Audience == rt.Audience && gatewayRouteAddress(o) != gatewayRouteAddress(rt)
		}); i >= 0 {
			return nil, fmt.Sprintf(gatewayMCPRunAudienceDup, rt.MCPKey, gatewayRouteLabel(routes[i]))
		}
		routes = append(routes, rt)
	}
	return routes, ""
}

// gatewayRouteAddress is a route's address as validateGatewayAudiences compares them.
func gatewayRouteAddress(rt proxy.GatewayIdentityRoute) gatewayDestination {
	return gatewayDestination{host: domainmatch.CanonHost(rt.Host), port: rt.Port, path: strings.TrimSuffix(rt.PathPrefix, "/")}
}

// gatewayRouteLabel names a route in a refusal.
func gatewayRouteLabel(rt proxy.GatewayIdentityRoute) string {
	if rt.MCPKey != "" {
		return fmt.Sprintf("MCP server %q", rt.MCPKey)
	}
	return fmt.Sprintf("model provider %q", rt.ProviderID)
}

// gatewayMCPAudienceUses is the organisation's remote MCP servers that send identity.
func gatewayMCPAudienceUses(rows []types.Component) []types.Component {
	return slices.DeleteFunc(slices.Clone(rows), func(c types.Component) bool {
		return c.Owner != "" || !c.Definition.RemoteMCP() || c.Definition.MCP.GatewayAudience == ""
	})
}

// gatewayMCPAudienceRefusal is a component save's half of validateGatewayAudiences, and the
// rule that identity is on for some provider. c is an organisation's remote server with an
// audience. "" msg means saveable. The reads fail closed.
func (s *Server) gatewayMCPAudienceRefusal(ctx context.Context, c types.Component) (reason, msg string) {
	if _, ok := gatewayDestinationOf(c.Definition.MCP.URL); !ok {
		return reasonComponentDefinitionInvalid, "invalid component: definition." + gatewayMCPAddress
	}
	cs, ok := s.cfg.Store.(store.ComponentStore)
	if !ok {
		return reasonGatewayIdentityUnavailable, gatewayUnavailableMsg
	}
	sc, err := s.cfg.Store.GetSiteConfig(ctx)
	if err != nil {
		slog.ErrorContext(ctx, "gateway identity: read the model providers for a component save", slog.Any("err", err))
		return reasonGatewayIdentityUnavailable, gatewayUnavailableMsg
	}
	if !slices.ContainsFunc(modelProviderRows(sc), gatewayProviderOn) {
		return reasonComponentDefinitionInvalid, "invalid component: definition." + gatewayMCPNeedsOn
	}
	if _, kept := s.runProxyConfigStore(); !kept {
		return reasonGatewayIdentityUnavailable, gatewayNotKeptMsg
	}
	rows, err := cs.ListComponents(ctx, "")
	if err != nil {
		slog.ErrorContext(ctx, "gateway identity: read the components for a component save", slog.Any("err", err))
		return reasonGatewayIdentityUnavailable, gatewayUnavailableMsg
	}
	rows = append(slices.DeleteFunc(rows, func(o types.Component) bool { return o.ID == c.ID }), c)
	if err := validateGatewayAudiences(sc.ModelProviders, gatewayMCPAudienceUses(rows)); err != nil {
		return reasonComponentDefinitionInvalid, "invalid component: definition." + err.Error()
	}
	return "", ""
}

// Every writer of an audience holds one organisation-wide lock (db.GatewayAudienceLockClass) across its
// audience check and its write, taken before the writer's other locks: a provider save before the site-config
// document lock, a component save before the MCP server key and row locks. The audiences live in two tables,
// so no row lock spans them, and two saves that each checked before the other wrote could both keep one audience.

// lockProviderDocument is a provider save's site-config document lock, taken after the audience lock when the
// block sends identity, so its audience check (refuseGatewayIdentityWrite) sees every component save's. A block
// without identity writes no audience and takes the document lock alone. ok=false: the 503 is written.
func (s *Server) lockProviderDocument(w http.ResponseWriter, r *http.Request, block *types.ModelProviders) (*http.Request, func(), bool) {
	if block == nil || !slices.ContainsFunc(block.Providers, func(p types.ModelProvider) bool { return p.GatewayIdentity != nil }) {
		return s.lockDoor(w, r, db.SiteConfigLockClass)
	}
	r, unlockAudience, ok := s.lockDoor(w, r, db.GatewayAudienceLockClass)
	if !ok {
		return r, nil, false
	}
	r, unlock, ok := s.lockDoor(w, r, db.SiteConfigLockClass)
	if !ok {
		unlockAudience()
		return r, nil, false
	}
	return r, func() { unlock(); unlockAudience() }, true
}

// lockGatewayAudienceComponent takes the audience lock for an organisation component save that sets an
// audience, and makes the save's audience check again under it, against a fresh read: the check
// componentSaveRefusal made ran before any lock. ok=false: the refusal or the 503 is written.
func (s *Server) lockGatewayAudienceComponent(w http.ResponseWriter, r *http.Request, c types.Component) (*http.Request, func(), bool) {
	if m := c.Definition.MCP; c.Owner != "" || m == nil || m.GatewayAudience == "" {
		return r, func() {}, true
	}
	r, unlock, ok := s.lockDoor(w, r, db.GatewayAudienceLockClass)
	if !ok {
		return r, nil, false
	}
	if reason, msg := s.gatewayMCPAudienceRefusal(r.Context(), c); reason != "" {
		unlock()
		saveRefusal(w, reason, msg)
		return r, nil, false
	}
	return r, unlock, true
}

// refuseGatewayAudienceWrite is a provider save's half: no audience the block gives a provider may be
// one an organisation MCP server already sends to a different address. It reads the components only when
// the block has a provider with identity, and answers for itself, returning true when it refused.
func (s *Server) refuseGatewayAudienceWrite(w http.ResponseWriter, r *http.Request, block *types.ModelProviders, prefix string) bool {
	if block == nil || !slices.ContainsFunc(block.Providers, func(p types.ModelProvider) bool { return p.GatewayIdentity != nil }) {
		return false
	}
	cs, ok := s.cfg.Store.(store.ComponentStore)
	if !ok {
		return false
	}
	rows, err := cs.ListComponents(r.Context(), "")
	if err != nil {
		slog.ErrorContext(r.Context(), "gateway identity: read the components for a provider save", slog.Any("err", err))
		writeErrorReason(w, http.StatusServiceUnavailable, reasonGatewayIdentityUnavailable, gatewayUnavailableMsg)
		return true
	}
	if err := validateGatewayAudiences(block, gatewayMCPAudienceUses(rows)); err != nil {
		writeErrorReasonField(w, http.StatusBadRequest, reasonSiteConfigInvalid, prefix+err.Error(), refusalField(err))
		return true
	}
	return false
}
