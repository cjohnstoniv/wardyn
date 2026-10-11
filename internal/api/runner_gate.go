// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"net/http"
)

var (
	errRunnersConfigUnavailable = errors.New("runner configuration is unavailable")
	errRunnersDisabled          = errors.New("runners are disabled for this organisation")
)

// runnersEnabled reads the stored switch on every call, so a stored disable
// takes effect without a restart.
func (s *Server) runnersEnabled(ctx context.Context) error {
	if s.cfg.Store == nil {
		return errRunnersConfigUnavailable
	}
	cfg, err := s.cfg.Store.GetSiteConfig(ctx)
	if err != nil {
		return errRunnersConfigUnavailable
	}
	if cfg.Runners == nil || !cfg.Runners.Enabled {
		return errRunnersDisabled
	}
	return nil
}

// Read on each admission so a stored disable takes effect without a restart.
func (s *Server) requireRunnersEnabled(w http.ResponseWriter, r *http.Request) bool {
	switch err := s.runnersEnabled(r.Context()); {
	case errors.Is(err, errRunnersDisabled):
		writeErrorReason(w, http.StatusForbidden, reasonRunnersDisabled, err.Error())
		return false
	case err != nil:
		writeErrorReason(w, http.StatusServiceUnavailable, reasonRunnersConfigUnavailable, "runner configuration is unavailable; retry later")
		return false
	}
	return true
}
