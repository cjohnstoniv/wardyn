// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

// model_providers_azure.go is the azure_foundry kind's half of the model-provider write boundary:
// its activation flag, its endpoint, route and harness rules, and the private-endpoint advisory.
// Every door reaches it through validateModelProviders.

import (
	"context"
	"fmt"
	"net"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/ipguard"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// azureFoundryGateReady is the activation boundary of the azure_foundry kind: while false, every write
// door refuses a row of the kind, so no tree can store one, let alone dispatch one, before the proxy route
// gate exists. A package-level var, not a const, so the table test can set it and restore it; az-a5 flips
// the literal when the route gate is merged.
var azureFoundryGateReady = false

// azureFoundryHostSuffixes is the closed list of hosts an azure_foundry endpoint may name, dot-anchored
// (host == s || HasSuffix(host, "."+s)): each person's Entra token is sent to this host, so it must be
// Azure's. Sovereign clouds are not in the list.
var azureFoundryHostSuffixes = []string{"openai.azure.com", "services.ai.azure.com", "cognitiveservices.azure.com"}

// The two harnesses an azure_foundry row can drive, each on the one route its API dialect needs.
const (
	azureMessagesHarness  = "claude-code"
	azureResponsesHarness = "codex-cli"
)

const (
	mp400AzureGate     = "model_providers: %q: the azure_foundry kind is not available in this release"
	mp400AzureUnused   = "model_providers: %q: azure applies only to the azure_foundry kind"
	mp400AzureNeed     = "model_providers: %q: an azure_foundry provider needs azure.endpoint and azure.route"
	mp400AzureOther    = "model_providers: %q: an azure_foundry provider's address is azure.endpoint — base_url, bedrock and auth do not apply"
	mp400AzureEndpoint = "model_providers: %q: azure.endpoint %q must be https://<host> — no path, port, userinfo, query or fragment, and a lower-case host with no trailing dot"
	mp400AzureHost     = "model_providers: %q: azure.endpoint host %q is not an Azure Foundry host — want one of %s, or a subdomain of one"
	mp400AzureRoute    = "model_providers: %q: azure.route %q must be %q (Anthropic Messages) or %q (OpenAI Responses)"
	mp400AzureHarness  = "model_providers: %q: the %s harness cannot use an azure_foundry provider — it serves " +
		azureMessagesHarness + " on route " + types.AzureRouteAnthropic + " and " + azureResponsesHarness + " on route " + types.AzureRouteOpenAIV1
	mp400AzurePair       = "model_providers: %q: %s uses the %s route, but this provider's azure.route is %q — set azure.route to %q, or serve %s from a provider on route %q"
	mp400AzureModelNeed  = "model_providers: %q: %s: model is required on an azure_foundry provider — the deployment name"
	mp400AzureFastModel  = "model_providers: %q: %s: fast_model applies only to the " + azureMessagesHarness + " harness"
	mp400AzureFast       = "model_providers: %q: %s: fast_model %q is not a model id — letters, digits and ._:/@+[]- , at most 256 characters"
	mp400AzureEntraLogin = "model_providers: %q: an azure_foundry provider signs each person in through the console's Entra login, and this install has none configured. " +
		"Set up Entra console login (the OIDC client and tenant), then grant that login application the delegated permission for this provider's audience (%s), with admin consent"
)

// azureHostPattern is a lower-case DNS name: dot-separated labels of letters, digits and inner hyphens.
var azureHostPattern = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?(\.[a-z0-9]([a-z0-9-]*[a-z0-9])?)*$`)

// providerWriteEnv is the server state validateModelProviders reads beyond the block: whether the console's
// Entra login application is configured, the operator's InternalHosts, and the resolver the private-endpoint
// advisory asks. Injected, so no validator reaches for the network or a Server of its own.
type providerWriteEnv struct {
	AllowTestEndpoints   bool
	EntraLoginConfigured bool
	InternalHosts        []types.InternalHost
	Resolve              func(host string) ([]net.IP, error)
}

// providerWriteEnv is the env a write door passes: the InternalHosts of the document it is about to
// store, and this install's Entra login and resolver.
func (s *Server) providerWriteEnv(internalHosts []types.InternalHost) providerWriteEnv {
	env := providerWriteEnv{AllowTestEndpoints: s.cfg.AllowTestEndpoints, InternalHosts: internalHosts, Resolve: s.cfg.HostResolver}
	if s.cfg.ADOLoginFacts != nil {
		clientID, tenantID, _ := s.cfg.ADOLoginFacts()
		env.EntraLoginConfigured = clientID != "" && tenantID != ""
	}
	if env.Resolve == nil {
		env.Resolve = lookupWithDeadline
	}
	return env
}

func lookupWithDeadline(host string) ([]net.IP, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	return net.DefaultResolver.LookupIP(ctx, "ip", host)
}

// hasAzureFoundry reports whether block holds a row of the kind.
func hasAzureFoundry(block *types.ModelProviders) bool {
	return block != nil && slices.ContainsFunc(block.Providers, func(p types.ModelProvider) bool {
		return p.Kind == types.ModelProviderAzureFoundry
	})
}

// normalizeAzureEndpoint trims an endpoint's whitespace and one trailing "/" and lower-cases its scheme and
// authority, leaving anything after them as written so a path or query is still refused.
func normalizeAzureEndpoint(ep string) string {
	ep = strings.TrimSuffix(strings.TrimSpace(ep), "/")
	if i := strings.Index(ep, "://"); i >= 0 {
		end := len(ep)
		if j := strings.IndexAny(ep[i+3:], "/?#"); j >= 0 {
			end = i + 3 + j
		}
		ep = strings.ToLower(ep[:end]) + ep[end:]
	}
	return ep
}

// validateProviderAzure is the azure_foundry kind's rules, and the refusal of Azure settings on every
// other kind. It runs first, so the activation flag is the first thing a write of the kind meets.
func validateProviderAzure(mp types.ModelProvider, env providerWriteEnv) error {
	if mp.Kind != types.ModelProviderAzureFoundry {
		if mp.Azure != nil {
			return fmt.Errorf(mp400AzureUnused, mp.ID)
		}
		return nil
	}
	if !azureFoundryGateReady {
		return fmt.Errorf(mp400AzureGate, mp.ID)
	}
	if mp.BaseURL != "" || mp.Bedrock != nil || mp.Auth != nil {
		return fmt.Errorf(mp400AzureOther, mp.ID)
	}
	if mp.Azure == nil || mp.Azure.Endpoint == "" || mp.Azure.Route == "" {
		return fmt.Errorf(mp400AzureNeed, mp.ID)
	}
	if err := validateAzureEndpoint(mp.ID, mp.Azure.Endpoint); err != nil {
		return err
	}
	audience, ok := azureAudienceForRoute(mp.Azure.Route)
	if !ok {
		return fmt.Errorf(mp400AzureRoute, mp.ID, mp.Azure.Route, types.AzureRouteAnthropic, types.AzureRouteOpenAIV1)
	}
	for _, h := range mp.Harnesses {
		if err := validateAzureHarness(mp, h); err != nil {
			return err
		}
	}
	if !env.EntraLoginConfigured {
		return fmt.Errorf(mp400AzureEntraLogin, mp.ID, audience)
	}
	return nil
}

// validateAzureEndpoint holds the endpoint to https://<host>, host on the closed dot-anchored suffix list.
func validateAzureEndpoint(id, endpoint string) error {
	u, err := url.Parse(endpoint)
	if err != nil || u.Scheme != "https" || u.Opaque != "" || u.User != nil || u.Path != "" || u.RawPath != "" ||
		strings.ContainsAny(endpoint, "?#") || u.Port() != "" || strings.HasSuffix(u.Host, ".") ||
		u.Host != strings.ToLower(u.Host) || !azureHostPattern.MatchString(u.Host) {
		return fmt.Errorf(mp400AzureEndpoint, id, endpoint)
	}
	if !slices.ContainsFunc(azureFoundryHostSuffixes, func(s string) bool { return u.Host == s || strings.HasSuffix(u.Host, "."+s) }) {
		return fmt.Errorf(mp400AzureHost, id, u.Host, strings.Join(azureFoundryHostSuffixes, ", "))
	}
	return nil
}

// validateAzureHarness: each harness only on the route its API dialect needs, the deployment named, and the
// small-model alias on the Messages harness alone.
func validateAzureHarness(mp types.ModelProvider, h types.ProviderHarness) error {
	route := mp.Azure.Route
	switch h.Harness {
	case azureMessagesHarness:
		if route != types.AzureRouteAnthropic {
			return fmt.Errorf(mp400AzurePair, mp.ID, h.Harness, types.AzureRouteAnthropic, route, types.AzureRouteAnthropic, h.Harness, types.AzureRouteOpenAIV1)
		}
	case azureResponsesHarness:
		if route != types.AzureRouteOpenAIV1 {
			return fmt.Errorf(mp400AzurePair, mp.ID, h.Harness, types.AzureRouteOpenAIV1, route, types.AzureRouteOpenAIV1, h.Harness, types.AzureRouteAnthropic)
		}
	default:
		return fmt.Errorf(mp400AzureHarness, mp.ID, h.Harness)
	}
	switch {
	case h.Model == "":
		return fmt.Errorf(mp400AzureModelNeed, mp.ID, h.Harness)
	case h.FastModel != "" && h.Harness != azureMessagesHarness:
		return fmt.Errorf(mp400AzureFastModel, mp.ID, h.Harness)
	case h.FastModel != "" && !modelIDPattern.MatchString(h.FastModel):
		return fmt.Errorf(mp400AzureFast, mp.ID, h.Harness, h.FastModel)
	}
	return nil
}

// azureEndpointWarnings is the private-endpoint advisory: one sentence per enabled azure_foundry row whose
// host resolves to a private address that no InternalHosts entry covers. An advisory, never an error: the
// proxy's private-address guard is what refuses the dial, and DNS at write time may differ from DNS at run
// time. A resolution failure says nothing.
func azureEndpointWarnings(block *types.ModelProviders, env providerWriteEnv) []string {
	if block == nil {
		return nil
	}
	var out []string
	for _, mp := range block.Providers {
		if mp.Kind != types.ModelProviderAzureFoundry || mp.Disabled || mp.Azure == nil {
			continue
		}
		u, err := url.Parse(mp.Azure.Endpoint)
		if err != nil || u.Host == "" {
			continue
		}
		ips, err := env.Resolve(u.Host)
		if err != nil || !slices.ContainsFunc(ips, func(ip net.IP) bool { return privateUncovered(u.Host, ip, env.InternalHosts) }) {
			continue
		}
		out = append(out, fmt.Sprintf("model_providers: %q: azure.endpoint host %s resolves to a private address and no internal_hosts entry covers it, "+
			"so the proxy will refuse to reach it. internal_hosts lifts the private-address guard for a host; upstream_proxy_no_proxy is a separate setting "+
			"that matters only when a corporate proxy is configured", mp.ID, u.Host))
	}
	return out
}

// privateUncovered reports whether ip is a private or reserved address that no InternalHosts entry lifts for
// host: the entry's suffix must match (itself or a dot-anchored subdomain), and the address must be in its
// CIDRs, or, with none declared, in the liftable set.
func privateUncovered(host string, ip net.IP, internal []types.InternalHost) bool {
	if reserved, _ := ipguard.PrivateReserved(ip); ip.IsGlobalUnicast() && !reserved {
		return false
	}
	return !slices.ContainsFunc(internal, func(h types.InternalHost) bool {
		if host != h.HostSuffix && !strings.HasSuffix(host, "."+h.HostSuffix) {
			return false
		}
		if len(h.CIDRs) == 0 {
			return ipguard.InLiftable(ip)
		}
		return slices.ContainsFunc(h.CIDRs, func(c string) bool {
			_, n, err := net.ParseCIDR(c)
			return err == nil && n.Contains(ip)
		})
	})
}
