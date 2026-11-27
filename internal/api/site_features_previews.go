// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// features.preview_urls: the switch and the ceiling of preview URLs (run_previews.go). The key registers here,
// and stays unavailable until this release serves previews end to end: until then it cannot be turned on or
// set, and reads off whatever is stored.

// previewServed is whether this release serves preview URLs end to end.
const previewServed = false

// previewPublicServed is whether this release serves public preview links. Until it does, max_visibility public
// is refused, and a stored public reads private.
const previewPublicServed = false

func init() {
	registerSiteFeature(featurePreviewURLs, siteFeatureHook{
		available:  func() bool { return previewServed },
		validate:   validatePreviewURLs,
		afterWrite: revokePreviewsAboveCeiling,
	})
}

// previewURLsOn reports whether previews may be created and opened: the key is available and turned on.
func previewURLsOn(sc types.SiteConfig) bool { return siteFeatureOn(sc, featurePreviewURLs) }

// previewCeiling is the widest a preview may be: the stored max_visibility. Absent, a value this release does not
// know, and public before public links are served all read private, the safest value, as setup's
// preview_max_visibility reports it (siteFeatureFlags).
func previewCeiling(sc types.SiteConfig) types.PreviewVisibility {
	p := siteFeatureSettings(sc).PreviewURLs
	if p == nil {
		return types.PreviewPrivate
	}
	switch v := types.PreviewVisibility(p.MaxVisibility); v {
	case types.PreviewOrg:
		return v
	case types.PreviewPublic:
		if previewPublicServed {
			return v
		}
	}
	return types.PreviewPrivate
}

// validatePreviewURLs checks a write that changes the key: turning it on needs the UI gateway
// (WARDYN_UI_SANDBOX_LISTEN), a ceiling of org or public needs per-run hosts (WARDYN_UI_SANDBOX_ORIGIN_TEMPLATE),
// public needs a release that serves public links, and any other ceiling is refused. Each answers
// site_config_invalid.
func validatePreviewURLs(_ context.Context, s *Server, next, _ *types.SiteFeatureSwitches) error {
	if next == nil || next.PreviewURLs == nil {
		return nil
	}
	p := next.PreviewURLs
	if p.Enabled != nil && *p.Enabled && !s.uiGatewayEnabled() {
		return errors.New("features.preview_urls: preview URLs are served by the UI gateway, which is off on this server; set WARDYN_UI_SANDBOX_LISTEN first")
	}
	switch v := types.PreviewVisibility(p.MaxVisibility); v {
	case "", types.PreviewPrivate:
		return nil
	case types.PreviewOrg, types.PreviewPublic:
		if v == types.PreviewPublic && !previewPublicServed {
			return errors.New("features.preview_urls.max_visibility: public is not available in this release")
		}
		if s.cfg.UIOriginTemplate == "" {
			return fmt.Errorf("features.preview_urls.max_visibility: %s previews need per-run hosts; set WARDYN_UI_SANDBOX_ORIGIN_TEMPLATE first", v)
		}
		return nil
	}
	return fmt.Errorf("features.preview_urls.max_visibility: %q is not private, org or public", p.MaxVisibility)
}

// revokePreviewsAboveCeiling is the key's afterWrite, run after each saved write that changes it: it revokes
// every live preview wider than the ceiling the write left (every one, when previews are now off), then drops
// this replica's cached inbound sessions, so their next request is checked again. A failed revoke writes a
// preview.revoke failure row, and the next write that changes the key runs it again; until then the gateway's
// re-check refuses the rows it missed.
func revokePreviewsAboveCeiling(ctx context.Context, s *Server, _, after types.SiteConfig) {
	defer s.dropInboundSessions()
	ceiling := types.PreviewVisibility("")
	if previewURLsOn(after) {
		ceiling = previewCeiling(after)
	}
	ps, ok := s.cfg.Store.(runPreviewStore)
	if !ok {
		return // this store keeps no previews
	}
	at, by := auditActorFromContext(ctx)
	revoked, err := ps.RevokeRunPreviewsAbove(ctx, ceiling, by)
	if err != nil {
		slog.ErrorContext(ctx, "wardynd: revoking previews above the ceiling failed; the next write that changes features.preview_urls runs it again",
			"ceiling", string(ceiling), "err", err)
		s.recordAudit(ctx, s.auditEvent(nil, at, by, "preview.revoke", "", "failure", mustJSON(map[string]any{"ceiling": string(ceiling)})))
		return
	}
	for _, p := range revoked {
		data := previewData(p)
		data["ceiling"] = string(ceiling)
		s.recordAudit(ctx, s.auditEvent(&p.RunID, at, by, "preview.revoke", previewApp(p.Port), "success", mustJSON(data)))
	}
}
