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
// ecosystems Wardyn has emit-time config support for — npm/pip/cargo/maven/go
// each get their own registry config file, nuget its own).
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

// PUT /site-config refusal bodies.
//
// DRAFT (M2 canon pending)
const (
	// legacyArtifactOverridesUnknownEcosystemRefusal names the offending key
	// directly, before the generic "invalid from \"\"" refusal two guards
	// later could ever fire for it.
	legacyArtifactOverridesUnknownEcosystemRefusal = "artifact_overrides.%s: unknown ecosystem"
	// egressRedirectDuplicateFromRefusal rejects a duplicate From:
	// findEgressRedirect resolves the first match only, so a second row
	// sharing a From would silently never fire.
	egressRedirectDuplicateFromRefusal = "egress_redirects[%d]: duplicate from %q — egress_redirects[%d] already uses it, and a lookup resolves the first match only"
)

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

// normalizeSiteConfigTopology canonicalizes PUT /site-config's plain-string
// topology fields in the handler, beside normalizeWorkspaceProviders, before
// validateSiteConfig runs. Without this, a field validated only
// because HostOf/validSiteHost trim+lowercase a THROWAWAY copy before
// checking it (hostrules.go) still PERSISTED whatever case/whitespace the
// operator typed: findEgressRedirect's read-time EqualFold masked the effect
// for that one lookup, but the stored document, `wardyn site-config get`, and
// this handler's own audit datum all echoed the uncanonicalized string back.
//
// Interior whitespace is left ALONE, not collapsed: TrimSpace only trims the
// ends, so "git .corp.example" survives as two tokens rather than folding
// into "git.corp.example" — the existing host/URL validators below (no
// character outside the DNS label set) already 400 it, and normalization
// must never launder a string past the validator that runs after it (the
// same rule normalizeProviderBaseURL documents for base URLs).
func normalizeSiteConfigTopology(cfg *types.SiteConfig) {
	for i, h := range cfg.ScmHosts {
		cfg.ScmHosts[i] = strings.ToLower(strings.TrimSpace(h))
	}
	if norm, ok := normalizedHTTPProxyURL(cfg.UpstreamProxyURL); ok {
		cfg.UpstreamProxyURL = normalizeRedirectEndpoint(norm)
	} else {
		cfg.UpstreamProxyURL = strings.TrimSpace(cfg.UpstreamProxyURL)
	}
	for i := range cfg.EgressRedirects {
		cfg.EgressRedirects[i].From = normalizeRedirectEndpoint(cfg.EgressRedirects[i].From)
		cfg.EgressRedirects[i].To = normalizeRedirectEndpoint(cfg.EgressRedirects[i].To)
	}
}

// normalizeRedirectEndpoint canonicalizes one EgressRedirect From/To: trim
// outer whitespace and lowercase the scheme+host (authority) portion only —
// DNS names and URL schemes are case-insensitive (RFC 3986 §3.1/§3.2.2), so
// this can never change what the string MEANS. Any path/query past the
// authority is left exactly as typed: a redirect's To can carry a
// case-sensitive repository path, the same reason normalizeProviderBaseURL
// (workspace_providers.go) folds every host's path but github.com's.
func normalizeRedirectEndpoint(raw string) string {
	s := strings.TrimSpace(raw)
	start := 0
	if i := strings.Index(s, "://"); i >= 0 {
		start = i + 3
	}
	authorityEnd := len(s)
	if j := strings.IndexByte(s[start:], '/'); j >= 0 {
		authorityEnd = start + j
	}
	return strings.ToLower(s[:authorityEnd]) + s[authorityEnd:]
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
		// in cleartext.
		if _, ok := normalizedHTTPProxyURL(cfg.UpstreamProxyURL); !ok {
			return fmt.Errorf("upstream_proxy_url: must be http:// — https is not supported (the hop to the corp proxy is a plaintext CONNECT that cannot be TLS-wrapped)")
		}
		// And the PORT, by the sidecar's OWN loader (proxy.ValidUpstreamProxyURL),
		// exactly as upstream_proxy_no_proxy delegates to proxy.ValidNoProxyEntry
		// (site_config_noproxy.go) — the gate above checks the scheme and
		// hostrules.HostOf discards the port entirely, so an invalid port like
		// ":0"/":99999" would otherwise pass here and only fail the sidecar's
		// applyDefaultsAndValidate at container start, os.Exit(1)ing the egress
		// proxy of every dispatched run. One matcher, at the trust boundary, so
		// the two can never drift again.
		if err := proxy.ValidUpstreamProxyURL(cfg.UpstreamProxyURL); err != nil {
			return fmt.Errorf("upstream_proxy_url: %w (the proxy sidecar loads this URL itself and refuses to start on it)", err)
		}
	}
	// A duplicate From is producible (no client/server guard existed) —
	// findEgressRedirect (site_config_probe_classify.go) resolves the FIRST
	// match only, so a second row sharing a From silently never fires; the
	// UI's own fix is disabling Add on a collision, not index keys —
	// re-keying by index would shift every LATER row's identity/verdict on
	// an unrelated edit. Case-insensitive, matching findEgressRedirect's own
	// EqualFold compare — two rows differing only in From's case collide at
	// read time exactly the same way.
	seenFrom := make(map[string]int, len(cfg.EgressRedirects))
	for i, red := range cfg.EgressRedirects {
		if !validSiteURLOrHost(red.From) {
			return fmt.Errorf("egress_redirects[%d]: invalid from %q", i, red.From)
		}
		if prev, dup := seenFrom[strings.ToLower(strings.TrimSpace(red.From))]; dup {
			return fmt.Errorf(egressRedirectDuplicateFromRefusal, i, red.From, prev)
		}
		seenFrom[strings.ToLower(strings.TrimSpace(red.From))] = i
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
		// everything from the first ':' onward. An unusable port would otherwise
		// mis-scope the MITM/token-injection set and leave downstream parsers to
		// disagree about which port a redirect actually names. One decision, at
		// the write, so no two readers of the stored string can disagree.
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
	if err := validateSignInHelp(cfg.SignInHelpText, cfg.SignInHelpURL); err != nil {
		return err
	}
	// The workspace-provider block, when the body carries one: ONE validator for
	// both write doors (this one and PUT /workspace-providers), so an
	// MDM-delivered document can never store a block the providers endpoint
	// would have refused (workspace_providers.go) — with exactly ONE stated
	// exception (#380 F2): `false` here means an explicit SSH lane exceeding its
	// row's own path scope is NOT refused on THIS door. A laptop re-applies
	// /etc/wardyn/site-config.json on every boot; refusing the whole document
	// for a row an admin ticked before 0.7.10 existed — with no migration path
	// and no undo — is a worse failure than the host-level over-admission #380
	// closes. handlePutSiteConfig reports the affected rows instead
	// (sshLaneWidePastPathRows) and warns loudly rather than failing silently.
	// The console door (handlePutWorkspaceProviders) keeps the hard refusal.
	//
	// The SIBLING agent_providers block is validated by its own gate at each of
	// those two doors instead of here (validateAgentProviders, agent_providers.go):
	// admitting a row needs the boot agent-image map, which is server state this
	// deliberately pure function has no access to. Both doors run it, so the
	// "one validator, two doors" property is the same.
	return validateWorkspaceProviders(cfg.WorkspaceProviders, false)
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
// A warn, not an acknowledgement flag: the write is operator-only,
// audited and Liftable-validated, and it grants no policy allow — the host must
// still pass allowed_domains separately. No declarations => silent.
func logWarnInternalHostsDeclared(hosts []types.InternalHost) {
	if len(hosts) == 0 {
		return
	}
	slog.Warn("wardynd: "+internalHostsDeclaredSentence(hosts), slog.Int("internal_hosts_count", len(hosts)))
}

// internalHostsDeclaredSentence is the ONE sentence naming what an
// InternalHosts declaration does and does not lift — shared by the write-time
// deployment log above and the console's own dedicated internal_hosts check
// row (internalHostsCheck, setup_checks.go), so an operator reads the
// identical claim on whichever surface they are looking at. Callers check
// len(hosts) > 0 themselves; this renders unconditionally.
func internalHostsDeclaredSentence(hosts []types.InternalHost) string {
	decls := make([]string, 0, len(hosts))
	for _, h := range hosts {
		scope := "the full RFC1918/ULA/CGNAT set"
		if len(h.CIDRs) > 0 {
			scope = strings.Join(h.CIDRs, ", ")
		}
		decls = append(decls, h.HostSuffix+" => "+scope)
	}
	return "site config declares INTERNAL HOSTS — the proxy's private/reserved-IP SSRF guard is LIFTED for these host suffixes, " +
		"scoped to the ranges named: " + strings.Join(decls, "; ") + ". Loopback, link-local, the cloud-metadata address, unspecified, multicast and " +
		"NAT64-embedded addresses stay denied regardless of what is declared here, and a policy's allowed_domains must still allow the host separately — " +
		"this lifts the built-in guard only. Remove the entry to restore the unconditional deny."
}

// maxAuditEgressRedirectPairs bounds how many from→to pairs site_config.write's
// datum embeds — egress_redirects_count stays the honest, UNBOUNDED
// total, so truncation costs review detail only, mirroring maxAuditFindings'
// own append-only-audit-log-size reasoning (internal.go): one operator
// declaring hundreds of redirects must not turn this row into the biggest
// thing in the log.
const maxAuditEgressRedirectPairs = 50

// auditEgressRedirectPairs renders saved.EgressRedirects as sorted "from→to"
// strings for site_config.write's datum, capped at maxAuditEgressRedirectPairs
// (second return reports whether it truncated). From/To are topology, not a
// credential — TokenSecretRef/TokenIntegrationRef are deliberately excluded,
// same restraint integration.write's own egress[] disclosure already takes.
func auditEgressRedirectPairs(redirects []types.EgressRedirect) ([]string, bool) {
	pairs := make([]string, 0, len(redirects))
	for _, red := range redirects {
		pairs = append(pairs, red.From+"→"+red.To)
	}
	slices.Sort(pairs)
	if len(pairs) > maxAuditEgressRedirectPairs {
		return pairs[:maxAuditEgressRedirectPairs], true
	}
	return pairs, false
}

// auditInternalHostSuffixes renders saved.InternalHosts as sorted host
// suffixes for site_config.write's datum — a suffix is exactly what
// logWarnInternalHostsDeclared already puts in the deployment's own log, so
// this adds nothing an operator couldn't already read there, just makes it
// reviewable from the audit trail too. CIDRs are left out: the scoping detail
// belongs to the log line above, not a row every SIEM sink fans out to.
func auditInternalHostSuffixes(hosts []types.InternalHost) []string {
	suffixes := make([]string, 0, len(hosts))
	for _, h := range hosts {
		suffixes = append(suffixes, h.HostSuffix)
	}
	slices.Sort(suffixes)
	return suffixes
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
		// Reject an unknown ecosystem key HERE, naming the offending
		// key. Left unchecked, ecosystemPublicURL[eco] resolves to "" for an
		// unknown eco, and the fold emits a redirect whose From is empty —
		// validateSiteConfig's very next pass then 400s it as
		// `egress_redirects[0]: invalid from ""`, an accurate but useless
		// message that never says WHICH artifact_overrides key was wrong or
		// that "unknown ecosystem" (the check that exists for exactly this,
		// two guards down) was unreachable behind it.
		if !validArtifactEcosystems[eco] {
			return fmt.Errorf(legacyArtifactOverridesUnknownEcosystemRefusal, eco)
		}
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
		writeServerError(w, r, "get site config", err)
		return
	}
	// The ETag hashes the STORED document, before the projection below: PUT
	// computes its own from what the store returned, so projecting first would
	// make GET's ETag one no If-Match could ever satisfy.
	w.Header().Set("ETag", computeETag(cfg))
	// effective_scm_hosts: the read-only union (workspace_providers.go) — ONE
	// spelling of the claim rule, in Go, so the console never re-implements it.
	cfg.EffectiveScmHosts = effectiveScmHosts(cfg)
	// workspace_providers.git_pat_broker_enabled: the SAME projection GET
	// /workspace-providers does, and for the same reason (#381) — this door
	// returns the identical nested block, so a console reading site-config
	// directly (or an MDM diffing its own `apply` against a `get`) must see
	// the same live switch value, not a stale/absent one.
	//
	// A COPY, never a mutation through the pointer: cfg.WorkspaceProviders may
	// be the SAME struct the store keeps (or another concurrent reader holds);
	// storedWorkspaceProviders's own dereference is this file's established
	// way of saying so. Setting the field straight through the pointer once
	// leaked the projection into the ETag hash for every OTHER reader —
	// including this door's own PUT, which re-GETs for its If-Match check.
	if providersConfigured(cfg) {
		wp := *cfg.WorkspaceProviders
		on := !s.cfg.DisableGitPATBroker
		wp.GitPatBrokerEnabled = &on
		cfg.WorkspaceProviders = &wp
	}
	writeJSON(w, http.StatusOK, cfg)
}

// siteConfigFieldsAfter066 are the SiteConfig keys that did not exist in the
// last release whose SDK/CLI is documented to GET-then-PUT this whole document
// (v0.6.6: upstream_proxy_secret_ref, upstream_proxy_url, artifact_overrides,
// egress_redirects, scm_hosts, integrations). A client built against that
// vocabulary cannot NAME these, so their absence from its body is not a
// decision.
//
// Add a key here when you add one to types.SiteConfig, and
// TestSiteConfigRoundTripKeepsFieldsAnOlderClientCannotName fails until you
// have decided which side of this line it sits on.
var siteConfigFieldsAfter066 = []string{
	"upstream_proxy_no_proxy", "internal_hosts", "workspace_providers", "agent_providers",
	"sign_in_help_text", "sign_in_help_url",
}

// carryForwardUnnamedSiteConfigFields preserves a stored value that the request
// body did not MENTION, for the fields an older client cannot know about.
//
// Absent is not cleared, and that distinction is the whole fix. PUT /site-config
// is a whole-document replace, so a v0.6.6 `wardyn site-config get | ... | apply`
// round trip — decode into a struct with no field for internal_hosts, re-marshal,
// PUT — silently erased the operator's internal_hosts and upstream_proxy_no_proxy
// with no warning on either side. Integrations and onboarding_completed_at were
// each rescued from this by hand; these two were added afterwards and were not.
//
// A bare carry-forward would make them unclearable, which is why this keys on
// the body's own keys rather than on emptiness: `{"internal_hosts": []}` and
// `{"internal_hosts": null}` both MENTION the field and both clear it, exactly
// as a client intends. Only silence is treated as silence.
func carryForwardUnnamedSiteConfigFields(cfg *types.SiteConfig, existing types.SiteConfig, present map[string]bool) {
	if !present["upstream_proxy_no_proxy"] {
		cfg.UpstreamProxyNoProxy = existing.UpstreamProxyNoProxy
	}
	if !present["internal_hosts"] {
		cfg.InternalHosts = existing.InternalHosts
	}
	// workspace_providers takes the same treatment, and it MATTERS more than the
	// two above: the MDM-delivered /etc/wardyn/site-config.json a laptop
	// re-applies on every boot is normally authored before this key existed, so
	// without the carry-forward every boot would silently delete the org's
	// provider policy. An explicit {} still clears it (the key is MENTIONED), and
	// normalizeWorkspaceProviders is what turns that {} back into an absent key
	// instead of an empty object rendered on every later GET.
	if !present["workspace_providers"] {
		cfg.WorkspaceProviders = existing.WorkspaceProviders
	}
	// agent_providers on identical terms, and for the identical MDM reason: the
	// /etc/wardyn/site-config.json a laptop re-applies on every boot predates
	// this key, so without the carry-forward every boot would silently delete the
	// org's agent roster — and an install whose roster vanished falls back to
	// legacy open mode, which is the OPPOSITE of what the admin wrote down.
	if !present["agent_providers"] {
		cfg.AgentProviders = existing.AgentProviders
	}
	// The sign-in help pair, for the same MDM reason: a boot-time re-apply of a
	// file written before these keys existed must not erase the admin's text.
	if !present["sign_in_help_text"] {
		cfg.SignInHelpText = existing.SignInHelpText
	}
	if !present["sign_in_help_url"] {
		cfg.SignInHelpURL = existing.SignInHelpURL
	}
	// effective_scm_hosts is NOT carried forward: it is server-owned and
	// PROJECTED on read (handleGetSiteConfig), never stored, so there is nothing
	// to preserve — the write clears it instead.
	cfg.EffectiveScmHosts = nil
	// git_pat_broker_enabled (nested in workspace_providers) takes the SAME
	// treatment, and for the same reason (#381 F1): it is projected on read
	// from the deployment's own env switch, never stored. Without this line a
	// value that arrived in the request body — or rode along unnoticed on the
	// just-carried-forward existing.WorkspaceProviders above — persisted into
	// the JSONB and was echoed back as truth on every later GET, exactly the
	// bug handlePutWorkspaceProviders was already fixed against.
	//
	// A COPY, never a mutation through the pointer, for the identical
	// aliasing reason handleGetSiteConfig's own projection states: in the
	// carried-forward branch above, cfg.WorkspaceProviders IS
	// existing.WorkspaceProviders — the very value this store's OTHER readers
	// may be holding right now.
	if cfg.WorkspaceProviders != nil {
		wp := *cfg.WorkspaceProviders
		wp.GitPatBrokerEnabled = nil
		cfg.WorkspaceProviders = &wp
	}
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
// onboarding_completed_at is server-owned in the same way, but is IGNORED
// rather than rejected: the stored mark is carried forward and a submitted one
// is reported back as onboarding_completed_at_ignored (see the comment at the
// carry-forward). GET emits that key, so refusing it broke the very round-trip
// above — and refusing it can protect nothing the carry-forward does not.
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
	// The keys the body carried, not just the values it decoded to — see the
	// carry-forward below for why this document cannot answer with the struct
	// alone.
	present, msg := decodeStrictKeys(w, r, &cfg)
	if present == nil && msg == "" {
		return // readCappedBody already answered
	}
	if msg != "" {
		writeError(w, http.StatusBadRequest, msg)
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
	// Normalize BEFORE validating, so what validateSiteConfig passes is exactly
	// what gets stored: {} becomes an absent key, base URLs get their canonical
	// lowercase-host/no-trailing-slash form.
	cfg.WorkspaceProviders = normalizeWorkspaceProviders(cfg.WorkspaceProviders)
	cfg.AgentProviders = normalizeAgentProviders(cfg.AgentProviders)
	// ScmHosts / EgressRedirects[].{From,To} / UpstreamProxyURL on the
	// same terms — see normalizeSiteConfigTopology's doc.
	normalizeSiteConfigTopology(&cfg)
	if err := validateAgentProviders(cfg.AgentProviders, s.cfg.AgentImages, s.cfg.BedrockModel); err != nil {
		writeError(w, http.StatusBadRequest, "invalid site config: "+err.Error())
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
		writeServerError(w, r, "get existing site config", err)
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
	// is carried forward from the stored document below — so a submitted value is
	// IGNORED, never refused. GET emits this key (it is a plain field of the same
	// types.SiteConfig this handler persists), so every honest client echoes it
	// back: `wardyn site-config get > f` / `wardyn site-config apply f`, the
	// MDM-delivered /etc/wardyn/site-config.json that deploy/desktop re-applies on
	// EVERY boot, and every console save that spreads the GET document. Rejecting
	// the key outright 400ed all three; rejecting only a value that names a
	// DIFFERENT instant than the stored one still 400ed the two recovery flows
	// this handler exists to serve — the MDM file, once that laptop finishes its
	// own funnel and holds a mark of its own, and capture / `make reset` /
	// re-onboard / apply, where the captured baseline names the install's PREVIOUS
	// mark. Neither refusal protected anything: the carry-forward below overwrites
	// the submitted value unconditionally, so a body can never SET, CLEAR or MOVE
	// the server's mark whatever it says. What the caller gets instead of a 400 is
	// a true report — onboarding_completed_at_ignored in the response, beside
	// dangling_secret_refs, which `wardyn site-config apply` prints as a warning
	// the way it prints the integrations one, so the drop is never silent.
	// Computed HERE, against the SAME read the carry-forward below uses, so it
	// cannot race another writer (SEAM-1).
	ignoredOnboardingMark := cfg.OnboardingCompletedAt != nil &&
		(existing.OnboardingCompletedAt == nil || !cfg.OnboardingCompletedAt.Equal(*existing.OnboardingCompletedAt))
	cfg.Integrations = existing.Integrations
	// Carry forward, or a round-trip PUT by any client erases the install's
	// onboarding state — the exact footgun already solved once for Integrations.
	cfg.OnboardingCompletedAt = existing.OnboardingCompletedAt
	carryForwardUnnamedSiteConfigFields(&cfg, existing, present)
	// Narrowing is never silent on this door either, and this is the door where
	// it matters most: a laptop re-applies /etc/wardyn/site-config.json on EVERY
	// boot, so an MDM-tightened base URL lands here, not on the providers page,
	// and nobody is watching a console toast when it does. Counted only when the
	// body actually NAMED the block (a carried-forward one narrows nothing new),
	// and an unreadable list FAILS the write rather than reporting a comforting
	// 0 — the same rule handlePutWorkspaceProviders follows.
	var narrowed *int
	if present["workspace_providers"] {
		n, cerr := s.sourcesNoLongerAdmitted(r.Context(), cfg)
		if cerr != nil {
			writeServerError(w, r, "count sources this block refuses", cerr)
			return
		}
		narrowed = &n
	}
	saved, err := s.cfg.Store.PutSiteConfig(r.Context(), cfg)
	if err != nil {
		writeServerError(w, r, "put site config", err)
		return
	}
	logWarnInternalHostsDeclared(saved.InternalHosts)
	sshWideRows := sshLaneWidePastPathRows(saved.WorkspaceProviders)
	logWarnSSHLaneWidePastPath(sshWideRows)
	redirectPairs, redirectsTruncated := auditEgressRedirectPairs(saved.EgressRedirects)
	hostSuffixes := auditInternalHostSuffixes(saved.InternalHosts)
	datum := map[string]any{
		// upstream_proxy_url/upstream_proxy_secret_ref, the egress_redirects
		// from→to pairs and internal_hosts[].host_suffix are in the clear on
		// purpose — same precedent as workspace_provider.write's base_urls:
		// topology, not a credential (a secret ref is a NAME, never the value it
		// names). Without them, an MDM-applied narrowing or opening of the
		// upstream proxy / redirect table / internal-host allowlist left nothing
		// but a count behind it, unreviewable from the audit log alone.
		"upstream_proxy_configured": saved.UpstreamProxySecretRef != "" || saved.UpstreamProxyURL != "",
		"upstream_proxy_url":        saved.UpstreamProxyURL,
		"upstream_proxy_secret_ref": saved.UpstreamProxySecretRef,
		"egress_redirects_count":    len(saved.EgressRedirects),
		"egress_redirects":          redirectPairs,
		"scm_hosts_count":           len(saved.ScmHosts),
		"internal_hosts_count":      len(saved.InternalHosts),
		"internal_hosts":            hostSuffixes,
		// The provider policy this door can also write (CLI/MDM): without these
		// two, an MDM-applied narrowing or opening left nothing but a count of
		// the legacy lists behind it.
		"git_providers":      enabledGitProviderCount(saved),
		"storage_configured": storageProvidersConfigured(saved),
		// The agent roster this door can also write (CLI/MDM): the count of
		// ENABLED rows, so a roster narrowed by an MDM push is reviewable
		// from the audit log alone.
		"agent_providers": enabledAgentProviderCount(saved),
	}
	if redirectsTruncated {
		datum["egress_redirects_truncated"] = true
	}
	// Only when the body NAMED the block — see the count above.
	if narrowed != nil {
		datum["sources_no_longer_admitted"] = *narrowed
	}
	s.recordAudit(r.Context(), s.auditEvent(nil, actorTypeFromRequest(r), principalFromRequest(r),
		"site_config.write", "site_config", "success", mustJSON(datum)))
	w.Header().Set("ETag", computeETag(saved))
	// Projected onto the response for the same reason GET projects it — and
	// AFTER the ETag above, which must hash the stored document.
	saved.EffectiveScmHosts = effectiveScmHosts(saved)
	// git_pat_broker_enabled (#381 F4): the SAME after-ETag projection, so a
	// `site-config apply` (or the console's own PUT round trip) sees the live
	// switch immediately rather than a dropped field until the next GET. A
	// COPY, never a mutation through the pointer — `saved` came straight back
	// from the store and may be the same object a concurrent reader holds.
	if providersConfigured(saved) {
		wp := *saved.WorkspaceProviders
		on := !s.cfg.DisableGitPATBroker
		wp.GitPatBrokerEnabled = &on
		saved.WorkspaceProviders = &wp
	}
	// dangling_secret_refs surfaces the "reset+apply came back green but every
	// credentialed path is dead" gap: this document round-trips secret NAMES
	// only, so an apply after a secret-store wipe (or a hand-edited file) can
	// reference a secret that was never restored — never rejected (dangling is
	// a valid mid-recovery state), always reported.
	writeJSON(w, http.StatusOK, siteConfigPutResponse{
		SiteConfig:                   saved,
		DanglingSecretRefs:           danglingSiteConfigSecretRefs(saved, s.presentSecretNames(r.Context())),
		OnboardingCompletedAtIgnored: ignoredOnboardingMark,
		AppliesFrom:                  siteConfigAppliesFromNextDispatch,
		SourcesNoLongerAdmitted:      narrowed,
		SSHLaneWidePastPath:          sshWideRows,
	})
}

// siteConfigPutResponse is PUT /site-config's response body: the persisted
// document plus the write's advisory signals — DanglingSecretRefs (see
// danglingSiteConfigSecretRefs) and OnboardingCompletedAtIgnored. GET
// /site-config deliberately returns the bare types.SiteConfig, not this type —
// both signals are freshly-computed, PUT-time-only facts, never persisted, so
// neither must ever round-trip through a `site-config get` capture back into a
// later `site-config apply` body.
type siteConfigPutResponse struct {
	types.SiteConfig
	DanglingSecretRefs []string `json:"dangling_secret_refs,omitempty"`
	// OnboardingCompletedAtIgnored reports that the request body named an
	// onboarding_completed_at the server did not keep — a different instant
	// than the stored mark, or any mark at all against a store that holds
	// none. The write still succeeded: the field is server-owned and always
	// carried forward, so this is a REPORT of a dropped value, never a
	// refusal (see handlePutSiteConfig). It is what the capture/reset/apply
	// and MDM every-boot flows see instead of a 400, and `wardyn
	// site-config apply` prints it as a warning.
	OnboardingCompletedAtIgnored bool `json:"onboarding_completed_at_ignored,omitempty"`
	// AppliesFrom names WHEN this write takes effect, because the honest answer
	// is not "now": the egress proxy sidecar loads its compiled config once at
	// sandbox start (internal/egress/proxy's LoadConfigBytes), so a run already
	// going keeps the network settings it started with. That was never a bug and
	// never documented either — a customer read a 403 naming a field they had
	// just fixed and retried the same run ten times. The console reads this key
	// into its save toast; the value is a fixed word, not a computed one, so a
	// client can switch on it.
	AppliesFrom string `json:"applies_from,omitempty"`
	// SourcesNoLongerAdmitted is how many already-onboarded repo locators the
	// workspace_providers block this body carried now refuses — the same count
	// PUT /workspace-providers returns, on the door MDM and `wardyn site-config
	// apply` actually use. A POINTER: absent when the body named no provider
	// block at all (there is nothing to report), and present as 0 when it did,
	// because 0 is the reassurance an admin narrowing a base URL is looking for.
	SourcesNoLongerAdmitted *int `json:"sources_no_longer_admitted,omitempty"`
	// SSHLaneWidePastPath (#380 F2) names every provider row this door
	// GRANDFATHERED rather than refused: an explicit SSH lane on a row whose
	// own addresses carry a path, which the console door (PUT
	// /workspace-providers) refuses outright. This door instead keeps applying
	// the rest of the document — an MDM-delivered config predates this rule and
	// has no migration path, and a laptop that cannot re-apply its own stored
	// document is a worse failure than the host-level over-admission the rule
	// closes — and reports the rows here (also warned at slog.Warn level,
	// logWarnSSHLaneWidePastPath) so the drop from "refused" to "admitted +
	// warned" is never silent, the same shape DanglingSecretRefs takes.
	SSHLaneWidePastPath []string `json:"ssh_lane_wide_past_path,omitempty"`
}

// siteConfigAppliesFromNextDispatch is the ONE value AppliesFrom takes today:
// the write lands immediately, and every run dispatched from now on compiles it.
const siteConfigAppliesFromNextDispatch = "next_dispatch"
