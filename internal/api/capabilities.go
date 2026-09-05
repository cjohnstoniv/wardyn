// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"strings"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// ─── the closed kind set ──────────────────────────────────────────────────────
//
// Six kinds, and this slice is the ONLY place the set is written down —
// migration 0042 deliberately puts no CHECK on capability_grants.capability, so
// a seventh kind is a constant here plus its enforcement call site, with no DDL
// (0.7 added the fifth and sixth on exactly those terms). The console's own
// list (ui/src/app/lib/permissions-copy.ts CAPABILITY_KINDS) mirrors these ids
// and must not drift.
//
// Five of the six NARROW what a member may already do; capImage WIDENS (a
// member cannot name a custom image at all today). Both directions resolve
// through the same rules below — the difference lives at the enforcement seam,
// not here.
const (
	// capEgressHost bounds the egress hosts a member may author on an inline
	// policy, and the hosts a member may approve an egress request for. Values
	// are a bare host or a "*.suffix" wildcard, matched by entryCoversAny — the
	// one host matcher this package already has.
	capEgressHost = "egress_host"
	// capSecret bounds which secret names a member may reference from an inline
	// policy, and which ones they can see listed. Exact names.
	capSecret = "secret"
	// capWorkspace bounds which onboarded workspaces a member may launch a run
	// against. Values are workspace uuids (as strings).
	capWorkspace = "workspace"
	// capImage WIDENS: it names the exact image refs a member may launch
	// directly, a power members do not have at all without a grant. Note that
	// devcontainer_repo is NOT a capability and stays unconditionally
	// admin-only — it executes attacker-authored build configuration, which is
	// not a thing to hand out one row at a time.
	capImage = "image"
	// capAgent NARROWS: it bounds which agent/harness a member may launch —
	// req.Agent, the member's own free-text choice, gated at denyMemberRequest.
	// Values are the exact `--agent` string plus `*`.
	//
	// DELIBERATELY narrowing rather than widening, and the direction is decided
	// by capGranted's own documented rule below: a WIDENING kind refuses on
	// !enforced, which would refuse every member run on every deployment that
	// has not enforced this kind — i.e. all of them on upgrade day. Launching an
	// agent is something every member could already do, so it stays allowed
	// until an admin enforces it.
	//
	// DELIBERATELY not constrained to the harness catalog either, here or at the
	// grant write boundary: harnessByID documents a WARDYN_AGENT_IMAGES-only
	// custom agent as supported, so a catalog check would make an operator's own
	// entry unwriteable.
	capAgent = "agent"
	// capIntegration NARROWS: it bounds which AI-provider integration a member
	// may name on a run — req.IntegrationID, and NOTHING ELSE.
	//
	// TIER 1 ONLY (PF-33). resolveRunIntegration has three tiers, and the other
	// two — a workspace's own LLMCred pin (tier 2) and the operator's
	// DefaultFor:agent_runs site default (tier 3) — are OPERATOR-authored. Gating
	// them would contradict the doctrine rendered on the very screen this kind
	// appears on ("a capability bounds what a member chose, never what an admin
	// pre-authorized", permissions-copy.ts PERM.DOCTRINE) and would let one `all`
	// deny row strip the site's model access deployment-wide. So the gate lives
	// at denyMemberRequest, on the one member-authored input, and never inside
	// resolveRunIntegration — which operator callers reach too.
	capIntegration = "integration"
)

// capabilityKinds is the closed set, in the order the admin surface shows them.
var capabilityKinds = []string{capEgressHost, capSecret, capWorkspace, capImage, capAgent, capIntegration}

// validCapabilityKind reports whether kind is one of the six. The API write
// boundary uses it in place of the CHECK the schema deliberately does not have.
func validCapabilityKind(kind string) bool { return slices.Contains(capabilityKinds, kind) }

// capWildcard matches every value of its kind. Spelled the same for all six so
// an admin does not have to learn a per-kind syntax for "all of them".
const capWildcard = "*"

// ─── resolution ───────────────────────────────────────────────────────────────

// capabilitySubjects returns the grant subjects that describe the caller on
// ctx: their user identities and their group snapshot.
//
// users carries BOTH the lowercased OIDC sub and the email, because an admin
// writing a grant knows one or the other and should not have to guess which one
// this IdP made authoritative. Matching either is a deliberate widening of who
// a `user` row hits — and it is safe in the direction that matters, since a
// DENY written against either identity also hits.
//
// groups is nil for a pre-0.6 cookie (the snapshot predates the field) and
// empty when the IdP sent nothing usable. stale reports that the group half is
// UNANSWERABLE, which is two shapes and not one: the nil snapshot above, and a
// snapshot that is present but PARTIAL — sessionGroups dropped entries at the
// cookie byte cap, or the IdP never sent the claim at all (an Entra groups
// overage), or the row is a pre-0.7 API token whose completeness was never
// recorded (Server.apiTokenAuth reads a NULL marker as truncated). Either way
// the caller's group grants cannot be evaluated in full until they sign in
// again or re-mint. Callers surface that; they must not silently treat it as
// "no groups" — that is the reading that lets a group DENY evaporate.
//
// The returned groups stay the PARTIAL list rather than being blanked: a row
// that matches one of them is a real match, and for the deny direction seeing
// more is strictly safer. It is the rows that are MISSING that stale is for
// (capScan's unresolvable-deny check).
func capabilitySubjects(ctx context.Context) (users, groups []string, stale bool) {
	if sub := oidcHumanFromContext(ctx); sub != "" {
		users = append(users, strings.ToLower(sub))
	}
	if email := strings.ToLower(strings.TrimSpace(oidcEmailFromContext(ctx))); email != "" && !slices.Contains(users, email) {
		users = append(users, email)
	}
	groups = oidcGroupsFromContext(ctx)
	return users, groups, groups == nil || oidcGroupsTruncatedFromContext(ctx)
}

// capAllowed answers "may this caller use `kind` at `value`". Precedence, in
// order, and the order IS the design:
//
//  1. Admin, admin token, and local mode are EXEMPT. A capability bounds a
//     member; the admin tier is the one writing the grants.
//  2. Any matching DENY ⇒ false. Deny beats everything, including a grant on
//     the caller's own user row — there is no user-over-group precedence,
//     because "Bob's user allow overrode the group deny" is a breach report.
//  3. Any matching ALLOW ⇒ true.
//  4. The kind is not enforced ⇒ true. An absent enforcement row is a
//     freshly-upgraded 0.5 deployment, which must behave byte-for-byte as it
//     did before this file existed.
//  5. Otherwise false.
//
// Deny sits ABOVE the enforcement switch on purpose: it makes deny rows the
// adoption on-ramp. An admin can blacklist one host for one contractor without
// flipping the whole deployment fail-closed, which is the only way this feature
// gets used before anyone trusts it.
//
// Errors are never allowed to read as permission. A store failure returns
// (false, err) so the caller answers 500 rather than deciding either way; a nil
// Store (test wiring only — wardynd always wires PG) is that same error, NOT a
// silent allow.
//
// ponytail: no cache. Two indexed reads per call on a small table, so a new
// grant takes effect on the very next request. A process-local cache is the HA
// blocker OPERATIONS already names for other state, and a stale permission
// cache is a security bug rather than a slow page — add one only behind a
// shared invalidation channel.
//
// DELIBERATELY isOperator, and capGranted below likewise — this is the
// INVARIANT that makes handing /permissions to a security admin safe at all:
// no capability kind, present or future, can ever widen the admin tier. A
// security admin is capability-BOUNDED exactly like a member (they may
// self-grant through /permissions, audited, and still reach nothing this
// exemption would give them). See the three-tier doctrine on
// internal/auth/oidc's RoleSecurityAdmin. A pinning test asserts it.
func (s *Server) capAllowed(ctx context.Context, kind, value string) (bool, error) {
	if !validCapabilityKind(kind) {
		return false, fmt.Errorf("api: unknown capability kind %q", kind)
	}
	if s.isOperator(ctx) {
		return true, nil
	}
	if s.cfg.Store == nil {
		return false, fmt.Errorf("api: capability %q cannot be resolved: no store configured", kind)
	}

	deny, allow, err := s.capScan(ctx, kind, value)
	switch {
	case err != nil:
		return false, err
	case deny:
		return false, nil
	case allow:
		return true, nil
	}

	enforced, err := s.capEnforced(ctx, kind)
	if err != nil {
		return false, err
	}
	return !enforced, nil
}

// capGranted answers the WIDENING question: does this caller hold an explicit
// grant of `kind` at `value`? It is not capAllowed with the sign flipped — the
// two differ on the one case that decides an upgrade's behavior:
//
//   - narrowing (capAllowed): an unenforced kind is ALLOWED, because 0.5
//     already let a member do it.
//   - widening (capGranted): an unenforced kind is REFUSED, because 0.5 already
//     refused it.
//
// Same rule — "an upgrade with no configuration changes nothing" — landing on
// opposite defaults because the two kinds start from opposite postures. `image`
// is the only widening kind today, and the console's own copy states exactly
// this contract (permissions-copy.ts KIND.image: unenforced means members
// cannot name an image at all; enforced means a granted ref becomes nameable).
//
// Deny still beats allow. A build with no store holds no rows, so it refuses,
// which is again 0.5.
func (s *Server) capGranted(ctx context.Context, kind, value string) (bool, error) {
	if !validCapabilityKind(kind) {
		return false, fmt.Errorf("api: unknown capability kind %q", kind)
	}
	if s.isOperator(ctx) {
		return true, nil
	}
	if s.cfg.Store == nil {
		return false, nil
	}
	enforced, err := s.capEnforced(ctx, kind)
	if err != nil || !enforced {
		return false, err
	}
	deny, allow, err := s.capScan(ctx, kind, value)
	if err != nil {
		return false, err
	}
	return allow && !deny, nil
}

// capScan walks the caller's own grants for one kind/value and reports whether
// a DENY and/or an ALLOW matched. The single place a stored row is compared
// against a request, so the narrowing and widening answers can never disagree
// about what a row covers.
//
// AN UNANSWERABLE GROUP SNAPSHOT IS NOT "NO GROUP ROWS" (PF-26, the capability
// half). capabilitySubjects' stale bit says the group list this scan matched
// against is missing or partial, so a group DENY the caller actually holds may
// simply not be in `grants` — and every seam above reads a clean scan as
// permission. effectiveCeiling (governance.go) already refuses on exactly this
// input; without the same treatment here, a member whose walling group fell off
// the cap keeps reaching the denied host, and on a deployment with group deny
// rows but no group-tier ASSIGNMENTS there is no ceiling refusal anywhere to
// catch it. So when the snapshot is unanswerable and a group deny row COULD
// cover this value, the scan reports the deny it cannot rule out.
func (s *Server) capScan(ctx context.Context, kind, value string) (deny, allow bool, err error) {
	users, groups, stale := capabilitySubjects(ctx)
	grants, err := s.cfg.Store.ListCapabilityGrantsFor(ctx, users, groups)
	if err != nil {
		return false, false, fmt.Errorf("api: resolve capability %q: %w", kind, err)
	}
	for _, g := range grants {
		if g.Capability != kind {
			continue
		}
		if g.Effect == types.CapabilityDeny {
			if capValueOverlaps(kind, g.Value, value) {
				return true, false, nil // scan no further: deny is final
			}
			continue
		}
		if capValueMatches(kind, g.Value, value) {
			allow = true
		}
	}
	if stale {
		unresolved, uerr := s.capUnresolvableGroupDeny(ctx, kind, value)
		if uerr != nil {
			return false, false, uerr
		}
		if unresolved {
			// Logged, not audited: the seam that asked writes its own
			// authz.denied with the reason it knows, and this line is what
			// tells an operator the refusal was about COMPLETENESS rather than
			// a row naming this human. Value is left out — a secret name or a
			// workspace id is the seam's to log, not the resolver's.
			slog.Warn("api: capability refused because the caller's group snapshot is unanswerable and a group deny grant of this kind exists",
				"capability", kind, "principal", oidcHumanFromContext(ctx))
			return true, false, nil
		}
	}
	return false, allow, nil
}

// capUnresolvableGroupDeny reports whether ANY group-subject DENY row of this
// kind could cover value. It is asked only when capabilitySubjects says the
// caller's group snapshot is unanswerable, and it is what keeps that refusal
// SCOPED — the same scoping ceilingWithUnusableGroups gets from
// HasGroupTierAssignments (governance.go). A blanket refusal on every stale
// snapshot would deny every pre-0.6 cookie and every pre-0.7 API token on every
// deployment, including the overwhelming majority that hold no group deny rows
// at all, and "an upgrade with no configuration changes nothing" is the rule
// this whole file is built on.
//
// ALLOW rows deliberately need no equivalent: a group allow the scan cannot see
// costs the caller access (capAllowed falls through to the enforcement switch,
// capGranted refuses outright), which is the fail-CLOSED direction already.
//
// THE ROWS ARE SELECTED IN SQL, NOT SCANNED IN GO, and the reason is not
// tidiness. This runs on the path taken by every caller whose group snapshot is
// unanswerable — which, by the fail-closed reading of a NULL groups_truncated
// column, is EVERY API token minted before 0.7, on every request it makes — and
// it runs once per value a handler checks, not once per request. Asking
// ListCapabilityGrants (the whole table) there meant the cost of an
// authorization check scaled with the size of the grant table: measured at
// 68 ms per call against 20k grants versus 0.35 ms for the indexed sibling, and
// a run create alone checks three values while a secrets narrowing checks one
// per paired name. That is an availability surface — a caller holding one
// pre-0.7 token can force an unbounded read per checked value — and it grows
// precisely as a deployment adopts the feature.
//
// ListGroupDenyGrants applies EXACTLY the predicate this loop applied
// (group + deny + this kind) in the query instead, leaving only the
// value-overlap test in Go, where the one host/wildcard matcher lives. Same
// rows considered, same answer; TestCapUnresolvableGroupDenyMatchesFullScan
// pins the new path against a full scan of the old shape over a generated
// matrix, because a FASTER fail-closed check that stops firing is a breach, not
// a regression.
//
// ponytail: still no per-request memo. The query now returns nothing at all on
// any deployment holding no group deny rows of this kind — the overwhelming
// majority, and the case the scoping above exists to protect — so a memo would
// add a request-scoped cache and a context holder to save a query that returns
// zero rows. The remaining per-VALUE repetition is the caller-controlled loop's
// problem, not this function's.
func (s *Server) capUnresolvableGroupDeny(ctx context.Context, kind, value string) (bool, error) {
	grants, err := s.cfg.Store.ListGroupDenyGrants(ctx, kind)
	if err != nil {
		return false, fmt.Errorf("api: resolve capability %q: %w", kind, err)
	}
	for _, g := range grants {
		if capValueOverlaps(kind, g.Value, value) {
			return true, nil
		}
	}
	return false, nil
}

// capSeamAllowed is capAllowed AT AN ENFORCEMENT SEAM — same answer, plus the
// one case a seam has and the resolver deliberately refuses to guess at: a
// deployment with no Store cannot hold a grant OR an enforcement row, so there
// is nothing to enforce and the seam behaves exactly as 0.5 did.
//
// capAllowed itself keeps erroring on a nil Store, because a RESOLVER that
// answered "allowed" for a question it could not resolve is the failure mode
// this file is built against. The difference is that the resolver is asked
// about a deployment that has capability state and cannot reach it, while a
// seam is running in a build that has none at all (this package's own
// harnesses; wardynd always wires PG). Those are not the same situation and
// must not fail the same way.
//
// ponytail: one call per value, so a seam asking about N values pays 2N indexed
// reads on a small table. N is a handful everywhere it is used today (a run's
// egress allowlist, a deployment's secret names). If one grows, resolve the
// caller's grants + enforcement ONCE and match in-process — capValueMatches is
// already the whole matcher — rather than caching across requests, which is the
// HA blocker capAllowed's own comment names.
func (s *Server) capSeamAllowed(ctx context.Context, kind, value string) (bool, error) {
	if s.cfg.Store == nil {
		return true, nil
	}
	return s.capAllowed(ctx, kind, value)
}

// capEnforced reports whether kind's switch is on. An absent row is off — the
// zero-config default that keeps an upgraded 0.5 deployment unchanged.
func (s *Server) capEnforced(ctx context.Context, kind string) (bool, error) {
	if s.cfg.Store == nil {
		return false, fmt.Errorf("api: capability %q cannot be resolved: no store configured", kind)
	}
	enf, err := s.cfg.Store.GetCapabilityEnforcement(ctx)
	if err != nil {
		return false, fmt.Errorf("api: read capability enforcement: %w", err)
	}
	return enf[kind], nil
}

// capValueMatches reports whether a grant written for grantValue covers want.
//
// egress_host reuses entryCoversAny (artifact_redirect.go), the SAME matcher
// the egress substitution drop uses — so "*.pythonhosted.org" and "pypi.org:443"
// behave here exactly as they do everywhere else egress hosts are compared.
// Deliberately not a second host matcher: two matchers that disagree is how a
// deny gets bypassed by a port suffix.
//
// Every other kind is an exact, case-sensitive compare. A secret name, a
// workspace uuid, an image ref, an agent id and an integration id are all
// identifiers where a near-miss must not match; only egress hosts have a
// defensible subdomain semantics. The two 0.7 kinds therefore need no arm of
// their own — this default IS their matcher, exact plus the shared wildcard.
func capValueMatches(kind, grantValue, want string) bool {
	grantValue = strings.TrimSpace(grantValue)
	if grantValue == capWildcard {
		return true
	}
	if kind == capEgressHost {
		return entryCoversAny(grantValue, map[string]bool{egressEntryHost(want): true})
	}
	return grantValue == strings.TrimSpace(want)
}

// capValueOverlaps is the DENY question, and it is deliberately not
// capValueMatches: an allow has to COVER the want, but a deny only has to
// OVERLAP it. For every exact kind the two are the same question, but an
// egress host is a SET — "*.example.com" is every host under it — and a want
// that is itself a wildcard can contain a denied host without being covered by
// it. Asking only "does the deny cover the want" let a member whose allowlist
// said "*.example.com" keep an entry that includes the denied
// "secret.example.com": the deny row protected nothing, which is the one thing
// this file promises it always does ("deny beats everything").
//
// So a deny bites when the sets intersect in EITHER direction — deny covers
// want, or want covers deny. Both directions go through capValueMatches, so
// there is still exactly one host matcher.
func capValueOverlaps(kind, grantValue, want string) bool {
	if capValueMatches(kind, grantValue, want) {
		return true
	}
	// Only egress hosts have set-valued entries; every other kind is an exact
	// identifier, where "want covers deny" is the same compare reversed.
	return kind == capEgressHost && capValueMatches(kind, want, grantValue)
}

// ─── the batch seam ───────────────────────────────────────────────────────────

// capBatch is capSeamAllowed for MANY values: it resolves the caller's grants,
// the enforcement map and (only when needed) the unresolvable-group-deny table
// ONCE, then answers every value in process with the same matchers capScan uses.
//
// This is the fix capSeamAllowed's own doc comment prescribes, taken at the
// moment its stated premise stopped holding. That comment reads "one call per
// value, so a seam asking about N values pays 2N indexed reads on a small
// table. N is a handful everywhere it is used today (a run's egress allowlist,
// a deployment's secret names). If one grows, resolve the caller's grants +
// enforcement ONCE and match in-process — capValueMatches is already the whole
// matcher." A run's egress allowlist is NOT a handful: it is
// spec.AllowedDomains, taken verbatim from the request body, and nothing on the
// member create/preflight path caps or de-duplicates it before
// narrowMemberInlinePolicy loops over it — validatePolicySpec's count caps
// (maxToolRulesPerPolicy, maxUIAppsPerPolicy) have no allowed_domains arm, and
// composer.Clamp's intersection keeps duplicates of a permitted entry (its
// partition appends every element that passes) and is skipped outright under a
// ceiling with allow_all_egress.
//
// MEASURED, against a real store.PG over loopback with an empty grants table:
// ~505µs per entry, linear, so one member request carrying the most entries
// that fit under maxJSONBody (52,425 x "api.anthropic.com") spent 104,850
// sequential round trips and 27.0s inside this one function — 157,275 and 45.3s
// when the caller's group snapshot is unanswerable. POST /runs/preflight is on
// the member router group and persists nothing, so that is repeatable for free.
// After: 2 round trips (3 stale), flat in N.
//
// NOT A CACHE, deliberately, and that distinction is the whole reason this is
// safe: the batch lives for ONE narrowMemberInlinePolicy call and is discarded,
// so a grant revoked between requests still binds on the next one. Caching
// across requests is the HA blocker capAllowed's own comment names, and nothing
// here reaches for it.
//
// LAZY, so a spec with nothing to check performs no reads at all and the ~30
// nil-store doubles in this package keep the behaviour capSeamAllowed gives
// them.
type capBatch struct {
	s        *Server
	operator bool
	noStore  bool

	users, groups []string
	stale         bool

	loaded bool
	grants []types.CapabilityGrant
	// byKind indexes grants by capability, built ONCE in load(). allowed() used
	// to walk the caller's WHOLE grant set per value, `continue`-ing past every
	// row of another kind — so a spec asking about N egress hosts paid
	// O(N x every grant the caller holds) inside one handler, on a path any
	// authenticated member reaches (POST /runs/preflight). The store round trips
	// were fixed; the CPU was not.
	byKind map[string][]types.CapabilityGrant
	enf    map[string]bool

	// groupDeny memoizes ListGroupDenyGrants PER KIND — the narrow read, not the
	// full table. It is a map because the store call is keyed by capability and a
	// spec can ask about more than one (egress_host, then secret).
	groupDeny map[string][]types.CapabilityGrant
}

// newCapBatch snapshots the cheap, store-free decisions capAllowed makes before
// it ever reads (operator short-circuit, nil store) so the hot loop below is a
// pure in-process match.
func (s *Server) newCapBatch(ctx context.Context) *capBatch {
	b := &capBatch{s: s, noStore: s.cfg.Store == nil}
	if !b.noStore {
		b.operator = s.isOperator(ctx)
		b.users, b.groups, b.stale = capabilitySubjects(ctx)
	}
	return b
}

// load performs the two per-request reads, once.
func (b *capBatch) load(ctx context.Context) error {
	if b.loaded {
		return nil
	}
	grants, err := b.s.cfg.Store.ListCapabilityGrantsFor(ctx, b.users, b.groups)
	if err != nil {
		return fmt.Errorf("api: resolve capability grants: %w", err)
	}
	enf, err := b.s.cfg.Store.GetCapabilityEnforcement(ctx)
	if err != nil {
		return fmt.Errorf("api: read capability enforcement: %w", err)
	}
	b.grants, b.enf, b.loaded = grants, enf, true
	b.byKind = make(map[string][]types.CapabilityGrant, len(capabilityKinds))
	for _, g := range grants {
		b.byKind[g.Capability] = append(b.byKind[g.Capability], g)
	}
	return nil
}

// allowed answers one value, with the SAME rule order capAllowed applies:
// unknown kind errors, an operator and a store-less build short-circuit, a deny
// beats an allow, and a value neither granted nor denied falls through to
// whether the kind is enforced.
func (b *capBatch) allowed(ctx context.Context, kind, value string) (bool, error) {
	if !validCapabilityKind(kind) {
		return false, fmt.Errorf("api: unknown capability kind %q", kind)
	}
	if b.noStore || b.operator {
		return true, nil
	}
	if err := b.load(ctx); err != nil {
		return false, err
	}
	allow := false
	// The kind's OWN rows, not every row the caller holds — same order, same
	// rules, same answer (a grant of another kind could only ever be skipped).
	for _, g := range b.byKind[kind] {
		b.s.capRowsScanned.Add(1)
		if g.Effect == types.CapabilityDeny {
			if capValueOverlaps(kind, g.Value, value) {
				return false, nil // deny is final
			}
			continue
		}
		if capValueMatches(kind, g.Value, value) {
			allow = true
		}
	}
	// UNCONDITIONAL on allow, exactly as capScan is: a group snapshot that
	// cannot be answered may be hiding a group DENY, and a deny beats an allow.
	// Gating this on !allow would let a value the caller holds a user-tier allow
	// for slip past a group deny nobody could evaluate — a widening, in the one
	// direction this resolver exists to refuse.
	if b.stale {
		unresolved, err := b.unresolvableGroupDeny(ctx, kind, value)
		if err != nil {
			return false, err
		}
		if unresolved {
			// Same line capScan logs, for the same reason: the seam that asked
			// writes its own authz.denied, and this says the refusal was about
			// COMPLETENESS rather than a row naming this human.
			slog.Warn("api: capability refused because the caller's group snapshot is unanswerable and a group deny grant of this kind exists",
				"capability", kind, "principal", oidcHumanFromContext(ctx))
			return false, nil
		}
	}
	if allow {
		return true, nil
	}
	return !b.enf[kind], nil
}

// unresolvableGroupDeny is capUnresolvableGroupDeny over a full-table read taken
// ONCE. Loaded only on the stale path, so an answerable caller never pays for it
// — exactly as before.
func (b *capBatch) unresolvableGroupDeny(ctx context.Context, kind, value string) (bool, error) {
	if b.groupDeny == nil {
		b.groupDeny = map[string][]types.CapabilityGrant{}
	}
	grants, loaded := b.groupDeny[kind]
	if !loaded {
		var err error
		// ListGroupDenyGrants, the same NARROW read capUnresolvableGroupDeny
		// makes: the store filters subject_type='group' AND effect='deny' AND
		// capability=$1 in SQL, so nothing is filtered again here and the match
		// is capValueOverlaps alone — one shared matcher, as capScan uses.
		//
		// This memo used to hold the WHOLE capability_grants table, because that
		// is what the predicate read when the batch was written. A concurrent
		// change narrowed the read, and keeping the old one would have quietly
		// reintroduced the full-table scan on exactly the path this batch exists
		// to make O(1) — the two would not have disagreed about the ANSWER, only
		// about the cost, which is the kind of drift nothing fails on.
		if grants, err = b.s.cfg.Store.ListGroupDenyGrants(ctx, kind); err != nil {
			return false, fmt.Errorf("api: resolve capability %q: %w", kind, err)
		}
		b.groupDeny[kind] = grants
	}
	for _, g := range grants {
		if capValueOverlaps(kind, g.Value, value) {
			return true, nil
		}
	}
	return false, nil
}
