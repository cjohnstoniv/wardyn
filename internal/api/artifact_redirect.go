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
	// actually names (W13-S1-5) rather than always assuming 443.
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

// redirectPort extracts the port from a redirect's To URL/host, defaulting to
// 443 (every corp mirror/relay this feature targets is HTTPS — the sandbox
// never dials it directly, the proxy always TLS-terminates it, see mitm.go)
// when none is given. Mirrors workspacescan.HostOf's own scheme/path
// stripping so the two agree on where the authority ends; unlike HostOf it
// keeps the port instead of discarding it — planArtifactRedirect needs both,
// so mitmHosts can carry "host:port" and the proxy's TLS termination dials
// the mirror's REAL port instead of always assuming 443 (W13-S1-5).
func redirectPort(rawURL string) int {
	s := strings.TrimSpace(rawURL)
	if i := strings.Index(s, "://"); i >= 0 {
		s = s[i+3:]
	}
	if i := strings.IndexByte(s, '/'); i >= 0 {
		s = s[:i]
	}
	if _, ps, err := net.SplitHostPort(s); err == nil {
		if p, err := strconv.Atoi(ps); err == nil && p > 0 && p < 65536 {
			return p
		}
	}
	return 443
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
// entry is subtracted like a bare "pypi.org" is (GAP-EGRESS-6).
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
// workspaces.go's envAsCodeFor) go through, so a network-only redirect can
// never accidentally grow a package-manager config file.
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
// from the operator-wide site-config. It:
//   - emits each Ecosystem-tier redirect's per-tool config (URL-only) as a
//     base64 env payload agent-run materializes under $HOME, plus go's
//     GOPROXY/GOSUMDB (network-only rows contribute no file — see
//     types.SiteConfig.EgressRedirects);
//   - for EVERY redirect's To host WITH a token secret THAT EXISTS (both
//     tiers), authors a stored-secret api_key grant + injection rule so the
//     token injects proxy-side, and marks the host for TLS-MITM (the injector
//     cannot rewrite an opaque CONNECT).
//
// A redirect with no token (or a token whose secret is absent) still redirects
// the URL/host — anonymous-read corp destinations work without a token, and a
// dangling token ref degrades to redirect-only rather than failing the run
// (non-blocking posture). Grant creation touches the store; a create failure is
// audited and that one redirect is skipped, never aborting the run.
//
// preDomains is the run's PRE-substitution egress allowlist. A token injection +
// TLS-MITM is authored for a redirect ONLY when the run actually reaches one of
// the public hosts it fronts (artifactRedirectApplies, GAP-EGRESS-2) — the SAME
// scope substituteArtifactEgress uses for the To-host add — so an unrelated sealed
// run never has the operator's registry token injected onto a host it never named.
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
		// SCOPE (GAP-EGRESS-2): inject the corp token only for a run that actually
		// reaches the public host this redirect fronts — never a run whose reviewed
		// egress named neither the From host nor the ecosystem.
		if !artifactRedirectApplies(r, have) {
			continue
		}
		host := strings.ToLower(hostrules.HostOf(r.To))
		if host == "" || seenHost[host] {
			continue
		}
		// W13-S1-5: the redirect's REAL port travels with the host into
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
		scope, _ := json.Marshal(map[string]string{
			"host":        host,
			"header":      tok.header,
			"format":      tok.format,
			"secret_name": tok.secretName,
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
//
// The two sources differ in more than where the secret name comes from:
//
//   - TokenSecretRef (the original): a bare secret, presented as
//     "Authorization: Bearer <secret>" because that is the only shape this path
//     ever supported. Unchanged, byte-for-byte, for every existing row.
//   - TokenIntegrationRef (the seam): the INTEGRATION owns the system and its
//     credential, so its own Header and Format come along with the secret name.
//     That is the actual gain — a feed authenticating with "X-JFrog-Art-Api" or
//     a bare token finally works, where the hardcoded Bearer above would have
//     sent a header the server rejects.
//
// Every failure degrades to redirect-only rather than failing the run, matching
// the dangling-secret posture this path already had: an unconfigured or
// disabled integration, one that delivers no header credential, or a secret
// that is not in the store.
func (s *Server) resolveRedirectToken(ctx context.Context, r types.EgressRedirect, present map[string]bool) (redirectToken, string) {
	if r.TokenIntegrationRef != "" {
		integ, found := s.resolveIntegrationRef(ctx, r.TokenIntegrationRef)
		switch {
		case !found:
			return redirectToken{}, "token_integration_ref names no configured integration"
		case integ.Disabled:
			return redirectToken{}, "the integration named by token_integration_ref is disabled"
		case slices.Contains(integ.DisabledCapabilities, "credential"):
			// PLATFORM-API-1's sibling: the read matrix reports this
			// integration's "credential" cell off (applyDisabled,
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
		// ROLE-AGNOSTIC, DELIBERATELY (base-component model, same rule as
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
