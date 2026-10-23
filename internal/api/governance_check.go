// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// POST /governance/check: which unit binds a principal carrying some claims.
package api

import (
	"cmp"
	"context"
	"fmt"
	"net/http"
	"slices"
	"strings"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/policyref"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// POST /governance/check

// maxGovernancePreviewClaims bounds ONE preview's claim lists. The body cap
// (decodeStrict's maxJSONBody) already bounds the request, but a 1 MiB body of
// one-character claims is ~250k array elements handed to `= ANY($1::text[])`
// and to the dedupe below — so the list gets its own ceiling, well past the few
// dozen claims a real token carries.
const maxGovernancePreviewClaims = 256

// governancePreviewRequest is POST /governance/check's body: the SAME two
// claim lists ListMembershipsFor itself takes, so this endpoint has
// nothing to derive and nothing to re-order.
//
// The ambiguity, and how this endpoint treats it. The console's preview field
// is one kind-LESS claims box (the People step's own, PREVIEW.FIELD_CLAIMS) —
// a pasted line may be a sign-in subject, an email, or a group, and the wire
// cannot tell. So the console sends EVERY typed claim in BOTH lists, and this
// endpoint offers each to both tiers exactly as given. That is the same shape
// POST /access/preview already takes ({roles: lines, groups: lines}), and it is
// honest because the response says which tier matched: matched_tier names the
// row that won, which is what the console renders ("matched by a group
// assignment"). An answer that could only have come from a group assignment
// says so, and one that could only have come from a user assignment says that.
//
// What this endpoint deliberately does NOT do is inspect the SHAPE of a claim.
// There is no "@ makes it an email" rule here, because that would be a second
// opinion about identity living beside the resolver's. Within the user tier the
// resolver ranks by array_position — the caller's own ordering IS the
// precedence — so the console sends user_subjects in the order capabilitySubjects
// would build them (sign-in subject before email), and the ranking stays the
// SQL's.
type governancePreviewRequest struct {
	UserSubjects []string `json:"user_subjects,omitempty"`
	Groups       []string `json:"groups,omitempty"`
	// UserType is the previewed person's type id; "" previews no type tier.
	UserType string `json:"user_type,omitempty"`
	// RunIn names one of the person's units (by id or exact name) to answer effective for, as POST
	// /runs' run_in does; "" answers it for the home context.
	RunIn string `json:"run_in,omitempty"`
}

// governancePreviewResponse names the profile that would bind a principal
// presenting those claims, and the TIER of the assignment that won.
//
// Every field is omitempty, so the answer for "no assignment matched" names
// no unit (the check adds contexts, dormant and effective to every answer; a
// person no membership places has the Default unit as their home context).
// That is the same additive/absent doctrine
// defaultPolicyResponse.GovernanceUnitName follows: an
// absent key already decodes as "no profile" in the TS mirror, while "" would
// be a value the console then has to special-case.
//
// ProfileID uses `omitzero`, not `omitempty`: uuid.UUID is an ARRAY type, and
// omitempty never elides one — the nil uuid would ship as "00000000-…" on
// exactly the answer that has no profile.
type governancePreviewResponse struct {
	UnitID      uuid.UUID                   `json:"unit_id,omitzero"`
	UnitName    string                      `json:"unit_name,omitempty"`
	MatchedTier types.CapabilitySubjectType `json:"matched_tier,omitempty"`
}

// handleGovernanceCheck answers "which unit would bind a principal
// carrying these claims, with no unit asked for" by running THE selection — the identical
// selectContext call effectiveCeiling makes on the enforcement path (the home context).
// securityOps (routes.go).
//
// That single call is the whole point: the ranking order — tier,
// sub-over-email, priority DESC, name ASC — must not be re-implemented
// anywhere else. A
// second implementation of the precedence rule is a second implementation of
// the answer, and a preview that drifts from enforcement is worse than no
// preview: it is confidently wrong at the moment an admin is deciding whether a
// ceiling is right. So there is no ORDER BY in Go here and none in TS; the
// ranking exists once, as the indexed read in internal/store/governance.go.
//
// Not audited, for the reason handleDirectorySearch states about searches: this
// is typed into, mints nothing and changes nothing, and a row per keystroke
// would turn the append-only log into a record of every claim an admin tried.
//
// ErrNotFound is a RESULT, not a failure — the absent-row doctrine, answered as
// an answer that names no unit. Only a real store failure is a 500, and it must be: a
// preview that silently reported "no assignment matches" on a database hiccup
// would tell an admin their profile does not bind someone it does.
func (s *Server) handleGovernanceCheck(w http.ResponseWriter, r *http.Request) {
	var req governancePreviewRequest
	if !decodeStrict(w, r, &req) {
		return
	}
	users, msg := normalizeGovernancePreviewClaims(req.UserSubjects, "user_subjects")
	if msg != "" {
		writeErrorReason(w, http.StatusBadRequest, reasonGovernancePreviewClaimsInvalid, msg)
		return
	}
	groups, msg := normalizeGovernancePreviewGroups(req.Groups)
	if msg != "" {
		writeErrorReason(w, http.StatusBadRequest, reasonGovernancePreviewClaimsInvalid, msg)
		return
	}
	q := contextQuery{users: users, groups: groups, userType: strings.TrimSpace(req.UserType)}
	var scope map[uuid.UUID]bool // a unit admin's: the units of the person's memberships they govern
	if s.isUnitAdminCaller(r.Context()) {
		var ok bool
		if scope, ok = s.checkScope(w, r, q); !ok {
			return
		}
	}
	pick, err := s.selectContext(r.Context(), q, contextChoice{})
	if err != nil {
		writeServerError(w, r, "resolve governance profile", err)
		return
	}
	var resp governanceCheckResponse
	if !pick.isDefault() && pick.row != nil {
		resp.governancePreviewResponse = governancePreviewResponse{
			UnitID: pick.unit.ID, UnitName: pick.unit.Name, MatchedTier: pick.row.Membership.SubjectType,
		}
	}
	if resp.Contexts, resp.Dormant, err = s.checkContexts(r.Context(), q, pick); err != nil {
		writeServerError(w, r, "list governance contexts", err)
		return
	}
	if resp.Restrictions, err = s.checkRestrictions(r.Context(), q); err == nil {
		resp.Workspaces, err = s.checkWorkspaces(r.Context(), q)
	}
	if err != nil {
		writeServerError(w, r, "list restrictions and workspaces", err)
		return
	}
	runIn, err := s.runInFromRequest(r.Context(), createRunRequest{RunIn: req.RunIn})
	if err == nil {
		resp.Effective, err = s.checkEffective(r.Context(), q, contextChoice{RunIn: runIn})
	}
	if err != nil {
		ceilingRunRefusal(err).write(s, w, r)
		return
	}
	if scope != nil {
		resp = resp.within(scope, runIn, pick)
	}
	writeJSON(w, http.StatusOK, resp)
}

// checkScope is a unit admin's scope over a check: the person the claims describe holds a membership
// in a unit the caller governs. It answers the in-scope units among the person's memberships, or
// writes the refusal (ok false).
func (s *Server) checkScope(w http.ResponseWriter, r *http.Request, q contextQuery) (map[uuid.UUID]bool, bool) {
	ctx := r.Context()
	ms, err := s.cfg.Store.ListMembershipsFor(ctx, q.users, q.groups, q.userType)
	if err != nil {
		writeServerError(w, r, "read unit scope", err)
		return nil, false
	}
	scope := map[uuid.UUID]bool{}
	for _, m := range ms {
		in, err := s.unitScope(ctx, m.Unit.ID, false)
		if err != nil {
			writeServerError(w, r, "read unit scope", err)
			return nil, false
		}
		if in {
			scope[m.Unit.ID] = true
		}
	}
	if len(scope) == 0 {
		s.refuseOutsideScope(w, r, "governance.check")
		return nil, false
	}
	return scope, true
}

// within is a check's answer as a unit admin may read it: the contexts and dormant memberships in the
// units they govern, the home context's fields only when it is one of those, and the effective walls
// only of a context that is.
func (c governanceCheckResponse) within(scope map[uuid.UUID]bool, runIn uuid.UUID, home contextPick) governanceCheckResponse {
	if !scope[c.UnitID] {
		c.governancePreviewResponse = governancePreviewResponse{}
	}
	c.Contexts = slices.DeleteFunc(c.Contexts, func(x checkContext) bool { return !scope[x.UnitID] })
	c.Dormant = slices.DeleteFunc(c.Dormant, func(x checkDormant) bool { return !scope[x.UnitID] })
	if asked := cmp.Or(runIn, home.unit.ID); !scope[asked] {
		c.Effective = nil
	}
	return c
}

// governanceCheckResponse is POST /governance/check's body: the home context's fields above (empty
// when it is the Default unit), every context of the person, their dormant memberships, and the
// effective walls of the context asked for (run_in) or the home one.
type governanceCheckResponse struct {
	governancePreviewResponse
	Contexts  []checkContext  `json:"contexts"`
	Dormant   []checkDormant  `json:"dormant"`
	Effective *checkEffective `json:"effective"`
	// Restrictions are the deny-only lists that cover the person, in every unit they run in.
	Restrictions []checkRestriction `json:"restrictions,omitempty"`
	// Workspaces are the workspaces the person owns, grouped by the unit each is pinned to (a nil
	// unit_id is the group pinned to none).
	Workspaces []checkWorkspaceGroup `json:"workspaces,omitempty"`
}

// checkRestriction is one restriction covering the person.
type checkRestriction struct {
	ID      uuid.UUID               `json:"id"`
	Name    string                  `json:"name"`
	Spec    types.RestrictionSpec   `json:"spec"`
	Limits  types.RestrictionLimits `json:"limits"`
	Contact *policyref.Contact      `json:"contact,omitempty"`
}

// checkWorkspaceGroup is the person's workspaces pinned to one unit.
type checkWorkspaceGroup struct {
	UnitID   *uuid.UUID `json:"unit_id"`
	UnitName string     `json:"unit_name,omitempty"`
	Count    int        `json:"count"`
	Names    []string   `json:"names"`
}

// unitPathLink is one unit of a path.
type unitPathLink struct {
	ID   uuid.UUID `json:"id"`
	Name string    `json:"name"`
}

// checkContext is one unit the person may run in. Home marks the one a run that names none runs in;
// Path is the unit and those above it, leaf first.
type checkContext struct {
	UnitID   uuid.UUID      `json:"unit_id"`
	UnitName string         `json:"unit_name"`
	Home     bool           `json:"home"`
	Legacy   bool           `json:"legacy"`
	Path     []unitPathLink `json:"path"`
}

// checkDormant is a legacy membership another legacy membership outranks for this person: it places
// them nowhere until promoted or until the one above it goes.
type checkDormant struct {
	MembershipID uuid.UUID `json:"membership_id"`
	UnitID       uuid.UUID `json:"unit_id"`
	UnitName     string    `json:"unit_name"`
}

// checkEffective is one context's resolved walls, after the Wardyn boundary, and which layer set each
// knob.
type checkEffective struct {
	Ceiling    types.RunPolicySpec    `json:"ceiling"`
	Limits     types.GovernanceLimits `json:"limits"`
	Provenance knobProvenance         `json:"provenance"`
}

// checkContexts lists q's contexts and dormant memberships from the rows the selection reads
// (activeMemberships): the Default unit alone when none places the person. home is the selection with
// no choice.
func (s *Server) checkContexts(ctx context.Context, q contextQuery, home contextPick) ([]checkContext, []checkDormant, error) {
	ms, err := s.cfg.Store.ListMembershipsFor(ctx, q.users, q.groups, q.userType)
	if err != nil {
		return nil, nil, err
	}
	units, err := s.unitOutlines(ctx)
	if err != nil {
		return nil, nil, err
	}
	byID := unitsByID(units)
	active := activeMemberships(ms)
	contexts := []checkContext{}
	add := func(id uuid.UUID, legacy bool) {
		if !slices.ContainsFunc(contexts, func(c checkContext) bool { return c.UnitID == id }) {
			contexts = append(contexts, checkContext{UnitID: id, UnitName: byID[id].Name, Home: id == home.unit.ID, Legacy: legacy, Path: unitPath(byID, id)})
		}
	}
	for _, m := range active {
		add(m.Unit.ID, m.Membership.Legacy)
	}
	if len(active) == 0 {
		add(store.DefaultUnitID, false)
	}
	dormant := []checkDormant{}
	for _, m := range ms {
		if !slices.ContainsFunc(active, func(a store.MembershipMatch) bool { return a.Membership.ID == m.Membership.ID }) {
			dormant = append(dormant, checkDormant{MembershipID: m.Membership.ID, UnitID: m.Unit.ID, UnitName: m.Unit.Name})
		}
	}
	return contexts, dormant, nil
}

// unitPath is id and the units above it, leaf first, bounded as the stored depth is.
func unitPath(byID map[uuid.UUID]types.GovernanceUnit, id uuid.UUID) []unitPathLink {
	path := []unitPathLink{}
	for cur := &id; cur != nil && len(path) <= store.MaxUnitDepthCap; {
		row, ok := byID[*cur]
		if !ok {
			break
		}
		path = append(path, unitPathLink{ID: row.ID, Name: row.Name})
		cur = row.ParentID
	}
	return path
}

// checkEffective resolves q's context under choice by the enforcement path's own steps: the context
// ceiling (contextCeiling, the body of resolveContext) and then the boundary. Provenance records the
// deployment, then the context's unit on each knob it changed, then the boundary, and marks a knob a
// lock above fixes, as a run's provenance does.
func (s *Server) checkEffective(ctx context.Context, q contextQuery, choice contextChoice) (*checkEffective, error) {
	deployment := governanceCeiling{Spec: s.cfg.DefaultPolicy.Clone(), MemberDefault: s.memberDefaultFor(s.cfg.DefaultPolicy.Resources)}
	base, err := s.contextCeilingBelowRestrictions(ctx, q, choice, deployment)
	if err != nil {
		return nil, err
	}
	c, err := s.applyRestrictions(ctx, q, base)
	if err != nil {
		return nil, err
	}
	prov := startProvenance(deploymentProvenance)
	prov.setBy(base.provenance(), deployment.Spec, base.Spec)
	for name, by := range c.restrictionKnobs {
		prov[name] = by
	}
	bounded, err := s.boundaryCeiling(ctx, c)
	if err != nil {
		return nil, err
	}
	prov.setBy(boundaryProvenance, c.Spec, bounded.Spec)
	prov.markLocked(bounded)
	return &checkEffective{Ceiling: redactSpecForRead(bounded.Spec, s.specReadTierFor(ctx)), Limits: bounded.Limits, Provenance: prov}, nil
}

// normalizeGovernancePreviewClaims lowercases, trims and de-duplicates one
// claim list, preserving ORDER — which matters, because order is the user
// tier's tie-break (array_position).
//
// User subjects only. The normalization is not a nicety: assignments are stored
// lowercased (validateGovernanceMembership) and the enforcement path's user
// input arrives already folded — capabilitySubjects lowercases the sub and the
// email. A preview that skipped it would answer "no assignment matches" for a
// claim typed `Eng` against a row the real run matches, which is the drift this
// endpoint exists to remove.
//
// Neither half is a plain ToLower. It is the whole rule for an ASCII subject only: strings.ToLower is
// a UNICODE fold, so U+212A becomes ASCII 'k' and U+0130 becomes 'i', and a
// preview claim typed in either spelling would answer for a DIFFERENT human's
// row than the run resolves. Both halves therefore ask the match surface's own
// canonicalizer — users through types.CanonicalUserSubject (the same function
// capabilitySubjects and the two write boundaries use), groups through
// normalizeGovernancePreviewGroups below (types.CanonicalGroupSubject).
func normalizeGovernancePreviewClaims(in []string, field string) ([]string, string) {
	if len(in) > maxGovernancePreviewClaims {
		return nil, fmt.Sprintf("%s: at most %d claims", field, maxGovernancePreviewClaims)
	}
	out := make([]string, 0, len(in))
	for _, v := range in {
		if c := types.CanonicalUserSubject(v); c != "" && !slices.Contains(out, c) {
			out = append(out, c)
		}
	}
	return out, ""
}

// normalizeGovernancePreviewGroups is the GROUP half, and it calls the snapshot's
// own normalizer rather than restating it.
//
// The divergence this closes. The preview answers one question — "which profile
// would bind a principal presenting this claim" — and there has to be one
// answer. A plain ToLower with no ASCII guard FOLDS: U+212A KELVIN SIGN becomes
// ASCII 'k', so a crafted "Kubernetes-admins" normalized onto the real,
// operator-authored group "kubernetes-admins" and the endpoint reported that the
// profile BINDS it. The enforcement path refuses the same claim outright
// (CanonicalGroupSubject ok=false), drops it from the snapshot and stamps the
// snapshot truncated. Preview said yes; enforcement said no.
//
// Dropped, not refused with a 400, because dropping is exactly what the
// enforcement path does with the same claim — a preview that 400s where a login
// silently drops would be a second, different answer rather than the same one.
// The count cap and the de-duplication stay identical to the user half.
//
// Residual, named rather than hidden: enforcement ALSO stamps the snapshot
// truncated when it drops a group, and a truncated snapshot makes
// effectiveCeiling answer errGroupsSnapshotStale (403) for that member whenever
// a group-tier assignment exists. The preview has no field for "and this claim
// would make your snapshot incomplete", so it reports the dropped claim as
// simply unmatched. That is strictly closer to enforcement than the fold was,
// and the gap is filed rather than invented as a new response field here.
func normalizeGovernancePreviewGroups(in []string) ([]string, string) {
	if len(in) > maxGovernancePreviewClaims {
		return nil, fmt.Sprintf("groups: at most %d claims", maxGovernancePreviewClaims)
	}
	out := make([]string, 0, len(in))
	for _, v := range in {
		c, ok := types.CanonicalGroupSubject(v)
		if !ok {
			continue // no login snapshot can carry it; enforcement drops it too
		}
		if !slices.Contains(out, c) {
			out = append(out, c)
		}
	}
	return out, ""
}

// checkRestrictions lists the restrictions covering q, in name order.
func (s *Server) checkRestrictions(ctx context.Context, q contextQuery) ([]checkRestriction, error) {
	list, err := s.restrictionsFor(ctx, q)
	if err != nil {
		return nil, err
	}
	out := make([]checkRestriction, 0, len(list))
	for _, r := range list {
		out = append(out, checkRestriction{ID: r.ID, Name: r.Name, Spec: r.Spec, Limits: r.Limits, Contact: r.Contact})
	}
	return out, nil
}

// checkWorkspaces groups the workspaces q's users own by owning unit: the pinned groups in unit-name
// order, then the unpinned one.
func (s *Server) checkWorkspaces(ctx context.Context, q contextQuery) ([]checkWorkspaceGroup, error) {
	out := []checkWorkspaceGroup{}
	if s.cfg.Store == nil || len(q.users) == 0 {
		return out, nil
	}
	all, err := s.cfg.Store.ListWorkspaces(ctx)
	if err != nil {
		return nil, err
	}
	byUnit := map[uuid.UUID]*checkWorkspaceGroup{}
	var unpinned checkWorkspaceGroup
	for _, ws := range all {
		if ws.OwnedBy == "" || !slices.Contains(q.users, ws.OwnedBy) {
			continue
		}
		g := &unpinned
		if ws.OwningUnitID != nil {
			if byUnit[*ws.OwningUnitID] == nil {
				id := *ws.OwningUnitID
				byUnit[id] = &checkWorkspaceGroup{UnitID: &id, UnitName: ws.OwningUnitName}
			}
			g = byUnit[*ws.OwningUnitID]
		}
		g.Count++
		g.Names = append(g.Names, ws.Name)
	}
	for _, g := range byUnit {
		slices.Sort(g.Names)
		out = append(out, *g)
	}
	slices.SortFunc(out, func(a, b checkWorkspaceGroup) int { return strings.Compare(a.UnitName, b.UnitName) })
	if unpinned.Count > 0 {
		slices.Sort(unpinned.Names)
		out = append(out, unpinned)
	}
	return out, nil
}
