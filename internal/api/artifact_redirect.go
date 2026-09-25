// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"maps"
	"net"
	"slices"
	"strconv"
	"strings"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/hostrules"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// artifactRedirectPlan carries the dispatch-time artifact-redirect wiring derived
// from the operator-wide site-config (NEVER from sandbox input): per-tool config
// files (delivered to the sandbox via WARDYN_ARTIFACT_CONFIG_B64), go's registry
// env, proxy-side token injections, and the corp hosts that must be TLS-MITM'd so
// a token can be injected on the wire (the sandbox never holds it). A zero plan
// means "no redirect configured" — dispatch is unchanged.
type artifactRedirectPlan struct {
	env        map[string]string       // go GOPROXY/GOSUMDB (env-honoring)
	configB64  string                  // per-tool config files, agent-run materializes
	injections []runner.InjectionGrant // proxy-side token injections (api_key grants)
	// mitmHosts is "host:port" (net.JoinHostPort) — NOT a bare host — for each
	// corp mirror to TLS-MITM for token injection, so the proxy's MITM-eligibility
	// and its post-decrypt dial are both scoped to the port this redirect
	// actually names rather than always assuming 443.
	mitmHosts []string
}

// redirectPublicHosts is the set of public hosts one redirect fronts: an
// ecosystem row's whole public-registry table, or a network-only row's single
// From host. Empty for a malformed From on a network-only row (the caller then
// treats the row as contributing nothing — fail safe).
func redirectPublicHosts(r types.EgressRedirect) []string {
	if r.Ecosystem != "" {
		return hostrules.PublicRegistryHosts(r.Ecosystem)
	}
	if from := strings.ToLower(hostrules.HostOf(r.From)); from != "" {
		return []string{from}
	}
	return nil
}

// redirectEndpointPort reads the port a redirect endpoint (a full URL or a bare
// "host[:port][/path]", per validSiteURLOrHost) SPELLS, off the same authority
// hostrules.HostOf reads its host from, so the two parsers cannot disagree; the
// authority ends at IndexAny("/?#") so a query never reaches strconv.
//
//	(p, true, true)   a port is spelled and is a decimal 1-65535
//	(0, false, true)  no port is spelled — the caller applies its scheme default
//	(0, true, false)  spelled but unusable: validateSiteConfig REFUSES it at PUT, never coerced
//
// The FIRST ':' reads a bracketed IPv6 authority as unusable: inert, since
// validSiteURLOrHost refuses IPv6 redirect endpoints, but a future IPv6 story
// must teach BOTH parsers at once.
func redirectEndpointPort(rawURL string) (port int, spelled, ok bool) {
	s := strings.TrimSpace(rawURL)
	if i := strings.Index(s, "://"); i >= 0 {
		s = s[i+3:]
	}
	if i := strings.IndexAny(s, "/?#"); i >= 0 {
		s = s[:i]
	}
	i := strings.IndexByte(s, ':')
	if i < 0 {
		return 0, false, true
	}
	p, err := strconv.Atoi(s[i+1:])
	if err != nil || p < 1 || p > 65535 {
		return 0, true, false
	}
	return p, true, true
}

// redirectPort is the port a redirect's To endpoint NAMES: the one it spells,
// else the default of the SCHEME it spells — 80 for an explicit "http://",
// 443 otherwise (a bare host and an "https://" URL are both dialed as a CONNECT
// tunnel the proxy TLS-terminates). Being scheme-blind would probe a plain-http
// mirror's 443 and report it blocked. planArtifactRedirect puts "host:port" in
// mitmHosts so TLS termination dials the mirror's REAL port; an http:// To goes
// through the proxy's handlePlain, never a CONNECT, so its 80 default narrows
// that MITM entry to a port no CONNECT arrives on rather than widening anything.
// A spelled-but-unusable port is refused at PUT (validateSiteConfig), so the
// scheme default also covers an older row, fail-safe. The Bedrock data-plane
// authority (WARDYN_BEDROCK_BASE_URL) uses this too, so both MITM-authoring
// lanes derive their port by the SAME rule.
func redirectPort(rawURL string) int {
	if p, spelled, ok := redirectEndpointPort(rawURL); ok && spelled {
		return p
	}
	if redirectIsCleartext(rawURL) {
		return 80
	}
	return 443
}

// redirectIsCleartext reports whether a redirect `to` asks for PLAIN HTTP — the
// operator spelled `http://`. One predicate, two readers (redirectPort's 80/443
// default and the injection scope's require_tls), because two hand-rolled
// scheme tests over one operator-authored field is how the port and the
// transport intent drift apart.
func redirectIsCleartext(rawURL string) bool {
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(rawURL)), "http://")
}

// artifactRunHostSet is the lowercased bare-host set of a run's egress entries,
// for deciding which redirects are in scope for it (artifactRedirectApplies /
// runReachesAny).
func artifactRunHostSet(domains []string) map[string]bool {
	have := make(map[string]bool, len(domains))
	for _, d := range domains {
		have[egressEntryHost(d)] = true
	}
	return have
}

// runReachesAny reports whether a run whose egress host set is `have` reaches any
// bare host in pub — exactly (the host is listed) or via a "*." wildcard entry the
// run declared. Mirrors gitBrokerManaged's wildcard normalization.
func runReachesAny(have map[string]bool, pub []string) bool {
	for _, h := range pub {
		hl := strings.ToLower(h)
		if have[hl] {
			return true
		}
		for entry := range have {
			if suffix, wild := strings.CutPrefix(entry, "*"); wild && strings.HasSuffix(hl, suffix) {
				return true
			}
		}
	}
	return false
}

// artifactRedirectApplies reports whether redirect r is in scope for a run whose
// pre-substitution egress host set is `have` — the run named the From host or (for
// an ecosystem row) any of that ecosystem's public registry hosts. The same
// predicate scopes both the To-host add (substituteArtifactEgress) and the corp
// token injection (planArtifactRedirect), so the two can never disagree about
// which runs a redirect touches.
func artifactRedirectApplies(r types.EgressRedirect, have map[string]bool) bool {
	return runReachesAny(have, redirectPublicHosts(r))
}

// entryCoversAny reports whether ONE egress-allowlist entry names (or, via a "*."
// wildcard, covers) any bare host in drop — the port/wildcard-aware match the
// ecosystem substitution drop needs so a "*.pythonhosted.org" or "pypi.org:443"
// entry is subtracted like a bare "pypi.org" is.
func entryCoversAny(entry string, drop map[string]bool) bool {
	h := egressEntryHost(entry)
	if suffix, wild := strings.CutPrefix(h, "*"); wild {
		for dh := range drop {
			if strings.HasSuffix(dh, suffix) {
				return true
			}
		}
		return false
	}
	return drop[h]
}

// artifactBaseURLs extracts ecosystem -> base URL (URL-only, no token) from a
// SiteConfig's EgressRedirects, skipping every NETWORK-ONLY row (Ecosystem ==
// ""), or nil when nothing is configured. This is the sole ecosystem-scoped
// filter both EmitArtifactConfig callers (planArtifactRedirect below,
// workspace_envcode.go's envAsCodeFor) go through, so a network-only redirect
// can never accidentally grow a package-manager config file.
func artifactBaseURLs(sc types.SiteConfig) map[string]string {
	if len(sc.EgressRedirects) == 0 {
		return nil
	}
	out := make(map[string]string, len(sc.EgressRedirects))
	for _, r := range sc.EgressRedirects {
		if r.Ecosystem == "" {
			continue // network-only: no per-tool config file
		}
		out[r.Ecosystem] = r.To
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// planArtifactRedirect builds the dispatch-time egress-redirect plan for a run
// from the operator-wide site-config: each Ecosystem-tier redirect's per-tool
// config (URL-only) as a base64 env payload agent-run materializes under $HOME,
// and, for each To host (BOTH tiers; a network-only row contributes no file)
// whose token secret EXISTS, a stored-secret api_key grant + injection rule with
// the host marked for TLS-MITM (the injector cannot rewrite an opaque CONNECT).
// No token, a dangling token ref or a failed grant create (audited) degrades to
// redirect-only, never failing the run: anonymous-read mirrors need no token.
//
// preDomains is the run's PRE-substitution allowlist: injection + TLS-MITM only
// when the run reaches a public host the redirect fronts (artifactRedirectApplies,
// as substituteArtifactEgress), so no run gets the token on a host it never named.
func (s *Server) planArtifactRedirect(ctx context.Context, run types.AgentRun, sc types.SiteConfig, preDomains []string) artifactRedirectPlan {
	var plan artifactRedirectPlan
	if len(sc.EgressRedirects) == 0 {
		return plan
	}
	have := artifactRunHostSet(preDomains)
	files, env := hostrules.EmitArtifactConfig(artifactBaseURLs(sc))
	if len(env) > 0 {
		plan.env = env
	}
	if len(files) > 0 {
		plan.configB64 = encodeArtifactConfig(files)
	}

	// Which stored secrets exist: token injection degrades to redirect-only when
	// the referenced secret is absent (never fail the run on a dangling ref).
	// Operator namespace: an artifact-registry token ref is site-config, not a
	// member's own row.
	present := map[string]bool{}
	if s.cfg.Secrets != nil {
		if names, err := s.cfg.Secrets.List(ctx); err == nil {
			for _, n := range names {
				present[n] = true
			}
		}
	}

	// Dedupe token injection by TO host — one corp mirror/relay commonly backs
	// several redirects (ecosystem or network-only alike). EgressRedirects is a
	// stored SLICE (operator-authored order, already deterministic), so a
	// shared host with divergent token refs resolves first-sighted-wins stably
	// without needing a sort.
	seenHost := map[string]bool{}
	for _, r := range sc.EgressRedirects {
		if r.TokenSecretRef == "" && r.TokenIntegrationRef == "" {
			continue // no token configured for this redirect
		}
		// Scope: inject the corp token only for a run that actually
		// reaches the public host this redirect fronts — never a run whose reviewed
		// egress named neither the From host nor the ecosystem.
		if !artifactRedirectApplies(r, have) {
			continue
		}
		host := strings.ToLower(hostrules.HostOf(r.To))
		if host == "" || seenHost[host] {
			continue
		}
		// Veto a To that is the configured model gateway or a public model-provider
		// host: proxy.buildInjector's byHost map is last-write-wins, so a colliding
		// row would swap an artifact token onto model traffic. isModelProviderRejectHost,
		// NOT isModelProviderHost: the Bedrock hosts (and WARDYN_BEDROCK_BASE_URL)
		// carry proxy-side bearer injection too (resolveBedrockAuth), so they are the
		// SAME collision. A ZERO types.Workspace is deliberate: this plan is composed
		// before resolveLLMTransport, and a run can reference several workspaces
		// (run.WorkspaceIDs), so only the daemon-wide BedrockRegion pair is decidable
		// here; bedrockLaneHosts' per-workspace half is not exercised at this site.
		if s.isModelProviderRejectHost(ctx, types.Workspace{}, host) {
			s.recordAudit(ctx, s.auditEvent(&run.ID, types.ActorSystem, "wardynd", "run.artifact.redirect",
				run.ID.String(), "warn", mustJSON(map[string]any{
					"ecosystem": r.Ecosystem, "host": host,
					"detail": "refused: To names a model-provider or configured gateway host, which would collide with the LLM injection route",
				})))
			continue
		}
		// The redirect's REAL port travels with the host into
		// plan.mitmHosts (below) so the proxy's TLS-MITM allowlist — and the dial
		// it performs once it has decrypted the tunnel — are scoped to the mirror
		// this redirect actually names, not always port 443.
		port := redirectPort(r.To)
		tok, why := s.resolveRedirectToken(ctx, r, present)
		if why != "" {
			s.recordAudit(ctx, s.auditEvent(&run.ID, types.ActorSystem, "wardynd", "run.artifact.redirect",
				run.ID.String(), "warn", mustJSON(map[string]any{
					"ecosystem": r.Ecosystem, "host": host,
					"detail": why + "; redirect applied without token injection",
				})))
			continue
		}
		seenHost[host] = true
		grantID := uuid.New()
		// require_tls: the redirect's OWN transport intent, declared by the one
		// producer that knows it. An `https://` To is TLS by construction (the proxy
		// injects on the decrypted leg), so a cleartext request to the mirror is the
		// SANDBOX choosing the transport and the corp token must be refused (403,
		// policy:require-tls), not silently withheld. It also keeps this from
		// widening the cleartext door: the port-qualified allowlist entry this
		// authors reads as declared intent (proxy's AuthoredPortFor). An explicit
		// `http://` To is the operator asking for cleartext, so it does NOT set the
		// flag; there proxy.injectableTransport's port-80 arm is the right answer.
		scope, _ := json.Marshal(map[string]any{
			"host":        host,
			"header":      tok.header,
			"format":      tok.format,
			"secret_name": tok.secretName,
			"require_tls": !redirectIsCleartext(r.To),
		})
		if _, gerr := s.cfg.Store.CreateGrant(ctx, types.CredentialGrant{
			ID: grantID, RunID: run.ID, CreatedAt: s.cfg.Now().UTC(),
			Spec: types.GrantSpec{Kind: types.GrantAPIKey, Scope: scope, TTLSeconds: 3600},
		}); gerr != nil {
			s.recordAudit(ctx, s.auditEvent(&run.ID, types.ActorSystem, "wardynd", "run.artifact.redirect",
				run.ID.String(), "failure", mustJSON(map[string]any{
					"ecosystem": r.Ecosystem, "host": host, "error": gerr.Error(),
				})))
			continue
		}
		rule, rerr := injectionRuleFromScope(scope)
		if rerr != nil {
			continue
		}
		plan.injections = append(plan.injections, runner.InjectionGrant{GrantID: grantID, Rule: rule})
		// host:port, NOT bare host: the proxy's mitmHosts/mitmPorts matching (and
		// therefore its dial once it has TLS-terminated the tunnel) is scoped to
		// exactly this port. The injection scope/rule above stays a BARE host —
		// buildInjector requires that — only the MITM-eligibility set carries the
		// port.
		plan.mitmHosts = append(plan.mitmHosts, net.JoinHostPort(host, strconv.Itoa(port)))
		s.recordAudit(ctx, s.auditEvent(&run.ID, types.ActorSystem, "wardynd", "run.artifact.redirect",
			run.ID.String(), "success", mustJSON(map[string]any{
				"ecosystem": r.Ecosystem, "host": host, "port": port, "tls_mitm": true, "secret_name": tok.secretName,
				"integration_id": r.TokenIntegrationRef,
				"detail":         "corporate mirror/relay token injected proxy-side; sandbox never holds it",
			})))
	}
	return plan
}

// redirectToken is one redirect's resolved credential presentation: which
// stored secret, in which header, wrapped how.
type redirectToken struct{ secretName, header, format string }

// resolveRedirectToken resolves a redirect row's token to the three facts the
// injection scope needs, from whichever of the two sources the row names.
// A non-empty `why` means no token — the caller applies the redirect WITHOUT
// injection and audits that reason.
//   - TokenSecretRef: a bare secret presented as "Authorization: Bearer <secret>",
//     byte-for-byte unchanged for every existing row.
//   - TokenIntegrationRef: the INTEGRATION owns the credential, so its Header and
//     Format come with the secret name (e.g. "X-JFrog-Art-Api" or a bare token).
//
// Every failure (unconfigured or disabled integration, no header credential, a
// missing secret) degrades to redirect-only rather than failing the run.
func (s *Server) resolveRedirectToken(ctx context.Context, r types.EgressRedirect, present map[string]bool) (redirectToken, string) {
	if r.TokenIntegrationRef != "" {
		integ, found := s.resolveIntegrationRef(ctx, "", r.TokenIntegrationRef)
		switch {
		case !found:
			return redirectToken{}, "token_integration_ref names no configured integration"
		case integ.Disabled:
			return redirectToken{}, "the integration named by token_integration_ref is disabled"
		case slices.Contains(integ.DisabledCapabilities, "credential"):
			// This is the read matrix's dispatch-side sibling: the read matrix
			// reports this integration's "credential" cell off (applyDisabled,
			// integrations.go) — dispatch must actually honor that, not just
			// the read surface, or the operator sees "off" while the token
			// keeps injecting on every matching run.
			return redirectToken{}, "the integration named by token_integration_ref has its credential capability disabled"
		}
		// HeaderSecret is the integration's proxy_header-delivered secret, its
		// empty stored format already materialized as "%s" (the raw secret IS
		// the header value; injectionRuleFromScope reads "" as "Bearer %s",
		// which would be wrong here).
		//
		// Role-agnostic, DELIBERATELY (base-component model, same rule as
		// applyIntegrationInjection): whatever role the row calls its secret,
		// its proxy_header delivery is what makes it a presentable credential —
		// so a row an operator points a redirect at with token_integration_ref
		// hands over that credential, full stop. The operator authored both
		// halves; the audit line below names the integration and the secret.
		secretName, header, format, ok := integ.HeaderSecret()
		if !ok {
			return redirectToken{}, "the integration named by token_integration_ref delivers no header credential"
		}
		if !present[secretName] {
			return redirectToken{}, "the integration's secret is not in the store"
		}
		return redirectToken{secretName: secretName, header: header, format: format}, ""
	}
	if !present[r.TokenSecretRef] {
		return redirectToken{}, "token_secret_ref not found"
	}
	return redirectToken{secretName: r.TokenSecretRef, header: "Authorization", format: "Bearer %s"}, ""
}

// encodeArtifactConfig serialises the per-tool config files into the
// WARDYN_ARTIFACT_CONFIG_B64 payload agent-run consumes: newline-delimited
// "<home-relative-path>\t<base64(content)>" records, sorted for determinism.
// base64 carries arbitrary file bytes (XML/TOML/newlines) through the env var and
// out of any shell parsing.
func encodeArtifactConfig(files map[string]string) string {
	paths := slices.Sorted(maps.Keys(files))
	var b strings.Builder
	for i, p := range paths {
		if i > 0 {
			b.WriteByte('\n')
		}
		b.WriteString(p)
		b.WriteByte('\t')
		b.WriteString(base64.StdEncoding.EncodeToString([]byte(files[p])))
	}
	return b.String()
}
