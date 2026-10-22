// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// The run context (the policy tree, 0.10.0): of the units a person is a member of, ONE governs a
// run, and only its path from the top composes the run's walls. Walls are never blended across
// sibling units. This file picks that unit (selectContext) and composes its path (contextCeiling);
// governance.go's resolveChainCeiling calls it for every governed caller, and the admin check route
// asks the same selection, so a preview and the enforcement path cannot disagree.
package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/authz"
	"github.com/cjohnstoniv/wardyn/internal/policyref"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// contextChoice is what a request says about its context: RunIn names one of the person's units to
// run in (Nil: none), and OwningUnits are the units owning the run's workspaces, each pinning the
// context to itself or a unit below it (none: no pin). A run can attach several workspaces, so it is a
// list; every pin holds, in order.
type contextChoice struct {
	RunIn       uuid.UUID
	OwningUnits []uuid.UUID
}

// key is the choice as the per-request memo keys it: two choices in one request are two resolves.
func (c contextChoice) key() string {
	var b strings.Builder
	b.WriteString(c.RunIn.String())
	for _, o := range c.OwningUnits {
		b.WriteString("/" + o.String())
	}
	return b.String()
}

type contextChoiceKey struct{}

type contextChoiceSlotKey struct{}

// contextChoiceSlot is the request's choice, filled once the request has been decoded.
type contextChoiceSlot struct {
	set    bool
	choice contextChoice
}

// withContextChoice returns ctx carrying c (the queued start and tests).
func withContextChoice(ctx context.Context, c contextChoice) context.Context {
	return context.WithValue(ctx, contextChoiceKey{}, c)
}

// withContextChoiceSlot puts an empty slot on r, so a choice decoded deeper in the handler
// (setContextChoice) reaches every later resolve on r: the create path decodes on a request it
// does not hand back.
func withContextChoiceSlot(r *http.Request) *http.Request {
	return r.WithContext(context.WithValue(r.Context(), contextChoiceSlotKey{}, &contextChoiceSlot{}))
}

// setContextChoice fills ctx's slot; a ctx with none keeps what it carries.
func setContextChoice(ctx context.Context, c contextChoice) {
	if slot, ok := ctx.Value(contextChoiceSlotKey{}).(*contextChoiceSlot); ok {
		slot.choice, slot.set = c, true
	}
}

// contextChoiceFrom is the choice a resolve on ctx answers for: the filled slot, else a value set by
// withContextChoice, else none.
func contextChoiceFrom(ctx context.Context) contextChoice {
	if slot, ok := ctx.Value(contextChoiceSlotKey{}).(*contextChoiceSlot); ok && slot.set {
		return slot.choice
	}
	c, _ := ctx.Value(contextChoiceKey{}).(contextChoice)
	return c
}

// contextChoiceForRequest is a create, preflight or preview request's choice.
func (s *Server) contextChoiceForRequest(ctx context.Context, req createRunRequest) (contextChoice, error) {
	runIn, err := runInFromRequest(req)
	if err != nil {
		return contextChoice{}, err
	}
	owning, err := s.owningUnitFromRequest(ctx, req)
	if err != nil {
		return contextChoice{}, err
	}
	return contextChoice{RunIn: runIn, OwningUnits: owning}, nil
}

// runInFromRequest is the unit the request names to run in; the TREE-API lane fills it.
func runInFromRequest(createRunRequest) (uuid.UUID, error) {
	return uuid.Nil, nil
}

// decodeStampedRunRequest is preflight's decode: the body, its repositories canonicalized, then its
// context choice stamped on the request it returns (false when it wrote a refusal).
func (s *Server) decodeStampedRunRequest(w http.ResponseWriter, r *http.Request) (*http.Request, createRunRequest, bool) {
	var req createRunRequest
	if !s.decodeRunRequest(w, r, &req) {
		return r, req, false
	}
	canonicalizeRunRepos(&req, s.adoHostsLoader(r.Context()))
	r, ok := s.stampContextChoice(w, r, req)
	return r, req, ok
}

// stampContextChoice sets the decoded request's choice on r's slot (the create path) and on the
// request it returns (preflight and preview), or writes the refusal a resolve error would get and
// returns false.
func (s *Server) stampContextChoice(w http.ResponseWriter, r *http.Request, req createRunRequest) (*http.Request, bool) {
	c, err := s.contextChoiceForRequest(r.Context(), req)
	if err != nil {
		ceilingRunRefusal(err).write(s, w, r)
		return r, false
	}
	setContextChoice(r.Context(), c)
	return r.WithContext(withContextChoice(r.Context(), c)), true
}

// owningUnitFromRequest is the units owning the request's workspaces; the RESTRICT-PIN lane fills it.
func (s *Server) owningUnitFromRequest(context.Context, createRunRequest) ([]uuid.UUID, error) {
	return nil, nil
}

// errUnitNotAContext is a RunIn that names no unit of the person's: an unknown unit and a unit the
// person is not a member of are refused alike, so the answer discloses nothing.
var errUnitNotAContext = errors.New("api: the unit to run in is not one of the person's contexts")

// errWorkspaceUnitNotYours is a workspace pin no context of the person's can satisfy: no unit of
// theirs is the owning unit, above it or below it.
type errWorkspaceUnitNotYours struct {
	Unit    uuid.UUID
	name    string
	contact *policyref.Contact
}

func (e *errWorkspaceUnitNotYours) Error() string {
	return fmt.Sprintf("api: the person is not in unit %s or a unit above or below it", e.Unit)
}

const (
	unitNotAContextMsg      = "the unit you asked to run in is not one of your units — name a unit you are a member of, or leave it out to run in your home unit"
	workspaceUnitNotYoursFm = "this workspace belongs to the unit %q, and you are not a member of it or of a unit above or below it — ask its contact for access, or launch without the workspace"
)

// contextRunRefusal maps a context choice the person cannot make to its 403, or nil for any other
// error.
func contextRunRefusal(err error) *runRefusal {
	if errors.Is(err, errUnitNotAContext) {
		return runError(http.StatusForbidden, reasonUnitNotAContext, unitNotAContextMsg)
	}
	var notYours *errWorkspaceUnitNotYours
	if errors.As(err, &notYours) {
		return &runRefusal{status: http.StatusForbidden, body: errorBody{Error: fmt.Sprintf(workspaceUnitNotYoursFm, notYours.name),
			Reason: reasonWorkspaceUnitNotYours, Policy: policyref.Layered(policyref.Project(policyref.SourceUnit, notYours.name, notYours.contact))}}
	}
	return nil
}

// contextQuery is the identity a context is selected for. unusable says the group half cannot be
// evaluated (a stale or truncated snapshot): groups are then not matched at all.
type contextQuery struct {
	users, groups []string
	userType      string
	unusable      bool
}

// contextPick is the selected context: its unit (Default when no membership places the person,
// the deeper unit when a workspace pin moved it) and the membership that put the person there (nil
// for Default).
type contextPick struct {
	unit types.GovernanceUnit
	row  *store.MembershipMatch
}

func (p contextPick) isDefault() bool {
	return p.unit.ID == store.DefaultUnitID || p.unit.Kind == types.UnitKindDefault
}

// resolveContext is the governed caller's ceiling below the boundary (DESIGN-TREE §4.1 steps 2-7
// and 9): their memberships, the one context the request's choice selects, and that context's
// path composed from the top. Step 1, the ungoverned caller, is resolveChainCeiling's.
// Restrictions (step 8) are the RESTRICT-PIN lane's, applied after this.
func (s *Server) resolveContext(ctx context.Context, deployment governanceCeiling) (governanceCeiling, error) {
	subj, err := s.callerSubjects(ctx)
	if err != nil {
		return governanceCeiling{}, err
	}
	// Truncated counts as stale. sessionGroups sorts the snapshot and drops its
	// alphabetically-last entries at the cookie byte cap, so a member in enough groups holds a
	// snapshot that is present, non-nil and INCOMPLETE, and the group whose membership walls them
	// is exactly as likely to be missing as any other.
	q := contextQuery{users: subj.users, groups: subj.groups, userType: subj.userType,
		unusable: subj.stale || oidcGroupsTruncatedFromContext(ctx)}
	return s.contextCeiling(ctx, q, contextChoiceFrom(ctx), deployment)
}

// contextCeiling selects q's context under choice and composes it. Default is the deployment's
// ceiling with no unit (Unit nil iff the context is Default); any other context is its path,
// composed from the top (governance_compose.go) and refused past the configured depth.
func (s *Server) contextCeiling(ctx context.Context, q contextQuery, choice contextChoice, deployment governanceCeiling) (governanceCeiling, error) {
	pick, err := s.selectContext(ctx, q, choice)
	if err != nil {
		return governanceCeiling{}, err
	}
	if pick.isDefault() {
		return deployment, nil
	}
	p, err := s.composeContext(ctx, pick.unit)
	return s.ceilingFromUnit(ctx, p, err, deployment)
}

// composeContext composes unit's path. A unit with no parent is its own path and costs no read.
func (s *Server) composeContext(ctx context.Context, unit types.GovernanceUnit) (*ResolvedUnit, error) {
	chain := []types.GovernanceUnit{unit}
	if unit.ParentID != nil {
		var err error
		if chain, err = s.cfg.Store.GetGovernanceUnitChain(ctx, unit.ID); err != nil {
			// Never ErrNotFound here: a unit a membership names cannot be deleted, and reading
			// "gone" as "no membership" would hand back the deployment ceiling.
			return nil, fmt.Errorf("api: read the governance unit chain: %v", err)
		}
	}
	if err := s.checkConfiguredDepth(ctx, chain); err != nil {
		return nil, err
	}
	return s.composeChain(chain)
}

// selectContext is the selection alone (§4.1 steps 2-5): no composition, so the admin check route
// asks it too. With q.unusable the group half is not matched, and the pick is served only when no
// group-tier membership it cannot see could change it (selectByTier's rule, below).
func (s *Server) selectContext(ctx context.Context, q contextQuery, choice contextChoice) (contextPick, error) {
	if !q.unusable {
		ms, err := s.cfg.Store.ListMembershipsFor(ctx, q.users, q.groups, q.userType)
		if err != nil {
			return contextPick{}, err
		}
		pick, _, err := s.pickContext(ctx, activeMemberships(ms), choice)
		return pick, err
	}
	// A hidden group row only ever ADDS memberships. So a legacy=false row that matched is a sound
	// candidate, and a legacy=true winner only when it is user-tier (a hidden group legacy row could
	// outrank a user_type or all row and leave it dormant). The pick is sound when RunIn names a
	// sound candidate, or when a user-tier row is the only candidate or the user-tier home. A sound
	// pick is handed to selectByTier as a user-tier answer; anything else is refused there when any
	// group-tier membership exists.
	var choiceErr error
	pick, _, err := selectByTier(ctx, s, authz.Deny(authz.ReasonGroupsSnapshotStale, "governance.ceiling", ""),
		func() (contextPick, types.CapabilitySubjectType, error) {
			ms, err := s.cfg.Store.ListMembershipsFor(ctx, q.users, nil, q.userType)
			if err != nil {
				return contextPick{}, "", err
			}
			pick, sound, err := s.pickContext(ctx, activeMemberships(ms), choice)
			if err != nil && contextRunRefusal(err) == nil {
				return contextPick{}, "", err
			}
			if choiceErr = err; err == nil && sound {
				if choice.RunIn == uuid.Nil && !pick.row.Membership.Home {
					// The lone candidate: a hidden group home row would win the home step and move the
					// run, so any group home row anywhere leaves the pick unsound (the stricter answer).
					hidden, err := s.groupHomeExists(ctx)
					if err != nil {
						return contextPick{}, "", err
					}
					if hidden {
						return pick, "", nil
					}
				}
				return pick, types.CapabilitySubjectUser, nil
			}
			return pick, "", nil
		}, s.cfg.Store.HasGroupTierMemberships)
	if err != nil {
		return contextPick{}, err
	}
	return pick, choiceErr
}

// groupHomeExists is whether any group membership row, live or dormant, is a home row.
func (s *Server) groupHomeExists(ctx context.Context) (bool, error) {
	rows, err := s.cfg.Store.ListGovernanceMemberships(ctx)
	if err != nil {
		return false, err
	}
	for _, m := range rows {
		if m.SubjectType == types.CapabilitySubjectGroup && m.Home {
			return true, nil
		}
	}
	return false, nil
}

// activeMemberships is the rows that are contexts (§4.1 step 2): every legacy=false row, plus the
// first legacy=true row, which is today's one assignment. The legacy rows after it are dormant for
// this person.
func activeMemberships(ms []store.MembershipMatch) []store.MembershipMatch {
	var out []store.MembershipMatch
	legacySeen := false
	for _, m := range ms {
		if m.Membership.Legacy {
			if legacySeen {
				continue
			}
			legacySeen = true
		}
		out = append(out, m)
	}
	return out
}

// soundRow is a row a hidden group membership cannot have made dormant.
func soundRow(m *store.MembershipMatch) bool {
	return m != nil && (!m.Membership.Legacy || m.Membership.SubjectType == types.CapabilitySubjectUser)
}

// pickContext is §4.1 steps 3-5 over the active rows, in precedence order. The candidates are the
// rows' units, or Default when there are none. Each workspace pin keeps the candidates on its path,
// mapped to the deeper of the two, and drops the rest. Then RunIn picks (it must be a candidate),
// else the only candidate, else the precedence winner among home rows, else among all. sound
// reports whether a hidden group membership could not have changed the answer (selectContext).
func (s *Server) pickContext(ctx context.Context, active []store.MembershipMatch, choice contextChoice) (contextPick, bool, error) {
	cands := make([]contextPick, 0, max(1, len(active)))
	for i := range active {
		cands = append(cands, contextPick{unit: active[i].Unit, row: &active[i]})
	}
	if len(cands) == 0 {
		cands = append(cands, contextPick{unit: types.GovernanceUnit{ID: store.DefaultUnitID, Kind: types.UnitKindDefault}})
	}
	if len(choice.OwningUnits) > 0 {
		var err error
		if cands, err = s.pinCandidates(ctx, cands, choice.OwningUnits); err != nil {
			return contextPick{}, false, err
		}
	}
	if choice.RunIn != uuid.Nil {
		for _, c := range cands {
			if c.unit.ID == choice.RunIn {
				return c, soundRow(c.row), nil
			}
		}
		return contextPick{}, false, errUnitNotAContext
	}
	userTier := func(c contextPick) bool {
		return c.row != nil && c.row.Membership.SubjectType == types.CapabilitySubjectUser
	}
	if distinctUnits(cands) == 1 {
		return cands[0], userTier(cands[0]), nil
	}
	for _, c := range cands {
		if c.row != nil && c.row.Membership.Home {
			return c, userTier(c), nil
		}
	}
	return cands[0], false, nil
}

func distinctUnits(cands []contextPick) int {
	seen := map[uuid.UUID]bool{}
	for _, c := range cands {
		seen[c.unit.ID] = true
	}
	return len(seen)
}

// pinCandidates is step 4 for each pin in order: a candidate on the pin's path maps to the deeper
// of the two (an ancestor of the pin to the pin, the pin or a unit below it to itself), and an
// off-path candidate drops out. The first pin that leaves none refuses.
func (s *Server) pinCandidates(ctx context.Context, cands []contextPick, pins []uuid.UUID) ([]contextPick, error) {
	rows, err := s.cfg.Store.ListGovernanceUnits(ctx)
	if err != nil {
		return nil, err
	}
	byID := unitsByID(rows)
	for _, o := range pins {
		pin, ok := byID[o]
		if !ok {
			return nil, &errWorkspaceUnitNotYours{Unit: o}
		}
		var kept []contextPick
		for _, c := range cands {
			switch {
			case onPath(byID, o, c.unit.ID):
				c.unit = pin
				kept = append(kept, c)
			case onPath(byID, c.unit.ID, o):
				kept = append(kept, c)
			}
		}
		if len(kept) == 0 {
			return nil, &errWorkspaceUnitNotYours{Unit: o, name: pin.Name, contact: pin.Contact}
		}
		cands = kept
	}
	return cands, nil
}

// onPath reports whether ancestor is id or one of the units above it.
func onPath(byID map[uuid.UUID]types.GovernanceUnit, id, ancestor uuid.UUID) bool {
	cur := &id
	for n := 0; cur != nil && n <= store.MaxUnitDepthCap; n++ {
		if *cur == ancestor {
			return true
		}
		row, ok := byID[*cur]
		if !ok {
			return false
		}
		cur = row.ParentID
	}
	return false
}

// unitDepthLimit is site_config.governance_max_depth: 0 is the default, 16. A site config that
// cannot be read is an error, never the default, so a depth check fails closed.
func (s *Server) unitDepthLimit(ctx context.Context) (int, error) {
	sc, err := s.cfg.Store.GetSiteConfig(ctx)
	return depthLimitOf(sc, err)
}

// unitDepthLimitQ is unitDepthLimit read on q, for a caller inside a transaction.
func unitDepthLimitQ(ctx context.Context, q store.Querier) (int, error) {
	sc, err := store.GetSiteConfigQ(ctx, q)
	return depthLimitOf(sc, err)
}

func depthLimitOf(sc types.SiteConfig, err error) (int, error) {
	if err != nil {
		return 0, fmt.Errorf("api: read the maximum governance unit depth: %w", err)
	}
	return governanceMaxDepthOf(sc.GovernanceMaxDepth), nil
}

// governanceMaxDepthOf is a stored governance_max_depth as a limit: nil and 0 are the default, 16.
func governanceMaxDepthOf(p *int) int {
	if p == nil || *p == 0 {
		return maxUnitDepthDefault
	}
	return *p
}

// checkConfiguredDepth refuses a path deeper than the configured maximum. A path of one unit is
// within every limit, so it reads nothing.
func (s *Server) checkConfiguredDepth(ctx context.Context, chain []types.GovernanceUnit) error {
	if len(chain) <= 1 {
		return nil
	}
	limit, err := s.unitDepthLimit(ctx)
	if err != nil {
		return err
	}
	if len(chain) > limit {
		return errUnitDepth
	}
	return nil
}

// validGovernanceMaxDepth is site_config.governance_max_depth's shape: 0 (the default) or 1..64.
func validGovernanceMaxDepth(p *int) error {
	if p == nil {
		return nil
	}
	n := *p
	if n < 0 || n > store.MaxUnitDepthCap {
		return fmt.Errorf("governance_max_depth: %d is not a depth — use 1 to %d, or 0 for the default (%d)", n, store.MaxUnitDepthCap, maxUnitDepthDefault)
	}
	return nil
}

// admitGovernanceMaxDepth refuses a governance_max_depth below the deepest unit path that exists,
// naming that path (a body that does not name it was carried forward, siteConfigFieldsAfter066).
// False when it answered. An unchanged value needs no read: every unit write is already held to it.
func (s *Server) admitGovernanceMaxDepth(w http.ResponseWriter, r *http.Request, cfg *types.SiteConfig, existing types.SiteConfig) bool {
	limit := governanceMaxDepthOf(cfg.GovernanceMaxDepth)
	if limit == governanceMaxDepthOf(existing.GovernanceMaxDepth) {
		return true
	}
	depth, path, err := s.cfg.Store.MaxUnitDepth(r.Context())
	if err != nil {
		writeServerError(w, r, "read the deepest governance unit path", err)
		return false
	}
	if depth > limit {
		sent := strconv.Itoa(limit)
		if cfg.GovernanceMaxDepth != nil && *cfg.GovernanceMaxDepth == 0 {
			sent = fmt.Sprintf("0 (the default, %d)", limit)
		}
		writeErrorReason(w, http.StatusBadRequest, reasonSiteConfigInvalid, fmt.Sprintf(
			"invalid site config: governance_max_depth: %s is below the deepest unit path, %d deep (%s) — move or remove units first",
			sent, depth, strings.Join(path, " > ")))
		return false
	}
	return true
}

// ownerMembershipRefusal is a revive's or an extension's check that the run's owner still holds a
// membership placing them in p, the unit the run was launched in (B4): a membership of p or of a
// unit above it. The owner's own session re-resolves their memberships in full. Anyone else knows
// the owner only by the sub and the user type stamped on the run, so the check passes while a
// membership row on p's path could still match the owner: a user row naming their sub, a row for
// their stamped type, an `all` row, or any group row (a restart or revive of a group-keyed owner's
// run keeps today's reach). An email-keyed user row never counts here: the run does not record the
// owner's email, so such a row could only admit a guess. The refusal names the arm
// taken (owner_session or row_can_match), which a revive audits.
func (s *Server) ownerMembershipRefusal(ctx context.Context, run types.AgentRun, p *ResolvedUnit) (*ownerRefusal, error) {
	if p == nil {
		return nil, nil
	}
	path := make(map[uuid.UUID]bool, len(p.Chain))
	for _, l := range p.Chain {
		path[l.ID] = true
	}
	arm := "row_can_match"
	found := false
	if sub := oidcHumanFromContext(ctx); sub != "" && sub == run.CreatedBy && !oidcGroupsTruncatedFromContext(ctx) {
		// A stale snapshot or a type deleted since sign-in leaves the session unable to answer in
		// full, so the row arm below decides, as for anyone else.
		subj, err := s.callerSubjects(ctx)
		if err != nil && !errors.Is(err, errUserTypeUnknown) {
			return nil, err
		}
		if err == nil && !subj.stale {
			arm = "owner_session"
			ms, err := s.cfg.Store.ListMembershipsFor(ctx, subj.users, subj.groups, subj.userType)
			if err != nil {
				return nil, err
			}
			for _, m := range activeMemberships(ms) {
				found = found || path[m.Unit.ID]
			}
		}
	}
	if arm == "row_can_match" {
		rows, err := s.cfg.Store.ListGovernanceMemberships(ctx)
		if err != nil {
			return nil, err
		}
		for _, m := range rows {
			found = found || (path[m.UnitID] && membershipCanMatchOwner(m, run))
		}
	}
	if found {
		return nil, nil
	}
	return &ownerRefusal{status: http.StatusConflict, reason: reasonUnitMembershipGone, arm: arm,
		msg: fmt.Sprintf("the run's owner is no longer a member of %q, the unit this run was launched in; start a new run", p.Name)}, nil
}

// membershipCanMatchOwner is whether row could match the run's owner, known only by the run row. A
// user row matches only the stamped sub: an email row never admits the owner here.
func membershipCanMatchOwner(m types.GovernanceMembership, run types.AgentRun) bool {
	switch m.SubjectType {
	case types.CapabilitySubjectUser:
		return m.Subject == types.CanonicalUserSubject(run.CreatedBy)
	case types.CapabilitySubjectUserType:
		return run.UserType != "" && m.Subject == run.UserType
	default: // all, and a group the run does not record
		return true
	}
}
