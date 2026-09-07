// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//lint:file-ignore SA1019 This file CONTAINS the compatibility fold: it reads
// the deprecated SiteConfig.ArtifactOverrides field precisely so a PUT body (or
// a `wardyn site-config apply` file) saved before EgressRedirects existed keeps
// working. foldLegacyArtifactOverrides is the fold; deprecating the field is
// what tells NEW callers to use EgressRedirects instead.

package api

import (
	"fmt"
	"log/slog"
	"maps"
	"net/http"
	"net/netip"
	"net/url"
	"slices"
	"strings"

	"github.com/cjohnstoniv/wardyn/internal/egress/proxy"
	"github.com/cjohnstoniv/wardyn/internal/hostrules"
	"github.com/cjohnstoniv/wardyn/internal/ipguard"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// validArtifactEcosystems is the closed set of ArtifactOverrides keys (the
// ecosystems Wardyn has emit-time config support for — R5 findings: npm/pip/
// cargo/maven/go each get their own registry config file, nuget its own).
var validArtifactEcosystems = map[string]bool{
	"npm": true, "pip": true, "cargo": true, "maven": true, "go": true, "nuget": true,
}

// ecosystemPublicURL maps an ArtifactOverride ecosystem key to the PUBLIC
// registry URL it redirects away from — used ONLY by foldLegacyArtifactOverrides
// to synthesize an EgressRedirect.From for a legacy ArtifactOverride (which had
// no From field of its own: it was implicitly "whatever that ecosystem's public
// registry is"). Copied verbatim into migration 0030's SQL (Postgres can't call
// Go), so keep the two in sync. Hosts match what workspacescan.PublicRegistryHosts
// already treats as each ecosystem's public registry (grepped, not invented);
// see 0030_egress_redirects.sql for the full per-ecosystem citation.
var ecosystemPublicURL = map[string]string{
	"npm":   "https://registry.npmjs.org/",
	"pip":   "https://pypi.org/simple/",
	"cargo": "https://index.crates.io/",
	"maven": "https://repo.maven.apache.org/maven2/",
	"go":    "https://proxy.golang.org",
	"nuget": "https://api.nuget.org/v3/index.json",
}

// shellSafeSiteString is the injection-safety half every persisted site-config
// string reaches this file through — with one documented exception, the bare
// host, which validSiteHost short-circuits ahead of this gate and which
// hostrules.ValidApprovedHost then bounds more tightly than this does
// (^[a-z0-9]([a-z0-9.-]{0,251}[a-z0-9])?$ after TrimSpace, so a control
// character never survives it either). In ONE place: non-empty, bounded, no control characters or
// DEL, and none of the shell/XML metacharacters hostrules.EmitArtifactConfig
// interpolates verbatim into .npmrc/pip.conf/.cargo/config.toml/settings.xml/
// NuGet.Config/GOPROXY (its doc names validateSiteConfig as the gate it relies
// on). Both validSiteURL and validSiteURLOrHost call it, so the shared property
// their docs claim is one function rather than two hand-kept copies that could
// drift apart on the next edit.
func shellSafeSiteString(raw string) bool {
	if raw == "" || len(raw) > 2048 {
		return false
	}
	for _, r := range raw {
		if r < 0x20 || r == 0x7f {
			return false
		}
	}
	return !strings.ContainsAny(raw, "`$;&|<>\"'\\")
}

// validSiteURL reports whether raw is safe to persist as a site-config URL
// (upstream proxy / artifact base URL): well-formed, http(s) scheme only, a
// real dotted host of the same shape ValidApprovedHost accepts, and
// shellSafeSiteString. These strings flow into proxy dial targets and emitted
// per-tool config files (.npmrc/pip.conf/settings.xml/GOPROXY/...), so this is
// SSRF/injection hardening, not cosmetic validation.
func validSiteURL(raw string) bool {
	if !shellSafeSiteString(raw) {
		return false
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return false
	}
	host := hostrules.HostOf(raw)
	return host != "" && hostrules.ValidApprovedHost(host)
}

// validSiteHost reports whether h is a bare host suitable for ScmHosts — the
// same shape the scanner's approved-egress promotion accepts (no scheme, port,
// path, or wildcard).
func validSiteHost(h string) bool {
	return hostrules.ValidApprovedHost(strings.ToLower(strings.TrimSpace(h)))
}

// validSecretRef reports whether ref names a real, non-reserved secret (the
// same rule handlePutSecret enforces on write): a valid secretNameRE identifier
// that is not one of the platform-internal reserved names.
func validSecretRef(ref string) bool {
	return secretNameRE.MatchString(ref) && !reservedSecret(ref)
}

// validSiteURLOrHost reports whether raw is a safe EgressRedirect endpoint for
// a role that NEVER gets interpolated into a config file — an Ecosystem row's
// From (substituteArtifactEgress only ever needs its HOST; the ecosystem's
// whole public-host table, not From, drives what gets dropped — see
// substituteArtifactEgress's doc) or either field of a network-only row. Three
// shapes are accepted: a full http(s) URL (validSiteURL) and a bare host with a
// path/port and no scheme share the shellSafeSiteString gate; a bare host
// (validSiteHost) short-circuits above it and is bounded instead by
// hostrules.ValidApprovedHost's stricter character class. The third shape is
// (e.g.
// "registry.corp.internal/ghcr-remote" or "10.40.2.11:8443") — the realistic
// shape for a redirect that is a destination, not a browsable URL. An
// Ecosystem row's To is NEVER validated by this: it is interpolated as a real
// base URL into a per-tool config file, so it must be validSiteURL exactly
// (validateSiteConfig enforces that directly).
func validSiteURLOrHost(raw string) bool {
	if validSiteURL(raw) || validSiteHost(raw) {
		return true
	}
	if !shellSafeSiteString(raw) || strings.Contains(raw, "://") {
		return false
	}
	return hostrules.HostOf(raw) != "" // tolerates a trailing /path or :port
}

// validateSiteConfig enforces the structural + security invariants of an
// admin-authored SiteConfig before it is persisted: secret refs must be a real,
// non-reserved secret name; URLs must be well-formed http(s) with a safe host;
// egress-redirect ecosystems must be one of the closed set. Fail closed — this
// is operator-wide config every run inherits (SSRF/injection hardening per the
// plan's security disposition).
//
// Callers MUST run foldLegacyArtifactOverrides first: this function validates
// EgressRedirects only, never the deprecated ArtifactOverrides (by the time
// anything reaches here, a legacy body has already been folded into the one
// canonical shape).
func validateSiteConfig(cfg types.SiteConfig) error {
	if cfg.UpstreamProxySecretRef != "" && !validSecretRef(cfg.UpstreamProxySecretRef) {
		return fmt.Errorf("upstream_proxy_secret_ref: invalid or reserved secret name %q", cfg.UpstreamProxySecretRef)
	}
	if cfg.UpstreamProxyURL != "" {
		// Mandatory server-side guard (a client-side check is only a courtesy):
		// a plain URL is topology, not a credential — an embedded user:pass@
		// belongs in a secret, where it can't be read back by an operator who
		// mistypes it, but also can't leak into logs/config dumps of this field.
		if u, err := url.Parse(cfg.UpstreamProxyURL); err == nil && u.User != nil {
			return fmt.Errorf("upstream_proxy_url: must not embed a credential (user:pass@) — store it as a secret via upstream_proxy_secret_ref instead")
		}
		if !validSiteURL(cfg.UpstreamProxyURL) {
			return fmt.Errorf("upstream_proxy_url: invalid URL %q", cfg.UpstreamProxyURL)
		}
		// http ONLY — reuse the EXACT gate resolveUpstreamProxyURL applies at
		// dispatch (runs_bedrock.go) so this can never drift from it. An https://
		// URL passed validSiteURL above (it accepts both schemes for its OTHER
		// callers) but the sidecar's own config validation rejects https: the hop
		// TO the corp proxy is a plaintext CONNECT + Proxy-Authorization, and an
		// https:// URL would need a TLS wrap first or leak that Basic credential
		// in cleartext. Before this gate, an https:// URL saved clean, displayed
		// as the live chain (site_config_probe.go), and was silently dropped at
		// dispatch — every run went direct with no signal anywhere (W13-S1-4).
		if _, ok := normalizedHTTPProxyURL(cfg.UpstreamProxyURL); !ok {
			return fmt.Errorf("upstream_proxy_url: must be http:// — https is not supported (the hop to the corp proxy is a plaintext CONNECT that cannot be TLS-wrapped)")
		}
		// And the PORT, by the sidecar's OWN loader (proxy.ValidUpstreamProxyURL),
		// exactly as upstream_proxy_no_proxy delegates to proxy.ValidNoProxyEntry
		// (site_config_noproxy.go) — the gate above checks the scheme and
		// hostrules.HostOf discards the port entirely, so ":0"/":99999" saved with
		// 200 OK and then failed the sidecar's applyDefaultsAndValidate at
		// container start, os.Exit(1)ing the egress proxy of every dispatched run.
		// One matcher, at the trust boundary, so the two can never drift again.
		if err := proxy.ValidUpstreamProxyURL(cfg.UpstreamProxyURL); err != nil {
			return fmt.Errorf("upstream_proxy_url: %w (the proxy sidecar loads this URL itself and refuses to start on it)", err)
		}
	}
	for i, red := range cfg.EgressRedirects {
		if !validSiteURLOrHost(red.From) {
			return fmt.Errorf("egress_redirects[%d]: invalid from %q", i, red.From)
		}
		if red.Ecosystem != "" {
			// Ecosystem tier: To is interpolated as a real base URL into a
			// per-tool config file (.npmrc/pip.conf/.cargo/config.toml/
			// .m2/settings.xml/NuGet.Config) or GOPROXY env — exactly what
			// ArtifactOverride.BaseURL required, or the emitted config is
			// silently non-functional (e.g. an npm registry with no scheme).
			if !validSiteURL(red.To) {
				return fmt.Errorf("egress_redirects[%d]: invalid to %q (an ecosystem row needs a full http(s) URL)", i, red.To)
			}
		} else if !validSiteURLOrHost(red.To) {
			// Network-only tier: To only ever resolves to a HOST (egress
			// allow + optional token injection) — no config file ever reads
			// it, so a bare "host[:port][/path]" is fine.
			return fmt.Errorf("egress_redirects[%d]: invalid to %q", i, red.To)
		}
		// The PORT of both endpoints, which NOTHING above examines:
		// validSiteURLOrHost bottoms out in hostrules.HostOf, and HostOf discards
		// everything from the first ':' onward. An unusable port therefore saved
		// with 200 OK and was then read differently by every downstream parser —
		// redirectPort coerced ":0"/":99999"/a query-bearing authority to 443
		// (mis-scoping the MITM/token-injection set and making the redirect probe
		// dial a port the operator never configured), while url.Parse refused
		// ":-1" outright and dropped the probe's --connect-to swap. One decision,
		// at the write, so no two readers of the stored string can disagree.
		for _, ep := range []struct{ field, raw string }{{"from", red.From}, {"to", red.To}} {
			if _, _, ok := redirectEndpointPort(ep.raw); !ok {
				return fmt.Errorf("egress_redirects[%d]: invalid port in %s %q — a port must be a decimal 1-65535, "+
					"and must not be followed by a query or fragment", i, ep.field, ep.raw)
			}
		}
		if red.Ecosystem != "" && !validArtifactEcosystems[red.Ecosystem] {
			return fmt.Errorf("egress_redirects[%d]: unknown ecosystem %q", i, red.Ecosystem)
		}
		if red.TokenSecretRef != "" && !validSecretRef(red.TokenSecretRef) {
			return fmt.Errorf("egress_redirects[%d]: invalid or reserved token_secret_ref %q", i, red.TokenSecretRef)
		}
		if red.TokenIntegrationRef != "" {
			if red.TokenSecretRef != "" {
				return fmt.Errorf("egress_redirects[%d]: set token_secret_ref OR token_integration_ref, not both — "+
					"an integration already names the secret it presents", i)
			}
			// Shape only; EXISTENCE is deliberately unchecked, matching the
			// dangling-token_secret_ref posture this row already has: a redirect
			// naming an integration that isn't configured yet still reroutes,
			// and simply carries no token until it is.
			if !secretNameRE.MatchString(red.TokenIntegrationRef) {
				return fmt.Errorf("egress_redirects[%d]: invalid token_integration_ref %q", i, red.TokenIntegrationRef)
			}
		}
	}
	for i, h := range cfg.ScmHosts {
		if !validSiteHost(h) {
			return fmt.Errorf("scm_hosts[%d]: invalid host %q", i, h)
		}
	}
	if err := validateInternalHosts(cfg.InternalHosts); err != nil {
		return err
	}
	if err := validateUpstreamProxyNoProxy(cfg.UpstreamProxyNoProxy); err != nil {
		return err
	}
	return nil
}

// validateInternalHosts enforces SiteConfig.InternalHosts's write-time
// invariant: every declared CIDR must lie ENTIRELY inside ipguard.Liftable
// (RFC1918, fc00::/7, or 100.64.0.0/10) — never loopback, link-local, metadata,
// multicast, or any other reserved range, which the proxy's blockKind
// classification keeps un-liftable regardless of what an operator declares
// here. A bare host_suffix must be a real host (validSiteHost); an entry with
// no CIDRs is valid (it lifts the full Liftable set for that host).
func validateInternalHosts(hosts []types.InternalHost) error {
	for i, h := range hosts {
		if !validSiteHost(h.HostSuffix) {
			return fmt.Errorf("internal_hosts[%d].host_suffix: invalid host %q", i, h.HostSuffix)
		}
		for j, c := range h.CIDRs {
			prefix, err := netip.ParsePrefix(c)
			liftable := err == nil && slices.ContainsFunc(ipguard.Liftable, func(l netip.Prefix) bool {
				return l.Bits() <= prefix.Bits() && l.Contains(prefix.Addr())
			})
			if !liftable {
				return fmt.Errorf("internal_hosts[%d].cidrs[%d]: %q must lie inside RFC1918, fc00::/7 or 100.64.0.0/10", i, j, c)
			}
		}
	}
	return nil
}

// logWarnInternalHostsDeclared is the loud, unmissable log an internal-host
// declaration earns: it is the ONLY operator override of the proxy's
// unconditional private/reserved-IP SSRF guard, so the deployment's log must
// name exactly which suffixes and ranges are lifted — an operator reading it
// back later cannot be left to infer the guard's shape from a count. Mirrors
// logWarnUnenforcedNetPolOptOut's contract (internal/runner/k8s/driver.go): the
// declaration itself, then what it does and does not lift.
//
// A warn, not an acknowledgement flag (PF-46): the write is operator-only,
// audited and Liftable-validated, and it grants no policy allow — the host must
// still pass allowed_domains separately. No declarations => silent.
func logWarnInternalHostsDeclared(hosts []types.InternalHost) {
	if len(hosts) == 0 {
		return
	}
	decls := make([]string, 0, len(hosts))
	for _, h := range hosts {
		scope := "the full RFC1918/ULA/CGNAT set"
		if len(h.CIDRs) > 0 {
			scope = strings.Join(h.CIDRs, ", ")
		}
		decls = append(decls, h.HostSuffix+" => "+scope)
	}
	slog.Warn("wardynd: site config declares INTERNAL HOSTS — the proxy's private/reserved-IP SSRF guard is LIFTED for these host suffixes, "+
		"scoped to the ranges named: "+strings.Join(decls, "; ")+". Loopback, link-local, the cloud-metadata address, unspecified, multicast and "+
		"NAT64-embedded addresses stay denied regardless of what is declared here, and a policy's allowed_domains must still allow the host separately — "+
		"this lifts the built-in guard only. Remove the entry to restore the unconditional deny.",
		slog.Int("internal_hosts_count", len(hosts)))
}

// foldLegacyArtifactOverrides folds a legacy request body's ArtifactOverrides
// into EgressRedirects for exactly ONE release: PUT /site-config is a
// whole-document replace and `wardyn site-config apply` takes a file an
// operator may have saved in the old shape — without this fold, applying a
// saved-and-reapplied legacy document would silently WIPE every redirect
// (decodeStrict still accepts the field on the wire since the deprecated
// struct field still exists; nothing else ever reads it back into
// EgressRedirects). Rejects a body that sets BOTH fields — ambiguous which one
// is authoritative — rather than guessing.
//
// Each folded entry's From is looked up per ecosystem exactly like migration
// 0030 does at the storage layer for the row already on disk, and entries are
// emitted in SORTED ecosystem-key order so the shared-host "first sighted
// wins" dedup (artifactMirrorRows, planArtifactRedirect) resolves identically
// to the old map-iteration code — this determinism is what makes the fold
// byte-identical to the pre-refactor behavior. cfg.ArtifactOverrides is always
// cleared before returning: only the folded shape is ever persisted.
func foldLegacyArtifactOverrides(cfg *types.SiteConfig) error {
	if len(cfg.ArtifactOverrides) == 0 {
		return nil
	}
	if len(cfg.EgressRedirects) > 0 {
		return fmt.Errorf("artifact_overrides and egress_redirects must not both be set — artifact_overrides is deprecated, migrate to egress_redirects")
	}
	for _, eco := range slices.Sorted(maps.Keys(cfg.ArtifactOverrides)) {
		ov := cfg.ArtifactOverrides[eco]
		cfg.EgressRedirects = append(cfg.EgressRedirects, types.EgressRedirect{
			From: ecosystemPublicURL[eco], To: ov.BaseURL,
			TokenSecretRef: ov.TokenSecretRef, Ecosystem: eco,
		})
	}
	cfg.ArtifactOverrides = nil
	return nil
}

// danglingSiteConfigSecretRefs returns the sorted, de-duplicated names of every
// secret ref sc points at that present does not currently hold — e.g. an
// UpstreamProxySecretRef or EgressRedirect.TokenSecretRef surviving a `site-config
// get` capture across a `make reset-all` that wiped the secret store but not the
// captured JSON. TokenIntegrationRef is deliberately excluded: it names an
// Integration, not a bare secret, and integration existence is its own
// (already-surfaced) concern. present is normally s.presentSecretNames(ctx) — the
// ONE name->present map every other secret-aware verdict is computed from, so
// this can never disagree with them. Advisory only: a dangling ref is never
// rejected (validateSiteConfig deliberately doesn't check existence either) —
// this only makes the gap visible instead of silent.
func danglingSiteConfigSecretRefs(sc types.SiteConfig, present map[string]bool) []string {
	seen := map[string]bool{}
	var out []string
	add := func(ref string) {
		if ref == "" || present[ref] || seen[ref] {
			return
		}
		seen[ref] = true
		out = append(out, ref)
	}
	add(sc.UpstreamProxySecretRef)
	for _, red := range sc.EgressRedirects {
		add(red.TokenSecretRef)
	}
	slices.Sort(out)
	return out
}

// handleGetSiteConfig returns the operator-wide site config. Secret VALUES are
// NEVER included — only the refs (names) the broker/proxy resolve at dispatch/
// injection time. A never-configured operator gets the zero value (empty refs/
// overrides/hosts) with 200, not a 404: "unconfigured" is a valid, common state.
//
// The response carries an ETag (etag.go) so a caller that means to base a
// later PUT on exactly this read can send it back as If-Match.
func (s *Server) handleGetSiteConfig(w http.ResponseWriter, r *http.Request) {
	cfg, err := s.cfg.Store.GetSiteConfig(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "get site config: "+err.Error())
		return
	}
	w.Header().Set("ETag", computeETag(cfg))
	writeJSON(w, http.StatusOK, cfg)
}

// handlePutSiteConfig validates and persists the operator-wide site config.
// Every URL/host field is checked (validateSiteConfig) before the write; the
// write REPLACES the whole document (no partial merge — the caller must
// round-trip a GET first to preserve fields it does not intend to change) and
// is always audited (site_config.write), mirroring secret.write.
//
// integrations are managed through their own endpoints, never through this
// one: a request body carrying a non-empty integrations is rejected outright
// (400), and the STORED integrations are carried forward verbatim onto the
// document this handler persists. Without this, an older client that GETs a
// config written before `integrations` existed, then PUTs it back unmodified
// (the documented round-trip above), would silently DELETE every stored
// integration — this is a whole-document replace, and a client with no
// knowledge of the field would naturally omit it.
//
// A legacy body's artifact_overrides is folded into egress_redirects
// (foldLegacyArtifactOverrides) before validation, so a document saved before
// EgressRedirects existed keeps applying rather than 400ing or silently
// dropping every redirect.
//
// If-Match (etag.go) is optional optimistic concurrency on top of this
// whole-document replace: an absent header behaves exactly as before (this is
// additive), a present one that no longer matches the document's CURRENT
// ETag is refused with 412 before the write reaches the store — the same
// "read, then write only if nothing else changed it first" guarantee
// PUT /permissions/enforcement gets in permissions.go.
func (s *Server) handlePutSiteConfig(w http.ResponseWriter, r *http.Request) {
	var cfg types.SiteConfig
	if !decodeStrict(w, r, &cfg) {
		return
	}
	if err := foldLegacyArtifactOverrides(&cfg); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if len(cfg.Integrations) > 0 {
		writeError(w, http.StatusBadRequest, "integrations are managed through their own endpoints, not PUT /site-config")
		return
	}
	if err := validateSiteConfig(cfg); err != nil {
		writeError(w, http.StatusBadRequest, "invalid site config: "+err.Error())
		return
	}
	// SEAM-1: serializes this read-modify-write (it carries the STORED
	// Integrations forward from its own read, below) against the three
	// integration-write handlers' own RMWs on the same document
	// (setup_integrations.go) — see handlePutIntegration's SEAM-1 comment.
	s.siteConfigMu.Lock()
	defer s.siteConfigMu.Unlock()
	existing, err := s.cfg.Store.GetSiteConfig(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "get existing site config: "+err.Error())
		return
	}
	// Checked under siteConfigMu, against the SAME read this handler's own
	// integrations-carry-forward uses below — no other writer can land between
	// this check and the Put that follows it.
	if !ifMatchSatisfied(r, computeETag(existing)) {
		writeError(w, http.StatusPreconditionFailed,
			"If-Match does not match the current site config — GET /site-config again and retry")
		return
	}
	// Same reasoning as Integrations above: onboarding state is server-owned and
	// is carried forward from the stored document below. A caller trying to SET
	// it is told why rather than watching it silently not take — but ONLY when
	// it names a different instant than the one already stored. GET emits this
	// key (it is a plain field of the same types.SiteConfig this handler
	// persists), so rejecting it unconditionally refused the very round-trip the
	// doc above mandates: `wardyn site-config get > f` / `wardyn site-config
	// apply f`, and every console save that spreads the GET document, 400ed on
	// any install whose operator had finished the Getting Started funnel. Echoing
	// the stored value changes nothing, so it is not an attempt to set it — and
	// neither does naming one on an install that holds NONE. `make reset` takes
	// the site config with the volume, so the captured corp-baseline.json
	// applied afterwards (the capture-before-a-reset, apply-after round-trip in
	// docs/OPERATIONS.md) and the MDM-delivered /etc/wardyn/site-config.json
	// landing on a fresh machine (docs/DESKTOP.md) are exactly that body against
	// a stored mark of nil — refusing them broke the very round-trip this
	// handler exists to serve. A submitted value can never take effect either
	// way, since the carry-forward below overwrites it unconditionally, so the
	// 400 is reserved for the one case where it tells the caller something
	// true: a mark the server actually HOLDS, named as a different instant.
	// Checked HERE, against the same read the carry-forward below uses, so the
	// comparison cannot race another writer (SEAM-1).
	if existing.OnboardingCompletedAt != nil && cfg.OnboardingCompletedAt != nil &&
		!cfg.OnboardingCompletedAt.Equal(*existing.OnboardingCompletedAt) {
		writeError(w, http.StatusBadRequest, "onboarding_completed_at is managed by the setup flow, not PUT /site-config")
		return
	}
	cfg.Integrations = existing.Integrations
	// Carry forward, or a round-trip PUT by any client erases the install's
	// onboarding state — the exact footgun already solved once for Integrations.
	cfg.OnboardingCompletedAt = existing.OnboardingCompletedAt
	saved, err := s.cfg.Store.PutSiteConfig(r.Context(), cfg)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "put site config: "+err.Error())
		return
	}
	logWarnInternalHostsDeclared(saved.InternalHosts)
	s.recordAudit(r.Context(), s.auditEvent(nil, actorTypeFromRequest(r), principalFromRequest(r),
		"site_config.write", "site_config", "success", mustJSON(map[string]any{
			"upstream_proxy_configured": saved.UpstreamProxySecretRef != "" || saved.UpstreamProxyURL != "",
			"egress_redirects_count":    len(saved.EgressRedirects),
			"scm_hosts_count":           len(saved.ScmHosts),
			"internal_hosts_count":      len(saved.InternalHosts),
		})))
	w.Header().Set("ETag", computeETag(saved))
	// dangling_secret_refs surfaces the "reset+apply came back green but every
	// credentialed path is dead" gap: this document round-trips secret NAMES
	// only, so an apply after a secret-store wipe (or a hand-edited file) can
	// reference a secret that was never restored — never rejected (dangling is
	// a valid mid-recovery state), always reported.
	writeJSON(w, http.StatusOK, siteConfigPutResponse{
		SiteConfig:         saved,
		DanglingSecretRefs: danglingSiteConfigSecretRefs(saved, s.presentSecretNames(r.Context())),
	})
}

// siteConfigPutResponse is PUT /site-config's response body: the persisted
// document plus DanglingSecretRefs (see danglingSiteConfigSecretRefs). GET
// /site-config deliberately returns the bare types.SiteConfig, not this type —
// DanglingSecretRefs is a freshly-computed, PUT-time-only signal, never
// persisted, so it must never round-trip through a `site-config get` capture
// back into a later `site-config apply` body.
type siteConfigPutResponse struct {
	types.SiteConfig
	DanglingSecretRefs []string `json:"dangling_secret_refs,omitempty"`
}
