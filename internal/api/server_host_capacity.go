// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"

	"github.com/cjohnstoniv/wardyn/internal/hostcapacity"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// HostCapacityConfig is Config's host-capacity admission, embedded so its
// wiring lives beside the feature rather than in server.go.
type HostCapacityConfig struct {
	// HostCapacity refuses every run launch while the host is over its memory
	// or load limits (WARDYN_HOST_*); nil admits everything.
	HostCapacity *hostcapacity.Guard
}

// admitHostCapacity is every launch door's host-capacity check: nil, or the
// hostcapacity.ErrRefused after a host_capacity.refuse audit row naming the
// door, who asked and the measured reason. The refusal is the daemon's, so the
// row's actor is wardynd; no run exists yet, so it carries no run id.
func (s *Server) admitHostCapacity(ctx context.Context, requestedBy, door string) error {
	err := s.cfg.HostCapacity.Admit()
	var refused hostcapacity.ErrRefused
	if errors.As(err, &refused) {
		s.recordAudit(ctx, s.auditEvent(nil, types.ActorSystem, "wardynd", "host_capacity.refuse", door, "denied",
			mustJSON(map[string]any{"reason": refused.Reason, "requested_by": requestedBy})))
	}
	return err
}
