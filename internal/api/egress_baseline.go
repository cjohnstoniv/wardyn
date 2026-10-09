// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// The operator-extensible egress baseline (SiteEgress.BaselineHosts). It LOWERS the egress grade and
// the CC3 floor an api_key to an internal host would otherwise raise, so it is a governance write:
// securityOps, held for a second human under WARDYN_GOVERNANCE_SECOND_HUMAN, audited with both sets.
// The set lives in the site-config document but PUT /site-config never writes it.
package api

import (
	"context"
	"fmt"
	"net/http"
	"net/netip"
	"slices"
	"strings"

	"github.com/cjohnstoniv/wardyn/internal/composer"
	"github.com/cjohnstoniv/wardyn/internal/db"
	"github.com/cjohnstoniv/wardyn/internal/hostrules"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

const (
	govKindEgressBaseline   = types.GovernanceTargetEgressBaseline
	govEgressBaselineKey    = "egress.baseline_hosts"
	maxEgressBaselineHosts  = 256
	egressBaselineWriteName = "governance.egress_baseline.write"
)

// egressBaselineBody is PUT /governance/egress-baseline's body and GET's response: the whole set.
type egressBaselineBody struct {
	BaselineHosts []string `json:"baseline_hosts"`
}

// normalizeBaselineHosts lowercases, validates, de-duplicates and sorts a submitted set. Exact
// hostnames only: ValidApprovedHost refuses a wildcard, scheme, port or path, and an IP literal is
// refused here (an address is not a name an operator vouches for).
func normalizeBaselineHosts(in []string) ([]string, error) {
	if len(in) > maxEgressBaselineHosts {
		return nil, fmt.Errorf("baseline_hosts: at most %d hosts", maxEgressBaselineHosts)
	}
	out := make([]string, 0, len(in))
	for i, raw := range in {
		h := strings.ToLower(strings.TrimSpace(raw))
		if _, err := netip.ParseAddr(h); err == nil || !hostrules.ValidApprovedHost(h) {
			return nil, fmt.Errorf("baseline_hosts[%d]: %q must be an exact hostname — no wildcard, scheme, port, path or IP address", i, raw)
		}
		out = append(out, h)
	}
	slices.Sort(out)
	return slices.Compact(out), nil
}

// egressBaselineHosts is the stored set, sorted, never nil: the state a proposal and its approval hash.
func egressBaselineHosts(sc types.SiteConfig) []string {
	if sc.Egress == nil {
		return []string{}
	}
	return sortedKeys(baselineSet(sc.Egress.BaselineHosts))
}

func baselineSet(in []string) map[string]bool {
	m := make(map[string]bool, len(in))
	for _, v := range in {
		m[v] = true
	}
	return m
}

// withEgressBaselineHosts stores hosts on sc; an empty set clears the block so the document reads as
// it did before the field existed.
func withEgressBaselineHosts(sc types.SiteConfig, hosts []string) types.SiteConfig {
	sc.Egress = nil
	if len(hosts) > 0 {
		sc.Egress = &types.SiteEgress{BaselineHosts: hosts}
	}
	return sc
}

func egressBaselineAuditData(before, after []string) map[string]any {
	b, a := baselineSet(before), baselineSet(after)
	added := slices.DeleteFunc(slices.Clone(after), func(h string) bool { return b[h] })
	removed := slices.DeleteFunc(slices.Clone(before), func(h string) bool { return a[h] })
	return map[string]any{"before": before, "after": after, "added": added, "removed": removed}
}

// egressBaselineOf is the effective extension the composer reads: the declared hosts and every
// internal_hosts entry the operator marked baseline.
func egressBaselineOf(sc types.SiteConfig) composer.Baseline {
	b := composer.Baseline{Hosts: egressBaselineHosts(sc)}
	for _, h := range sc.InternalHosts {
		if h.Baseline {
			b.Suffixes = append(b.Suffixes, strings.ToLower(strings.TrimSpace(h.HostSuffix)))
		}
	}
	return b
}

// egressBaseline reads the site config for the composer, once per request, so preview, preflight and
// create grade on the same set. A store that cannot be read is an error,
// never an empty set: a grade computed on a baseline nobody could read is a guess.
func (s *Server) egressBaseline(ctx context.Context) (composer.Baseline, error) {
	if s.cfg.Store == nil {
		return composer.Baseline{}, nil
	}
	sc, err := s.cfg.Store.GetSiteConfig(ctx)
	if err != nil {
		return composer.Baseline{}, err
	}
	return egressBaselineOf(sc), nil
}

// baselineOr500 is egressBaseline for a handler: the 500 is written and ok is false on a failed read.
func (s *Server) baselineOr500(w http.ResponseWriter, r *http.Request) (composer.Baseline, bool) {
	b, err := s.egressBaseline(r.Context())
	if err != nil {
		writeServerError(w, r, "get site config", err)
	}
	return b, err == nil
}

// handleGetEgressBaseline returns the declared set. securityOps (routes.go).
func (s *Server) handleGetEgressBaseline(w http.ResponseWriter, r *http.Request) {
	sc, err := s.cfg.Store.GetSiteConfig(r.Context())
	if err != nil {
		writeServerError(w, r, "get site config", err)
		return
	}
	hosts := egressBaselineHosts(sc)
	w.Header().Set("ETag", computeETag(hosts))
	writeJSON(w, http.StatusOK, egressBaselineBody{BaselineHosts: hosts})
}

// handlePutEgressBaseline replaces the whole declared set. securityOps (routes.go). If-Match is
// optional optimistic concurrency against GET's ETag, as PUT /permissions/enforcement does.
func (s *Server) handlePutEgressBaseline(w http.ResponseWriter, r *http.Request) {
	mode, ok := s.governanceWriteMode(w, r)
	if !ok {
		return
	}
	var body egressBaselineBody
	if !decodeStrict(w, r, &body) {
		return
	}
	hosts, err := normalizeBaselineHosts(body.BaselineHosts)
	if err != nil {
		writeErrorReason(w, http.StatusBadRequest, reasonEgressBaselineInvalid, "invalid egress baseline: "+err.Error())
		return
	}
	if mode == govQueue {
		// Never exempt: any host added lowers a grade, and nothing proves a replacement only removes.
		s.holdEgressBaseline(w, r, hosts)
		return
	}
	r, unlock, ok := s.lockDoor(w, r, db.SiteConfigLockClass)
	if !ok {
		return
	}
	defer unlock()
	sc, err := s.cfg.Store.GetSiteConfig(r.Context())
	if err != nil {
		writeServerError(w, r, "get site config", err)
		return
	}
	before := egressBaselineHosts(sc)
	if !ifMatchSatisfied(r, computeETag(before)) {
		writeErrorReason(w, http.StatusPreconditionFailed, reasonEgressBaselineStale,
			"If-Match does not match the current egress baseline — GET /governance/egress-baseline again and retry")
		return
	}
	if _, err := s.cfg.Store.PutSiteConfig(r.Context(), withEgressBaselineHosts(sc, hosts)); err != nil {
		writeServerError(w, r, "put site config", err)
		return
	}
	s.recordAudit(r.Context(), s.auditEvent(nil, actorTypeFromRequest(r), principalFromRequest(r),
		egressBaselineWriteName, govEgressBaselineKey, "success", mustJSON(egressBaselineAuditData(before, hosts))))
	if mode == govBypass {
		s.recordGovernanceBypass(r, govKindEgressBaseline, govEgressBaselineKey, "success", nil)
	}
	w.Header().Set("ETag", computeETag(hosts))
	writeJSON(w, http.StatusOK, egressBaselineBody{BaselineHosts: hosts})
}

// holdEgressBaseline stores a validated replacement as a pending change. The approval compares the
// base hash, which is the ETag If-Match is judged against here.
func (s *Server) holdEgressBaseline(w http.ResponseWriter, r *http.Request, hosts []string) {
	var before []string
	err := s.readGovernanceState(r, func(q store.Querier) error {
		sc, err := store.GetSiteConfigQ(r.Context(), q)
		before = egressBaselineHosts(sc)
		return err
	})
	if err != nil {
		writeServerError(w, r, "get site config", err)
		return
	}
	if !ifMatchSatisfied(r, computeETag(before)) {
		writeErrorReason(w, http.StatusPreconditionFailed, reasonEgressBaselineStale,
			"If-Match does not match the current egress baseline — GET /governance/egress-baseline again and retry")
		return
	}
	after := egressBaselineBody{BaselineHosts: hosts}
	s.proposeGovernanceChange(w, r, govProposal{
		kind: govKindEgressBaseline, op: "replace", key: govEgressBaselineKey, payload: after,
		before: egressBaselineBody{BaselineHosts: before}, after: after,
		changed:   changedPaths(egressBaselineBody{BaselineHosts: before}, after),
		baseState: before,
	})
}

// applyEgressBaselineChange applies a held replacement inside the decision transaction: the target
// lock, then the site-config lock every site-config writer holds, the set read as it now stands and
// compared with what the proposal reviewed, then the whole document written back on q.
func applyEgressBaselineChange(s *Server, r *http.Request, q store.Querier, ch types.GovernanceChange) (govApplied, error) {
	ctx := r.Context()
	var body egressBaselineBody
	if err := decodeHeldPayload(ch.Payload, &body); err != nil {
		return govApplied{}, fmt.Errorf("governance change %s: payload: %w", ch.ID, err)
	}
	hosts, err := normalizeBaselineHosts(body.BaselineHosts)
	if err != nil {
		return govApplied{}, writeRefusal(http.StatusBadRequest, reasonEgressBaselineInvalid, "invalid egress baseline: %s", err.Error())
	}
	if err := store.LockGovernanceTarget(ctx, q, govKindEgressBaseline); err != nil {
		return govApplied{}, err
	}
	if err := store.LockSiteConfig(ctx, q); err != nil {
		return govApplied{}, err
	}
	sc, err := store.GetSiteConfigQ(ctx, q)
	if err != nil {
		return govApplied{}, err
	}
	before := egressBaselineHosts(sc)
	if computeETag(before) != ch.BaseHash {
		return govApplied{}, store.ErrGovernanceChangeStale
	}
	if _, err := store.PutSiteConfigQ(ctx, q, withEgressBaselineHosts(sc, hosts)); err != nil {
		return govApplied{}, err
	}
	return govApplied{action: egressBaselineWriteName, target: govEgressBaselineKey, data: egressBaselineAuditData(before, hosts)}, nil
}

// refuseInlineEgress reports whether a PUT /site-config body names an egress block other than the
// stored one. A round trip of GET passes; a change belongs to PUT /governance/egress-baseline, whose
// tier and four-eyes gate this document's writer does not have.
func refuseInlineEgress(submitted, stored *types.SiteEgress) bool {
	if submitted == nil {
		return false
	}
	got, err := normalizeBaselineHosts(submitted.BaselineHosts)
	return err != nil || !slices.Equal(got, egressBaselineHosts(types.SiteConfig{Egress: stored}))
}
