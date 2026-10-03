// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"net/http"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// errOrgRevoked is a hybrid laptop's refusal to create a run once its
// organisation has revoked the device. writeServerError answers it 503 with
// this sentence, so every launcher that already maps its store failure there
// needs no branch of its own.
var errOrgRevoked = errors.New(orgRevokedMsg)

const orgRevokedMsg = "this device's enrolment with its organisation was revoked; new runs are refused " +
	"until it is re-enrolled (deliver a fresh WARDYN_ORG_ENROLMENT_TOKEN and restart wardynd)"

// runCapCreator is the store's atomic capped insert (store.PG), asserted rather
// than carried by store.Store like Pager: a store without it refuses a capped
// create, so the cap fails closed.
type runCapCreator interface {
	CreateRunUnderCap(ctx context.Context, r types.AgentRun, limit int) (types.AgentRun, error)
}

// runCounter is the store's lock-free count of non-terminal runs, for the
// deployment-cap refusal that precedes the identity mint (refuseRunCapFull).
type runCounter interface {
	CountNonTerminalRuns(ctx context.Context) (int, error)
}

// refuseRunCapFull writes the 422 run_quota (or the count's 5xx) and reports
// true when the deployment already holds its cap of non-terminal runs. It is the
// early refusal only: CreateRunUnderCap, under its lock, stays the authority,
// and a store that cannot count admits here so createRun's own fail-closed
// check decides.
func (s *Server) refuseRunCapFull(w http.ResponseWriter, r *http.Request) bool {
	c, ok := s.cfg.Store.(runCounter)
	if s.cfg.MaxConcurrentRuns <= 0 || !ok {
		return false
	}
	n, err := c.CountNonTerminalRuns(r.Context())
	if err == nil && n < s.cfg.MaxConcurrentRuns {
		return false
	}
	if err == nil {
		err = store.ErrRunCapReached
	}
	writeServerError(w, r, "count active runs", err)
	return true
}

// createRun is the ONE door to Store.CreateRun: a revoked hybrid laptop creates
// no new local run, whichever launcher asks (POST /runs, harness login, record
// runs, source scans, site-config probes). Every sandbox is dispatched for a run
// this created, so gating the row gates the sandbox too.
// TestRunCreationRoutesThroughTheRevocationGate fails on a launcher that calls
// the store directly.
//
// It is also where WARDYN_MAX_CONCURRENT_RUNS bites: the store counts and inserts
// under one advisory lock, so no door can slip past the cap. Revive adds no row
// and never comes through here.
//
// A run created between the revocation and the forwarder's next call is
// legitimately local: the gate is only as fresh as that call.
func (s *Server) createRun(ctx context.Context, run types.AgentRun) (types.AgentRun, error) {
	if s.cfg.OrgFederation != nil && s.cfg.OrgFederation().Revoked {
		return types.AgentRun{}, errOrgRevoked
	}
	if s.cfg.MaxConcurrentRuns <= 0 {
		return s.cfg.Store.CreateRun(ctx, run)
	}
	capped, ok := s.cfg.Store.(runCapCreator)
	if !ok {
		return types.AgentRun{}, errors.New("store cannot enforce WARDYN_MAX_CONCURRENT_RUNS")
	}
	return capped.CreateRunUnderCap(ctx, run, s.cfg.MaxConcurrentRuns)
}
