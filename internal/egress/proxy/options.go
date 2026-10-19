// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"context"
	"crypto/tls"
	"net"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/contentscan"
	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/cjohnstoniv/wardyn/internal/policyref"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// Options configures a Proxy. Nil fields fall back to production defaults.
type Options struct {
	RunID  uuid.UUID
	Policy *Policy
	// Evaluator overrides the pluggable host/method policy-verdict engine. Nil =>
	// the builtin evaluator wrapping Policy (default; unchanged behavior).
	Evaluator egress.Evaluator
	Approval  *approvalClient
	Injector  *injector
	Sink      *decisionSink
	Resolver  resolver
	// Scanner is the optional outbound content-inspection engine (nil == off).
	Scanner *contentscan.Engine
	// CA is the optional TLS-MITM certificate authority (nil == opaque CONNECT).
	CA *certAuthority
	// MITMHosts are operator-configured corp artifact hosts eligible for TLS-MITM
	// beyond the built-in LLM hosts (exact hostnames). See Proxy.mitmHosts.
	MITMHosts []string
	// MITMLLM gates TLS-MITM of the built-in LLM hosts on actual intent. See
	// Proxy.mitmLLM.
	MITMLLM bool
	// GitGrants is the git-broker per-repo allowlist ("<org>/<repo>" -> grant id)
	// backing the /wardyn/gh/ route. See Proxy.gitGrants.
	GitGrants map[string]uuid.UUID
	// PATGrants is the git_pat broker's per-HOST allowlist backing /wardyn/git/.
	// See Config.PATGrants and pat_broker.go for why it is per-host rather than
	// per-repo.
	PATGrants map[string]PATGrant
	// BrokeredPATGrantIDs backs isBrokeredPATGrant. See Config.BrokeredPATGrantIDs.
	BrokeredPATGrantIDs []uuid.UUID
	// ADOGrants backs the Azure DevOps REST gate (ado_gate.go). Nil == off.
	ADOGrants adoGrantsByHost
	// AzureGates backs the Azure route gate (azure_gate.go). Empty == off.
	AzureGates []AzureGateConfig
	// GitHubAPI backs the GitHub CLI door (github_api.go). Nil == off.
	GitHubAPI *GitHubAPIGrant
	// AzureARM backs the Azure CLI door (azure_arm_door.go). Nil == off.
	AzureARM *AzureARMGrant
	// ControlPlaneURL and RunToken back the local brokered routes. The run
	// token is injected only toward the control plane and never reaches the
	// sandbox or any LLM upstream.
	ControlPlaneURL string
	RunToken        *tokenSource
	// Upstream is the OPTIONAL corporate parent proxy (parsed form). Nil ==
	// direct dial. NewServer parses it from Config.UpstreamProxyURL; tests may
	// build one via parseUpstreamProxy.
	Upstream *upstreamProxy
	// UpstreamNoProxy is the operator's upstream BYPASS list
	// (SiteConfig.UpstreamProxyNoProxy, forwarded verbatim): host/domain
	// suffixes and CIDRs dialed DIRECTLY rather than CONNECTed through
	// Upstream. Control-plane-authored, same trust boundary as InternalHosts —
	// the sandbox cannot set it. Empty (the default) => no bypass. Ignored
	// entirely when Upstream is nil. See Proxy.noProxy / Proxy.bypassUpstream.
	UpstreamNoProxy []string
	// InternalHosts are the operator-declared internal hostnames (site-config,
	// CONTROL-PLANE-authored — the sandbox cannot set this) eligible for
	// vetHost's private-IP-guard lift. Empty == no lift. See Proxy.internalHosts.
	InternalHosts []types.InternalHost
	// LocalSubnets are the proxy's own interface subnets (net.InterfaceAddrs,
	// captured once by NewServer before constructing Options). See
	// Proxy.localSubnets.
	LocalSubnets []*net.IPNet
	// ControlPlaneIPs are every address this run's wardynd resolves to,
	// resolved once by NewServer before constructing Options. See
	// Proxy.controlPlaneIPs.
	ControlPlaneIPs []net.IP
	// ExclusionUnknown reports that NewServer's startup capture of LocalSubnets
	// or ControlPlaneIPs failed, so the own-subnet/control-plane clamp must
	// refuse every lift/trust rather than silently permit them. See
	// Proxy.exclusionUnknown.
	ExclusionUnknown bool
	// LLMUpstreams maps a public vendor host to an operator-configured internal
	// gateway base URL (Config.LLMUpstreams, forwarded verbatim). Empty == every
	// brokered LLM route dials the vendor host. See Proxy.llmUpstreams.
	LLMUpstreams map[string]string
	// LLMChannelHosts is Config.LLMChannelHosts, forwarded verbatim.
	LLMChannelHosts map[string]string
	// LLMUnavailableDetail is the control-plane's reason the brokered-LLM 404
	// gives when no credential is behind the route (Config.LLMUnavailableDetail,
	// forwarded verbatim). Empty == the generic route sentence. See llm404Detail.
	LLMUnavailableDetail string
	// LocalModels is Config.LocalModels, forwarded verbatim. See Proxy.localModels.
	LocalModels map[string]types.LocalModelSettings
	// Unattended is Config.Unattended: a review-path push is refused, not held.
	Unattended bool
	// Attribution is Config.Attribution, already re-projected by LoadConfigBytes.
	Attribution *policyref.Ref

	// Dial overrides the connection dialer (tests). Production leaves it nil
	// and a net.Dialer is used.
	Dial func(ctx context.Context, network, addr string) (net.Conn, error)
	// ViaOrgRoutes are a runner proxy's via_org destinations (via_org.go).
	ViaOrgRoutes []ViaOrgRoute
	// TLSClientConfig is the EGRESS transport's TLS config. Production sets it
	// from Config.TrustedCAPEM (system roots plus the operator's corporate CA
	// bundle — see NewServer); nil means system roots alone. Tests use it to
	// trust an httptest TLS server standing in for an HTTPS upstream.
	TLSClientConfig *tls.Config
	// ControlTLS pins the control-plane transport to wardynd's internal CA
	// (hoptls.ClientConfig). nil pins an EMPTY pool: https fails closed.
	ControlTLS *tls.Config
	Now        func() time.Time
}
