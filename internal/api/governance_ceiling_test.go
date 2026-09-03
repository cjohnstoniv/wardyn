// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// ─── fixtures ─────────────────────────────────────────────────────────────────

// govDeployment is the DEPLOYMENT ceiling (Config.DefaultPolicy) every case
// below falls through to when no assignment applies. Deliberately WIDER than
// govProfileSpec: the whole feature is "this principal is narrower than the
// deployment", and a fixture where the two are equal could not tell a routed
// site from an unrouted one.
func govDeployment() types.RunPolicySpec {
	return types.RunPolicySpec{
		AllowedDomains:      []string{"api.anthropic.com", "pypi.org", "wide.example"},
		MinConfinementClass: types.CC1,
		FirstUseApproval:    types.FirstUseWaitForReview,
	}
}

// govProfileSpec is the assigned ceiling: narrower egress, a higher confinement
// floor, a stricter first-use posture.
func govProfileSpec() types.RunPolicySpec {
	return types.RunPolicySpec{
		AllowedDomains:      []string{"api.anthropic.com"},
		DeniedDomains:       []string{"corp.internal"},
		MinConfinementClass: types.CC2,
		FirstUseApproval:    types.FirstUseAlwaysDeny,
	}
}

func govProfile(name string) *types.GovernanceProfile {
	return &types.GovernanceProfile{
		ID: uuid.New(), Name: name, Ceiling: govProfileSpec(),
		Limits: types.GovernanceLimits{DenyInteractive: true},
	}
}

// govServer builds a Server whose DefaultPolicy is the wide deployment ceiling
// and whose store is st (nil for the no-store arm).
func govServer(st *capStore) *Server {
	cfg := Config{DefaultPolicy: govDeployment()}
	if st != nil {
		cfg.Store = st
	}
	return &Server{cfg: cfg}
}

// govMemberCtx is what humanOrAdminAuth publishes for a signed-in MEMBER:
// identity, group snapshot, and the snapshot's PF-26 completeness bit.
func govMemberCtx(groups []string, truncated bool) context.Context {
	return withOIDCGroupsTruncated(
		withOIDCGroups(operatorCtx("sub-gov-bob", "bob@corp.example", oidc.RoleMember), groups),
		truncated)
}

// ─── the precedence table ─────────────────────────────────────────────────────

// TestEffectiveCeilingPrecedence walks effectiveCeiling's resolution order. It
// is the pin on the one function every routed site now trusts, and each case is
// a decision that could have gone the other way — the counterfactual is named
// per group.
//
// The store's own ORDER BY (user > group > all, sub over email, priority, name)
// is NOT re-tested here: it is SQL, and internal/store's
// TestPG_ResolveGovernanceProfile owns it. What this owns is everything the
// resolver decides AROUND that read — who skips it, what a failure means, and
// the one shape in which an unanswerable group snapshot is a refusal.
func TestEffectiveCeilingPrecedence(t *testing.T) {
	t.Run("operator short-circuits to the deployment ceiling with no store read", func(t *testing.T) {
		// A nil store would PANIC on any governance read (store.Store is a nil
		// interface here), so this arm proves the short-circuit really happens
		// before the read rather than merely returning the right value — and it
		// is what keeps the ~30 nil-store doubles in this package alive.
		// Counterfactual: drop `s.isOperator(ctx) ||` and this panics.
		srv := govServer(nil)
		got, err := srv.effectiveCeiling(operatorCtx("sub-admin", "admin@corp.example", oidc.RoleAdmin))
		if err != nil {
			t.Fatalf("effectiveCeiling(admin): %v", err)
		}
		if got.Profile != nil {
			t.Errorf("an operator resolved to profile %q; the ceiling-setting authority is not bounded by one", got.Profile.Name)
		}
		if len(got.Spec.AllowedDomains) != len(govDeployment().AllowedDomains) {
			t.Errorf("operator ceiling = %v, want the deployment's %v", got.Spec.AllowedDomains, govDeployment().AllowedDomains)
		}
	})

	t.Run("operator short-circuit fires even with an assignment that would match", func(t *testing.T) {
		// The dangerous mis-implementation is resolving first and short-circuiting
		// after: an admin would then be bounded by a profile someone assigned to
		// `all`, and the deployment's own authority would be capped by a row a
		// security admin can write. Counterfactual: move the isOperator check
		// below the resolve and this returns "everyone".
		st := &capStore{govProfile: govProfile("everyone"), govTier: types.CapabilitySubjectAll}
		got, err := govServer(st).effectiveCeiling(operatorCtx("sub-admin", "admin@corp.example", oidc.RoleAdmin))
		if err != nil {
			t.Fatalf("effectiveCeiling(admin): %v", err)
		}
		if got.Profile != nil {
			t.Errorf("an `all` assignment bound the super admin (profile %q)", got.Profile.Name)
		}
	})

	t.Run("nil store falls through to the deployment ceiling", func(t *testing.T) {
		// A build with NO store holds no assignments, so there is nothing to
		// resolve and the answer is 0.6's answer. Deliberately NOT the error
		// capAllowed returns for a nil store — see effectiveCeiling's step 2.
		got, err := govServer(nil).effectiveCeiling(govMemberCtx([]string{"eng"}, false))
		if err != nil {
			t.Fatalf("effectiveCeiling(nil store): %v", err)
		}
		if got.Profile != nil || len(got.Spec.AllowedDomains) != 3 {
			t.Errorf("nil store resolved to %+v, want the deployment ceiling", got.Spec.AllowedDomains)
		}
	})

	t.Run("store error is an ERROR, never the deployment ceiling", func(t *testing.T) {
		// THE fail-open test (F-47 analog). The adjacent GetSiteConfig idiom
		// logs and carries on with a zero value; copying it here would mean a
		// database hiccup silently widens every walled principal back to the
		// deployment ceiling, with the run proceeding normally and nothing in
		// the audit naming which ceiling it ran under.
		// Counterfactual: `return deployment, nil` on the error path and this
		// case reports a 200-shaped success carrying wide.example.
		boom := errors.New("pg: connection refused")
		st := &capStore{govErr: boom}
		got, err := govServer(st).effectiveCeiling(govMemberCtx([]string{"eng"}, false))
		if err == nil {
			t.Fatalf("a store failure resolved to a ceiling (%v) instead of erroring", got.Spec.AllowedDomains)
		}
		if !errors.Is(err, boom) {
			t.Errorf("error = %v, want the store failure wrapped", err)
		}
		if code := ceilingErrorStatus(err); code != http.StatusInternalServerError {
			t.Errorf("store failure maps to %d, want 500 — it is not the caller's fault and not a permission answer", code)
		}
		// And the HTTP shape a routed site would produce.
		w := httptest.NewRecorder()
		writeCeilingError(w, err)
		if w.Code != http.StatusInternalServerError {
			t.Errorf("writeCeilingError(store failure) = %d, want 500", w.Code)
		}
	})

	t.Run("no assignment matched falls through to the deployment ceiling", func(t *testing.T) {
		// ErrNotFound is the absent-row doctrine: no assignment, no behaviour
		// change, byte-for-byte 0.6. Asserted for BOTH shapes a store can say it
		// in — ErrNotFound, and a bare (nil, "", nil) — because the second one
		// must mean the same thing rather than dereferencing a nil profile.
		for _, st := range []*capStore{
			{},                             // ErrNotFound
			{govProfile: nil, govTier: ""}, // the same, spelled as a nil answer
		} {
			got, err := govServer(st).effectiveCeiling(govMemberCtx([]string{"eng"}, false))
			if err != nil {
				t.Fatalf("effectiveCeiling: %v", err)
			}
			if got.Profile != nil {
				t.Errorf("no assignment produced profile %q; it must read as the deployment ceiling", got.Profile.Name)
			}
			if len(got.Spec.AllowedDomains) != 3 {
				t.Errorf("ceiling = %v, want the deployment's", got.Spec.AllowedDomains)
			}
			if got.Limits != (types.GovernanceLimits{}) {
				t.Errorf("limits = %+v for an unassigned member, want the zero (unrestricted) value", got.Limits)
			}
		}
	})

	t.Run("a matched profile replaces the deployment ceiling and is cloned", func(t *testing.T) {
		p := govProfile("walled")
		st := &capStore{govProfile: p, govTier: types.CapabilitySubjectGroup}
		srv := govServer(st)
		a, err := srv.effectiveCeiling(govMemberCtx([]string{"eng"}, false))
		if err != nil {
			t.Fatalf("effectiveCeiling: %v", err)
		}
		if a.Profile == nil || a.Profile.Name != "walled" {
			t.Fatalf("profile = %+v, want the assigned one", a.Profile)
		}
		if !a.Limits.DenyInteractive {
			t.Error("limits did not ride along with the profile")
		}
		// REPLACE, not compose: the deployment's extra hosts must be gone, not
		// merged in. Counterfactual: fold the two through composer.Clamp and
		// this ceiling grows pypi.org/wide.example back.
		if len(a.Spec.AllowedDomains) != 1 || a.Spec.AllowedDomains[0] != "api.anthropic.com" {
			t.Errorf("allowed_domains = %v, want ONLY the profile's — an assigned profile REPLACES DefaultPolicy", a.Spec.AllowedDomains)
		}
		// Cloned: two resolutions must not share a backing array, or one run's
		// egress union corrupts the next (resolvePolicy's own aliasing bug, one
		// layer up).
		b, err := srv.effectiveCeiling(govMemberCtx([]string{"eng"}, false))
		if err != nil {
			t.Fatalf("effectiveCeiling: %v", err)
		}
		if &a.Spec.AllowedDomains[0] == &b.Spec.AllowedDomains[0] {
			t.Error("two resolutions share one AllowedDomains backing array — a per-run append will corrupt a sibling run")
		}
	})

	// ─── the stale/truncated 403 and its scoping ──────────────────────────────
	//
	// Four cases, and the three that DO NOT refuse are the point: a blanket
	// refusal here would lock every pre-0.6 cookie out of every deployment,
	// including the ones that never adopted governance profiles at all.

	t.Run("stale snapshot + a group-tier row + no user match = 403", func(t *testing.T) {
		// nil groups is a pre-0.6 cookie: the group identity is unanswerable, a
		// group row exists that might apply, and nothing user-tier settles it.
		// Serving ANY ceiling here is a guess, and the wrong guess is the
		// widening one. Counterfactual: fall through to DefaultPolicy and a
		// member simply sheds their group's profile, silently.
		st := &capStore{govHasGroupTier: true} // resolves ErrNotFound on users alone
		_, err := govServer(st).effectiveCeiling(govMemberCtx(nil, false))
		if !errors.Is(err, errGroupsSnapshotStale) {
			t.Fatalf("err = %v, want errGroupsSnapshotStale", err)
		}
		if code := ceilingErrorStatus(err); code != http.StatusForbidden {
			t.Errorf("stale snapshot maps to %d, want 403", code)
		}
		w := httptest.NewRecorder()
		writeCeilingError(w, err)
		if w.Code != http.StatusForbidden {
			t.Errorf("writeCeilingError(stale) = %d, want 403", w.Code)
		}
		if body := w.Body.String(); !containsAll(body, "groups_snapshot_stale", "sign in again") {
			t.Errorf("403 body does not name the condition and the remedy: %s", body)
		}
	})

	t.Run("TRUNCATED snapshot is treated exactly like a stale one", func(t *testing.T) {
		// PF-26. The snapshot is present and non-nil — it just is not all of
		// them, and the group whose assignment walls this member is as likely to
		// have been dropped as any other. Counterfactual: read only
		// capabilitySubjects' `stale` and this resolves happily to the
		// deployment ceiling, which is the tier evaporating in silence.
		st := &capStore{govHasGroupTier: true}
		_, err := govServer(st).effectiveCeiling(govMemberCtx([]string{"a-team"}, true))
		if !errors.Is(err, errGroupsSnapshotStale) {
			t.Fatalf("a truncated snapshot resolved (err = %v); truncated must be as unanswerable as nil", err)
		}
	})

	t.Run("a USER-tier match suppresses the refusal", func(t *testing.T) {
		// PF-25. user > group > all, so an explicitly-named principal's ceiling
		// is fully determined no matter what their groups are — refusing would
		// lock out exactly the people an admin took the trouble to name.
		// Counterfactual: refuse whenever a group-tier row exists and this
		// member is locked out by a snapshot that could not have changed their
		// answer.
		st := &capStore{
			govProfile: govProfile("bob-only"), govTier: types.CapabilitySubjectUser,
			govHasGroupTier: true,
		}
		for _, tc := range []struct {
			name      string
			groups    []string
			truncated bool
		}{
			{"stale (pre-0.6 cookie)", nil, false},
			{"truncated snapshot", []string{"a-team"}, true},
		} {
			got, err := govServer(st).effectiveCeiling(govMemberCtx(tc.groups, tc.truncated))
			if err != nil {
				t.Fatalf("%s: a user-tier match was refused: %v", tc.name, err)
			}
			if got.Profile == nil || got.Profile.Name != "bob-only" {
				t.Errorf("%s: profile = %+v, want the user-tier assignment", tc.name, got.Profile)
			}
		}
	})

	t.Run("an ALL-tier match does NOT suppress the refusal", func(t *testing.T) {
		// The case the tier return value exists for. An `all` row sits BELOW the
		// group tier, so a group row could have outranked it and we cannot see
		// whether one did — serving the all-tier answer would be a guess in the
		// widening direction. Indistinguishable from the user-tier case by the
		// profile alone, which is why re-deriving the tier in Go was never an
		// option. Counterfactual: treat any non-ErrNotFound resolve as
		// determinative and this serves "everyone" over a group's stricter row.
		st := &capStore{
			govProfile: govProfile("everyone"), govTier: types.CapabilitySubjectAll,
			govHasGroupTier: true,
		}
		if _, err := govServer(st).effectiveCeiling(govMemberCtx(nil, false)); !errors.Is(err, errGroupsSnapshotStale) {
			t.Fatalf("an all-tier match settled an unanswerable group snapshot (err = %v)", err)
		}
	})

	t.Run("zero group-tier rows suppress the refusal", func(t *testing.T) {
		// PF-21. With no group-tier row there is nothing an unknown group could
		// have matched, so nothing a nil snapshot can be hiding. Refusing here
		// would break "no assignment ⇒ byte-for-byte today" for every
		// pre-upgrade session on every deployment that never adopted group
		// profiles — which, on the day this ships, is all of them.
		// Counterfactual: drop the HasGroupTierAssignments gate and every
		// pre-0.6 cookie 403s deployment-wide.
		st := &capStore{govHasGroupTier: false}
		got, err := govServer(st).effectiveCeiling(govMemberCtx(nil, false))
		if err != nil {
			t.Fatalf("a deployment with no group assignments refused a stale snapshot: %v", err)
		}
		if got.Profile != nil {
			t.Errorf("profile = %+v, want the deployment ceiling", got.Profile)
		}
	})

	// The two governance reads ceilingWithUnusableGroups makes fail
	// INDEPENDENTLY, and each needs its own case. They used to share one
	// fixture field, which meant the second could never be reached: the
	// resolver errors and returns before the gate is ever asked, so the case
	// named for the gate was re-testing the resolver. Deleting the gate's
	// error check left the ENTIRE package green — executed, 53s — with this
	// subtest still passing under its old name.
	t.Run("ResolveGovernanceProfile failing is an error, not a pass", func(t *testing.T) {
		boom := errors.New("pg: connection refused")
		st := &capStore{govErr: boom}
		if _, err := govServer(st).effectiveCeiling(govMemberCtx(nil, false)); !errors.Is(err, boom) {
			t.Fatalf("err = %v, want the store failure — a failed resolve must never read as `no assignment matched`", err)
		}

		// AND IT MUST NOT BE MASKED AS A REFUSAL. Deleting
		// ceilingWithUnusableGroups' own resolve-error check does NOT fail-open
		// — ceilingFromProfile re-checks the same error downstream, which is
		// why the arm above passes without it — but with a group-tier row
		// present the flow reaches the stale refusal FIRST and answers
		// groups_snapshot_stale: a 403 telling the human to sign in again for
		// what is actually a database outage, and one no re-login can clear.
		// Executed: with that check removed this returns groups_snapshot_stale,
		// not boom. Without this second case, the early check could be deleted
		// with the suite green.
		masked := &capStore{govErr: boom, govHasGroupTier: true}
		_, err := govServer(masked).effectiveCeiling(govMemberCtx(nil, true))
		if !errors.Is(err, boom) {
			t.Fatalf("err = %v, want the store failure — a store outage must not be reported as `groups_snapshot_stale`, "+
				"which sends the caller to re-login for something re-logging in cannot fix", err)
		}
	})

	t.Run("HasGroupTierAssignments failing is an error, not a pass", func(t *testing.T) {
		// THE GATE ITSELF, reached at last: an unreadable "does a group row
		// exist" is not evidence that none does. The fixture is the ordinary
		// production shape this branch exists for — the resolver answers
		// ErrNotFound (nobody has a user-tier assignment) while the gate's own
		// query fails, which is one statement timing out or one table denying
		// a read, not a whole database outage.
		//
		// Counterfactual: change governance.go's `hasGroupTier, herr :=` to
		// `hasGroupTier, _ :=` and this case reports the DEPLOYMENT ceiling
		// with a nil error — a walled member silently widened by an unreadable
		// gate, which is the exact fail-open ceilingWithUnusableGroups exists
		// to refuse.
		boom := errors.New("pg: statement timeout")
		st := &capStore{govHasGroupTierErr: boom}
		got, err := govServer(st).effectiveCeiling(govMemberCtx(nil, true))
		if !errors.Is(err, boom) {
			t.Fatalf("err = %v (ceiling %+v), want the store failure — an unreadable gate must never read as `no group rows`",
				err, got.Spec.AllowedDomains)
		}
	})
}

// containsAll reports whether s contains every substring.
func containsAll(s string, subs ...string) bool {
	for _, sub := range subs {
		if !strings.Contains(s, sub) {
			return false
		}
	}
	return true
}

// ─── PF-22, resolve-time ──────────────────────────────────────────────────────

// TestEffectiveCeilingReintersectsGrants is PF-22's second half. The write-time
// monotone-⊆ bound (governance_grantbound.go) cannot be the whole story:
// Config.DefaultPolicy is ENV-BORNE, so an operator who removes a pairing from
// WARDYN_DEFAULT_POLICY and redeploys has revoked that eligibility for
// everyone — except that the profiles written while it existed are ROWS, and
// rows survive redeploys.
//
// Counterfactual: skip the resolve-time call and the stale profile goes on
// serving a pairing the deployment no longer provisions, forever, with the
// profile-authoring surface having become a way to pin credential eligibility
// past the deployer's own revocation.
func TestEffectiveCeilingReintersectsGrants(t *testing.T) {
	const corpHost, corpSecret = "api.corp.example", "corp-api-key"
	profile := govProfile("has-a-grant")
	profile.Ceiling.EligibleGrants = []types.GrantSpec{{
		Kind: types.GrantAPIKey, Scope: apiKeyScope(t, corpHost, corpSecret), TTLSeconds: 60,
	}}
	st := &capStore{govProfile: profile, govTier: types.CapabilitySubjectGroup}

	// While the deployment still provisions the pairing, it survives.
	srv := govServer(st)
	srv.cfg.DefaultPolicy.EligibleGrants = []types.GrantSpec{{
		Kind: types.GrantAPIKey, Scope: apiKeyScope(t, corpHost, corpSecret), TTLSeconds: 300,
	}}
	got, err := srv.effectiveCeiling(govMemberCtx([]string{"eng"}, false))
	if err != nil {
		t.Fatalf("effectiveCeiling: %v", err)
	}
	if len(got.Spec.EligibleGrants) != 1 {
		t.Fatalf("a grant the deployment still provisions was dropped: %+v (%v)", got.Spec.EligibleGrants, got.Warnings)
	}
	if len(got.Warnings) != 0 {
		t.Errorf("warnings on the ordinary path: %v", got.Warnings)
	}

	// Redeploy: the operator drops the pairing from the env-borne default.
	srv.cfg.DefaultPolicy.EligibleGrants = nil
	got, err = srv.effectiveCeiling(govMemberCtx([]string{"eng"}, false))
	if err != nil {
		t.Fatalf("the re-intersect 500'd instead of dropping: %v", err)
	}
	if len(got.Spec.EligibleGrants) != 0 {
		t.Errorf("eligible_grants = %+v, want empty — the deployment no longer provisions this pairing", got.Spec.EligibleGrants)
	}
	// DROPPED WITH A WARNING, not refused: a redeploy is somebody else's act
	// arriving between a member's two runs, and failing their run for it turns
	// one env edit into an outage.
	if !containsAll(strings.Join(got.Warnings, "\n"), "has-a-grant", "api_key") {
		t.Errorf("warnings = %v, want one naming the profile and the dropped grant kind", got.Warnings)
	}
}

// ─── the routed read surfaces ─────────────────────────────────────────────────

// TestGovernanceRoutedReadSites covers the two routed sites the escape table
// cannot reach through a run create: GET /policies/default (which its own
// documentation defines as "the ceiling a member is clamped against", so
// answering with Config.DefaultPolicy made it a lie for anyone under a profile)
// and the member-facing secret listing (which offers the menu of operator
// secrets a member may pair — a menu that has to match what their run would
// actually accept, or it teaches members to ignore it).
//
// Counterfactual for both: revert the site to s.cfg.DefaultPolicy and the
// walled member is shown the deployment's wider answer.
func TestGovernanceRoutedReadSites(t *testing.T) {
	t.Run("GET /policies/default answers the caller's own ceiling", func(t *testing.T) {
		profile := govProfile("walled")
		st := newGovEscapeStore(&capStore{govProfile: profile, govTier: types.CapabilitySubjectGroup})
		h := newHarness(t)
		cfg := baseTestConfig(h, st)
		cfg.OIDC = &oidc.Authenticator{}
		cfg.DefaultPolicy = govDeployment()
		srv := New(cfg)

		w := doSSO(t, srv, http.MethodGet, "/api/v1/policies/default",
			govSession(t, "sub-walled", []string{"eng"}, false), "")
		if w.Code != http.StatusOK {
			t.Fatalf("GET /policies/default = %d: %s", w.Code, w.Body.String())
		}
		var got defaultPolicyResponse
		if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
			t.Fatalf("decode: %v; body=%s", err, w.Body.String())
		}
		if got.GovernanceProfileName != "walled" {
			t.Errorf("governance_profile_name = %q, want %q — the member cannot see which profile bounds them", got.GovernanceProfileName, "walled")
		}
		if slices.Contains(got.AllowedDomains, "wide.example") {
			t.Errorf("allowed_domains = %v — the endpoint reported the DEPLOYMENT ceiling to a walled member", got.AllowedDomains)
		}

		// And for an UNASSIGNED member the field is ABSENT, not "" — an absent
		// key already decodes as "no profile" everywhere, while "" is a value
		// every consumer would have to special-case.
		st2 := newGovEscapeStore(&capStore{})
		cfg2 := baseTestConfig(newHarness(t), st2)
		cfg2.OIDC = &oidc.Authenticator{}
		cfg2.DefaultPolicy = govDeployment()
		w = doSSO(t, New(cfg2), http.MethodGet, "/api/v1/policies/default",
			govSession(t, "sub-plain", []string{"eng"}, false), "")
		if w.Code != http.StatusOK {
			t.Fatalf("GET /policies/default (unassigned) = %d: %s", w.Code, w.Body.String())
		}
		if strings.Contains(w.Body.String(), "governance_profile_name") {
			t.Errorf("an unassigned member's body carries the key: %s", w.Body.String())
		}
		if !strings.Contains(w.Body.String(), "wide.example") {
			t.Errorf("an unassigned member did not get the deployment ceiling: %s", w.Body.String())
		}
	})

	t.Run("member secret listing is bounded by the caller's ceiling", func(t *testing.T) {
		const blessed, other = "profile-blessed-key", "corp-api-key"
		profile := govProfile("walled")
		profile.Ceiling.EligibleGrants = []types.GrantSpec{{
			Kind: types.GrantAPIKey, Scope: apiKeyScope(t, "api.anthropic.com", blessed),
		}}
		srv := govServer(&capStore{govProfile: profile, govTier: types.CapabilitySubjectGroup})
		// The DEPLOYMENT pairs BOTH secrets, so only the profile can hide one.
		srv.cfg.DefaultPolicy.EligibleGrants = []types.GrantSpec{
			{Kind: types.GrantAPIKey, Scope: apiKeyScope(t, "api.anthropic.com", blessed)},
			{Kind: types.GrantAPIKey, Scope: apiKeyScope(t, "api.anthropic.com", other)},
		}
		srv.cfg.Secrets = &memSecrets{m: map[string][]byte{blessed: []byte("v"), other: []byte("v")}}

		names, err := srv.memberVisibleOperatorSecretNames(govMemberCtx([]string{"eng"}, false))
		if err != nil {
			t.Fatalf("memberVisibleOperatorSecretNames: %v", err)
		}
		if !slices.Contains(names, blessed) {
			t.Errorf("names = %v, want the profile's own pairing listed", names)
		}
		if slices.Contains(names, other) {
			t.Errorf("names = %v — a secret only the DEPLOYMENT ceiling pairs was offered to a walled member", names)
		}
	})
}
