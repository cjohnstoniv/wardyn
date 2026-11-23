// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"cmp"
	"context"
	"log/slog"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/policyref"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// GET /api/v1/runs/{id}/walls serves one run's walls to the security tier and to the run's owner: the layers that
// set its served policy, the ways out that policy allows, the entries struck from it and, when the re-fold verifies
// it, the layer that struck each, its grants, holds and doors, its counters over the last 15 minutes, and where each
// way out leaves. Anyone else, a unit admin of the run's unit included, gets the missing run's 404. The owner's
// projection names no restriction: a restriction reads as "policy".

// The bounds of one run walls read.
const (
	runWallsWindow    = 15 * time.Minute
	runWallsPerMin    = 30  // the run limiter's rate, for Retry-After
	runWallsMaxStruck = 256 // struck entries on one page, and the default page
	runWallsMaxHolds  = 256
)

// The two projections, and the owner's name for a restriction layer.
const (
	runWallsSecurity    = "security"
	runWallsOwner       = "owner"
	runWallsPolicyLayer = "policy"
)

// runWallsActions are the decision rows the run's counters count.
var runWallsActions = []string{"egress.allow", "egress.deny", "egress.hold"}

// runWallsOwnEvidence are the explain causes whose own records say what struck the entry, so they are never
// re-folded: overlay is the run's own settings, and the rest each have a run.create or launch row.
var runWallsOwnEvidence = map[string]bool{
	causeOverlay: true, causeWalls: true, causeWallPreset: true, causeGitBroker: true, causeMirror: true, causeRestart: true,
}

// handleRunWalls is GET /api/v1/runs/{id}/walls?cursor=&limit=256. cursor and limit page struck; exits, gates and
// inner go on the first page only.
func (s *Server) handleRunWalls(w http.ResponseWriter, r *http.Request) {
	id, ok := parseIDParam(w, r, "id", "run")
	if !ok {
		return
	}
	run, ok := s.getRunAuthorizedBy(w, r, id, s.ownsRunOrAdmin)
	if !ok {
		return
	}
	_, _, limiter := s.wallsLimiters()
	if s.refuseWallsRate(w, r, limiter, runWallsPerMin) {
		return
	}
	reader, ok := s.cfg.Store.(store.WallsAuditReader)
	if !ok {
		writeErrorReason(w, http.StatusNotImplemented, reasonWallsViewUnavailable, "this server's store cannot serve the Walls view")
		return
	}
	offset, limit, ok := runWallsPageOf(r.URL.Query())
	if !ok {
		writeErrorReason(w, http.StatusBadRequest, reasonWallsQueryInvalid, "limit must be from 1 to 256, and cursor one this route returned")
		return
	}
	ctx := r.Context()
	owner := !s.isSecurityOperator(ctx)
	body := runWallsBody(run, owner)
	if !owner {
		s.runWallsSecurityItem(r, &body.Run, run)
	}
	body.Run.Egress = s.wallsEgressOf(ctx, []types.AgentRun{run})[run.ID]

	pe, err := s.readRunPolicyEvidence(ctx, run.ID)
	if err != nil {
		writeServerError(w, r, "read run policy", err)
		return
	}
	envelope, _, found, err := s.runEffectivePolicy(ctx, run.ID)
	if err != nil {
		writeServerError(w, r, "read run policy", err)
		return
	}
	var (
		ev      auditEvidence
		served  types.RunPolicySpec
		strikes []wallsStrike
	)
	hidden := pe.Create.Source != nil && personalPresetHidden(r, run, pe.Create.Source.WallPreset)
	if found {
		if ev, served, err = s.runPolicyServed(ctx, run, pe, envelope); err != nil {
			writeServerError(w, r, "read run restarts", err)
			return
		}
		body.Layers = runWallsLayers(envelope.Provenance, pe.Create, hidden, owner)
		strikes = s.runWallsStrikes(ctx, run, pe.Create, served, ev, hidden)
	}
	// The spec as GET /runs/{id}/policy serves it to this caller; nothing else of it is served.
	out := redactSpecForRead(served, s.specReadTierFor(ctx))
	if found {
		body.Paths = runWallsPaths(out)
	}
	body.Doors = runWallsDoors(out)
	end := min(offset+limit, len(strikes))
	if offset < end {
		body.Struck = s.runWallsStruck(ctx, run, envelope.Provenance, strikes[offset:end], owner)
	}
	if end < len(strikes) {
		body.NextCursor = strconv.Itoa(end)
	}

	if body.Grants, err = s.runWallsGrants(ctx, run.ID); err != nil {
		writeServerError(w, r, "read run grants", err)
		return
	}
	if body.Holds, err = s.runWallsHolds(ctx, run.ID); err != nil {
		writeServerError(w, r, "read run approvals", err)
		return
	}
	rows, partial, err := runWallsReadRows(ctx, reader, run.ID, s.cfg.Now().Add(-runWallsWindow), s.runWallsCaps())
	if err != nil {
		writeServerError(w, r, "read run walls rows", err)
		return
	}
	body.Counters, body.Partial = runWallsCount(rows), partial
	if offset == 0 {
		body.Exits, body.Gates, body.Inner = s.wallsRunExits(ctx, run, out, rows, len(body.Grants), s.isOperator(ctx), owner)
	}
	writeJSON(w, http.StatusOK, body)
}

// runWallsPageOf reads cursor and limit: limit from 1 to runWallsMaxStruck (the default), and cursor a struck
// offset this route returned, absent on the first page. ok is false for any other value.
func runWallsPageOf(q url.Values) (offset, limit int, ok bool) {
	limit = runWallsMaxStruck
	if raw := q.Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > runWallsMaxStruck {
			return 0, 0, false
		}
		limit = n
	}
	if raw := q.Get("cursor"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 {
			return 0, 0, false
		}
		offset = n
	}
	return offset, limit, true
}

// runWallsBody is the answer's skeleton: the run item every projection gets, and every list empty, never null.
func runWallsBody(run types.AgentRun, owner bool) RunWalls {
	body := RunWalls{Projection: runWallsSecurity, Layers: []RunWallsLayer{}, Paths: []RunWallsPath{}, Struck: []RunWallsStruck{},
		Grants: []RunWallsGrant{}, Holds: []RunWallsHold{}, Doors: []RunWallsDoor{}}
	if owner {
		body.Projection = runWallsOwner
	}
	body.Run = WallsRunItem{ID: run.ID, UnitID: run.UnitID, State: run.State, ConfinementClass: run.ConfinementClass,
		Placement: run.Placement, RunnerID: run.RunnerID, RunnerPoolID: run.RunnerPoolID, MachineKind: run.MachineKind,
		CreatedBy: run.CreatedBy, CreatedAt: run.CreatedAt}
	return body
}

// runWallsSecurityItem adds the run item's security-only fields: the restrictions the run captured, its attention
// (as GET /runs serves an admin), and the owner kind of the claimed runner hosting it. A failed read leaves its
// field out.
func (s *Server) runWallsSecurityItem(r *http.Request, item *WallsRunItem, run types.AgentRun) {
	item.RestrictionIDs = run.RestrictionIDs
	runs := []types.AgentRun{run}
	if err := s.projectAttention(r, runs, true); err != nil {
		slog.WarnContext(r.Context(), "wardyn: the Walls view could not read the run's attention; the run item omits it", slog.Any("err", err))
	} else {
		item.Attention = runs[0].Attention
	}
	if rg, ok := s.cfg.Store.(runnerGetter); ok && run.RunnerID != nil {
		if rn, err := rg.GetRunner(r.Context(), *run.RunnerID); err == nil && rn.State == types.RunnerClaimed {
			item.OwnerKind = rn.OwnerKind
		}
	}
}

// runWallsLayers is the run wall's bands: one per layer the enforced envelope's provenance names, with the knobs it
// set and those it locked. The owner reads each restriction as "policy" with no name, a personal preset hidden from
// this caller has no name, and a shown preset carries the name its run.create row records.
func runWallsLayers(prov knobProvenance, create createFacts, hidden, owner bool) []RunWallsLayer {
	type key struct{ layer, name string }
	byKey := map[key]*RunWallsLayer{}
	for knob, rec := range prov {
		k := key{layer: rec.Layer, name: rec.Name}
		switch {
		case k.layer == legacyLayerProfile:
			k.layer = policyref.LayerUnit
		case k.layer == policyref.LayerRestriction && owner:
			k = key{layer: runWallsPolicyLayer}
		case k.layer == policyref.LayerWallPreset && hidden:
			k.name = ""
		case k.layer == policyref.LayerWallPreset && create.Source != nil && create.Source.WallPreset != nil:
			k.name = create.Source.WallPreset.Name
		}
		l := byKey[k]
		if l == nil {
			l = &RunWallsLayer{Layer: k.layer, Name: k.name, Knobs: []string{}}
			byKey[k] = l
		}
		l.Knobs = append(l.Knobs, knob)
		if rec.Locked {
			l.Locked = append(l.Locked, knob)
		}
	}
	out := make([]RunWallsLayer, 0, len(byKey))
	for _, l := range byKey {
		slices.Sort(l.Knobs)
		slices.Sort(l.Locked)
		out = append(out, *l)
	}
	slices.SortFunc(out, func(a, b RunWallsLayer) int {
		return cmp.Or(cmp.Compare(a.Layer, b.Layer), cmp.Compare(a.Name, b.Name))
	})
	return out
}

// runWallsPaths is every way out spec allows: "*" first when it allows all egress, then each allowlist entry as
// written.
func runWallsPaths(spec types.RunPolicySpec) []RunWallsPath {
	out := make([]RunWallsPath, 0, len(spec.AllowedDomains)+1)
	if spec.AllowAllEgress {
		out = append(out, RunWallsPath{Entry: "*", Kind: "allow_all"})
	}
	for _, e := range spec.AllowedDomains {
		kind := "host"
		if strings.HasPrefix(e, "*.") {
			kind = "wildcard"
		}
		out = append(out, RunWallsPath{Entry: e, Kind: kind})
	}
	return out
}

// runWallsDoors is every way in spec declares: its UI apps, each with no path and no callers.
func runWallsDoors(spec types.RunPolicySpec) []RunWallsDoor {
	out := make([]RunWallsDoor, 0, len(spec.UIApps))
	for _, a := range spec.UIApps {
		out = append(out, RunWallsDoor{Kind: "ui_app", Name: a.Name, Port: a.Port, Inbound: a.Inbound})
	}
	return out
}

// runWallsStrikes is every entry the run's policy removed from allowed_domains or added to denied_domains, with its
// explain cause, in the order GET /runs/{id}/policy lists its changes. The base and the site config are read as
// that view reads them, and a personal preset hidden from this caller keeps its entries out, as that view does.
func (s *Server) runWallsStrikes(ctx context.Context, run types.AgentRun, create createFacts, served types.RunPolicySpec, ev auditEvidence, hidden bool) []wallsStrike {
	var base *types.RunPolicySpec
	if create.Source != nil {
		base = &create.Source.Spec
	}
	sc, err := s.cfg.Store.GetSiteConfig(ctx)
	if err != nil {
		slog.WarnContext(ctx, "api: run walls without the site config", "run_id", run.ID, "err", err)
	}
	var out []wallsStrike
	for _, c := range explainRunPolicy(base, served, ev, sc, run) {
		if hidden && c.Cause == causeWallPreset {
			continue
		}
		entries := c.Removed
		switch c.Field {
		case fieldAllowed:
		case fieldDenied:
			entries = c.Added
		default:
			continue
		}
		for _, e := range entries {
			out = append(out, wallsStrike{Field: c.Field, Entry: e, Cause: c.Cause})
		}
	}
	return out
}

// runWallsStruck is page's strikes on the wire. A limits strike, and a deny-list strike whose cause has no record of
// its own, is re-folded (wallsRefold), and removed_by is set only when the re-fold verified its remover. The owner
// reads a restriction remover as "policy" with no name.
func (s *Server) runWallsStruck(ctx context.Context, run types.AgentRun, prov knobProvenance, page []wallsStrike, owner bool) []RunWallsStruck {
	out := make([]RunWallsStruck, len(page))
	var refold []wallsStrike
	var at []int
	for i, st := range page {
		out[i] = RunWallsStruck{Entry: st.Entry, Field: st.Field, Cause: st.Cause}
		if st.Cause == causeLimits || st.Field == fieldDenied && !runWallsOwnEvidence[st.Cause] {
			refold = append(refold, st)
			at = append(at, i)
		}
	}
	if len(refold) == 0 {
		return out
	}
	setter := prov[fieldAllowed]
	if setter.Layer == legacyLayerProfile {
		setter.Layer = policyref.LayerUnit // as the policy view serves the record (knobProvenance.withContacts)
	}
	for j, rm := range s.wallsRefold(ctx, run, prov, setter, refold) {
		if !rm.Verified {
			continue
		}
		by := &RunWallsRemovedBy{Layer: rm.Layer, Name: rm.Name, Verified: true}
		if owner && rm.Layer == policyref.LayerRestriction {
			by = &RunWallsRemovedBy{Layer: runWallsPolicyLayer, Verified: true}
		}
		out[at[j]].RemovedBy = by
	}
	return out
}

// runWallsGrants is each of the run's grants with whether its value is inside the sandbox (grantResidency). The
// broker's repos are not recorded on the run, so a github_token reads as the broker being off. Re-signing at the proxy
// is passed as the server-wide switch (gatewayBedrockSSOAvailable), since the grant row does not say whether this run's
// AWS sign-in was re-signed: off, the AWS session reads as inside; on, its residency is unknown and omitted.
func (s *Server) runWallsGrants(ctx context.Context, runID uuid.UUID) ([]RunWallsGrant, error) {
	rows, err := s.cfg.Store.ListGrantsByRun(ctx, runID)
	if err != nil {
		return nil, err
	}
	sigv4AtProxy := s.gatewayBedrockSSOAvailable()
	out := make([]RunWallsGrant, 0, len(rows))
	for _, g := range rows {
		item := RunWallsGrant{ID: g.ID, Kind: g.Spec.Kind}
		if inside, known := grantResidency(g.Spec, false, nil, sigv4AtProxy); known {
			item.InsideSandbox = &inside
		}
		out = append(out, item)
	}
	return out, nil
}

// runWallsHolds is the run's pending approvals, read as GET /approvals?run_id= reads them, at most
// runWallsMaxHolds. A server with no approval service has none.
func (s *Server) runWallsHolds(ctx context.Context, runID uuid.UUID) ([]RunWallsHold, error) {
	out := []RunWallsHold{}
	if s.cfg.Approvals == nil {
		return out, nil
	}
	var rows []types.ApprovalRequest
	var err error
	if pager, ok := s.cfg.Approvals.(store.ApprovalsByRunPager); ok {
		rows, err = pager.ListApprovalsPageByRun(ctx, runID, types.ApprovalPending, store.Page{Limit: runWallsMaxHolds})
	} else {
		rows, err = s.cfg.Approvals.List(ctx, types.ApprovalPending)
	}
	if err != nil {
		return nil, err
	}
	for _, ap := range rows {
		if ap.RunID != runID || ap.State != types.ApprovalPending || len(out) == runWallsMaxHolds {
			continue
		}
		out = append(out, RunWallsHold{ID: ap.ID, Kind: string(ap.Kind), RequestedAt: ap.RequestedAt})
	}
	return out, nil
}

// runWallsCaps is s.wallsCaps' two run bounds, each zero one taken from the defaults.
func (s *Server) runWallsCaps() wallsCaps {
	c := wallsDefaultCaps()
	if s.wallsCaps.readLimit > 0 {
		c.readLimit = s.wallsCaps.readLimit
	}
	if s.wallsCaps.runReads > 0 {
		c.runReads = s.wallsCaps.runReads
	}
	return c
}

// runWallsReadRows reads the run's decision rows recorded since since, caps.readLimit a read, until a read returns
// the cursor it was sent (nothing left to scan) or after caps.runReads reads. Only then does a probe of one row from
// the last cursor decide partial: set when it scans a row, or when it fails; unset when the rows ended exactly at the
// cap.
func runWallsReadRows(ctx context.Context, reader store.WallsAuditReader, runID uuid.UUID, since time.Time, caps wallsCaps) ([]store.DecisionRow, bool, error) {
	q := store.DecisionQuery{Actions: runWallsActions, Since: since, RunIDs: []uuid.UUID{runID}, Limit: caps.readLimit}
	var rows []store.DecisionRow
	for range caps.runReads {
		got, next, err := reader.AuditWallsRows(ctx, q)
		if err != nil {
			return nil, false, err
		}
		rows = append(rows, got...)
		if next == q.After {
			return rows, false, nil
		}
		q.After = next
	}
	q.Limit = 1
	_, next, err := reader.AuditWallsRows(ctx, q)
	return rows, err != nil || next != q.After, nil
}

// runWallsCount is rows' counters, each row by its weight under its wallsClassify class. A late row counts in late
// only, and guest is the part of refused a guest VM's flows make up.
func runWallsCount(rows []store.DecisionRow) WallsCounters {
	var c WallsCounters
	for _, row := range rows {
		v, n := wallsClassify(row), row.Weight
		if v.Late {
			c.Late += n
			continue
		}
		switch v.Class {
		case wallsAllowed:
			c.Allowed += n
		case wallsRefused:
			c.Refused += n
			if v.Guest {
				c.Guest += n
			}
		case wallsHeld:
			c.Held += n
		case wallsFault:
			c.Fault += n
		case wallsFidelity:
			c.Fidelity += n
		case wallsToolRule:
			c.ToolRule += n
		case wallsControl:
			c.Control += n
		case wallsConfig:
			c.Config += n
		default:
			c.Unclassified += n
		}
	}
	return c
}
