// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import "net/http"

// Read on each admission so a stored disable takes effect without a restart.
func (s *Server) requireRunnersEnabled(w http.ResponseWriter, r *http.Request) bool {
	cfg, err := s.cfg.Store.GetSiteConfig(r.Context())
	if err != nil {
		writeErrorReason(w, http.StatusServiceUnavailable, reasonRunnersConfigUnavailable, "runner configuration is unavailable; retry later")
		return false
	}
	if cfg.Runners == nil || !cfg.Runners.Enabled {
		writeErrorReason(w, http.StatusForbidden, reasonRunnersDisabled, "runners are disabled for this organisation")
		return false
	}
	return true
}
