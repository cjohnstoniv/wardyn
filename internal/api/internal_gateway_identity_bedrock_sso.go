// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"fmt"
	"slices"
	"strings"

	"github.com/cjohnstoniv/wardyn/internal/egress/proxy"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// Run identity on a bedrock_sso provider. SigV4 signs the Host header, so the sandbox's SDK, aimed
// at the gateway, cannot sign a call AWS accepts. With re-signing at the proxy on
// (WARDYN_AWS_SIGV4_AT_PROXY), the proxy signs each call again as the person for the regional
// Bedrock host behind the gateway (proxy.AWSSigV4Gateway) and sends it to the gateway with the
// run's identity token; the gateway forwards it to AWS unchanged. Off per provider, as identity is
// for every kind: an admin turns it on, and only on an install that re-signs. The person's role
// credentials never enter the sandbox on this lane, and the gateway never gets the secret key.

const (
	mp400GatewayBedrockSSOAWSHost = "model_providers: %q: gateway_identity on a bedrock_sso provider needs a bedrock.base_url " +
		"on a gateway name of your own, not an AWS address, or the token would go to AWS"
	mp400GatewayBedrockSSORegion = "model_providers: %q: gateway_identity on a bedrock_sso provider needs bedrock.region, " +
		"which names the Bedrock address each call is signed for"
	mp400GatewayBedrockSSOResign = "model_providers: %q: gateway_identity on a bedrock_sso provider needs WARDYN_AWS_SIGV4_AT_PROXY on, " +
		"so Wardyn signs each AWS request at its proxy"
)

// mpRunGatewaySigV4 refuses a run whose sign-in provider sends identity to its gateway when this
// install does not re-sign the lane (%s is why, awsSigV4Why). Such a run never falls back to the
// person's role credentials in the sandbox with no identity.
const mpRunGatewaySigV4 = "Model provider %q sends run identity to a gateway, which needs Wardyn to sign its AWS requests " +
	"at the proxy; this install doesn't (%s). Ask your operator."

// validateBedrockSSOGateway is the sign-in row's own half of validateProviderGatewayIdentity, past
// the address checks it shares with the key row: the gateway is on a name AWS does not own
// (proxy.AWSProviderOwnedHost, the same predicate the proxy applies to the sealed config), and the
// region names the host each call is signed for.
func validateBedrockSSOGateway(mp types.ModelProvider, d gatewayDestination) error {
	if proxy.AWSProviderOwnedHost(d.host) {
		return atField("bedrock.base_url", fmt.Errorf(mp400GatewayBedrockSSOAWSHost, mp.ID))
	}
	if providerBedrockSettings(mp).Region == "" {
		return atField("bedrock.region", fmt.Errorf(mp400GatewayBedrockSSORegion, mp.ID))
	}
	return nil
}

// gatewayBedrockSSOAvailable reports whether this install re-signs a sign-in lane (awsSigV4Lane's
// switches), so a sign-in row may take identity: the model-providers answer's
// gateway_identity_bedrock_sso and the save door's precondition.
func (s *Server) gatewayBedrockSSOAvailable() bool {
	return s.cfg.AWSSigV4AtProxy && s.cfg.AWSSSOProxyInject && s.cfg.AWSSSOEndpointOverride == ""
}

// bedrockSSOGatewayWriteRefusal refuses a save that turns identity on for a sign-in row while this
// install does not re-sign. A row already sending it is not refused here, so a later switch change
// never wedges the block's saves; dispatch refuses its runs instead (mpRunGatewaySigV4).
func (s *Server) bedrockSSOGatewayWriteRefusal(block, stored *types.ModelProviders) error {
	if block == nil || s.gatewayBedrockSSOAvailable() {
		return nil
	}
	for _, p := range block.Providers {
		prior, ok := modelProviderByID(stored, p.ID)
		if p.Kind == types.ModelProviderBedrockSSO && gatewayProviderOn(p) && (!ok || prior.Kind != p.Kind || !gatewayProviderOn(prior)) {
			return fmt.Errorf(mp400GatewayBedrockSSOResign, p.ID)
		}
	}
	return nil
}

// bedrockSSOGatewayOf is the gateway entry a sign-in row's re-signed lane writes, nil when p sends
// no identity or its address can hold no route (dispatch refuses that run with mpRunGatewayAddress).
func bedrockSSOGatewayOf(p types.ModelProvider) *proxy.AWSSigV4Gateway {
	if p.Kind != types.ModelProviderBedrockSSO {
		return nil
	}
	rt, ok := gatewayRouteOf(p)
	if !ok {
		return nil
	}
	return &proxy.AWSSigV4Gateway{Host: rt.Host, Port: rt.Port, PathPrefix: rt.PathPrefix,
		SigningHost: bedrockRuntimeHost(providerBedrockSettings(p).Region)}
}

// gatewaySignedHosts is a lane's hosts as the re-signer signs for them: the gateway's host
// replaced by the host the gateway's calls are signed for.
func gatewaySignedHosts(hosts []string, gw *proxy.AWSSigV4Gateway) []string {
	if gw == nil {
		return hosts
	}
	out := make([]string, 0, len(hosts))
	for _, h := range hosts {
		if strings.EqualFold(strings.TrimSuffix(h, "."), gw.Host) {
			h = gw.SigningHost
		}
		out = append(out, h)
	}
	return out
}

// awsSigV4GatewayHosts is the gateways' hosts, which the re-signer's credential reaches too.
func awsSigV4GatewayHosts(gws []proxy.AWSSigV4Gateway) []string {
	out := make([]string, 0, len(gws))
	for _, gw := range gws {
		out = append(out, gw.Host)
	}
	return out
}

// dropSignedGatewayRoutes is routes without the identity routes of gws, which go with the re-signer
// that signs for them.
func dropSignedGatewayRoutes(routes []proxy.GatewayIdentityRoute, gws []proxy.AWSSigV4Gateway) []proxy.GatewayIdentityRoute {
	return slices.DeleteFunc(slices.Clone(routes), func(rt proxy.GatewayIdentityRoute) bool {
		return rt.ProviderID != "" && slices.ContainsFunc(gws, func(gw proxy.AWSSigV4Gateway) bool {
			return strings.EqualFold(rt.Host, gw.Host) && rt.Port == gw.Port &&
				strings.TrimSuffix(rt.PathPrefix, "/") == strings.TrimSuffix(gw.PathPrefix, "/")
		})
	})
}
