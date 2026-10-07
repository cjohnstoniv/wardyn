// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"slices"
	"strings"

	"github.com/cjohnstoniv/wardyn/internal/db"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// SiteConfigSeed is the network part of the site config that
// WARDYN_SITE_CONFIG_SEED_FILE can restore at boot: the upstream proxy (its URL
// and secret reference, one setting), its bypass list and the internal hosts.
// These live only in the site-config row, so a rebuilt database lost them with
// nothing to say so. A seed fills a setting the database does not have and never
// overwrites one it does.
type SiteConfigSeed struct {
	UpstreamProxyURL       string               `json:"upstream_proxy_url,omitempty"`
	UpstreamProxySecretRef string               `json:"upstream_proxy_secret_ref,omitempty"`
	UpstreamProxyNoProxy   []string             `json:"upstream_proxy_no_proxy,omitempty"`
	InternalHosts          []types.InternalHost `json:"internal_hosts,omitempty"`
}

// The names a seed's settings carry in the log, the audit row and the
// /setup/status row.
const (
	seedSettingUpstreamProxy = "upstream_proxy"
	seedSettingNoProxy       = "upstream_proxy_no_proxy"
	seedSettingInternalHosts = "internal_hosts"
)

// siteConfigSeedKeys is the closed set of keys a seed file may carry, named in
// the refusal of any other key.
const siteConfigSeedKeys = "upstream_proxy_url, upstream_proxy_secret_ref, upstream_proxy_no_proxy and internal_hosts"

// LoadSiteConfigSeed reads and checks the seed file at path; an empty path is no
// seed (nil, nil). Any key outside siteConfigSeedKeys, malformed JSON, or a value
// PUT /site-config would refuse is an error, and the caller refuses boot on it:
// the seed is checked on its own, whatever the database holds.
func LoadSiteConfigSeed(path string) (*SiteConfigSeed, error) {
	if path == "" {
		return nil, nil
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("WARDYN_SITE_CONFIG_SEED_FILE: %w", err)
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var seed SiteConfigSeed
	if err := dec.Decode(&seed); err != nil {
		if strings.HasPrefix(err.Error(), "json: unknown field") {
			return nil, fmt.Errorf("WARDYN_SITE_CONFIG_SEED_FILE %s: %w; a seed may carry only %s", path, err, siteConfigSeedKeys)
		}
		return nil, fmt.Errorf("WARDYN_SITE_CONFIG_SEED_FILE %s: malformed seed: %w", path, err)
	}
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("WARDYN_SITE_CONFIG_SEED_FILE %s: malformed seed: more than one JSON value", path)
	}
	// The PUT handler's own normalization and validation, on the seed alone.
	cfg := types.SiteConfig{
		UpstreamProxyURL: seed.UpstreamProxyURL, UpstreamProxySecretRef: seed.UpstreamProxySecretRef,
		UpstreamProxyNoProxy: seed.UpstreamProxyNoProxy, InternalHosts: seed.InternalHosts,
	}
	normalizeSiteConfigTopology(&cfg)
	if err := validateSiteConfig(cfg); err != nil {
		return nil, fmt.Errorf("WARDYN_SITE_CONFIG_SEED_FILE %s: invalid seed: %w", path, err)
	}
	seed.UpstreamProxyURL = cfg.UpstreamProxyURL
	return &seed, nil
}

// apply fills each setting sc does not have from the seed, and names separately
// each setting both hold with different values (left as the database has it).
func (seed *SiteConfigSeed) apply(sc *types.SiteConfig) (applied, differs []string) {
	if seed.UpstreamProxyURL != "" || seed.UpstreamProxySecretRef != "" {
		switch {
		case sc.UpstreamProxyURL == "" && sc.UpstreamProxySecretRef == "":
			sc.UpstreamProxyURL, sc.UpstreamProxySecretRef = seed.UpstreamProxyURL, seed.UpstreamProxySecretRef
			applied = append(applied, seedSettingUpstreamProxy)
		case sc.UpstreamProxyURL != seed.UpstreamProxyURL || sc.UpstreamProxySecretRef != seed.UpstreamProxySecretRef:
			differs = append(differs, seedSettingUpstreamProxy)
		}
	}
	if len(seed.UpstreamProxyNoProxy) > 0 {
		switch {
		case len(sc.UpstreamProxyNoProxy) == 0:
			sc.UpstreamProxyNoProxy = slices.Clone(seed.UpstreamProxyNoProxy)
			applied = append(applied, seedSettingNoProxy)
		case !slices.Equal(sortedCopy(sc.UpstreamProxyNoProxy), sortedCopy(seed.UpstreamProxyNoProxy)):
			differs = append(differs, seedSettingNoProxy)
		}
	}
	if len(seed.InternalHosts) > 0 {
		switch {
		case len(sc.InternalHosts) == 0:
			sc.InternalHosts = slices.Clone(seed.InternalHosts)
			applied = append(applied, seedSettingInternalHosts)
		case !slices.Equal(sortedInternalHosts(sc.InternalHosts), sortedInternalHosts(seed.InternalHosts)):
			differs = append(differs, seedSettingInternalHosts)
		}
	}
	return applied, differs
}

// sortedCopy and sortedInternalHosts are what apply compares: the proxy matches
// any entry of a bypass list, an internal-hosts list or a host's CIDRs, so the
// same entries in another order are the same setting. Both sort copies and
// leave the stored order alone.
func sortedCopy(in []string) []string {
	out := slices.Clone(in)
	slices.Sort(out)
	return out
}

// sortedInternalHosts renders each host as "suffix cidr,cidr" with its CIDRs
// sorted, then sorts the hosts.
func sortedInternalHosts(in []types.InternalHost) []string {
	out := make([]string, len(in))
	for i, h := range in {
		out[i] = h.HostSuffix + " " + strings.Join(sortedCopy(h.CIDRs), ",")
	}
	slices.Sort(out)
	return out
}

// SeedSiteConfig writes the seed's settings the stored site config does not
// have, once, at boot. Under the site-config lock it reads the WHOLE stored
// document, sets only the missing settings on it and writes the whole document
// back: PutSiteConfig replaces every key types.SiteConfig declares, so a
// seed-only struct would delete integrations, providers and the onboarding mark.
// A setting the database has is left alone; one that differs from the seed is
// logged once and shown on /setup/status (siteConfigSeedCheck). A nil seed does
// nothing.
func (s *Server) SeedSiteConfig(ctx context.Context, seed *SiteConfigSeed) error {
	if seed == nil {
		return nil
	}
	s.siteConfigSeed = seed
	lctx, unlock, err := s.lock(ctx, db.SiteConfigLockClass)
	if err != nil {
		return fmt.Errorf("site-config seed: take the site-config lock: %w", err)
	}
	defer unlock()
	cfg, err := s.cfg.Store.GetSiteConfig(lctx)
	if err != nil {
		return fmt.Errorf("site-config seed: read the site config: %w", err)
	}
	applied, differs := seed.apply(&cfg)
	if len(differs) > 0 {
		slog.Warn("wardynd: the site-config seed file and the database disagree; the database value is in effect",
			slog.String("settings", strings.Join(differs, ",")))
	}
	if len(applied) == 0 {
		return nil
	}
	if err := validateSiteConfig(cfg); err != nil {
		return fmt.Errorf("site-config seed: the seeded site config is invalid: %w", err)
	}
	saved, err := s.cfg.Store.PutSiteConfig(lctx, cfg)
	if err != nil {
		return fmt.Errorf("site-config seed: write the site config: %w", err)
	}
	s.recordAudit(lctx, s.auditEvent(nil, types.ActorSystem, "wardynd", "site_config.seed", "site_config", "success",
		mustJSON(map[string]any{"settings": applied})))
	slog.Info("wardynd: site-config seed applied", slog.String("settings", strings.Join(applied, ",")))
	if slices.Contains(applied, seedSettingInternalHosts) {
		logWarnInternalHostsDeclared(saved.InternalHosts)
	}
	// The seed restores the secret's NAME, never its value: after a rebuild on
	// the default secret store the proxy credential is gone too. A warning, as
	// on PUT; never a refusal.
	if slices.Contains(applied, seedSettingUpstreamProxy) {
		if dangling := danglingSiteConfigSecretRefs(types.SiteConfig{UpstreamProxySecretRef: saved.UpstreamProxySecretRef}, s.presentSecretNames(lctx)); len(dangling) > 0 {
			slog.Warn("wardynd: the seeded upstream_proxy_secret_ref names a secret that is not set; re-create it",
				slog.String("secret", dangling[0]))
		}
	}
	return nil
}

// siteConfigSeedCheck is the /setup/status info row shown while the database
// holds a seeded setting with a value other than the seed file's: the seed
// never overwrites one, so the admin is told which value is in effect.
func siteConfigSeedCheck(seed *SiteConfigSeed, sc types.SiteConfig) (SetupCheck, bool) {
	if seed == nil {
		return SetupCheck{}, false
	}
	_, differs := seed.apply(&sc)
	if len(differs) == 0 {
		return SetupCheck{}, false
	}
	return SetupCheck{
		ID: "site_config_seed", Label: "Site-config seed file", Status: "info",
		Detail: "The database value is in effect for " + strings.Join(differs, ", ") +
			": it differs from the seed file, and a seed never overwrites a setting the database has.",
		Fix: "To use the file's value, clear the setting in the Network step (or PUT /api/v1/site-config) and restart wardynd. To keep the database value, change the seed file to match.",
	}, true
}
