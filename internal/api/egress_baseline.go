// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// The operator-extensible egress baseline: SiteEgress.BaselineHosts (exact hostnames) and the
// internal_hosts entries marked Baseline (a suffix and its subdomains). Both LOWER the egress grade and
// the CC3 floor an api_key to an internal host would otherwise raise, so they have ONE writer and one
// gate: securityOps, held for a second human under WARDYN_GOVERNANCE_SECOND_HUMAN, audited with both
// before and after sets. Both live in the site-config document, but PUT /site-config never changes
// either; only the boot seed file may declare a mark, once, on an empty document.
package api

import (
	"context"
	"fmt"
	"net/http"
	"net/netip"
	"slices"
	"strings"

	"golang.org/x/net/publicsuffix"

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

// egressBaselineBody is PUT /governance/egress-baseline's body, GET's response and the state a change
// is hashed over: the whole declaration. A PUT replaces both lists, so an omitted one is cleared.
type egressBaselineBody struct {
	BaselineHosts        []string `json:"baseline_hosts"`
	InternalHostSuffixes []string `json:"internal_host_suffixes"`
}

func normalizeHostName(raw string) string { return strings.ToLower(strings.TrimSpace(raw)) }

// validBaselineName refuses anything but an exact hostname: ValidApprovedHost refuses a wildcard,
// scheme, port or path, and an address is not a name an operator vouches for.
func validBaselineName(h string) bool {
	_, err := netip.ParseAddr(h)
	return err != nil && hostrules.ValidApprovedHost(h)
}

// validBaselineSuffix is validBaselineName plus a width guard: a registry-controlled suffix (co.uk,
// github.io) would grade every host under it as baseline.
func validBaselineSuffix(h string) error {
	if !validBaselineName(h) {
		return fmt.Errorf("%q must be a hostname", h)
	}
	if suffix, _ := publicsuffix.PublicSuffix(h); suffix == h {
		return fmt.Errorf("%q is a public suffix: it would mark every host under it as baseline", h)
	}
	return nil
}

// normalizeEgressBaselineBody lowercases, validates, de-duplicates and sorts both lists.
func normalizeEgressBaselineBody(in egressBaselineBody) (egressBaselineBody, error) {
	if len(in.BaselineHosts) > maxEgressBaselineHosts || len(in.InternalHostSuffixes) > maxEgressBaselineHosts {
		return egressBaselineBody{}, fmt.Errorf("at most %d hosts in each list", maxEgressBaselineHosts)
	}
	out := egressBaselineBody{BaselineHosts: []string{}, InternalHostSuffixes: []string{}}
	for i, raw := range in.BaselineHosts {
		h := normalizeHostName(raw)
		if !validBaselineName(h) {
			return out, fmt.Errorf("baseline_hosts[%d]: %q must be an exact hostname — no wildcard, scheme, port, path or IP address", i, raw)
		}
		out.BaselineHosts = append(out.BaselineHosts, h)
	}
	for i, raw := range in.InternalHostSuffixes {
		h := normalizeHostName(raw)
		if err := validBaselineSuffix(h); err != nil {
			return out, fmt.Errorf("internal_host_suffixes[%d]: %w", i, err)
		}
		out.InternalHostSuffixes = append(out.InternalHostSuffixes, h)
	}
	slices.Sort(out.BaselineHosts)
	slices.Sort(out.InternalHostSuffixes)
	out.BaselineHosts, out.InternalHostSuffixes = slices.Compact(out.BaselineHosts), slices.Compact(out.InternalHostSuffixes)
	return out, nil
}

// egressBaselineState is the declaration as stored: sorted, never nil.
func egressBaselineState(sc types.SiteConfig) egressBaselineBody {
	b := egressBaselineBody{BaselineHosts: []string{}, InternalHostSuffixes: []string{}}
	if sc.Egress != nil {
		b.BaselineHosts = sortedKeys(baselineSet(sc.Egress.BaselineHosts))
	}
	marked := map[string]bool{}
	for _, h := range sc.InternalHosts {
		if h.Baseline {
			marked[normalizeHostName(h.HostSuffix)] = true
		}
	}
	b.InternalHostSuffixes = sortedKeys(marked)
	return b
}

func baselineSet(in []string) map[string]bool {
	m := make(map[string]bool, len(in))
	for _, v := range in {
		m[v] = true
	}
	return m
}

// undeclaredSuffixes lists the suffixes that name no stored internal_hosts entry: a mark rides an
// entry, so there is nothing to mark.
func undeclaredSuffixes(sc types.SiteConfig, suffixes []string) []string {
	declared := map[string]bool{}
	for _, h := range sc.InternalHosts {
		declared[normalizeHostName(h.HostSuffix)] = true
	}
	return slices.DeleteFunc(slices.Clone(suffixes), func(s string) bool { return declared[s] })
}

// egressBaselineRefusal is the 400 a body gets when a suffix names no stored entry.
func egressBaselineRefusal(missing []string) *profileWriteError {
	return writeRefusal(http.StatusBadRequest, reasonEgressBaselineInvalid,
		"invalid egress baseline: internal_host_suffixes %s name no internal_hosts entry; declare it in site config first", strings.Join(missing, ", "))
}

// withEgressBaseline stores b on sc: the exact hosts (an empty set clears the block, so the document
// reads as it did before the field existed) and the mark on every internal_hosts entry.
func withEgressBaseline(sc types.SiteConfig, b egressBaselineBody) types.SiteConfig {
	sc.Egress = nil
	if len(b.BaselineHosts) > 0 {
		sc.Egress = &types.SiteEgress{BaselineHosts: b.BaselineHosts}
	}
	marked := baselineSet(b.InternalHostSuffixes)
	sc.InternalHosts = slices.Clone(sc.InternalHosts)
	for i := range sc.InternalHosts {
		sc.InternalHosts[i].Baseline = marked[normalizeHostName(sc.InternalHosts[i].HostSuffix)]
	}
	return sc
}

func egressBaselineAuditData(before, after egressBaselineBody) map[string]any {
	diff := func(a, b []string) (added, removed []string) {
		in, out := baselineSet(a), baselineSet(b)
		return slices.DeleteFunc(slices.Clone(b), func(h string) bool { return in[h] }),
			slices.DeleteFunc(slices.Clone(a), func(h string) bool { return out[h] })
	}
	added, removed := diff(before.BaselineHosts, after.BaselineHosts)
	sAdded, sRemoved := diff(before.InternalHostSuffixes, after.InternalHostSuffixes)
	return map[string]any{
		"before": before.BaselineHosts, "after": after.BaselineHosts, "added": added, "removed": removed,
		"suffixes_before": before.InternalHostSuffixes, "suffixes_after": after.InternalHostSuffixes,
		"suffixes_added": sAdded, "suffixes_removed": sRemoved,
	}
}

// egressBaselineOf is the effective extension the composer reads.
func egressBaselineOf(sc types.SiteConfig) composer.Baseline {
	st := egressBaselineState(sc)
	return composer.Baseline{Hosts: st.BaselineHosts, Suffixes: st.InternalHostSuffixes}
}

// egressBaseline reads the site config for the composer. Each door calls it ONCE and passes the value
// down, so the confinement floor, the posture and the facts of one request grade on the same set. A
// store that cannot be read is an error, never an empty set: a grade computed on a baseline nobody
// could read is a guess.
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

// handleGetEgressBaseline returns the whole declaration, the marks included: the security tier is
// accountable for the effective baseline and cannot read GET /site-config. securityOps (routes.go).
func (s *Server) handleGetEgressBaseline(w http.ResponseWriter, r *http.Request) {
	sc, err := s.cfg.Store.GetSiteConfig(r.Context())
	if err != nil {
		writeServerError(w, r, "get site config", err)
		return
	}
	state := egressBaselineState(sc)
	w.Header().Set("ETag", computeETag(state))
	writeJSON(w, http.StatusOK, state)
}

// handlePutEgressBaseline replaces the whole declaration. securityOps (routes.go). If-Match is
// optional optimistic concurrency against GET's ETag, as PUT /permissions/enforcement does.
func (s *Server) handlePutEgressBaseline(w http.ResponseWriter, r *http.Request) {
	mode, ok := s.governanceWriteMode(w, r)
	if !ok {
		return
	}
	var raw egressBaselineBody
	if !decodeStrict(w, r, &raw) {
		return
	}
	body, err := normalizeEgressBaselineBody(raw)
	if err != nil {
		writeErrorReason(w, http.StatusBadRequest, reasonEgressBaselineInvalid, "invalid egress baseline: "+err.Error())
		return
	}
	if mode == govQueue {
		// Never exempt: any host added lowers a grade, and nothing proves a replacement only removes.
		s.holdEgressBaseline(w, r, body)
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
	before := egressBaselineState(sc)
	if !ifMatchSatisfied(r, computeETag(before)) {
		writeErrorReason(w, http.StatusPreconditionFailed, reasonEgressBaselineStale,
			"If-Match does not match the current egress baseline — GET /governance/egress-baseline again and retry")
		return
	}
	if missing := undeclaredSuffixes(sc, body.InternalHostSuffixes); len(missing) > 0 {
		refusal := egressBaselineRefusal(missing)
		writeErrorReason(w, refusal.status, refusal.reason, refusal.msg)
		return
	}
	if _, err := s.cfg.Store.PutSiteConfig(r.Context(), withEgressBaseline(sc, body)); err != nil {
		writeServerError(w, r, "put site config", err)
		return
	}
	s.recordAudit(r.Context(), s.auditEvent(nil, actorTypeFromRequest(r), principalFromRequest(r),
		egressBaselineWriteName, govEgressBaselineKey, "success", mustJSON(egressBaselineAuditData(before, body))))
	if mode == govBypass {
		s.recordGovernanceBypass(r, govKindEgressBaseline, govEgressBaselineKey, "success", nil)
	}
	w.Header().Set("ETag", computeETag(body))
	writeJSON(w, http.StatusOK, body)
}

// holdEgressBaseline stores a validated replacement as a pending change. The approval compares the
// base hash, which is the ETag If-Match is judged against here.
func (s *Server) holdEgressBaseline(w http.ResponseWriter, r *http.Request, after egressBaselineBody) {
	var before egressBaselineBody
	var missing []string
	err := s.readGovernanceState(r, func(q store.Querier) error {
		sc, err := store.GetSiteConfigQ(r.Context(), q)
		before, missing = egressBaselineState(sc), undeclaredSuffixes(sc, after.InternalHostSuffixes)
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
	if len(missing) > 0 {
		refusal := egressBaselineRefusal(missing)
		writeErrorReason(w, refusal.status, refusal.reason, refusal.msg)
		return
	}
	s.proposeGovernanceChange(w, r, govProposal{
		kind: govKindEgressBaseline, op: "replace", key: govEgressBaselineKey, payload: after,
		before: before, after: after, changed: changedPaths(before, after), baseState: before,
	})
}

// applyEgressBaselineChange applies a held replacement inside the decision transaction: the target
// lock, then the site-config lock every site-config writer holds, the declaration read as it now
// stands and compared with what the proposal reviewed, the suffixes re-checked against the entries
// that now exist, then the whole document written back on q.
func applyEgressBaselineChange(s *Server, r *http.Request, q store.Querier, ch types.GovernanceChange) (govApplied, error) {
	ctx := r.Context()
	var held egressBaselineBody
	if err := decodeHeldPayload(ch.Payload, &held); err != nil {
		return govApplied{}, fmt.Errorf("governance change %s: payload: %w", ch.ID, err)
	}
	body, err := normalizeEgressBaselineBody(held)
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
	before := egressBaselineState(sc)
	if computeETag(before) != ch.BaseHash {
		return govApplied{}, store.ErrGovernanceChangeStale
	}
	if missing := undeclaredSuffixes(sc, body.InternalHostSuffixes); len(missing) > 0 {
		return govApplied{}, egressBaselineRefusal(missing)
	}
	if _, err := store.PutSiteConfigQ(ctx, q, withEgressBaseline(sc, body)); err != nil {
		return govApplied{}, err
	}
	return govApplied{action: egressBaselineWriteName, target: govEgressBaselineKey, data: egressBaselineAuditData(before, body)}, nil
}

// refuseInlineBaseline reports whether a PUT /site-config body would change the baseline: its egress
// block against the stored one, or its set of baseline-marked internal hosts against the stored set.
// cfg has already had unnamed fields carried forward, so a body that is silent passes. A round trip of
// GET passes; a change belongs to PUT /governance/egress-baseline, whose tier and four-eyes gate this
// document's writer does not have.
func refuseInlineBaseline(cfg, existing types.SiteConfig) bool {
	got := egressBaselineState(cfg)
	if cfg.Egress == nil {
		got.BaselineHosts = egressBaselineState(existing).BaselineHosts
	}
	want := egressBaselineState(existing)
	return !slices.Equal(got.BaselineHosts, want.BaselineHosts) || !slices.Equal(got.InternalHostSuffixes, want.InternalHostSuffixes)
}
