// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Governance-profile CRUD (migration 0052): the admin-facing surface over
// governance_units + governance_memberships — named, assignable ceilings and
// the rows binding them to a user, a group, or everyone.
//
// Modeled on access.go / permissions.go: one read that shows the whole picture,
// small validated writes, an audit event per write. Two things are specific to
// this table and neither is optional — the monotone-⊆ bound on a profile's
// eligible grants (governance_grantbound.go), which stops a profile from
// MINTING credential eligibility the deployment never provisioned, and the ON
// DELETE RESTRICT surfaced as a 409, which stops a profile delete from silently
// widening everyone it bound.
package api

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/authz"
	"github.com/cjohnstoniv/wardyn/internal/composer"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// maxGovernanceUnitNameLen bounds a profile name on write. A name is a
// human handle rendered in a picker and used as the resolver's deterministic
// tie-break, not a description.
const maxGovernanceUnitNameLen = 128

// mountGovernanceRoutes registers the /governance family — NINE routes, all on
// the SECURITY tier.
//
// The parameter is still spelled `operatorOnly` because the six CRUD routes
// were born there, before oidc.RoleSecurityAdmin existed; routes.go now hands
// this function the `securityOps` group instead, which is the widening they
// were registered on the safe tier to wait for. The group a mount function
// receives is decided at the call site, never by this parameter's name —
// routes.go says so at the call, and authz_test.go's chi.Walk matrix is what
// enforces it.
//
// The seventh, POST /governance/check, was born on this tier: a READ that
// answers "which profile would bind these claims" by running the resolver
// itself, so the console never re-implements the precedence rule to show it.
//
// The membership move (PUT) and promotion (POST …/promote) are the policy tree's: they follow the
// second-human switch as every assignment write does.
func (s *Server) mountGovernanceRoutes(operatorOnly chi.Router) {
	operatorOnly.Get("/governance", s.handleGetGovernance)
	operatorOnly.Post("/governance/units", s.handleCreateGovernanceUnit)
	operatorOnly.Put("/governance/units/{id}", s.handleUpdateGovernanceUnit)
	operatorOnly.Delete("/governance/units/{id}", s.handleDeleteGovernanceUnit)
	operatorOnly.Post("/governance/memberships", s.handleUpsertGovernanceMembership)
	operatorOnly.Put("/governance/memberships/{id}", s.handleMoveGovernanceMembership)
	operatorOnly.Delete("/governance/memberships/{id}", s.handleDeleteGovernanceMembership)
	operatorOnly.Post("/governance/memberships/{id}/promote", s.handlePromoteGovernanceMembership)
	operatorOnly.Post("/governance/check", s.handleGovernanceCheck)
	s.mountGovernanceChangeRoutes(operatorOnly)
}

// GET /governance

// governanceResponse is GET /governance's body: every profile plus every
// assignment, in ONE call — the console's whole Governance screen, the same
// "one read shows the picture" shape GET /permissions takes. The two lists are
// separate rather than nested because an assignment's whole meaning is WHICH
// profile it points at, and nesting profiles under assignments would duplicate
// a ceiling per binding while hiding an unassigned profile entirely.
type governanceResponse struct {
	Units       []governanceUnitView         `json:"units"`
	Memberships []types.GovernanceMembership `json:"memberships"`
	// MaxDepth is the maximum unit depth in force (site_config.governance_max_depth, 16 when unset).
	MaxDepth int `json:"max_depth"`
	// Boundary is the Wardyn boundary as GET /governance/boundary returns it.
	Boundary types.Boundary `json:"boundary"`
}

// handleGetGovernance returns the whole governance picture. operatorOnly
// (routes.go).
func (s *Server) handleGetGovernance(w http.ResponseWriter, r *http.Request) {
	resolved, err := s.resolveAllUnits(r.Context())
	if err != nil {
		writeServerError(w, r, "list governance profiles", err)
		return
	}
	limit, err := s.unitDepthLimit(r.Context())
	if err != nil {
		writeServerError(w, r, "list governance profiles", err)
		return
	}
	assignments, err := s.cfg.Store.ListGovernanceMemberships(r.Context())
	if err != nil {
		writeServerError(w, r, "list governance assignments", err)
		return
	}
	// Belt-and-braces, the same reason redactPolicyForRead applies on the
	// stored-policy read path: validatePolicySpec already refuses a WRITE that
	// puts a raw llm_inspection secret VALUE in a spec, so a stored ceiling
	// should never carry one — but a read path must never re-expose it if that
	// invariant is ever broken by a migration or a direct DB edit. newUnitView
	// redacts the effective ceiling the same way.
	boundary, err := s.governanceBoundary(r.Context())
	if err != nil {
		writeServerError(w, r, "read the Wardyn boundary", err)
		return
	}
	people := unitPeople(resolved, assignments)
	profiles := make([]governanceUnitView, len(resolved))
	for i, p := range resolved {
		profiles[i] = newUnitView(p.Row, p.Resolved, p.Err)
		if p.Row.ID != store.DefaultUnitID {
			n := len(people[p.Row.ID])
			profiles[i].People = &n
		}
	}
	writeJSON(w, http.StatusOK, governanceResponse{Units: profiles, Memberships: assignments, MaxDepth: limit, Boundary: boundary})
}

// unitPeople is, per unit, the membership subjects in it or in a unit below it. The Default unit's
// people hold no rows, so it has none here.
func unitPeople(units []unitResolution, ms []types.GovernanceMembership) map[uuid.UUID]map[string]bool {
	parent := map[uuid.UUID]*uuid.UUID{}
	for _, u := range units {
		parent[u.Row.ID] = u.Row.ParentID
	}
	out := map[uuid.UUID]map[string]bool{}
	for _, m := range ms {
		key := string(m.SubjectType) + ":" + m.Subject
		cur := &m.UnitID
		for n := 0; cur != nil && n <= store.MaxUnitDepthCap; n++ {
			if out[*cur] == nil {
				out[*cur] = map[string]bool{}
			}
			out[*cur][key] = true
			cur = parent[*cur]
		}
	}
	return out
}

// governanceRefusedAllPlacements is the frozen sentence (GOVERNANCE.REFUSED_ALL_PLACEMENTS) the
// profile writer answers 400 with when deny_placements lists both placements.
const governanceRefusedAllPlacements = "deny_placements: a profile can't deny every placement — the people it binds could run nothing"

// governanceLimitsRefusal is the negative-value boundary on GovernanceLimits, or
// "" when every number is writable. It is the profile's half of
// validateStorageProviders and borrows that function's sentence
// (providers400Negative) for the two SIZES, so one deployment refusing the same
// typo at two doors does not read as two different rules.
//
// MaxConcurrentRuns takes its own wording because it is a COUNT and not MiB —
// the one place the shared sentence would be a lie.
//
// AutonomyRubric gets its own leg, calling AutonomyRubric.Validate rather than
// a range check: it is nine string fields, not a number, so "negative" has no
// meaning here — the boundary is "every set field is one of the four defined
// levels". Validate names which of the nine is bad; this prefixes "limits." so
// the 400 reads like every other field's.
func governanceLimitsRefusal(l types.GovernanceLimits) string {
	if l.MaxConcurrentRuns < 0 {
		return fmt.Sprintf("limits.max_concurrent_runs: %d is not a count of runs — use 0 for unlimited",
			l.MaxConcurrentRuns)
	}
	if l.MaxCPUMillis < 0 {
		return fmt.Sprintf(providers400Negative, "limits.max_cpu_millis", l.MaxCPUMillis)
	}
	if l.MaxMemoryMiB < 0 {
		return fmt.Sprintf(providers400Negative, "limits.max_memory_mib", l.MaxMemoryMiB)
	}
	if l.MaxEphemeralDiskMiB < 0 {
		return fmt.Sprintf(providers400Negative, "limits.max_ephemeral_disk_mib", l.MaxEphemeralDiskMiB)
	}
	if l.MaxDriveSizeMiB < 0 {
		return fmt.Sprintf(providers400Negative, "limits.max_drive_size_mib", l.MaxDriveSizeMiB)
	}
	for _, p := range l.DenyPlacements {
		if !p.Valid() {
			return fmt.Sprintf("limits.deny_placements: %q is not a placement — use remote or local", p)
		}
	}
	if types.DeniesEveryPlacement(l.DenyPlacements) {
		return governanceRefusedAllPlacements
	}
	if l.AutonomyRubric != nil {
		if err := l.AutonomyRubric.Validate(); err != nil {
			return "limits.autonomy_rubric." + err.Error()
		}
	}
	return runLimitsRefusal(l.RunLimits)
}

// the resolver

// governanceCeiling is ONE principal's resolved ceiling: the RunPolicySpec they
// are bounded by, the request-shape limits beside it, and the profile it came
// from — nil for a principal with no assignment, which is the whole
// absent-row-is-today rule expressed in one field. Every routed site keys its
// new behaviour on `Profile != nil`, never on the spec's contents, because a
// profile that happens to equal DefaultPolicy is still an admin declaring this
// principal's ceiling and an unassigned member must stay byte-for-byte today.
type governanceCeiling struct {
	Spec     types.RunPolicySpec
	Limits   types.GovernanceLimits
	Unit     *ResolvedUnit
	Warnings []string
	// MemberDefault is a governed member's size before any ask (composer.MemberDefault from the
	// deployment's own size, lowered by this ceiling), set when the ceiling is resolved. Read it
	// through memberSize.
	MemberDefault types.ResourceLimits
	// Operator is true when this principal is not clamped (isOperator: the
	// admin view, the admin token, local mode). The zero value is the clamped
	// member, so a ceiling built without asking stays bounded. Dispatch reads it
	// for the Azure DevOps standing rule (adoStandingBound).
	Operator bool
	// boundary is the Wardyn boundary this ceiling resolved under (nil in no_limits); boundaryKnobs
	// are the knobs it narrowed here (boundary_meet.go).
	boundary      *types.Boundary
	boundaryKnobs []string
}

// errGroupsSnapshotStale is the 403-class resolver failure: this caller's group
// identity cannot be answered (a pre-0.6 cookie's nil snapshot, or one the
// cookie byte cap truncated), a group-tier assignment exists that might apply
// to them, and no user-tier row settles the question. Serving them ANY ceiling
// would be a guess, and the only wrong guess is the widening one.
//
// Its Error() text is the full message, not the bare sentinel, and that is the
// fix rather than a flourish: governance.go names ONE mapping for a resolver
// failure (writeCeilingError below) "so a resolver failure cannot answer 403 at
// one site and 500 at the next for the same cause" — but a site that takes only
// the STATUS half via ceilingErrorStatus and composes its own body from
// err.Error() bypassed that rule and shipped a 403 saying nothing but
// "groups_snapshot_stale". The remedy is one the human can actually perform, and
// the alternative is a support ticket. Attaching it to the VALUE means every
// site that prints the error carries the remedy, including sites written later,
// instead of each one having to remember the rule.
var errGroupsSnapshotStale = errors.New(groupsSnapshotStaleMsg)

// groupsSnapshotStaleMsg is what the caller reads. It names the remedy, because
// the remedy is one the human can actually perform and the alternative is a
// support ticket: sign in again (or re-mint the API token) and the snapshot is
// rebuilt from the current claims.
const groupsSnapshotStaleMsg = "groups_snapshot_stale: your group membership snapshot is missing or was truncated at sign-in, " +
	"and this deployment assigns governance profiles by group — sign in again (or re-mint your API token) so your ceiling can be resolved"

// writeCeilingError answers an effectiveCeiling failure at an HTTP site: 403
// for the stale/truncated snapshot or an unknown user type, 500 for everything
// else.
//
// 500 is the point for the everything-else arm. A store failure means the
// ceiling is unknown, and the adjacent GetSiteConfig idiom — log it, carry on
// with the zero value — must NOT be copied here: carrying on means silently
// substituting the deployment ceiling for a profile that may be far narrower,
// which is a widening triggered by a database hiccup.
func writeCeilingError(w http.ResponseWriter, r *http.Request, err error) {
	if errors.Is(err, errGroupsSnapshotStale) {
		// Identical to err.Error() now that the sentinel carries the message;
		// spelled out because THIS is the site that defines what the body is,
		// and a reader should not have to chase the sentinel to find out.
		writeErrorReason(w, http.StatusForbidden, reasonGroupsSnapshotStale, groupsSnapshotStaleMsg)
		return
	}
	if u, ok := isOverlayUnsatisfiable(err); ok {
		writeErrorReason(w, http.StatusForbidden, reasonGovernanceOverlayUnsatisfiable, u.Error())
		return
	}
	writeServerError(w, r, "resolve governance ceiling", err)
}

// writeCeilingErrorPrefixed is writeCeilingError for a seam that has its own
// error prefix ("list secrets: ", "policy: ", …).
//
// The stale arm drops the prefix on purpose. groupsSnapshotStaleMsg exists
// because the remedy is one the human can actually perform — sign in again, or
// re-mint the API token — and the alternative is a support ticket. A seam that
// pastes its prefix onto err.Error() instead publishes the bare
// `groups_snapshot_stale` sentinel: an internal identifier naming a condition a
// member has no vocabulary for and no documented way to clear. The status was
// already shared (ceilingErrorStatus); this shares the SENTENCE, so a refusal
// cannot name the remedy at one member-reachable seam and withhold it at the
// next.
//
// Everything else keeps the seam's own prefix over the underlying error, which
// is the 500 an operator reads, not the member.
func writeCeilingErrorPrefixed(w http.ResponseWriter, r *http.Request, prefix string, err error) {
	if errors.Is(err, errGroupsSnapshotStale) {
		writeErrorReason(w, http.StatusForbidden, reasonGroupsSnapshotStale, groupsSnapshotStaleMsg)
		return
	}
	if u, ok := isOverlayUnsatisfiable(err); ok {
		writeErrorReason(w, http.StatusForbidden, reasonGovernanceOverlayUnsatisfiable, u.Error())
		return
	}
	writeServerError(w, r, strings.TrimRight(prefix, ": "), err)
}

// ceilingErrorStatus is writeCeilingError's status half, for the two seams that
// hand a code back up to a caller instead of writing the response themselves.
// One mapping, so a resolver failure cannot answer 403 at one site and 500 at
// the next for the same cause.
func ceilingErrorStatus(err error) int {
	if _, unsat := isOverlayUnsatisfiable(err); unsat || errors.Is(err, errGroupsSnapshotStale) || errors.Is(err, errUserTypeUnknown) {
		return http.StatusForbidden
	}
	return http.StatusInternalServerError
}

// effectiveCeiling resolves the ceiling that bounds the caller on ctx.
//
// This is THE consolidation the campaign exists for: fifteen sites read
// Config.DefaultPolicy as "the ceiling", six of them meaning "this principal's
// ceiling", and those six now come through here instead. Resolution order
// mirrors capAllowed's, and each step is a decision rather than a fallback:
//
//  1. Operator ⇒ DefaultPolicy, with NO store read. A super admin IS the
//     ceiling-setting authority, so there is nothing to bound them by — and the
//     short-circuit is also what keeps the ~30 nil-store test doubles in this
//     package alive. DELIBERATELY isOperator and not isSecurityOperator: a
//     security admin authors profiles and is BOUNDED by their own, which is the
//     single assumption governance_grantbound.go's monotone-⊆ bound rests on.
//  2. No store ⇒ DefaultPolicy. A build with no store holds no assignments, so
//     there is nothing to resolve, and the answer is the one it has always
//     been. (Distinct
//     from capAllowed, which errors on a nil store: that resolver is asked
//     about a deployment that HAS capability state and cannot reach it, while
//     this arm is a build with none at all — the capSeamAllowed split, same
//     reasoning.)
//  3. Store error ⇒ error, and the caller 500s. NEVER fall open to
//     DefaultPolicy: a database hiccup would silently widen every walled
//     principal back to the deployment ceiling, with the run proceeding
//     normally and nothing in the audit saying which ceiling it ran under.
//  4. Unanswerable group snapshot ⇒ 403, but only in one shape (see below).
//  5. ErrNotFound ⇒ DefaultPolicy. No assignment matched: absent row, absent
//     behaviour change, the 0042 doctrine.
//  6. Always Clone. The resolved spec is handed to callers that append to its
//     slices (unionAllowedDomains, the workspace/SCM egress unions); a shallow
//     copy shares the backing array, which is the exact race resolvePolicy's
//     own Clone comment documents — and here the shared value would be a row
//     read fresh per request rather than a process global, so the corruption
//     would be intermittent instead of merely wrong.
//
// ponytail: no cache ACROSS REQUESTS, matching capAllowed's own note — a stale
// ceiling is a security bug, not a slow page. Within ONE request it is memoized
// (see the memo below), which is a different claim: the request is the unit the
// answer must be consistent over.
//
// The repeated reads this memo replaces defended themselves on latency
// grounds — "the alternative buys latency at the cost of the one property
// that matters here, which is that no site can forget to ask." Latency was
// never the real cost: three independent, untransacted reads per member
// create (denyUserGovernance, resolveRunPolicy, filterUserGrants) plus
// dispatch's fourth can return DIFFERENT ANSWERS if a security admin narrows
// a profile mid-request, and resolveRunPolicy asserts the opposite in words
// ("a create must never resolve two different ceilings for one request").
// The property is not lost: every site still asks — the memo just answers.
func (s *Server) effectiveCeiling(ctx context.Context) (governanceCeiling, error) {
	// One ceiling per request. The memo is checked first and filled on the way
	// out, so every site in one request sees the SAME answer. A member create
	// alone takes THREE independent, uncached, untransacted reads
	// (denyUserGovernance -> resolveRunPolicy -> filterUserGrants) plus
	// dispatch a fourth, so without the memo a security admin narrowing a
	// profile mid-flight — the incident-response
	// action — could land a run whose egress was clamped under the PRE-narrowing
	// ceiling while its grants were filtered under the post-narrowing one.
	//
	// It preserves the property the repeated reads were defended for ("no site
	// can forget to ask"): every site still asks. It just asks the memo first.
	//
	// Not a cache across requests, which is the HA blocker effectiveCeiling's own
	// note names: the memo lives on the request context, so it dies with the
	// request and the next one resolves afresh. A background caller (reconcile,
	// the boot heal) carries no memo and resolves normally.
	if memo := ceilingMemoFromContext(ctx); memo != nil {
		return memo.do(ctx, s.resolveEffectiveCeiling)
	}
	return s.resolveEffectiveCeiling(ctx)
}

// resolveEffectiveCeiling is effectiveCeiling's uncached body — the resolution
// order its doc comment describes, then the Wardyn boundary over whatever it resolved
// (boundaryCeiling), the outermost layer, operators included. Split out so the memo
// above wraps it exactly once and no call site can reach the raw resolve by accident.
func (s *Server) resolveEffectiveCeiling(ctx context.Context) (governanceCeiling, error) {
	c, err := s.resolveChainCeiling(ctx)
	if err != nil {
		return c, err
	}
	return s.boundaryCeiling(ctx, c)
}

// resolveChainCeiling is the ceiling below the boundary: the operator's, the
// deployment's, or the caller's profile chain.
func (s *Server) resolveChainCeiling(ctx context.Context) (governanceCeiling, error) {
	deployment := governanceCeiling{Spec: s.cfg.DefaultPolicy.Clone(), MemberDefault: s.memberDefaultFor(s.cfg.DefaultPolicy.Resources)}
	if s.runUngoverned(ctx) {
		deployment.Operator = true
		return deployment, nil
	}
	if s.cfg.Store == nil {
		return deployment, nil
	}

	// The caller's one context, its path composed (governance_context.go). Restrictions on the
	// context are applied after it (the RESTRICT-PIN lane).
	return s.resolveContext(ctx, deployment)
}

// ceilingWithUnusableGroups is the context resolve when the caller's group identity cannot be
// evaluated: the context is selected on their user subjects and type alone, and selectByTier's
// one stale rule decides whether that answer is trustworthy (target governance.ceiling). The
// drive preview enters here too.
func (s *Server) ceilingWithUnusableGroups(ctx context.Context, users []string, userType string, deployment governanceCeiling) (governanceCeiling, error) {
	return s.contextCeiling(ctx, contextQuery{users: users, userType: userType, unusable: true}, contextChoiceFrom(ctx), deployment)
}

// ceilingFromUnit turns one resolver answer into a ceiling: ErrNotFound (or
// a nil profile) is the deployment's, anything else is the profile's, already composed from its
// chain (governance_compose.go) with its grants re-intersected at every step.
//
// Order matters and it is the whole function. The REAL-ERROR check runs FIRST,
// ahead of the nil-profile one, because a failed resolve also returns a nil
// profile: checking nil first would turn every store failure into "no
// assignment matched" and hand the caller the DEPLOYMENT ceiling — the
// fail-open this file exists to refuse, arriving through the back door of a
// convenience guard. The nil check that follows is still worth having: it makes
// a store answering (nil, "", nil) mean the absent-row doctrine rather than a
// dereference, so no caller has to have checked on its behalf.
//
// A composition nothing satisfies is the explicit deny-all state: it is refused with the authz
// reason governance_overlay_unsatisfiable and audited here, once per resolve, unless the read only
// displays a state. It is never read as the deployment's ceiling or as either side of the meet.
func (s *Server) ceilingFromUnit(ctx context.Context, p *ResolvedUnit, err error, deployment governanceCeiling) (governanceCeiling, error) {
	if u, ok := isOverlayUnsatisfiable(err); ok {
		if !isDisplayRead(ctx) {
			s.recordRefusal(ctx, nil, authz.Deny(authz.ReasonGovernanceOverlayUnsatisfiable, "governance.ceiling", u.Error()))
		}
		return governanceCeiling{}, err
	}
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return governanceCeiling{}, fmt.Errorf("api: resolve governance profile: %w", err)
	}
	if p == nil {
		return deployment, nil
	}
	return governanceCeiling{Spec: p.Ceiling.Clone(), Limits: p.Limits, Unit: p, Warnings: p.Warnings,
		MemberDefault: s.memberDefaultFor(p.Ceiling.Resources)}, nil
}

// authoredSize is the ceiling policy's own CPU and memory as the member cap reads it (#1949): a
// profile's AUTHORED size, zero where it inherited the deployment's, so a maximum with no size of its
// own is the size; any other ceiling (the deployment's, an operator's) inherits nothing, so its spec
// is its own.
func (c governanceCeiling) authoredSize() types.ResourceLimits {
	if c.Unit != nil {
		return c.Unit.AuthoredResources
	}
	if r := c.Spec.Resources; r != nil {
		return types.ResourceLimits{CPUMillis: r.CPUMillis, MemoryMiB: r.MemoryMiB}
	}
	return types.ResourceLimits{}
}

// memberDefaultFor is the member default under a ceiling of this size: the deployment's own size
// (inheritDeploymentResources), lowered by the ceiling.
func (s *Server) memberDefaultFor(ceiling *types.ResourceLimits) types.ResourceLimits {
	d := s.inheritDeploymentResources(nil)
	return composer.MemberDefault(types.ResourceLimits{CPUMillis: d.CPUMillis, MemoryMiB: d.MemoryMiB}, ceiling)
}

// memberSize is the member default the member cap starts from. A ceiling that was not resolved (a
// hand-built one) carries none and falls back to the platform default lowered by its own size.
func (c governanceCeiling) memberSize() types.ResourceLimits {
	if c.MemberDefault.CPUMillis > 0 && c.MemberDefault.MemoryMiB > 0 {
		return c.MemberDefault
	}
	return composer.MemberDefault(types.ResourceLimits{}, c.Spec.Resources)
}

// inheritDeploymentResources fills what a profile's ceiling leaves unset (no resources block, or a
// zero field) from the deployment: DefaultPolicy.Resources, else runner.EffectiveLimits(), the same
// order the runners and composer.Clamp fall back in. Without it an omitted size fell through to
// the compiled-in platform default, which could be larger than the deployment's own. A set
// field is the profile's own and stays. DiskMiB has no platform default, so it inherits only
// DefaultPolicy's.
func (s *Server) inheritDeploymentResources(r *types.ResourceLimits) *types.ResourceLimits {
	eff := runner.EffectiveLimits()
	out := types.ResourceLimits{CPUMillis: int(eff.CPUMillis), MemoryMiB: int(eff.MemoryMiB), PidsLimit: int(eff.PidsLimit)}
	if d := s.cfg.DefaultPolicy.Resources; d != nil {
		out.CPUMillis = cmp.Or(d.CPUMillis, out.CPUMillis)
		out.MemoryMiB = cmp.Or(d.MemoryMiB, out.MemoryMiB)
		out.PidsLimit = cmp.Or(d.PidsLimit, out.PidsLimit)
		out.DiskMiB = d.DiskMiB
	}
	if r != nil {
		out.CPUMillis = cmp.Or(r.CPUMillis, out.CPUMillis)
		out.MemoryMiB = cmp.Or(r.MemoryMiB, out.MemoryMiB)
		out.PidsLimit = cmp.Or(r.PidsLimit, out.PidsLimit)
		out.DiskMiB = cmp.Or(r.DiskMiB, out.DiskMiB)
	}
	return &out
}

// droppedPushRulesWarning is ceilingFromUnit's push_rules mirror of
// reintersectGovernanceGrants's grant-kind drop: when the deployment default
// carries push_rules and this profile's own ceiling does not, the deployment's
// content rules silently stop applying to this profile's members.
//
// clampPushRules itself cannot say this — it clamps an already-resolved
// ceiling and never sees what the deployment default would have said — and a
// bare ceiling with no push_rules is otherwise indistinguishable from "the
// operator deliberately left this narrower". This is the one seam where
// resolving a profile assignment sees BOTH specs at once, so it is the one
// place the drop can be said out loud (see #272).
func droppedPushRulesWarning(profile, deployment types.RunPolicySpec, profileName string) []string {
	if deployment.PushRules.IsSet() && !profile.PushRules.IsSet() {
		return []string{fmt.Sprintf(
			"governance profile %q: push_rules dropped — this profile's ceiling sets none, "+
				"so the deployment default's content rules do not apply to members of it",
			profileName)}
	}
	return nil
}

// reintersectGovernanceGrants re-applies the monotone-⊆ bound at RESOLVE time,
// dropping any profile grant the deployment ceiling no
// longer dominates.
//
// The write-time check is not enough on its own because Config.DefaultPolicy is
// ENV-BORNE: an operator who removes a pairing from WARDYN_DEFAULT_POLICY and
// redeploys has revoked that eligibility for everyone — except that the
// profiles written while it existed are rows, and rows survive redeploys. They
// would go on serving a pairing the deployment no longer provisions, and the
// profile-authoring surface would have become a way to pin credential
// eligibility past the deployer's own revocation.
//
// Drops with a warning, never a 500 or a refusal, and the asymmetry is
// deliberate: a write is a caller's own act and can be sent back for
// correction, but a redeploy is somebody ELSE's act arriving between a member's
// two runs. Failing their run for it would turn one operator's env edit into an
// outage for every profile that mentioned the pairing, while dropping the grant
// leaves the run exactly as governed as the operator just asked for and says so
// in the warning list resolveRunPolicy already surfaces on the 201.
func reintersectGovernanceGrants(profile, deployment []types.GrantSpec, profileName string) ([]types.GrantSpec, []string) {
	if len(profile) == 0 {
		return profile, nil
	}
	kept := profile[:0:0]
	var warns []string
	for _, g := range profile {
		if err := governanceGrantWithinCeiling(g, deployment); err != nil {
			warns = append(warns, fmt.Sprintf(
				"governance profile %q: dropped %s grant no longer within the deployment's eligible grants (%v)",
				profileName, g.Kind, err))
			continue
		}
		kept = append(kept, g)
	}
	return kept, warns
}
