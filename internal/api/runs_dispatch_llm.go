// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"log/slog"
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// llmTransport is what the chosen model provider's arm resolved for one
// dispatch (resolveProviderLane, every kind): which provider credentials the
// run and which proxy-side injections/MITM that implies. Consumed by the CA /
// grant-authoring / SandboxSpec phases of dispatchRun. The zero value is a run
// no provider credentials.
type llmTransport struct {
	// bedrock is the resolved Bedrock auth posture; ready gates all Bedrock use.
	bedrock      bedrockAuth
	bedrockReady bool
	// injectBedrockBearer: Bedrock in BEARER mode — proxy-side token injection
	// into bedrock-runtime (never-resident), the only inspectable Bedrock path.
	injectBedrockBearer bool
	// injectBedrockSSO: Bedrock on the CAPTURED-AWS-SSO lane with Phase B on —
	// the SSO access token is injected proxy-side onto this run's own
	// portal.sso host (TLS-MITM) and the sandbox's token cache holds only a
	// placeholder. Distinct from injectBedrockBearer: a different host, a
	// different header, a different credential, and the only one of the two
	// whose credential can lapse mid-run and be recovered by a person signing
	// in (see internal/api/injection_awssso.go).
	injectBedrockSSO bool
	// azure is the resolved azure_foundry lane: the endpoint host whose person-held Entra token the
	// proxy injects (authorAzureInjection). Nil on every other kind.
	azure *providerAzureLane
	// provider is the model provider this run chose and whose owner's own
	// credential its arm authors; nil when no provider serves the run. A
	// Bedrock provider's arm also fills the bedrock* fields above, so every
	// consumer of those (mounts, the ceiling, the grant authors) reads it
	// unchanged.
	provider *chosenProvider
	// secretEnvKeys are the sandboxEnv variables applyBedrockTransport filled
	// with REAL credential material — the captured AWS SSO blob. Nil for every
	// never-resident mode (bearer) and for every non-Bedrock transport, whose
	// env holds only placeholders. Read by dispatch's splitSecretEnv, which
	// moves them onto SandboxSpec.SecretEnv so a substrate does not have to
	// publish them in a readable pod spec.
	secretEnvKeys []string
	// bedrockAudit is the run.bedrock.configure row applyBedrockTransport computed but
	// did NOT record — recording it is deferred to resolveLLMInjections, past
	// every gate that can still refuse the run (bedrockCredGradeHolds, MITM CA
	// provisioning, grant authoring, enforceInspectableLLM), so a run any of
	// them refuses never gets a "success" injection row for a credential it
	// was never handed (#518). Zero value unless bedrockReady.
	bedrockAudit bedrockTransportAudit
}

// bedrockTransportAudit is the detail applyBedrockTransport computes about
// WHICH Bedrock mode (bearer / sso-inject / sso-inject-proxy) credentials a
// run, for the run.bedrock.configure audit row. Carried on llmTransport rather
// than recorded immediately, so the caller can record it only once the dispatch
// gates that follow (bedrockCredGradeHolds, grant authoring) have actually let
// the run through.
type bedrockTransportAudit struct {
	region, model, endpoint, mode, detail string
	hosts                                 []string
}

// providerSubscription reports whether this run chose a Claude subscription
// provider — the provider arm whose sentinel grant and built-in-host MITM
// authorProviderSubscriptionInjection authors.
func (t llmTransport) providerSubscription() bool {
	return t.provider != nil && t.provider.provider.Kind == types.ModelProviderAnthropicSubscription
}

// isModelRun reports whether a dispatch actually invokes the model. Two run
// kinds make NO model call and so must receive NO LLM credential (least
// privilege): a scan run (execs wardyn-scan — workspaceID/sourceID set,
// non-interactive), and a task-mode=exec run — the BYOA/CI plain-command lane
// whose `wardyn run --task-mode exec` contract is literally "no agent, no LLM
// credentials". Without the exec term, a CI exec job with a connected
// managed/resident subscription plus any egress (which docs/CI.md itself
// tells operators to add) silently gets a live Anthropic OAuth token injected
// proxy-side + api.anthropic.com appended to its allow-list, for a plain
// shell command that never asked for a model. An INTERACTIVE workspace-linked
// run (Record Mode) is human-driven, not a scan, so it stays a model run.
// Mirrors the WARDYN_SCAN_ONLY discriminator. Extracted as its own function
// (rather than inlined in resolveLLMInjections) so it's independently unit
// testable — this gate gets it wrong once and every non-model run leaks a
// live model credential.
func isModelRun(taskMode string, workspaceID, sourceID *uuid.UUID, interactive bool) bool {
	return taskMode != "exec" && !((workspaceID != nil || sourceID != nil) && !interactive)
}

// applyBedrockTransport wires a Bedrock provider's READY posture onto the run:
// it copies the resolved Bedrock env into the sandbox env, appends the Bedrock
// egress hosts to the policy allow-list, and computes which of the modes
// (bearer / sso-inject / sso-inject-proxy) credentials the run, as a
// bedrockTransportAudit the caller records once dispatch's gates hold (see
// bedrockTransportAudit's doc comment — recording here, unconditionally, would
// audit a "success" injection for a run the gates go on to refuse).
//
// The returned keys are the sandboxEnv variables holding REAL credential
// material: only the captured SSO blob, whose base64 payload IS the access and
// refresh token. The bearer mode holds a placeholder.
func (s *Server) applyBedrockTransport(b bedrockAuth, policy *types.RunPolicySpec, sandboxEnv map[string]string) ([]string, bedrockTransportAudit) {
	for k, v := range b.env {
		sandboxEnv[k] = v
	}
	var secretEnvKeys []string
	if b.ssoInject && b.env[awsSSOConfigEnvVar] != "" {
		secretEnvKeys = append(secretEnvKeys, awsSSOConfigEnvVar)
	}
	unionAllowedDomains(policy, b.egressHosts)
	var mode, detail string
	switch {
	case b.bearer:
		detail = "bearer token injected proxy-side into bedrock-runtime (TLS-MITM); sandbox holds only a placeholder — never resident"
		mode = "bearer"
	case b.ssoProxyInject:
		// Phase B. The synthetic ~/.aws still exists — the SDK needs the
		// profile to know WHICH account/role to ask for — but its token cache
		// holds an inert placeholder, and the session itself is set on the wire
		// by the proxy at that one host. The ROLE credentials the SDK mints from
		// it are still resident; SigV4 signs in-process and always will.
		detail = "captured AWS SSO session injected proxy-side as x-amz-sso_bearer_token on the run's own portal.sso host (TLS-MITM); the sandbox's token cache holds only a placeholder — the SSO access token is never resident. The short-lived role credentials the SDK mints from it still are (SigV4 signs client-side)"
		mode = "sso-inject-proxy"
	default:
		detail = "captured AWS SSO session materialized as a minimal synthetic ~/.aws; the sandbox SDK exchanges it for short-lived role credentials (portal.sso GetRoleCredentials). The SSO access token IS resident — Phase B (WARDYN_AWS_SSO_PROXY_INJECT) injects it proxy-side on portal.sso instead"
		mode = "sso-inject"
	}
	return secretEnvKeys, bedrockTransportAudit{
		region: b.region, model: b.model, hosts: b.egressHosts,
		// The EFFECTIVE data-plane host — the provider's own base URL
		// (PrivateLink) host, else the regional public one — so the record names
		// where the call actually went rather than leaving an auditor to infer
		// it from the region.
		endpoint: b.runtimeHost,
		mode:     mode, detail: detail,
	}
}

// recordBedrockTransport records, for a Bedrock run, the run.bedrock.configure row
// applyBedrockTransport computed, once resolveLLMInjections' gates have all
// held (#518). `provider` names the model provider a provider run chose (#530).
func (s *Server) recordBedrockTransport(ctx context.Context, run types.AgentRun, llm llmTransport) {
	if !llm.bedrockReady {
		return
	}
	a := llm.bedrockAudit
	data := map[string]any{
		"region": a.region, "model": a.model, "hosts": a.hosts, "endpoint": a.endpoint,
		"mode": a.mode, "detail": a.detail,
	}
	if run.ModelProviderID != "" {
		data["provider"] = run.ModelProviderID
	}
	s.recordAudit(ctx, s.auditEvent(&run.ID, types.ActorSystem, "wardynd", "run.bedrock.configure",
		run.ID.String(), "success", mustJSON(data)))
}

// provisionDispatchMITMCA provisions the per-run TLS-MITM CA when any consumer
// needs it (subscription/managed injection, intercept_tls content inspection,
// artifact-token injection, or Bedrock bearer injection). The PRIVATE key
// reaches ONLY the proxy sidecar (ProxyConfig); the sandbox trusts the PUBLIC
// cert, installed by agent-run from WARDYN_MITM_CA_PEM and pointed at via
// NODE_EXTRA_CA_CERTS (additive for Node clients like Claude Code). On failure
// it marks the run FAILED (CAS from STARTING so a concurrent kill's KILLED
// state is preserved), audits, and returns ok=false — the dispatch must stop.
// Extracted verbatim from dispatchRun.
func (s *Server) provisionDispatchMITMCA(ctx context.Context, run types.AgentRun, sandboxEnv map[string]string) (certPEM, keyPEM string, ok bool) {
	pemCert, pemKey, caErr := generateRunCA(time.Now())
	if caErr != nil {
		slog.ErrorContext(ctx, "wardynd: could not provision the per-run TLS-interception CA",
			slog.String("run_id", run.ID.String()), slog.Any("err", caErr))
		s.failAndRevoke(ctx, run.ID, types.RunStarting, "could not provision the per-run TLS-interception CA")
		s.recordAudit(ctx, s.auditEvent(&run.ID, types.ActorSystem, "wardynd", "run.create",
			run.ID.String(), "failure", mustJSON(map[string]any{"error": "mitm ca: " + caErr.Error()})))
		return "", "", false
	}
	certPEM, keyPEM = string(pemCert), string(pemKey)
	sandboxEnv["WARDYN_MITM_CA_PEM"] = certPEM
	// CA files live under /tmp/wardyn (any-uid-writable, works for images with
	// arbitrary USER/HOME — the old /home/agent/.wardyn pin dangled for
	// envbuilder/BYOI images whose HOME differs; precedent: mainProcCastDir).
	// WHICH variables, and why their values differ, is sandboxCATrustVars'
	// business — this run's own CA is authoritative here, so nothing is spared.
	setSandboxCATrustVars(sandboxEnv, false)
	return certPEM, keyPEM, true
}

// sandboxCATrustVars is THE list of CA-trust variables a sandbox's toolchains
// read, and the single place any of them is named. It exists because this list
// was duplicated across the two callers here and drifted: AWS_CA_BUNDLE was
// added to one and missed on the other, which left the sandbox's AWS CLI
// distrusting Wardyn's OWN per-run interception CA on the Bedrock lane the
// product drives itself. One list, two callers, nothing to keep in sync.
//
// "Install the CA" is one action PER TOOLCHAIN, and the values are not
// interchangeable. NODE_EXTRA_CA_CERTS is ADDITIVE (Node keeps its bundled
// roots) so it takes the BARE CA. Every other variable REPLACES its trust
// store outright, so each takes the COMBINED bundle (system roots + the
// per-run CA) that install_mitm_ca/agentIdleScript assemble — the bare CA
// there would break verification of every non-MITM'd CONNECT-tunneled host.
// AWS CLI v2 needs its own entry because it ships its own Python and its own
// CA store and reads none of the other four. The JVM keystore and Deno's
// DENO_CERT are covered by no variable at all and are documented as not
// covered (deploy/images/README.md).
func sandboxCATrustVars() map[string]string {
	return map[string]string{
		"NODE_EXTRA_CA_CERTS": "/tmp/wardyn/mitm-ca.pem",
		"SSL_CERT_FILE":       "/tmp/wardyn/ca-bundle.pem",
		"REQUESTS_CA_BUNDLE":  "/tmp/wardyn/ca-bundle.pem",
		"CURL_CA_BUNDLE":      "/tmp/wardyn/ca-bundle.pem",
		"AWS_CA_BUNDLE":       "/tmp/wardyn/ca-bundle.pem",
	}
}

// setSandboxCATrustVars writes that list into sandboxEnv. onlyIfUnset leaves
// any value already staged alone, which is what the corporate-CA append needs:
// a MITM'd run's own /tmp/wardyn paths must never be clobbered.
func setSandboxCATrustVars(sandboxEnv map[string]string, onlyIfUnset bool) {
	for k, v := range sandboxCATrustVars() {
		if onlyIfUnset && sandboxEnv[k] != "" {
			continue
		}
		sandboxEnv[k] = v
	}
}

// installSandboxTrustedCA appends the operator's corporate PEM
// (WARDYN_TRUSTED_CA_FILE, corpPEM) to this run's sandbox CA trust, so a
// TLS-inspecting corporate middlebox on the path between wardyn-proxy and the
// real internet is trusted by the sandbox's OWN TLS clients on a passthrough
// (non-MITM'd) CONNECT tunnel — not only by wardynd and wardyn-proxy
// themselves (see trusted_ca.go / proxy/server.go). corpPEM == "" (the knob
// unset) is a no-op: sandboxEnv is left exactly as provisionDispatchMITMCA
// (or nothing, on a non-MITM run) left it.
//
// install_mitm_ca (agent-run-lib.sh) treats WARDYN_MITM_CA_PEM as an opaque
// PEM bundle already — multiple certificates just concatenate — so appending
// here is additive to whatever provisionDispatchMITMCA staged for this run's
// OWN per-run interception CA. On a run with NO Wardyn-side MITM (that block
// above never ran), WARDYN_MITM_CA_PEM would otherwise be entirely absent and
// install_mitm_ca would no-op, so this also SEEDS it plus the five bundle
// vars the sandbox's toolchains need — but only when provisionDispatchMITMCA
// has not already set them, so a MITM'd run's own /tmp/wardyn paths are never
// touched here.
//
// Five, not four: "install the CA" is one action PER TOOLCHAIN, and AWS CLI v2
// ships its OWN Python and its OWN CA store — it reads none of the other four.
// Without AWS_CA_BUNDLE a MITM'd Bedrock/STS call still fails, in a lane the
// product drives itself. deploy/images/README.md carries the per-toolchain
// table; the JVM keystore is addressed by no variable at all.
func installSandboxTrustedCA(corpPEM string, sandboxEnv map[string]string) {
	if corpPEM == "" {
		return
	}
	if existing := sandboxEnv["WARDYN_MITM_CA_PEM"]; existing != "" {
		sandboxEnv["WARDYN_MITM_CA_PEM"] = existing + "\n" + corpPEM
	} else {
		sandboxEnv["WARDYN_MITM_CA_PEM"] = corpPEM
	}
	setSandboxCATrustVars(sandboxEnv, true)
}

// authorBedrockBearerInjection authors the Bedrock BEARER injection: an api_key
// grant whose Authorization: Bearer header injects the run owner's own Bedrock
// API key for the chosen provider into bedrock-runtime, and marks that host
// TLS-MITM-eligible for THIS run. This is the same operator-configured
// MITM-host + paired-injection pattern as corp artifact hosts (isCorpMITMHost)
// — bedrock-runtime is not a wildcard, and the CA key stays in proxy memory.
// The sandbox holds only the placeholder bearer. Returns the updated injections and
// the MITM host list; ok=false means the grant write failed, the run was marked
// FAILED (CAS from STARTING), and dispatch must stop. Extracted verbatim from
// dispatchRun.
//
// The MITM entry is "host:port" (net.JoinHostPort), NOT a bare host, for the
// same reason planArtifactRedirect authors one (artifact_redirect.go): a bare
// entry is ANY-PORT in the proxy (parseMITMHostPort returns port 0, and
// handleConnect's `cport == 0 || cport == port` then matches everything), so
// an agent that can reach the Bedrock host at all could CONNECT to it on a
// port nobody configured and have that tunnel TLS-terminated with the Wardyn
// leaf and the operator's Bearer injected onto whatever answered there.
// The injection SCOPE below stays a bare host — buildInjector requires that —
// only the MITM-eligibility set carries the port.
//
// The scope's snapshot records WHOSE key it is and for which provider: the
// sink resolves the key under the provider's UID from exactly that namespace
// (resolveProviderKeyInjection). Only the provider arm sets injectBedrockBearer,
// so t.provider is always set here.
func (s *Server) authorBedrockBearerInjection(ctx context.Context, run types.AgentRun, t llmTransport, injections []runner.InjectionGrant) ([]runner.InjectionGrant, []string, bool) {
	mitmHosts := []string{net.JoinHostPort(t.bedrock.runtimeHost, strconv.Itoa(t.bedrock.runtimePort))}
	c := t.provider
	beScope, _ := json.Marshal(map[string]any{
		"host":        t.bedrock.runtimeHost,
		"header":      "Authorization",
		"format":      "Bearer %s",
		"secret_name": providerSecretName(c.provider.UID, providerKeyPart),
		"snapshot":    providerGrantSnapshot{ProviderUID: c.provider.UID, OwnerSubject: c.owner},
	})
	beGrantID := uuid.New()
	if _, gerr := s.createDispatchGrant(ctx, run, types.CredentialGrant{
		ID: beGrantID, RunID: run.ID, CreatedAt: time.Now(),
		Spec: types.GrantSpec{Kind: types.GrantAPIKey, Scope: beScope, TTLSeconds: 3600},
	}, false); gerr != nil {
		// CAS from STARTING (claimed at dispatch entry) so a concurrent kill's
		// KILLED state is preserved rather than clobbered back to FAILED.
		// The hint is member-visible: a fixed sentence, never the store's text.
		slog.ErrorContext(ctx, "wardynd: could not record the Bedrock bearer credential grant",
			slog.String("run_id", run.ID.String()), slog.Any("err", gerr))
		s.failAndRevoke(ctx, run.ID, types.RunStarting, "could not record the Bedrock bearer credential grant")
		s.recordAudit(ctx, s.auditEvent(&run.ID, types.ActorSystem, "wardynd", "run.create",
			run.ID.String(), "failure", mustJSON(map[string]any{"error": "bedrock bearer inject grant: " + gerr.Error()})))
		return injections, nil, false
	}
	if rule, derr := injectionRuleFromScope(beScope); derr == nil {
		injections = append(injections, runner.InjectionGrant{GrantID: beGrantID, Rule: rule})
	}
	return injections, mitmHosts, true
}

// llmInspectMITMEnabled reports whether the policy's intercept_tls content
// inspection is active (mode set and not "off") — the content-inspection reason
// to provision the per-run MITM CA and TLS-terminate the built-in LLM hosts.
func llmInspectMITMEnabled(policy *types.RunPolicySpec) bool {
	li := policy.LLMInspection
	return li != nil && li.InterceptTLS && li.Mode != "" && !strings.EqualFold(li.Mode, "off")
}

// inspectableLLMRefusal is require_inspectable_llm's refusal. It enumerates
// BOTH Bedrock sub-modes: the gate below fails the bearer lane closed too, and
// a sentence that named only SigV4 told an operator their bearer run was
// refused for a reason that did not apply to it. It is a run failure hint AND
// the quoted `error` value on the run.create failure row.
//
// DRAFT (M2 canon pending)
const inspectableLLMRefusal = "require_inspectable_llm: the resolved LLM transport is opaque (Bedrock — both " +
	"SigV4 and bearer, which Wardyn can decrypt but has no extractor for); choose an inspectable model provider"

// enforceInspectableLLM fails CLOSED at schedule time when inspection is
// REQUIRED but the resolved LLM transport is OPAQUE: BEDROCK, BOTH sub-modes —
// proxy-injected + MITM'd does NOT make Bedrock inspectable:
// require_inspectable_llm is a RUNTIME guarantee (policy.go), and MITM only
// makes a body READABLE — SCANNING it needs an extractor and a prompt-bearing
// channel, and there is neither for Bedrock. contentscan.Extract handles
// anthropic.messages / openai.chat / generic / mcp.jsonrpc only, and
// channelForHost gives a bedrock-runtime host ChannelGeneric, which classifyLLM
// treats as not prompt-bearing — so a bearer Bedrock run admitted as
// "inspectable" would get ZERO scan coverage. Both sub-modes therefore fail
// closed until a Bedrock extractor + channel exist; when they do, re-exempt the
// bearer arm HERE (one predicate) and say so in THREAT-MODEL 5.1a. Every other
// provider kind is inspectable: the key and endpoint kinds ride the brokered
// route, and a Claude subscription is always injected under MITM. The default
// (require_inspectable_llm=false) instead degrades visibly rather than
// failing. Returns false when the run was marked FAILED (CAS from STARTING so
// a concurrent kill's KILLED is not clobbered) and dispatch must stop.
func (s *Server) enforceInspectableLLM(ctx context.Context, run types.AgentRun, policy *types.RunPolicySpec, llm llmTransport) bool {
	li := policy.LLMInspection
	if li == nil || !li.RequireInspectableLLM || li.Mode == "" || strings.EqualFold(li.Mode, "off") {
		return true
	}
	if llm.bedrockReady {
		s.failAndRevoke(ctx, run.ID, types.RunStarting, inspectableLLMRefusal)
		s.recordAudit(ctx, s.auditEvent(&run.ID, types.ActorSystem, "wardynd", "run.create",
			run.ID.String(), "failure", mustJSON(map[string]any{"error": inspectableLLMRefusal})))
		return false
	}
	return true
}

// dispatchLLMPlan is what the LLM/credential-injection phase decides: the
// resolved transport, the injection list it authored, and the TLS-MITM material
// the proxy sidecar needs. A struct because the phase produces six values and
// threading six returns through dispatchRun is how one of them gets dropped.
type dispatchLLMPlan struct {
	llm        llmTransport
	injections []runner.InjectionGrant
	// mitmCACertPEM/mitmCAKeyPEM are the per-run CA; empty unless some consumer
	// needs one. The PRIVATE key reaches ONLY the proxy sidecar.
	mitmCACertPEM string
	mitmCAKeyPEM  string
	// bedrockMITMHosts is this run's per-run MITM host list beyond the built-in
	// api.anthropic.com/api.openai.com, if any, each as "host:port" (never a
	// bare host): the Bedrock bearer's runtime host (authorBedrockBearerInjection),
	// the captured-SSO portal host (authorBedrockSSOInjection), and a Claude
	// subscription provider's route-through host
	// (authorProviderSubscriptionInjection) — all three are mutually exclusive
	// per run, so one slice is safe.
	bedrockMITMHosts []string
	// azure is the azure_foundry lane's share of the proxy config: its MITM entry (a fourth producer
	// beside the artifact, Bedrock and Azure DevOps ones), its route gate and its channel host.
	azure azurePlan
	// llmUnavailableDetail is the self-explaining detail the proxy's brokered-LLM
	// 404 renders when this run reaches that route with no credential behind it
	// (mpNoProviderDetail). Empty => the route's own generic detail.
	llmUnavailableDetail string
	// llmUpstreams is ProxyConfig.LLMUpstreams: only the chosen provider's own
	// address (nil when requests go to the vendor host) — a gateway reaches
	// only the runs that chose it.
	llmUpstreams map[string]string
	// mitmLLM is whether the BUILT-IN LLM hosts should be intercepted —
	// subscription injection or intercept_tls inspection, never a CA minted
	// purely for artifact tokens. Computed here because every input to it is
	// decided here.
	mitmLLM bool
}

// resolveLLMInjections resolves the LLM transport and authors every proxy-side
// injection that follows from it, as ONE phase.
//
// It is a phase and not a slice taken to satisfy a complexity counter: the
// chosen model provider's kind is what determines whether this run needs a
// MITM CA, a subscription sentinel grant, a Bedrock bearer, or none of them —
// so the authoring steps are consequences of one decision rather than
// neighbours that happen to be adjacent. The corporate-CA install rides along
// because its placement is load-bearing: before resolveEnvSecretGrants, so a
// user env_secret named SSL_CERT_FILE cannot clobber the bundle it stages.
//
// Fail closed, and the ok return is the whole contract: each authoring helper
// has already marked the run FAILED before returning false, so a false here
// means "stop dispatching, the run is already terminal".
//
// policy and sandboxEnv are MUTATED (egress widening for Bedrock, the auth env,
// the trust store); injections is returned rather than mutated because the
// strip and the grant authors reslice it.
func (s *Server) resolveLLMInjections(ctx context.Context, run types.AgentRun, p dispatchParams,
	policy *types.RunPolicySpec, sandboxEnv map[string]string, injections []runner.InjectionGrant,
	proxyURL string, artifactPlan artifactRedirectPlan, artifactInject bool, siteCfg types.SiteConfig, siteCfgOK bool,
	adoInject bool, bedrockGrade bedrockCredGrade,
) (dispatchLLMPlan, bool) {
	// The model provider the run chose owns its model credential: that
	// provider, from its owner's own credential, or none (resolveProviderLane,
	// every kind). Every other injection that would credential the model is
	// stripped there, whoever authored it.
	llm, injections, prov, ok := s.resolveProviderLane(ctx, run, p, policy, sandboxEnv, injections, proxyURL, siteCfg, siteCfgOK)
	if !ok {
		return dispatchLLMPlan{}, false
	}
	// A Bedrock provider's session grant, and its reauth hold, record this
	// scope; zero on every other kind.
	sso := llm.providerAWSScope()
	// And none the autonomy gate graded this run without (bedrockCredGradeHolds).
	if !s.bedrockCredGradeHolds(ctx, run, bedrockGrade, llm) {
		return dispatchLLMPlan{}, false
	}
	// Optional TLS-MITM of opaque LLM CONNECT tunnels: provision a per-run CA
	// when ANY consumer needs one — intercept_tls content inspection,
	// subscription credential injection, artifact-token injection, Bedrock
	// injection, the per-person Azure DevOps credential
	// (authorADOEntraInjection, which REFUSES a run that reaches it without
	// one), or a component's header, which the proxy can set only inside a
	// connection it terminates. The PRIVATE key reaches ONLY the proxy sidecar (ProxyConfig below);
	// the sandbox trusts the PUBLIC cert. See provisionDispatchMITMCA for the
	// trust-store wiring.
	mitmForInspect := llmInspectMITMEnabled(policy)
	var mitmCACertPEM, mitmCAKeyPEM string
	if llm.providerSubscription() || mitmForInspect || artifactInject || llm.injectBedrockBearer || llm.injectBedrockSSO || adoInject || llm.azureInject() || p.PATAPI || len(p.Components.MITMHosts) > 0 {
		if mitmCACertPEM, mitmCAKeyPEM, ok = s.provisionDispatchMITMCA(ctx, run, sandboxEnv); !ok {
			return dispatchLLMPlan{}, false
		}
	}

	// Corporate CA trust (WARDYN_TRUSTED_CA_FILE): append the operator's PEM to
	// this run's sandbox CA trust exactly as provisionDispatchMITMCA does for
	// the per-run MITM cert — see installSandboxTrustedCA. Runs unconditionally
	// (no-op when the knob is unset) so a non-MITM run gets it too. Placed
	// before resolveEnvSecretGrants below, so a user env_secret named
	// SSL_CERT_FILE can never clobber the bundle this just staged.
	installSandboxTrustedCA(s.cfg.TrustedCAPEM, sandboxEnv)

	// The subscription arm's grant: its owner's own sign-in, recording whose it
	// is. After resolveProviderLane's strip, like every arm's grant. A failed
	// grant write already marked the run FAILED — stop.
	var bedrockMITMHosts []string
	if llm.providerSubscription() {
		if injections, bedrockMITMHosts, ok = s.authorProviderSubscriptionInjection(ctx, run, llm, policy, injections); !ok {
			return dispatchLLMPlan{}, false
		}
	}

	// Bedrock BEARER injection + its per-run MITM host (see
	// authorBedrockBearerInjection). Same stop-on-failure contract; appends to
	// the SAME MITM host list as the subscription block above — the two are
	// mutually exclusive per run.
	if llm.injectBedrockBearer {
		if injections, bedrockMITMHosts, ok = s.authorBedrockBearerInjection(ctx, run, llm, injections); !ok {
			return dispatchLLMPlan{}, false
		}
	}

	// Captured-AWS-SSO injection + its per-run MITM host (PHASE B, see
	// authorBedrockSSOInjection). Same stop-on-failure contract as the bearer
	// block above, and it appends to the SAME MITM host list: a run is on one
	// Bedrock lane or the other, never both, so the two never collide.
	if llm.injectBedrockSSO {
		var ssoMITMHosts []string
		if injections, ssoMITMHosts, ok = s.authorBedrockSSOInjection(ctx, run, llm, sso, injections); !ok {
			return dispatchLLMPlan{}, false
		}
		bedrockMITMHosts = append(bedrockMITMHosts, ssoMITMHosts...)
	}

	// The azure_foundry lane's grant, MITM entry, route gate and channel host. Same stop-on-failure
	// contract; it needs the per-run CA minted above and refuses a run that has none.
	var azure azurePlan
	if llm.azureInject() {
		if injections, azure, ok = s.authorAzureInjection(ctx, run, llm, mitmCACertPEM, mitmCAKeyPEM, policy, injections); !ok {
			return dispatchLLMPlan{}, false
		}
	}

	// Artifact-redirect token injections (authored in planArtifactRedirect, whose
	// egress substitution already added each corp host to policy.AllowedDomains, so
	// the injector's exact-allowlist check passes). Appended AFTER the grant
	// authors above, which reslice `injections` in place.
	injections = append(injections, artifactPlan.injections...)

	// Fail CLOSED at schedule time when inspection is REQUIRED but the resolved
	// LLM transport is OPAQUE — see enforceInspectableLLM.
	if !s.enforceInspectableLLM(ctx, run, policy, llm) {
		return dispatchLLMPlan{}, false
	}

	// Only NOW — every gate above held, including the MITM CA provisioning and
	// grant-authoring steps between here and bedrockCredGradeHolds, any one of
	// which can still fail closed — is it true that this run actually gets the
	// Bedrock credential applyBedrockTransport resolved. Recording the
	// run.bedrock.configure row here is what keeps a run ANY later refusal
	// takes from showing a "success" injection row for a credential it was
	// never handed (#518).
	s.recordBedrockTransport(ctx, run, llm)

	return dispatchLLMPlan{
		llm: llm, injections: injections,
		mitmCACertPEM: mitmCACertPEM, mitmCAKeyPEM: mitmCAKeyPEM,
		bedrockMITMHosts:     bedrockMITMHosts,
		azure:                azure,
		mitmLLM:              llm.providerSubscription() || mitmForInspect,
		llmUnavailableDetail: prov.detail, llmUpstreams: prov.upstreams,
	}, true
}
