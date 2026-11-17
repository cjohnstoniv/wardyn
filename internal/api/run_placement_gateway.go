// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"slices"

	"github.com/cjohnstoniv/wardyn/internal/egress/proxy"
)

// localGatewayRoutes is the ProxyConfig.GatewayIdentity a local run's dispatch would author, from what
// create knows: the chosen providers' routes, then the route of each organisation remote MCP server that
// sends run identity (dispatchMCP, gatewayMCPRoutesOf). Create, Review and the preview judge it on the gate
// dispatch judges, so a run that dispatch would refuse on a runner for its run identity is refused before
// it is made. A server whose address can hold no route is named by its key alone: dispatch refuses its
// address anyway, and a runner never takes it.
func localGatewayRoutes(providers []proxy.GatewayIdentityRoute, comps runComponents) []proxy.GatewayIdentityRoute {
	routes := slices.Clone(providers)
	for _, m := range comps.dispatch().MCP {
		rt, sends, ok := gatewayMCPDispatchRoute(m)
		if !sends {
			continue
		}
		if !ok {
			rt = proxy.GatewayIdentityRoute{MCPKey: m.Key, Audience: m.GatewayAudience}
		}
		routes = append(routes, rt)
	}
	return routes
}
