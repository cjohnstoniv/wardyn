// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// ─── the store double ─────────────────────────────────────────────────────────

// noGovernanceStore is store.Store with the two governance-resolver reads
// answered as "this deployment has no assignments": no profile matches, no
// group-tier row exists. Embed it INSTEAD OF store.Store in any double whose
// test path resolves a ceiling (effectiveCeiling now runs on every member
// create, on GET /policies/default, and on the member secret listing).
//
// It has to embed store.Store rather than sit beside it: two embeds at the same
// depth would both offer ResolveGovernanceProfile, the selector would be
// ambiguous, and the double would silently stop implementing store.Store.
//
// Answering rather than panicking is the RIGHT default here — a nil-interface
// panic is a useful signal for a method a test never meant to reach, but every
// one of these doubles is modelling a 0.6-shaped deployment, and "no assignment
// ⇒ Config.DefaultPolicy" is exactly what that deployment does.
type noGovernanceStore struct{ store.Store }

func (noGovernanceStore) ResolveGovernanceProfile(context.Context, []string, []string) (*types.GovernanceProfile, types.CapabilitySubjectType, error) {
	return nil, "", store.ErrNotFound
}

func (noGovernanceStore) HasGroupTierAssignments(context.Context) (bool, error) { return false, nil }

// capStore holds grants and enforcement in memory. Its ListCapabilityGrantsFor
// MIRRORS the SQL predicate (store_capabilities.go / its pg test) rather than
// returning everything: subject fan-out is the store's job, and a fake that
// ignored it would let the resolver look correct while handing one member
// another member's grants.
// It also answers the two governance-resolver reads, because effectiveCeiling
// runs on the SAME member paths this double already backs (a member create
// resolves a ceiling before it resolves a capability). The zero value is a
// deployment with no governance assignments at all — ErrNotFound and no
// group-tier rows — which is precisely "byte-for-byte 0.6" and is what keeps
// every pre-existing test in this package meaning what it meant.
type capStore struct {
	store.Store
	grants []types.CapabilityGrant
	enf    map[string]bool
	err    error

	// govProfile, when set, is the profile the resolver returns for ANY caller
	// — the store's own ORDER BY has its own pg test (TestPG_Resolve
	// GovernanceProfile); what the api-side tests need to drive is the ANSWER.
	govProfile *types.GovernanceProfile
	// govTier is the tier that answer matched at. Load-bearing for the
	// stale/truncated scoping: a user-tier winner is served, an all-tier one is
	// refused when a group row exists.
	govTier types.CapabilitySubjectType
	// govHasGroupTier is HasGroupTierAssignments' answer — the gate that keeps
	// the stale-snapshot 403 off deployments that never adopted group profiles.
	govHasGroupTier bool
	// govErr fails BOTH governance reads, for the never-fail-open arm.
	govErr error
}

func (s *capStore) ResolveGovernanceProfile(_ context.Context, _, _ []string) (*types.GovernanceProfile, types.CapabilitySubjectType, error) {
	if s.govErr != nil {
		return nil, "", s.govErr
	}
	if s.govProfile == nil {
		return nil, "", store.ErrNotFound
	}
	return s.govProfile, s.govTier, nil
}

func (s *capStore) HasGroupTierAssignments(context.Context) (bool, error) {
	if s.govErr != nil {
		return false, s.govErr
	}
	return s.govHasGroupTier, nil
}

func (s *capStore) ListCapabilityGrantsFor(_ context.Context, users, groups []string) ([]types.CapabilityGrant, error) {
	if s.err != nil {
		return nil, s.err
	}
	out := []types.CapabilityGrant{}
	for _, g := range s.grants {
		switch g.SubjectType {
		case types.CapabilitySubjectAll:
			out = append(out, g)
		case types.CapabilitySubjectUser:
			if slices.Contains(users, g.Subject) {
				out = append(out, g)
			}
		case types.CapabilitySubjectGroup:
			if slices.Contains(groups, g.Subject) {
				out = append(out, g)
			}
		}
	}
	return out, nil
}

func (s *capStore) GetCapabilityEnforcement(context.Context) (map[string]bool, error) {
	if s.err != nil {
		return nil, s.err
	}
	if s.enf == nil {
		return map[string]bool{}, nil
	}
	return s.enf, nil
}

// ─── fixtures ─────────────────────────────────────────────────────────────────

const (
	capSub   = "sub-bob"
	capEmail = "bob@corp.example"
)

// memberCtx is what humanOrAdminAuth publishes for a signed-in MEMBER, group
// snapshot included. Passing nil groups models a pre-0.6 cookie.
func memberCtx(groups []string) context.Context {
	return withOIDCGroups(operatorCtx(capSub, capEmail, oidc.RoleMember), groups)
}

func grant(st types.CapabilitySubjectType, subject, kind, value string, effect types.CapabilityEffect) types.CapabilityGrant {
	return types.CapabilityGrant{
		ID: uuid.New(), SubjectType: st, Subject: subject,
		Capability: kind, Value: value, Effect: effect,
	}
}

func capServer(st store.Store) *Server { return &Server{cfg: Config{Store: st}} }

// ─── the matrix ───────────────────────────────────────────────────────────────

// TestCapAllowedMatrix walks the precedence rules capAllowed documents. Each
// row names the real-world outcome, because every one of them is either a
// member locked out of something they should have or a member holding
// something they should not.
func TestCapAllowedMatrix(t *testing.T) {
	allow, deny := types.CapabilityAllow, types.CapabilityDeny
	tests := []struct {
		name    string
		grants  []types.CapabilityGrant
		enf     map[string]bool
		ctx     context.Context
		kind    string
		value   string
		want    bool
		wantErr bool
	}{
		// Back-compat: an upgraded 0.5 deployment has no grants and no
		// enforcement rows, and must behave exactly as it did before.
		{
			name: "no grants, kind unenforced: allowed (this is the whole upgrade story)",
			ctx:  memberCtx(nil), kind: capEgressHost, value: "pypi.org", want: true,
		},
		{
			name: "no grants, kind enforced: denied",
			enf:  map[string]bool{capEgressHost: true},
			ctx:  memberCtx(nil), kind: capEgressHost, value: "pypi.org", want: false,
		},
		{
			name: "enforcement is PER KIND: secret on does not gate egress_host",
			enf:  map[string]bool{capSecret: true},
			ctx:  memberCtx(nil), kind: capEgressHost, value: "pypi.org", want: true,
		},

		// Subject fan-out: a grant hits through sub, email, group, or `all`.
		{
			name:   "allow on the caller's sub",
			grants: []types.CapabilityGrant{grant(types.CapabilitySubjectUser, capSub, capEgressHost, "pypi.org", allow)},
			enf:    map[string]bool{capEgressHost: true},
			ctx:    memberCtx(nil), kind: capEgressHost, value: "pypi.org", want: true,
		},
		{
			name:   "allow on the caller's email — the identity an admin can actually write down",
			grants: []types.CapabilityGrant{grant(types.CapabilitySubjectUser, capEmail, capEgressHost, "pypi.org", allow)},
			enf:    map[string]bool{capEgressHost: true},
			ctx:    memberCtx(nil), kind: capEgressHost, value: "pypi.org", want: true,
		},
		{
			name:   "allow on a group from the login snapshot",
			grants: []types.CapabilityGrant{grant(types.CapabilitySubjectGroup, "eng", capEgressHost, "pypi.org", allow)},
			enf:    map[string]bool{capEgressHost: true},
			ctx:    memberCtx([]string{"eng"}), kind: capEgressHost, value: "pypi.org", want: true,
		},
		{
			name:   "allow on `all` — the baseline for an IdP with no usable groups claim",
			grants: []types.CapabilityGrant{grant(types.CapabilitySubjectAll, "", capEgressHost, "pypi.org", allow)},
			enf:    map[string]bool{capEgressHost: true},
			ctx:    memberCtx(nil), kind: capEgressHost, value: "pypi.org", want: true,
		},
		{
			name:   "somebody else's grant does not apply",
			grants: []types.CapabilityGrant{grant(types.CapabilitySubjectUser, "sub-mallory", capEgressHost, "pypi.org", allow)},
			enf:    map[string]bool{capEgressHost: true},
			ctx:    memberCtx(nil), kind: capEgressHost, value: "pypi.org", want: false,
		},
		{
			name:   "a group the caller is not in does not apply",
			grants: []types.CapabilityGrant{grant(types.CapabilitySubjectGroup, "finance", capEgressHost, "pypi.org", allow)},
			enf:    map[string]bool{capEgressHost: true},
			ctx:    memberCtx([]string{"eng"}), kind: capEgressHost, value: "pypi.org", want: false,
		},

		// Precedence. Deny beats allow no matter which subject carried which.
		{
			name: "deny beats an allow on the very same row's subject",
			grants: []types.CapabilityGrant{
				grant(types.CapabilitySubjectUser, capSub, capEgressHost, "pypi.org", allow),
				grant(types.CapabilitySubjectAll, "", capEgressHost, "pypi.org", deny),
			},
			enf: map[string]bool{capEgressHost: true},
			ctx: memberCtx(nil), kind: capEgressHost, value: "pypi.org", want: false,
		},
		{
			name: "a USER allow does not override a GROUP deny — there is no user-wins rule",
			grants: []types.CapabilityGrant{
				grant(types.CapabilitySubjectUser, capEmail, capEgressHost, "pypi.org", allow),
				grant(types.CapabilitySubjectGroup, "eng", capEgressHost, "pypi.org", deny),
			},
			enf: map[string]bool{capEgressHost: true},
			ctx: memberCtx([]string{"eng"}), kind: capEgressHost, value: "pypi.org", want: false,
		},
		{
			// The adoption on-ramp: blacklist one host for one contractor
			// without flipping the deployment fail-closed.
			name:   "deny applies with the switch OFF, and only to the named value",
			grants: []types.CapabilityGrant{grant(types.CapabilitySubjectUser, capSub, capEgressHost, "evil.example.com", deny)},
			ctx:    memberCtx(nil), kind: capEgressHost, value: "evil.example.com", want: false,
		},
		{
			name:   "the same deny leaves every other host alone",
			grants: []types.CapabilityGrant{grant(types.CapabilitySubjectUser, capSub, capEgressHost, "evil.example.com", deny)},
			ctx:    memberCtx(nil), kind: capEgressHost, value: "pypi.org", want: true,
		},

		// Value matching.
		{
			name:   "the * wildcard covers every value of its kind",
			grants: []types.CapabilityGrant{grant(types.CapabilitySubjectAll, "", capSecret, capWildcard, allow)},
			enf:    map[string]bool{capSecret: true},
			ctx:    memberCtx(nil), kind: capSecret, value: "anything", want: true,
		},
		{
			name:   "egress_host: a *.suffix grant covers a subdomain",
			grants: []types.CapabilityGrant{grant(types.CapabilitySubjectAll, "", capEgressHost, "*.example.com", allow)},
			enf:    map[string]bool{capEgressHost: true},
			ctx:    memberCtx(nil), kind: capEgressHost, value: "api.example.com", want: true,
		},
		{
			name:   "egress_host: a port on the requested host does not dodge the match",
			grants: []types.CapabilityGrant{grant(types.CapabilitySubjectAll, "", capEgressHost, "*.example.com", allow)},
			enf:    map[string]bool{capEgressHost: true},
			ctx:    memberCtx(nil), kind: capEgressHost, value: "api.example.com:443", want: true,
		},
		{
			name:   "egress_host: a port on the GRANT does not dodge the match either",
			grants: []types.CapabilityGrant{grant(types.CapabilitySubjectAll, "", capEgressHost, "pypi.org:443", allow)},
			enf:    map[string]bool{capEgressHost: true},
			ctx:    memberCtx(nil), kind: capEgressHost, value: "pypi.org", want: true,
		},
		{
			name:   "egress_host: a suffix grant does not cover an unrelated host",
			grants: []types.CapabilityGrant{grant(types.CapabilitySubjectAll, "", capEgressHost, "*.example.com", allow)},
			enf:    map[string]bool{capEgressHost: true},
			ctx:    memberCtx(nil), kind: capEgressHost, value: "example.com.evil.net", want: false,
		},
		{
			name:   "secret: a near-miss name is not a match",
			grants: []types.CapabilityGrant{grant(types.CapabilitySubjectAll, "", capSecret, "prod-db", allow)},
			enf:    map[string]bool{capSecret: true},
			ctx:    memberCtx(nil), kind: capSecret, value: "prod-db-2", want: false,
		},
		{
			name:   "a grant of a DIFFERENT kind at the same value does not carry over",
			grants: []types.CapabilityGrant{grant(types.CapabilitySubjectAll, "", capSecret, "shared", allow)},
			enf:    map[string]bool{capSecret: true, capImage: true},
			ctx:    memberCtx(nil), kind: capImage, value: "shared", want: false,
		},

		// Exemptions.
		{
			name:   "an ADMIN session is exempt even from a deny written against them",
			grants: []types.CapabilityGrant{grant(types.CapabilitySubjectUser, capSub, capEgressHost, "pypi.org", deny)},
			enf:    map[string]bool{capEgressHost: true},
			ctx:    withOIDCGroups(operatorCtx(capSub, capEmail, oidc.RoleAdmin), nil),
			kind:   capEgressHost, value: "pypi.org", want: true,
		},
		{
			name: "the admin token carries no per-human role to demote: exempt",
			enf:  map[string]bool{capEgressHost: true},
			ctx:  context.Background(), kind: capEgressHost, value: "pypi.org", want: true,
		},
		{
			name: "local mode is exempt",
			enf:  map[string]bool{capEgressHost: true},
			ctx:  withLocalPrincipal(context.Background(), "local:alice"),
			kind: capEgressHost, value: "pypi.org", want: true,
		},

		// A pre-0.6 cookie loses its group grants until the human logs in
		// again. That is the published ceiling, not a bug — but it must never
		// be silent, which is what groups_snapshot_stale is for (see
		// TestCapabilitySubjectsStaleSnapshot).
		{
			name:   "a nil group snapshot cannot satisfy a group grant",
			grants: []types.CapabilityGrant{grant(types.CapabilitySubjectGroup, "eng", capEgressHost, "pypi.org", allow)},
			enf:    map[string]bool{capEgressHost: true},
			ctx:    memberCtx(nil), kind: capEgressHost, value: "pypi.org", want: false,
		},
		{
			name:   "...but their USER grants still work, which is the escape hatch to tell them about",
			grants: []types.CapabilityGrant{grant(types.CapabilitySubjectUser, capEmail, capEgressHost, "pypi.org", allow)},
			enf:    map[string]bool{capEgressHost: true},
			ctx:    memberCtx(nil), kind: capEgressHost, value: "pypi.org", want: true,
		},

		// Failures must never read as permission.
		{
			name: "an unknown kind is an error, not an allow",
			ctx:  memberCtx(nil), kind: "devcontainer_repo", value: "x", want: false, wantErr: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv := capServer(&capStore{grants: tc.grants, enf: tc.enf})
			got, err := srv.capAllowed(tc.ctx, tc.kind, tc.value)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr = %v", err, tc.wantErr)
			}
			if got != tc.want {
				t.Errorf("capAllowed(%s, %q) = %v, want %v", tc.kind, tc.value, got, tc.want)
			}
		})
	}
}

// TestCapAllowedFailsClosedOnStoreTrouble: a resolver that cannot read the
// grants must say so, so the caller answers 500. Returning true would hand out
// a permission on the strength of a database hiccup; returning false silently
// would look like a legitimate denial and send an admin hunting for a grant
// that is already there.
func TestCapAllowedFailsClosedOnStoreTrouble(t *testing.T) {
	boom := errors.New("connection refused")
	for _, tc := range []struct {
		name string
		srv  *Server
	}{
		{"store error", capServer(&capStore{err: boom})},
		{"no store wired at all", capServer(nil)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := tc.srv.capAllowed(memberCtx(nil), capEgressHost, "pypi.org")
			if err == nil {
				t.Fatal("no error; a failed resolution must not be answerable")
			}
			if got {
				t.Error("capAllowed returned true on a failed read — a permission granted by an outage")
			}
		})
	}
}

// TestCapAllowedEnforcementReadIsSkippedOnAllow pins the short-circuit: once a
// matching allow is found the switch does not matter, so the second query is
// not made. Cheap to keep true, and it is the difference between one and two
// round trips on the hot path.
func TestCapAllowedEnforcementReadIsSkippedOnAllow(t *testing.T) {
	st := &capStore{
		grants: []types.CapabilityGrant{
			grant(types.CapabilitySubjectAll, "", capEgressHost, "pypi.org", types.CapabilityAllow),
		},
		// GetCapabilityEnforcement would fail; reaching it fails the test.
		err: nil,
	}
	srv := capServer(st)
	ok, err := srv.capAllowed(memberCtx(nil), capEgressHost, "pypi.org")
	if err != nil || !ok {
		t.Fatalf("capAllowed = %v, %v; want true, nil", ok, err)
	}
	// Now flip the switch on: the answer must not change, since the allow
	// already settled it.
	st.enf = map[string]bool{capEgressHost: true}
	if ok, err := srv.capAllowed(memberCtx(nil), capEgressHost, "pypi.org"); err != nil || !ok {
		t.Fatalf("capAllowed with the switch on = %v, %v; want true, nil", ok, err)
	}
}

// ─── subjects ─────────────────────────────────────────────────────────────────

// TestCapabilitySubjects: both identities are offered, lowercased, and never
// duplicated — an admin who wrote the grant against the email must get the same
// answer as one who wrote it against the sub.
func TestCapabilitySubjects(t *testing.T) {
	ctx := withOIDCGroups(operatorCtx("Sub-BOB", "BOB@Corp.Example", oidc.RoleMember), []string{"eng"})
	users, groups, stale := capabilitySubjects(ctx)
	if !slices.Equal(users, []string{"sub-bob", "bob@corp.example"}) {
		t.Errorf("users = %v, want the lowercased sub then email", users)
	}
	if !slices.Equal(groups, []string{"eng"}) {
		t.Errorf("groups = %v, want [eng]", groups)
	}
	if stale {
		t.Error("stale = true for a session that carries a snapshot")
	}

	// A session whose sub and email are the same string must not offer it twice
	// — a duplicated subject would double-count nothing today but makes the
	// eventual /me/capabilities listing lie about where a grant came from.
	same := withOIDCGroups(operatorCtx("bob@corp.example", "bob@corp.example", oidc.RoleMember), []string{})
	users, _, _ = capabilitySubjects(same)
	if len(users) != 1 {
		t.Errorf("users = %v, want one entry when sub and email are identical", users)
	}
}

// TestCapabilitySubjectsStaleSnapshot is the nil-vs-empty discriminator, end to
// end from the context key. Empty means the IdP had nothing to say; nil means
// this cookie predates the field, and telling those apart is the only way a
// member can be told to log in again instead of being left to wonder why their
// group grant does nothing.
func TestCapabilitySubjectsStaleSnapshot(t *testing.T) {
	if _, _, stale := capabilitySubjects(memberCtx(nil)); !stale {
		t.Error("a nil snapshot did not report stale; a pre-0.6 cookie would silently lose its group grants")
	}
	if _, _, stale := capabilitySubjects(memberCtx([]string{})); stale {
		t.Error("an empty snapshot reported stale; this human simply has no groups")
	}
}

// ─── the closed kind set ──────────────────────────────────────────────────────

// TestCapabilityKindsAreTheClosedSet: with no CHECK in the schema, this slice
// IS the validation, so it has to stay in step with the console's own list
// (permissions-copy.ts CAPABILITY_KINDS) and reject everything else — including
// devcontainer_repo, which is deliberately NOT a capability: it executes
// attacker-authored build config and stays unconditionally admin-only.
func TestCapabilityKindsAreTheClosedSet(t *testing.T) {
	want := []string{"egress_host", "secret", "workspace", "image"}
	if !slices.Equal(capabilityKinds, want) {
		t.Errorf("capabilityKinds = %v, want %v (and ui/src/app/lib/permissions-copy.ts must match)", capabilityKinds, want)
	}
	for _, k := range want {
		if !validCapabilityKind(k) {
			t.Errorf("validCapabilityKind(%q) = false", k)
		}
	}
	for _, k := range []string{"", "devcontainer_repo", "EGRESS_HOST", "egress_host "} {
		if validCapabilityKind(k) {
			t.Errorf("validCapabilityKind(%q) = true; the set is closed and exact", k)
		}
	}
}

// TestCapValueMatchesIsExactOffTheHostLane: only egress_host has a defensible
// subdomain semantics. A secret name, a workspace uuid and an image ref are
// identifiers where a prefix, a suffix, or a case fold must never match.
func TestCapValueMatchesIsExactOffTheHostLane(t *testing.T) {
	for _, kind := range []string{capSecret, capWorkspace, capImage} {
		if capValueMatches(kind, "*.corp", "api.corp") {
			t.Errorf("%s: a wildcard-looking grant matched a suffix; only egress_host may do that", kind)
		}
		if capValueMatches(kind, "Prod-DB", "prod-db") {
			t.Errorf("%s: matched across a case fold", kind)
		}
		if !capValueMatches(kind, " prod-db ", "prod-db") {
			t.Errorf("%s: surrounding whitespace on a stored grant must not stop it matching", kind)
		}
	}
	// The wildcard is spelled the same for every kind so an admin learns one
	// syntax, not four.
	for _, kind := range capabilityKinds {
		if !capValueMatches(kind, capWildcard, strings.Repeat("x", 40)) {
			t.Errorf("%s: the * wildcard did not match", kind)
		}
	}
}

// TestCapDenyOverlapsWildcardWant pins the DENY-vs-ALLOW asymmetry
// capValueOverlaps exists for: a deny only has to OVERLAP the requested value,
// while an allow still has to COVER it. Before this, a member whose allowlist
// entry was itself a wildcard ("*.example.com") kept it under a deny for a host
// underneath it ("secret.example.com") — the deny row protected nothing.
func TestCapDenyOverlapsWildcardWant(t *testing.T) {
	tests := []struct {
		name            string
		denyValue, want string
		wantDeny        bool
	}{
		{"want wildcard swallows the denied host", "secret.example.com", "*.example.com", true},
		{"deny wildcard covers the wanted host", "*.corp", "evil.corp", true},
		{"want wildcard swallows the denied subdomain", "a.b", "*.b", true},
		{"exact host, exact deny", "example.com", "example.com", true},
		{"want * asks for everything, so every deny bites", "secret.example.com", "*", true},
		// "*.example.com" is every host UNDER example.com, never example.com
		// itself (the proxy matcher's own label-boundary rule), so these two
		// sets do not intersect and the deny must not fire.
		{"deny on the bare apex, want only its subdomains", "example.com", "*.example.com", false},
		{"unrelated hosts", "secret.example.com", "pypi.org", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := capValueOverlaps(capEgressHost, tc.denyValue, tc.want); got != tc.wantDeny {
				t.Fatalf("capValueOverlaps(deny %q, want %q) = %v, want %v", tc.denyValue, tc.want, got, tc.wantDeny)
			}
			// End to end through the resolver: a matching deny is final,
			// with or without the kind's enforcement switch on — and a
			// non-matching one leaves the unenforced default (allowed) alone.
			enfs := []map[string]bool{nil, {capEgressHost: true}}
			if !tc.wantDeny {
				enfs = enfs[:1] // no covering allow for every want, so only the unenforced lane is decidable
			}
			for _, enf := range enfs {
				st := &capStore{
					grants: []types.CapabilityGrant{
						grant(types.CapabilitySubjectAll, "", capEgressHost, tc.denyValue, types.CapabilityDeny),
						grant(types.CapabilitySubjectAll, "", capEgressHost, "*.example.com", types.CapabilityAllow),
						grant(types.CapabilitySubjectAll, "", capEgressHost, "*.corp", types.CapabilityAllow),
					},
					enf: enf,
				}
				ok, err := capServer(st).capAllowed(memberCtx(nil), capEgressHost, tc.want)
				if err != nil {
					t.Fatalf("capAllowed: %v", err)
				}
				if ok == tc.wantDeny {
					t.Fatalf("capAllowed(%q) = %v with enforcement %v; deny row %q", tc.want, ok, enf, tc.denyValue)
				}
			}
		})
	}
}

// TestCapAllowStillHasToCoverTheWant is the other half: widening the DENY
// direction must not widen the ALLOW one. An allow for one host under a
// wildcard does NOT authorize the whole wildcard.
func TestCapAllowStillHasToCoverTheWant(t *testing.T) {
	st := &capStore{
		grants: []types.CapabilityGrant{
			grant(types.CapabilitySubjectAll, "", capEgressHost, "a.example.com", types.CapabilityAllow),
		},
		enf: map[string]bool{capEgressHost: true},
	}
	srv := capServer(st)
	if ok, err := srv.capAllowed(memberCtx(nil), capEgressHost, "*.example.com"); err != nil || ok {
		t.Fatalf("capAllowed(*.example.com) = %v, %v; an allow for one host must not cover the wildcard", ok, err)
	}
	if ok, err := srv.capAllowed(memberCtx(nil), capEgressHost, "a.example.com"); err != nil || !ok {
		t.Fatalf("capAllowed(a.example.com) = %v, %v; the exact allow must still hold", ok, err)
	}
}
