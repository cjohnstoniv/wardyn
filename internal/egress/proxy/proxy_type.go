// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"context"
	"net"
	"net/http"
	"regexp"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/contentscan"
	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/cjohnstoniv/wardyn/internal/policyref"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// The Proxy type and its per-run bounds. Split out of proxy.go for scripts/check-file-size.sh;
// newProxy, evaluate and the CONNECT path stay there.

// Proxy is the L2 forward proxy. It serves both absolute-URI plain HTTP
// requests and CONNECT tunnels for TLS (hostname-only visibility; no
// interception in v0). All egress flows through the decision pipeline:
//
//	policy (deny>allow, default-deny) -> first-use approval (if unknown &&
//	  first_use_approval) -> method check -> IP vetting (block private/
//	  loopback/link-local/metadata) -> dial the vetted IP explicitly.
//
// Every decision emits an egress.DecisionLog (async to the control plane,
// mirrored to stdout).
type Proxy struct {
	runID  uuid.UUID
	policy *Policy
	// vm, portRule and vmFlows: the port rule and wardyn-vmnet's flow
	// contract (port_rule.go, vmnet_flows.go).
	vm       bool
	portRule string
	vmFlows  vmFlows
	engine   bool // a via_org engine's proxy (serverHooks.start): its relay lane is WithRelayLane's
	// evaluator decides the host allow/deny/unknown verdict + method (pluggable
	// seam). Defaults to the builtin RunPolicySpec evaluator. The IP guard,
	// approval FSM, IP vetting, and injection stay hardwired in evaluate().
	evaluator egress.Evaluator
	approval  *approvalClient
	inject    *injector
	sink      *decisionSink
	res       resolver
	// scanner is the OPTIONAL outbound content-inspection engine. Nil == off
	// (the common case). It is consulted only on the brokered LLM routes.
	scanner      *contentscan.Engine
	deferredScan *deferredScan // a gateway route's cut engine (gateway_identity.go); nil, every request gets scanner
	// scan is this engine's inspection slot and retained-bytes budget.
	scan scanBudget
	// ca is the OPTIONAL TLS-MITM certificate authority. Non-nil only when both a
	// scanner and a CA are configured; it makes opaque CONNECT tunnels to known
	// LLM hosts inspectable (subscription-OAuth path). Nil == opaque passthrough.
	ca *certAuthority
	// mitmHosts is the OPERATOR-CONFIGURED set of corp artifact hosts (exact,
	// lowercased) the proxy may TLS-MITM in addition to the built-in LLM hosts, so
	// a corporate registry token injects on the wire. Compiled at dispatch from
	// site-config; see isMITMHost's trust-boundary comment. Empty == LLM only.
	mitmHosts map[string]bool
	// mitmPorts pairs each mitmHosts entry (same lowercased host key) with the
	// CONNECT port that entry is scoped to, parsed from an Options.MITMHosts
	// entry's optional ":port" suffix. 0 means the entry carried no port (the
	// bare-host format every existing caller sends — Bedrock's runtimeHost, an
	// un-migrated redirect) and stays eligible on ANY port. A positive value (an
	// artifact-redirect entry now authors "host:port") is EXACT: a CONNECT to
	// the same host on a DIFFERENT port is never MITM'd or token-injected —
	// handleConnect enforces this alongside isCorpMITMHost so the
	// allowlist stays as tight as isMITMHost's doc comment claims.
	mitmPorts map[string]int
	// mitmPlaintext marks the mitmHosts entries whose ORIGIN serves plain HTTP,
	// so the MITM's upstream leg re-originates in cleartext. Absent = TLS, which
	// is every entry unless an operator configured an http:// endpoint override.
	mitmPlaintext map[string]bool
	// mitmLLM gates TLS-MITM of the built-in LLM hosts on actual intent (subscription
	// injection or intercept_tls) — a CA minted only for artifact token injection
	// must NOT make Anthropic/OpenAI MITM-eligible. See isMITMHost / handleConnect.
	mitmLLM bool
	// blindHosts deduplicates the one-time llm.scan.bypass signal emitted when an
	// inspection-enabled run tunnels to an LLM host over opaque CONNECT (no MITM
	// yet), so coverage is reported honestly without flooding the audit log.
	blindMu    sync.Mutex
	blindHosts map[string]struct{}
	// bedrockFaulted: data-plane hosts whose last model call AWS refused (bedrock_fault.go).
	bedrockFaulted sync.Map

	// privIP is B6's per-run builtin:private-ip memo — the one egress verdict
	// that cannot change mid-run, so an identical retry is answered without a
	// re-resolve and without a second decision row. See private_ip_memo.go for
	// the rule, the ceiling and why no other refusal is memoed.
	privIP privateIPMemo

	// gitGrants is the per-run git-broker allowlist: canonical lowercased
	// "<org>/<repo>" -> the github_token grant to mint from. It is the unit of
	// trust for the /wardyn/gh/ route — a repo absent here is 403. Empty/nil ==
	// no repo brokered (the route always 403s). See git_broker.go.
	gitGrants map[string]uuid.UUID
	// gitPushWithheld is Options.GitPushWithheld, keys lowercased (refuseWithheldPush).
	gitPushWithheld map[string]string
	// viaOrgRoutes are the destinations forwarded to the organisation (via_org.go).
	viaOrgRoutes []ViaOrgRoute
	// maskKey files the secrets this proxy learns in procRegistry: uuid.Nil in a
	// sidecar, a ViaOrgEngine's own key at the organisation (procMaskFor).
	maskKey uuid.UUID
	// scrubResponses is Options.ScrubResponses (scrub.go).
	scrubResponses bool
	// canonicalPushRepos is Options.CanonicalPushRepos (patPushTarget).
	canonicalPushRepos bool
	// pushHoldRemovals is Options.PushHoldRemovals (forgeRepo.removedFromTip).
	pushHoldRemovals bool
	// patGrants is the per-run git_pat broker allowlist: lowercased host -> the
	// grant to mint from. Empty/nil == no host brokered (the route always 403s),
	// which is also what a deployment with the lane switched off looks like.
	patGrants map[string]PATGrant
	// brokeredPATGrantIDs is every git_pat grant id of the run while the PAT
	// broker is on; the mint relay refuses a body naming one (isBrokeredPATGrant).
	brokeredPATGrantIDs []uuid.UUID
	residentMints       map[uuid.UUID]ResidentMint // runner_resident mints answered locally (resident_mint.go)
	// sshAgent holds the run's ssh_key grants' keys for the ssh agent route
	// (ssh_agent.go).
	sshAgent sshAgents
	// adoGrants answers the run's Azure DevOps grant per host for the REST gate
	// (ado_gate.go). Nil == no host gated.
	adoGrants adoGrantsByHost
	// azure is the run's Azure route gates (azure_gate.go). Nil == no host gated.
	azure *azureGates
	// patAPI is the run's git_pat grants with api: true, by host (pat_api.go): each host's
	// connection on 443 is terminated and every request judged by its forge's operation table.
	patAPI map[string]PATGrant
	// ghAPI is the run's GitHub CLI door (github_api.go). Nil == no door.
	ghAPI *GitHubAPIGrant
	// ghAPIStopped is set when shutdown starts handing the door's tokens back; the door mints and
	// forwards nothing after it.
	ghAPIStopped atomic.Bool
	// arm is the run's Azure CLI door (azure_arm_door.go). Nil == no door.
	arm *azureARMDoor
	// sigv4 is the run's AWS SigV4 re-signing door (aws_sigv4.go); nil when off.
	sigv4 *awsSigV4Door
	// kube is the kubectl door (kube_api_door.go); nil without a kubectl row.
	kube *kubeAPIDoor
	// hostCalls is the run's host rule holds and their answers (host_call_hold.go).
	hostCalls mcpCalls
	// gitTokens caches minted installation tokens per grant so a single clone
	// (info/refs + git-upload-pack) does not re-mint — mandatory for single-use
	// approval-gated grants. Guarded by gitTokMu; each entry single-flights its
	// own re-mint via gitTokEntry.reMu.
	gitTokMu  sync.Mutex
	gitTokens map[uuid.UUID]*gitTokEntry

	// controlPlaneURL is the base URL of wardynd, used ONLY by the local
	// brokered routes (/wardyn/v1/...) to forward to the internal API.
	controlPlaneURL string
	// topologyRe are the operator-configured endpoints redactTopology removes
	// from a SANDBOX-facing error body (httpError) — the control-plane base URL,
	// the corp upstream proxy and LLM gateway hosts. See topologyPatterns.
	topologyRe []*regexp.Regexp
	// runToken is the per-run token that authenticates internal calls. It is
	// held ONLY here in proxy memory and injected toward controlPlaneURL on the
	// local mint/approvals routes; it is NEVER exposed to the sandbox and NEVER
	// injected toward any LLM upstream.
	runToken *tokenSource
	// localClient forwards local-route requests to the control plane. It uses
	// the proxy's pinned, IP-vetted transport so the control-plane host is
	// resolved+vetted once and dialed explicitly (no transport re-resolution).
	localClient *http.Client
	// streamMoved is set when a tunnel, a MITM stream or a plain via_org request moves a byte (activity.go).
	streamMoved atomic.Bool
	// signin caches the sign-in credentials the control plane accepted
	// (desktop_signin_lane.go).
	signin signinChecks

	// dial connects to a vetted "ip:port" target. Both the plain-HTTP
	// transport and the CONNECT path use this single seam; tests override it.
	dial func(ctx context.Context, network, addr string) (net.Conn, error)
	// transport forwards plain-HTTP requests AND the MITM LLM path. Its
	// DialContext is pinned to the vetted IP via the request context (no
	// transport-side re-resolution) UNLESS an upstream corp proxy is configured,
	// in which case it chains every forward-egress dial through that proxy.
	transport *http.Transport
	// controlTransport backs localClient (control-plane forwards). It ALWAYS
	// dials the vetted IP directly and NEVER chains through the upstream corp
	// proxy — the split that keeps the run token off the corp-proxy wire.
	controlTransport *http.Transport
	// h2 is the egress lane's HTTP/2 fallback and per-run memo of peers that
	// speak HTTP/2 without negotiating it (upstream_protocol.go).
	h2 h2Fallback
	// upstream is the OPTIONAL corporate parent proxy. Nil == direct dial (the
	// common, backward-compatible case). When set, forward egress is issued as
	// CONNECT <real-host> to it; see upstream.go and dialThroughUpstream.
	upstream *upstreamProxy
	// noProxy is the compiled upstream BYPASS list (site-config
	// UpstreamProxyNoProxy) — the destinations dialed DIRECTLY instead of
	// through the corp upstream. Empty == every forward dial chains through the
	// upstream. Consulted only via Proxy.bypassUpstream; it is a routing
	// decision and never lifts the SSRF guard or grants a policy allow. See
	// egress_target.go.
	noProxy []noProxyRule

	// internalHosts are the OPERATOR-DECLARED internal hostnames (site-config
	// InternalHosts) eligible for vetHost's private-IP-guard lift — see
	// Proxy.vetHost / liftInternalHost. Empty == no lift.
	internalHosts []internalHostRule
	// localSubnets are this proxy's OWN interface subnets, captured ONCE at
	// construction (net.InterfaceAddrs, NewServer) — never re-read per request.
	// liftInternalHost refuses to lift an address on one of these: the sidecar
	// shares its control-plane network with postgres/dex/registry, so an
	// internal-host declaration must not let a run reach the proxy's own
	// network neighbors.
	localSubnets []*net.IPNet
	// controlPlaneIPs are EVERY address this run's wardynd resolves to,
	// resolved ONCE at construction (NewServer, before the Proxy exists) —
	// never re-read per request. liftInternalHost refuses to lift any of them
	// for the same reason as localSubnets. All of them, not just the first: a
	// wardynd behind more than one A record had only its first address
	// excluded, while THREAT-MODEL.md states the exclusion covers "its resolved
	// control-plane host".
	controlPlaneIPs []net.IP
	// exclusionUnknown is set when NewServer's startup capture of localSubnets
	// or controlPlaneIPs FAILED. Both admin-authored exceptions to the private-
	// IP guard (the InternalHosts lift and the exact-literal-IP redirect trust)
	// are clamped by onOwnSubnetOrControlPlane, so a silently empty clamp made
	// those exceptions fire MORE widely, not less — the opposite of the
	// fail-closed NewServer's comment claimed. With this set the clamp answers
	// "yes" for every address, which refuses every lift/trust.
	exclusionUnknown bool

	// hardDeny and lapse are set on k8s runners only (nil elsewhere): the
	// in-cluster deny set, and the self-lapse gate every agent request passes.
	hardDeny *hardDeny
	lapse    *lapseGate

	// llmUpstreams is the OPERATOR-CONFIGURED internal-gateway table (vendor
	// public host -> {host,port,prefix}), parsed once from Options.LLMUpstreams.
	// Empty == every brokered LLM route dials the vendor host, byte-identical
	// to today. See llmUpstream.
	llmUpstreams map[string]llmUpstream
	// llmUnavailableDetail is the CONTROL-PLANE-composed reason the brokered-LLM
	// 404 gives when no credential is behind the route (Options.
	// LLMUnavailableDetail). Empty == the generic route sentence; the
	// below-policy clause is appended either way. See llm404Detail.
	llmUnavailableDetail string
	// localModels is each local_model upstream's route gate, keyed by
	// localModelKey (local_model.go). Empty == no local model route.
	localModels map[string]*localModel
	// allowedModels is Config.AllowedModels and BedrockRoutes compiled (allowed_models.go). Nil == off.
	allowedModels *modelLimit
	// gatewayRoutes is Options.GatewayIdentity, gatewayHolders each route's token (gateway_identity.go)
	// and gatewayCert the run's client certificate (gateway_client_cert.go). Empty and nil == off.
	gatewayRoutes  []GatewayIdentityRoute
	gatewayHolders map[string]*gatewayTokenHolder
	gatewayCert    *gatewayCertHolder
	// pushHolds is the held-push state (push_hold.go).
	pushHolds pushHolds
	// attribution is Options.Attribution: the policy named in a policy-decided
	// refusal, nil when there is none to name.
	attribution *policyref.Ref
	// pushSets are the push content rules per SCM entry (Config.PushRuleSets);
	// empty means the run is on the older run-wide regime (policy.pushRules).
	pushSets []types.PushRuleSet
	// pushBase is Config.PushRuleBase compiled: the rules of an entry with no set.
	pushBase *pushRuleSet
	// forgeHosts is Config.ForgeHosts compiled (push_deny_all_forge.go).
	forgeHosts map[string]bool
	// harnessRules and harnessCreds are Config.HarnessToolRules and
	// Config.HarnessCredentials, the per-harness regime of the approval route
	// (tool_rules.go); empty means the older run-wide policy.ToolRules.
	harnessRules []types.HarnessToolRules
	harnessCreds []types.HarnessCredential
	// mcp is the run's MCP servers and harness bindings (mcp_broker.go); nil
	// for a run with none.
	mcp *mcpBroker
	// gatewayVendor is the REVERSE of llmUpstreams (gateway host -> vendor
	// public host), feeding isLLMHost/channelForHost so gateway traffic is
	// recognised as LLM traffic (coverage/classification only — the SSRF vet
	// for the gateway host lives in gatewayTarget, not here).
	gatewayVendor map[string]string
	// channelHosts is host -> vendor for the hosts classified as model hosts without being gateways
	// (llm_channel_hosts.go).
	channelHosts map[string]string

	now func() time.Time

	// openTunnels counts the run's CONNECT tunnels, opaque (openTunnel) and terminated
	// (mitmConnect), from before the dial or hijack to the tunnel's end; maxTunnels bounds it
	// (maxOpenTunnels; tests lower it).
	openTunnels atomic.Int32
	maxTunnels  int32
	meterOff    bool // Config.DisableModelUsage: no model-token metering; the zero value meters
	noNetwork   bool // Config.NoNetwork: refuse every sandbox request (literalIPGuard, noNetworkDial)
}

// maxOpenTunnels is the most CONNECT tunnels a run's proxy holds at once, opaque and
// terminated (MITM) alike, failed and pending dials included: the desktop VM relay's
// maxOpaqueTunnels (internal/vmnet/relay.go), so both egress paths bound a run the same way.
// A tunnel past it is refused with ruleSourceTunnelLimit and a retry hint (takeTunnelSlot); it
// never evicts an established one. A relay flow resumed from its ask is not counted: it already
// holds the relay's slot.
const maxOpenTunnels = 1024

// ruleSourceTunnelLimit is the deny reason for a tunnel past maxOpenTunnels.
const ruleSourceTunnelLimit = "builtin:tunnel-limit"

// llmUpstream is one configured internal-gateway target: the bare host, port
// (443 when the base URL carried none), and path prefix (empty when the base
// URL carried none) parsed from an api.Config.LLMGateways entry.
type llmUpstream struct {
	host   string
	port   int
	prefix string
}

// internalHostRule is one compiled SiteConfig.InternalHosts entry: a
// label-suffix hostname match paired with the CIDRs its lift is scoped to (nil
// == the full ipguard.Liftable set, per SiteConfig.InternalHosts's doc).
type internalHostRule struct {
	suffix string
	cidrs  []*net.IPNet
}

// vettedIPKey carries the pre-resolved, policy-checked dial target through the
// request context so the transport dials it directly instead of re-resolving
// the hostname (TOCTOU / DNS-rebinding guard).
type vettedIPKey struct{}
