// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// POST /governance/preview — the console's resolved-profile dry run.
//
// THE CONTRACT UNDER TEST is not "the preview ranks correctly". It is that the
// preview HAS NO RANKING OF ITS OWN: every case below asserts the endpoint's
// answer against a DIRECT Store.ResolveGovernanceProfile call on the same
// fixtures and the same claim lists — the identical call the enforcement path
// (effectiveCeiling) takes — so the two cannot disagree without this file going
// red. The console's first cut answered this question client-side, with a
// hand-written TypeScript mirror of the SQL ORDER BY; a second implementation
// of the precedence rule is a second implementation of the answer, and a
// preview that drifts is confidently wrong exactly where an admin is deciding
// whether a ceiling is right.
//
// The precedence rule itself is still pinned where it lives — store's own
// governance_pg_test.go, against the same real Postgres. What is added here is
// the equality: same fixtures, same claims, same answer through HTTP.
//
// Postgres-backed, guarded by WARDYN_TEST_PG (pgHarness skips cleanly when
// unset). A fake store cannot carry this test: the thing being pinned is the
// SQL, so a Go double asserting itself would prove nothing.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// previewFixture is one self-contained governance picture: seven profiles and
// the seven assignments that exercise every rank of the resolver's ORDER BY.
//
// Names carry a per-run suffix so the shared substrate can hold several of
// these at once, and a SORTABLE prefix so the name-ASC floor is assertable —
// with a bare uuid suffix, "which name sorts first" would be a coin flip and
// the tie-break case could not be written at all.
type previewFixture struct {
	sub, email        string // the two user-tier subjects
	hi, lo            string // two groups, priority 20 and 10
	tie1, tie2        string // two groups at the SAME priority
	userSub, userMail string
	groupHi, groupLo  string
	tieFirst          string // the name-ASC winner between the two tie profiles
	all               string
}

// seedPreviewFixture writes the fixture and registers its teardown. Profiles
// are deleted after the assignments that point at them (t.Cleanup is LIFO),
// which ON DELETE RESTRICT requires.
//
// The 'all' row is UNIQUE on (subject_type, subject) = ('all', empty), so seeding
// it REPOINTS whatever everyone-row the database already held and the cleanup
// removes it. That is a deliberate cost of testing the everyone tier at all
// against a shared substrate, and it is why these tests do not run parallel.
func seedPreviewFixture(t *testing.T, st store.PG) previewFixture {
	t.Helper()
	ctx := context.Background()
	sfx := uuid.NewString()[:8]

	profile := func(name string) types.GovernanceProfile {
		p, err := st.UpsertGovernanceProfile(ctx, types.GovernanceProfile{
			Name:      name,
			Ceiling:   types.RunPolicySpec{AllowedDomains: []string{"pypi.org"}, MinConfinementClass: types.CC2},
			CreatedBy: "preview-test",
		})
		if err != nil {
			t.Fatalf("seed profile %q: %v", name, err)
		}
		t.Cleanup(func() { _ = st.DeleteGovernanceProfile(ctx, p.ID) })
		return p
	}
	assign := func(kind types.CapabilitySubjectType, subject string, p types.GovernanceProfile, priority int) {
		t.Helper()
		a, err := st.UpsertGovernanceAssignment(ctx, types.GovernanceAssignment{
			SubjectType: kind, Subject: subject, ProfileID: p.ID,
			Priority: priority, CreatedBy: "preview-test",
		})
		if err != nil {
			t.Fatalf("seed assignment %s/%q: %v", kind, subject, err)
		}
		t.Cleanup(func() { _ = st.DeleteGovernanceAssignment(ctx, a.ID) })
	}

	pSub := profile("aaa-sub-" + sfx)
	pMail := profile("aab-email-" + sfx)
	pHi := profile("aac-hi-" + sfx)
	pLo := profile("aad-lo-" + sfx)
	pTieA := profile("aae-tie-a-" + sfx)
	pTieB := profile("aaf-tie-b-" + sfx)
	pAll := profile("aag-all-" + sfx)

	f := previewFixture{
		sub: "sub-" + sfx, email: "alice-" + sfx + "@corp.example",
		hi: "hi-" + sfx, lo: "lo-" + sfx,
		tie1: "tie1-" + sfx, tie2: "tie2-" + sfx,
		userSub: pSub.Name, userMail: pMail.Name,
		groupHi: pHi.Name, groupLo: pLo.Name,
		tieFirst: pTieA.Name, all: pAll.Name,
	}
	assign(types.CapabilitySubjectUser, f.sub, pSub, 0)
	assign(types.CapabilitySubjectUser, f.email, pMail, 0)
	assign(types.CapabilitySubjectGroup, f.hi, pHi, 20)
	assign(types.CapabilitySubjectGroup, f.lo, pLo, 10)
	// tie1 points at the LATER name and tie2 at the earlier one, so a resolver
	// that fell back to insertion order (or to the caller's group order) would
	// answer tie-b and redden the name-ASC case.
	assign(types.CapabilitySubjectGroup, f.tie1, pTieB, 5)
	assign(types.CapabilitySubjectGroup, f.tie2, pTieA, 5)
	assign(types.CapabilitySubjectAll, "", pAll, 0)
	return f
}

// previewViaHTTP posts one preview and decodes it, failing on any non-200.
func previewViaHTTP(t *testing.T, srv *Server, users, groups []string) governancePreviewResponse {
	t.Helper()
	body, err := json.Marshal(governancePreviewRequest{UserSubjects: users, Groups: groups})
	if err != nil {
		t.Fatalf("marshal preview request: %v", err)
	}
	w := do(t, srv, http.MethodPost, "/api/v1/governance/preview", adminToken, string(body))
	if w.Code != http.StatusOK {
		t.Fatalf("preview %v/%v: code = %d, want 200; body=%s", users, groups, w.Code, w.Body.String())
	}
	var got governancePreviewResponse
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode preview: %v (body=%s)", err, w.Body.String())
	}
	return got
}

// resolveDirect is the enforcement path's own call, made straight against the
// store. ErrNotFound comes back as the zero answer — the absent-row doctrine,
// which is exactly what the endpoint ships as an empty object.
func resolveDirect(t *testing.T, pool *pgxpool.Pool, users, groups []string) governancePreviewResponse {
	t.Helper()
	p, tier, err := store.NewPG(pool).ResolveGovernanceProfile(context.Background(), users, groups)
	if errors.Is(err, store.ErrNotFound) {
		return governancePreviewResponse{}
	}
	if err != nil {
		t.Fatalf("direct resolve %v/%v: %v", users, groups, err)
	}
	return governancePreviewResponse{ProfileID: p.ID.String(), ProfileName: p.Name, MatchedTier: tier}
}

// TestGovernancePreview_AgreesWithTheResolver walks every rank of the ORDER BY
// through the endpoint and, for each, against a direct resolver call on the
// same claims. Both halves matter: `want` pins the RULE (so a fixture that
// stopped exercising a rank is visible), and the direct comparison pins that
// the endpoint did not derive the answer some other way.
func TestGovernancePreview_AgreesWithTheResolver(t *testing.T) {
	srv, pool := pgHarness(t)
	f := seedPreviewFixture(t, store.NewPG(pool))

	for _, tc := range []struct {
		name     string
		users    []string
		groups   []string
		want     string
		wantTier types.CapabilitySubjectType
	}{{
		// No priority in the group tier can beat an explicitly named person.
		name:  "a user assignment beats a group one, whatever its priority",
		users: []string{f.sub}, groups: []string{f.hi},
		want: f.userSub, wantTier: types.CapabilitySubjectUser,
	}, {
		// array_position: the caller's OWN ordering is the tie-break, and the
		// console sends user_subjects in the order capabilitySubjects builds
		// them — sign-in subject first, email second.
		name:  "within the user tier the sign-in subject beats the email",
		users: []string{f.sub, f.email}, groups: nil,
		want: f.userSub, wantTier: types.CapabilitySubjectUser,
	}, {
		// The same two claims, sent the other way round, answer the other
		// profile — because POSITION decides, not the shape of the string.
		// This is the endpoint's stated ceiling, asserted rather than implied.
		name:  "the user tier ranks by position, not by an @-shape guess",
		users: []string{f.email, f.sub}, groups: nil,
		want: f.userMail, wantTier: types.CapabilitySubjectUser,
	}, {
		name:  "a group assignment beats the everyone row",
		users: nil, groups: []string{f.lo},
		want: f.groupLo, wantTier: types.CapabilitySubjectGroup,
	}, {
		name:  "between groups the higher priority wins",
		users: nil, groups: []string{f.lo, f.hi},
		want: f.groupHi, wantTier: types.CapabilitySubjectGroup,
	}, {
		name:  "at equal priority the profile name is the floor",
		users: nil, groups: []string{f.tie1, f.tie2},
		want: f.tieFirst, wantTier: types.CapabilitySubjectGroup,
	}, {
		name:  "claims naming nobody fall through to the everyone row",
		users: []string{"nobody-" + f.sub}, groups: []string{"nobody-" + f.hi},
		want: f.all, wantTier: types.CapabilitySubjectAll,
	}} {
		t.Run(tc.name, func(t *testing.T) {
			got := previewViaHTTP(t, srv, tc.users, tc.groups)
			if got.ProfileName != tc.want || got.MatchedTier != tc.wantTier {
				t.Errorf("preview = %q/%s, want %q/%s", got.ProfileName, got.MatchedTier, tc.want, tc.wantTier)
			}
			// THE assertion this file exists for: the endpoint and the
			// enforcement path's own call, on identical inputs, agree in full.
			if want := resolveDirect(t, pool, tc.users, tc.groups); got != want {
				t.Errorf("preview = %+v, direct ResolveGovernanceProfile = %+v — the preview has grown a second matcher", got, want)
			}
		})
	}
}

// TestGovernancePreview_FoldsClaimsLikeTheEnforcementPath pins the one
// transformation the handler does perform. Assignments are stored lowercased
// (validateGovernanceAssignment) and BOTH enforcement-path inputs arrive
// already folded — capabilitySubjects lowercases the sub and the email,
// sessionGroups lowercases every group — so a preview that passed a typed
// `HI-…` through raw would answer "no assignment matches" for a claim the real
// run matches.
func TestGovernancePreview_FoldsClaimsLikeTheEnforcementPath(t *testing.T) {
	srv, pool := pgHarness(t)
	f := seedPreviewFixture(t, store.NewPG(pool))

	got := previewViaHTTP(t, srv, nil, []string{"  " + strings.ToUpper(f.hi) + "  "})
	if want := resolveDirect(t, pool, nil, []string{f.hi}); got != want {
		t.Errorf("preview of an unfolded claim = %+v, want the folded answer %+v", got, want)
	}
	if got.ProfileName != f.groupHi {
		t.Errorf("preview = %q, want %q — the fixture stopped exercising the fold", got.ProfileName, f.groupHi)
	}
}

// TestGovernancePreview_NoMatchIsTheDeploymentCeiling pins the empty object.
// An absent profile_name is how the console reads "no assignment matches these
// claims", the same additive/absent shape defaultPolicyResponse's
// governance_profile_name uses — "" would be a value the console then has to
// special-case.
//
// Seeds NOTHING on purpose, so there is no everyone-row for the claims to fall
// through to. A leftover 'all'-tier assignment from another test would surface
// here as a clear mismatch rather than a silent pass.
func TestGovernancePreview_NoMatchIsTheDeploymentCeiling(t *testing.T) {
	srv, pool := pgHarness(t)
	claims := []string{"nobody-" + uuid.NewString()}

	want := resolveDirect(t, pool, claims, claims)
	if want != (governancePreviewResponse{}) {
		t.Fatalf("the database already assigns a profile to unknown claims (%+v) — a leftover everyone-row breaks this test's isolation", want)
	}
	if got := previewViaHTTP(t, srv, claims, claims); got != want {
		t.Errorf("preview = %+v, want the empty object (no assignment matched)", got)
	}
}

// TestGovernancePreview_RefusesAnOversizedClaimList pins the list ceiling. The
// body cap already bounds the request; this bounds the ARRAY handed to
// `= ANY($1::text[])` and to the handler's own de-duplication, which is O(n²).
func TestGovernancePreview_RefusesAnOversizedClaimList(t *testing.T) {
	srv, _ := pgHarness(t)
	claims := make([]string, maxGovernancePreviewClaims+1)
	for i := range claims {
		claims[i] = "g" + uuid.NewString()
	}
	body, err := json.Marshal(governancePreviewRequest{Groups: claims})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	w := do(t, srv, http.MethodPost, "/api/v1/governance/preview", adminToken, string(body))
	if w.Code != http.StatusBadRequest {
		t.Errorf("oversized claim list: code = %d, want 400; body=%s", w.Code, w.Body.String())
	}
}

// TestGovernancePreview_IsSecurityTierOnly is the local, readable half of the
// gate. The EXHAUSTIVE pin is routeMatrix's own row (TestAuthzMatrix proves the
// table covers every mounted route; TestSecurityAdminRouteTier probes both
// directions of the tier) — this states the member refusal beside the endpoint
// it refuses, and needs no Postgres to do it.
func TestGovernancePreview_IsSecurityTierOnly(t *testing.T) {
	srv, _, _, _ := newAuthzMatrixServer(t)
	member := ssoSession(t, "sub-preview-member", "member-preview@corp.example", oidc.RoleMember)
	if w := doSSO(t, srv, http.MethodPost, "/api/v1/governance/preview", member, `{}`); w.Code != http.StatusForbidden {
		t.Errorf("member on the preview: code = %d, want 403; body=%s", w.Code, w.Body.String())
	}
	sec := ssoSession(t, secAdminSub, secAdminMail, oidc.RoleSecurityAdmin)
	if w := doSSO(t, srv, http.MethodPost, "/api/v1/governance/preview", sec, `{}`); w.Code != http.StatusOK {
		t.Errorf("security_admin on the preview: code = %d, want 200; body=%s", w.Code, w.Body.String())
	}
}
