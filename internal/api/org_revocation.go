// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// errOrgRevoked is a hybrid laptop's refusal to create a run once its
// organisation has revoked the device. writeServerError answers it 503 with
// this sentence, so every launcher that already maps its store failure there
// needs no branch of its own.
var errOrgRevoked = errors.New(orgRevokedMsg)

const orgRevokedMsg = "this device's enrolment with its organisation was revoked; new runs are refused " +
	"until it is re-enrolled (deliver a fresh WARDYN_ORG_ENROLMENT_TOKEN and restart wardynd)"

// createRun is the ONE door to Store.CreateRun: a revoked hybrid laptop creates
// no new local run, whichever launcher asks (POST /runs, harness login, record
// runs, source scans, site-config probes). Every sandbox is dispatched for a run
// this created, so gating the row gates the sandbox too.
// TestRunCreationRoutesThroughTheRevocationGate fails on a launcher that calls
// the store directly.
//
// A run created between the revocation and the forwarder's next call is
// legitimately local: the gate is only as fresh as that call.
func (s *Server) createRun(ctx context.Context, run types.AgentRun) (types.AgentRun, error) {
	if s.cfg.OrgFederation != nil && s.cfg.OrgFederation().Revoked {
		return types.AgentRun{}, errOrgRevoked
	}
	return s.cfg.Store.CreateRun(ctx, run)
}
