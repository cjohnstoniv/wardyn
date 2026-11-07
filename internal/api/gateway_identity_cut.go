// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"

	"github.com/cjohnstoniv/wardyn/internal/egress/proxy"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// A revoke that cuts the gateway access of the runs a portal launched. The option is per call and
// off by default, so a revoke without it leaves those runs running as the person's runs with their
// gateway access, as before. The cut is gateway access only: the run, its files, its other egress
// and any credential already in its proxy are untouched; POST /runs/{id}/kill stops a run.
const (
	cutInFlightParam = "cut_in_flight"

	// gatewayCutMsg is short on purpose: the proxy reads at most 1 KiB of the refusal for its reason.
	gatewayCutMsg       = "This run's gateway access was cut when the portal that started it was revoked. Start a new run if it is still needed."
	gatewayReviveCutMsg = "This run's gateway access was cut when the portal that started it was revoked, so nothing was revived. Start a new run if it is still needed."
)

// cutInFlightOf reads ?cut_in_flight= on a portal revoke: absent or false is a plain revoke, true
// also cuts. Any other value, the parameter twice, or a query that does not parse is refused before
// anything is written: URL.Query drops a pair it cannot parse, which would read as a plain revoke.
func cutInFlightOf(w http.ResponseWriter, r *http.Request) (cut, ok bool) {
	query, err := url.ParseQuery(r.URL.RawQuery)
	vals, set := query[cutInFlightParam]
	switch {
	case err != nil:
	case !set:
		return false, true
	case len(vals) == 1 && vals[0] == "false":
		return false, true
	case len(vals) == 1 && vals[0] == "true":
		return true, true
	}
	writeErrorReason(w, http.StatusBadRequest, reasonInvalidCutInFlightParam, "cut_in_flight must be true or false")
	return false, false
}

// revokeAuditData is the delegate.revoke row's data. A cut adds cut_in_flight and the count of the
// portal's runs still going; a cut of a portal revoked earlier says so. Both stamps come from the same
// statement on a fresh revoke, so they differ only when the revoke came first.
func revokeAuditData(d types.Delegate, cut bool, inFlight int) map[string]any {
	data := map[string]any{"name": d.Name}
	if !cut {
		return data
	}
	data["cut_in_flight"], data["runs_in_flight"] = true, inFlight
	if d.RevokedAt != nil && d.CutInFlightAt != nil && !d.RevokedAt.Equal(*d.CutInFlightAt) {
		data["already_revoked"] = true
	}
	return data
}

func gatewayCut() *ownerRefusal {
	return &ownerRefusal{status: http.StatusForbidden, reason: reasonGatewayAccessCut, msg: gatewayCutMsg}
}

// gatewayMintVia refuses a run whose portal's revoke cut the gateway access of the runs it launched.
// It is read at every mint, on every replica, so a run created while the revoke ran is covered and
// nothing is marked on runs. A run a person launched themselves reads nothing. A portal row that is
// gone refuses too (rows are soft-deleted, so this is defensive); a store that cannot answer, or
// cannot read portals at all, fails closed.
func (s *Server) gatewayMintVia(ctx context.Context, run types.AgentRun) *ownerRefusal {
	cut, err := s.runPortalCut(ctx, run)
	switch {
	case err != nil:
		slog.ErrorContext(ctx, "gateway identity: read the run's portal", slog.String("run_id", run.ID.String()), slog.Any("err", err))
		return gatewayUnavailable()
	case cut:
		return gatewayCut()
	}
	return nil
}

// runPortalCut reports whether run was launched through a portal whose revoke cut its gateway access.
func (s *Server) runPortalCut(ctx context.Context, run types.AgentRun) (bool, error) {
	if run.CreatedVia == nil {
		return false, nil
	}
	dr, ok := s.cfg.Store.(store.DelegateReader)
	if !ok {
		return false, errors.New("the store cannot read portals")
	}
	d, err := dr.GetDelegateByID(ctx, *run.CreatedVia)
	switch {
	case errors.Is(err, store.ErrNotFound):
		return true, nil
	case err != nil:
		return false, err
	}
	return d.CutInFlightAt != nil, nil
}

// gatewayReviveCut is the revive's form of the cut: a run that would refuse every gateway call is not
// revived. The run is read by id because the revive's check has only the config.
func (s *Server) gatewayReviveCut(ctx context.Context, cfg *proxy.Config) (*ownerRefusal, error) {
	run, err := s.cfg.Store.GetRun(ctx, cfg.RunID)
	if err != nil {
		return nil, fmt.Errorf("read the run: %w", err)
	}
	cut, err := s.runPortalCut(ctx, run)
	if err != nil {
		return nil, fmt.Errorf("read the run's portal: %w", err)
	}
	if cut {
		return &ownerRefusal{status: http.StatusConflict, reason: reasonGatewayAccessCut, msg: gatewayReviveCutMsg}, nil
	}
	return nil, nil
}

// refuseGatewayCut answers a cut run's mint. The denied row is written once per run and destination
// on this replica: a proxy from before the cut asks again every 30 seconds for the run's life.
// The cut check runs before the destination is validated, so target is what the caller sent, and a
// malformed or empty id is recorded as sent.
func (s *Server) refuseGatewayCut(w http.ResponseWriter, run types.AgentRun, kind, target string, ref *ownerRefusal, record func(string, map[string]any)) {
	holder := target
	if kind == "mcp_server" {
		holder = "mcp\x00" + target
	}
	if s.gatewayFirstMint("cut\x00"+run.ID.String(), holder) {
		record("denied", map[string]any{"reason": ref.reason, "cause": "delegate_revoked", "delegate": run.CreatedVia.String(), kind: target})
	}
	writeErrorReason(w, ref.status, ref.reason, ref.msg)
}
