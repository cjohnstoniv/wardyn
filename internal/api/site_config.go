// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"fmt"
	"net/http"
	"net/url"
	"sort"
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
	return secretNameRE.MatchString(ref) && !reservedSecretNames[ref]
}

// validateSiteConfig enforces the structural + security invariants of an
// admin-authored SiteConfig before it is persisted: secret refs must be a real,
// non-reserved secret name; URLs must be well-formed http(s) with a safe host;
// artifact ecosystems must be one of the closed set. Fail closed — this is
// operator-wide config every run inherits (SSRF/injection hardening per the
// plan's security disposition).
func validateSiteConfig(cfg types.SiteConfig) error {
	if cfg.UpstreamProxySecretRef != "" && !validSecretRef(cfg.UpstreamProxySecretRef) {
		return fmt.Errorf("upstream_proxy_secret_ref: invalid or reserved secret name %q", cfg.UpstreamProxySecretRef)
	}
	for eco, ov := range cfg.ArtifactOverrides {
		if !validArtifactEcosystems[eco] {
			return fmt.Errorf("artifact_overrides: unknown ecosystem %q", eco)
		}
		if !validSiteURL(ov.BaseURL) {
			return fmt.Errorf("artifact_overrides[%s]: invalid base_url %q", eco, ov.BaseURL)
		}
		if ov.TokenSecretRef != "" && !validSecretRef(ov.TokenSecretRef) {
			return fmt.Errorf("artifact_overrides[%s]: invalid or reserved token_secret_ref %q", eco, ov.TokenSecretRef)
		}
	}
	for i, h := range cfg.ScmHosts {
		if !validSiteHost(h) {
			return fmt.Errorf("scm_hosts[%d]: invalid host %q", i, h)
		}
	}
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
func (s *Server) handlePutSiteConfig(w http.ResponseWriter, r *http.Request) {
	var cfg types.SiteConfig
	if !decodeStrict(w, r, &cfg) {
		return
	}
	if err := validateSiteConfig(cfg); err != nil {
		writeError(w, http.StatusBadRequest, "invalid site config: "+err.Error())
		return
	}
	saved, err := s.cfg.Store.PutSiteConfig(r.Context(), cfg)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "put site config: "+err.Error())
		return
	}
	ecosystems := make([]string, 0, len(saved.ArtifactOverrides))
	for eco := range saved.ArtifactOverrides {
		ecosystems = append(ecosystems, eco)
	}
	sort.Strings(ecosystems)
	s.recordAudit(r.Context(), s.auditEvent(nil, actorTypeFromRequest(r), principalFromRequest(r),
		"site_config.write", "site_config", "success", mustJSON(map[string]any{
			"upstream_proxy_configured": saved.UpstreamProxySecretRef != "",
			"artifact_overrides":        ecosystems,
			"scm_hosts_count":           len(saved.ScmHosts),
		})))
	writeJSON(w, http.StatusOK, saved)
}
