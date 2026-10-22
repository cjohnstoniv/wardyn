// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"log/slog"
)

// WarnUnresolvableUnits logs, once at boot, each composed governance profile whose chain no
// longer composes (governance_overlay_unsatisfiable: a base was narrowed after it was written).
// Runs under such a profile are refused at launch; this names the profile before the first one is.
// Best-effort: a failed read is logged and the walk ends.
func (s *Server) WarnUnresolvableUnits(ctx context.Context) {
	all, err := s.resolveAllUnits(ctx)
	if err != nil {
		slog.WarnContext(ctx, "wardynd: governance profile walk deferred", slog.Any("err", err))
		return
	}
	for _, p := range all {
		if !p.Row.Composed() {
			continue
		}
		err := p.Err
		if u, ok := isOverlayUnsatisfiable(err); ok {
			slog.WarnContext(ctx, "wardynd: a composed governance profile no longer resolves; runs under it are refused",
				slog.String("profile", p.Row.Name), slog.String("reason", string(reasonGovernanceOverlayUnsatisfiable)), slog.String("detail", u.Detail))
		}
	}
}
