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
	"maps"
	"net/http"
	"net/url"
	"slices"
	"strings"

	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/internal/workspacescan"
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

// validSiteURL reports whether raw is safe to persist as a site-config URL
// (upstream proxy / artifact base URL): well-formed, http(s) scheme only, a
// real dotted host of the same shape ValidApprovedHost accepts, and free of
// control characters or shell metacharacters. These strings flow into proxy
// dial targets and emitted per-tool config files (.npmrc/pip.conf/settings.xml/
// GOPROXY/...), so this is SSRF/injection hardening, not cosmetic validation.
func validSiteURL(raw string) bool {
	if raw == "" || len(raw) > 2048 {
		return false
	}
	for _, r := range raw {
		if r < 0x20 || r == 0x7f {
			return false
		}
	}
	if strings.ContainsAny(raw, "`$;&|<>\"'\\") {
		return false
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return false
	}
	host := workspacescan.HostOf(raw)
	return host != "" && workspacescan.ValidApprovedHost(host)
}

// validSiteHost reports whether h is a bare host suitable for ScmHosts — the
// same shape the scanner's approved-egress promotion accepts (no scheme, port,
// path, or wildcard).
func validSiteHost(h string) bool {
	return workspacescan.ValidApprovedHost(strings.ToLower(strings.TrimSpace(h)))
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
// shapes are accepted, all sharing the SAME control-char/shell-metacharacter
// safety validSiteURL enforces: a full http(s) URL (validSiteURL), a bare host
// (validSiteHost), or a bare host with a path/port and no scheme (e.g.
// "registry.corp.internal/ghcr-remote" or "10.40.2.11:8443") — the realistic
// shape for a redirect that is a destination, not a browsable URL. An
// Ecosystem row's To is NEVER validated by this: it is interpolated as a real
// base URL into a per-tool config file, so it must be validSiteURL exactly
// (validateSiteConfig enforces that directly).
func validSiteURLOrHost(raw string) bool {
	if validSiteURL(raw) || validSiteHost(raw) {
		return true
	}
	if raw == "" || len(raw) > 2048 || strings.Contains(raw, "://") {
		return false
	}
	for _, r := range raw {
		if r < 0x20 || r == 0x7f {
			return false
		}
	}
	if strings.ContainsAny(raw, "`$;&|<>\"'\\") {
		return false
	}
	return workspacescan.HostOf(raw) != "" // tolerates a trailing /path or :port
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
		if red.Ecosystem != "" && !validArtifactEcosystems[red.Ecosystem] {
			return fmt.Errorf("egress_redirects[%d]: unknown ecosystem %q", i, red.Ecosystem)
		}
		if red.TokenSecretRef != "" && !validSecretRef(red.TokenSecretRef) {
			return fmt.Errorf("egress_redirects[%d]: invalid or reserved token_secret_ref %q", i, red.TokenSecretRef)
		}
	}
	for i, h := range cfg.ScmHosts {
		if !validSiteHost(h) {
			return fmt.Errorf("scm_hosts[%d]: invalid host %q", i, h)
		}
	}
	return nil
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

// handleGetSiteConfig returns the operator-wide site config. Secret VALUES are
// NEVER included — only the refs (names) the broker/proxy resolve at dispatch/
// injection time. A never-configured operator gets the zero value (empty refs/
// overrides/hosts) with 200, not a 404: "unconfigured" is a valid, common state.
func (s *Server) handleGetSiteConfig(w http.ResponseWriter, r *http.Request) {
	cfg, err := s.cfg.Store.GetSiteConfig(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "get site config: "+err.Error())
		return
	}
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
	existing, err := s.cfg.Store.GetSiteConfig(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "get existing site config: "+err.Error())
		return
	}
	cfg.Integrations = existing.Integrations
	saved, err := s.cfg.Store.PutSiteConfig(r.Context(), cfg)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "put site config: "+err.Error())
		return
	}
	s.recordAudit(r.Context(), s.auditEvent(nil, actorTypeFromRequest(r), principalFromRequest(r),
		"site_config.write", "site_config", "success", mustJSON(map[string]any{
			"upstream_proxy_configured": saved.UpstreamProxySecretRef != "" || saved.UpstreamProxyURL != "",
			"egress_redirects_count":    len(saved.EgressRedirects),
			"scm_hosts_count":           len(saved.ScmHosts),
		})))
	writeJSON(w, http.StatusOK, saved)
}
