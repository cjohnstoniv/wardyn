// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// carryForwardSiteDesktopWatermark keeps the stored desktop watermark block when the body does not name it, on the
// provider blocks' MDM re-apply terms, and stores an all-default block as none so a GET stays byte-identical to an
// install that never set one.
func carryForwardSiteDesktopWatermark(cfg *types.SiteConfig, existing types.SiteConfig, present map[string]bool) {
	if !present["desktop_watermark"] {
		cfg.DesktopWatermark = existing.DesktopWatermark
	} else if cfg.DesktopWatermark != nil && *cfg.DesktopWatermark == (types.SiteDesktopWatermark{}) {
		cfg.DesktopWatermark = nil
	}
}

// auditSiteDesktopWatermark puts whether the organisation requires the watermark on the site_config.write row, once
// a block is stored or the body named one, so an install that never set it writes the row it always wrote.
func auditSiteDesktopWatermark(datum map[string]any, w *types.SiteDesktopWatermark, named bool) {
	if w == nil && !named {
		return
	}
	datum["desktop_watermark_required"] = w != nil && w.Required
}

// desktopWatermarkView is what a desktop view is handed to draw its watermark: who is looking, at which run, and
// whether the organisation requires the mark. The server decides; no view reads site config itself.
type desktopWatermarkView struct {
	Who      string `json:"who"`
	Run      string `json:"run"`
	Required bool   `json:"required"`
}

// desktopWatermarkRequired reads the organisation's rule.
// An unreadable document reads as required: a failed read never removes a mark the organisation asked for.
func (s *Server) desktopWatermarkRequired(ctx context.Context) bool {
	sc, ok := s.siteConfigSnapshot(ctx)
	return !ok || (sc.DesktopWatermark != nil && sc.DesktopWatermark.Required)
}

// desktopWatermarkFor is the view a desktop is handed: who is the person's verified email, else their principal.
func (s *Server) desktopWatermarkFor(ctx context.Context, email, principal, run string) desktopWatermarkView {
	who := email
	if who == "" {
		who = principal
	}
	return desktopWatermarkView{Who: who, Run: run, Required: s.desktopWatermarkRequired(ctx)}
}
