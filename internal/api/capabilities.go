// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// ─── the closed kind set ──────────────────────────────────────────────────────
//
// Four kinds, and this slice is the ONLY place the set is written down —
// migration 0042 deliberately puts no CHECK on capability_grants.capability, so
// a fifth kind is a constant here plus its enforcement call site, with no DDL.
// The console's own list (ui/src/app/lib/permissions-copy.ts CAPABILITY_KINDS)
// mirrors these ids and must not drift.
//
// Three of the four NARROW what a member may already do; capImage WIDENS (a
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
)

// capabilityKinds is the closed set, in the order the admin surface shows them.
var capabilityKinds = []string{capEgressHost, capSecret, capWorkspace, capImage}

// validCapabilityKind reports whether kind is one of the four. The API write
// boundary uses it in place of the CHECK the schema deliberately does not have.
func validCapabilityKind(kind string) bool { return slices.Contains(capabilityKinds, kind) }

// capWildcard matches every value of its kind. Spelled the same for all four so
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
// empty when the IdP sent nothing usable. stale reports the former: the caller
// has a session but no answerable group identity, so their group grants cannot
// be evaluated until they log in again. Callers surface that; they must not
// silently treat it as "no groups".
func capabilitySubjects(ctx context.Context) (users, groups []string, stale bool) {
	if sub := oidcHumanFromContext(ctx); sub != "" {
		users = append(users, strings.ToLower(sub))
	}
	if email := strings.ToLower(strings.TrimSpace(oidcEmailFromContext(ctx))); email != "" && !slices.Contains(users, email) {
		users = append(users, email)
	}
	groups = oidcGroupsFromContext(ctx)
	return users, groups, groups == nil
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
func (s *Server) capScan(ctx context.Context, kind, value string) (deny, allow bool, err error) {
	users, groups, _ := capabilitySubjects(ctx)
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
	return false, allow, nil
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
// workspace uuid and an image ref are all identifiers where a near-miss must
// not match; only egress hosts have a defensible subdomain semantics.
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
