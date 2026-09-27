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

// admitHostCapacity is the host-capacity check every launch door and Review
// share: nil, or the hostcapacity.ErrRefused. On a launch (launch=true) a
// refusal first writes a host_capacity.refuse audit row naming the door, who
// asked and the measured reason; the refusal is the daemon's, so the row's
// actor is wardynd, and no run exists yet, so it carries no run id. Review
// (launch=false) answers the same refusal and writes nothing, the preview
// contract every other shared create/preflight gate keeps.
func (s *Server) admitHostCapacity(ctx context.Context, requestedBy, door string, launch bool) error {
	err := s.cfg.HostCapacity.Admit()
	var refused hostcapacity.ErrRefused
	if launch && errors.As(err, &refused) {
		s.recordAudit(ctx, s.auditEvent(nil, types.ActorSystem, "wardynd", "host_capacity.refuse", door, "denied",
			mustJSON(map[string]any{"reason": refused.Reason, "requested_by": requestedBy})))
	}
	return err
}
