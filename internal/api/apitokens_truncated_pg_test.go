// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// PROBE — 0.7 hardening review, lane F3-api-token-truncated-snapshot.
//
// INTENDED DESTINATION: internal/api/apitokens_truncated_pg_test.go (package api).
// Copy the file there unchanged; it reuses this package's existing test helpers
// (throwawayPGPool — injection_owner_pg_test.go:37; newHarness/baseTestConfig/do
// — api_test.go; govSession — governance_nonescape_test.go; mintToken —
// apitokens_test.go). Guarded by WARDYN_TEST_PG; skipped cleanly when unset.
//
// RUN (from the repo root, one heavy gate at a time):
//
//	WARDYN_TEST_PG='postgres://USER:PASS@HOST:PORT/DBNAME?sslmode=disable' \
//	  nice -n 10 GOMAXPROCS=8 go test -p 4 -race -count=1 ./internal/api/ \
//	  -run 'TestPG_APIToken_TruncatedSnapshot' -v
//
// The DSN must have CREATE DATABASE rights (throwawayPGPool mints a fresh
// wardyn_inj_* database per test and drops it on cleanup, so nothing here
// touches shared rows).
//
// WHAT IT PINS (the traced invariant, see ../F3-api-token-truncated-snapshot.md):
// a wdn_ token replays the minting session's group snapshot TOGETHER WITH that
// snapshot's completeness, and an UNKNOWN completeness (SQL NULL, a 0.6-era row)
// reads as INCOMPLETE. Concretely, through the real Postgres store and the real
// HTTP stack:
//
//  1. A token minted from a truncated session lands in api_tokens with
//     groups_truncated = TRUE (not NULL, not FALSE).
//  2. With a group-tier governance assignment present, that token cannot
//     resolve ANY ceiling: GET /policies/default is 403 groups_snapshot_stale.
//  3. A 0.6-era row (groups_truncated NULL, inserted by raw SQL exactly as a
//     pre-0052 binary would have left it) round-trips as nil — three-valued —
//     and is refused identically.
//  4. Controls: with NO group-tier row the truncated token resolves the
//     deployment ceiling (PF-21 scoping); a token minted from a COMPLETE
//     session resolves its group profile; a user-tier row suppresses the
//     refusal (PF-25). These keep "fail closed" from passing as "lane broken".
//
// A SECOND test (TestPG_APIToken_TruncatedSnapshot_CapabilityDenyEvaporates)
// is EXPECTED RED on fa910735: it documents hypothesis H2 of the trace — the
// capability-grant resolver (capScan, capabilities.go:223) ignores the
// truncation bit, so a group DENY grant whose group fell off the cookie cap
// silently stops matching for that token. Green there means H2 was fixed.
package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

const (
	// truncProbeGroupKept survives the cap in every fixture below.
	truncProbeGroupKept = "a-team"
	// truncProbeGroupWalled is the group the governance assignment (and the
	// deny grant) is written against. Alphabetically LAST on purpose: it is the
	// entry sessionGroups (derive.go:564-579) drops first, i.e. the realistic
	// shape of "the walling group fell off the snapshot".
	truncProbeGroupWalled = "zz-walled"
	truncProbeSub         = "sub-trunc-probe"
)

// truncProbeServer wires a Server against a throwaway, fully-migrated Postgres
// with SSO configured (so /me/tokens can mint) and a WIDE deployment ceiling,
// so a resolved group profile is visibly narrower than the default.
func truncProbeServer(t *testing.T) (*Server, store.PG, *pgxpool.Pool) {
	t.Helper()
	pool := throwawayPGPool(t)
	h := newHarness(t)
	pg := store.NewPG(pool)
	cfg := baseTestConfig(h, pg)
	cfg.OIDC = &oidc.Authenticator{}
	cfg.DefaultPolicy = types.RunPolicySpec{
		AllowedDomains:      []string{"api.anthropic.com", "pypi.org", "github.com"},
		MinConfinementClass: types.CC2,
	}
	return New(cfg), pg, pool
}

// truncProbeSeedGroupProfile writes one profile and binds it to
// truncProbeGroupWalled at the GROUP tier — the deployment shape the refusal
// is gated on (HasGroupTierAssignments, governance.go:652).
func truncProbeSeedGroupProfile(t *testing.T, pg store.PG) types.GovernanceProfile {
	t.Helper()
	ctx := context.Background()
	p, err := pg.UpsertGovernanceProfile(ctx, types.GovernanceProfile{
		ID:   uuid.New(),
		Name: "walled-" + uuid.NewString()[:8],
		Ceiling: types.RunPolicySpec{
			AllowedDomains:      []string{"pypi.org"},
			MinConfinementClass: types.CC2,
		},
		Limits:    types.GovernanceLimits{DenyInteractive: true},
		CreatedBy: "probe",
	})
	if err != nil {
		t.Fatalf("seed profile: %v", err)
	}
	if _, err := pg.UpsertGovernanceAssignment(ctx, types.GovernanceAssignment{
		ID: uuid.New(), SubjectType: types.CapabilitySubjectGroup,
		Subject: truncProbeGroupWalled, ProfileID: p.ID, CreatedBy: "probe",
	}); err != nil {
		t.Fatalf("seed group assignment: %v", err)
	}
	return p
}

// truncProbeCeiling GETs /policies/default as the bearer and returns status,
// raw body and the resolved governance_profile_name ("" for the deployment).
func truncProbeCeiling(t *testing.T, srv *Server, bearer string) (int, string, string) {
	t.Helper()
	w := do(t, srv, http.MethodGet, "/api/v1/policies/default", bearer, "")
	var resp struct {
		GovernanceProfileName string `json:"governance_profile_name"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	return w.Code, w.Body.String(), resp.GovernanceProfileName
}

// truncProbeInsertLegacyRow inserts an api_tokens row EXACTLY as a pre-0052
// binary would have left it: every 0045 column, and NO groups_truncated at all
// (the column's default is NULL — 0052:110 adds it without DEFAULT on purpose).
// Returns the plaintext bearer.
func truncProbeInsertLegacyRow(t *testing.T, pool *pgxpool.Pool, groups []string) string {
	t.Helper()
	raw := apiTokenPrefix + strings.ReplaceAll(uuid.NewString(), "-", "")
	sum := sha256.Sum256([]byte(raw))
	gj, _ := json.Marshal(groups)
	_, err := pool.Exec(context.Background(), `
		INSERT INTO api_tokens (id, principal, email, role, groups, name, token_sha256, created_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`,
		uuid.New(), truncProbeSub, truncProbeSub+"@corp.example", oidc.RoleMember,
		gj, "legacy-0.6", hex.EncodeToString(sum[:]), time.Now().UTC())
	if err != nil {
		t.Fatalf("insert legacy row: %v", err)
	}
	return raw
}

// TestPG_APIToken_TruncatedSnapshot is the invariant probe. It FAILS if any hop
// of the trace regresses: the mint stops stamping the bit, the store stops
// round-tripping NULL as nil, apiTokenAuth stops reading nil as truncated, or
// effectiveCeiling stops refusing a truncated snapshot.
func TestPG_APIToken_TruncatedSnapshot(t *testing.T) {
	srv, pg, pool := truncProbeServer(t)
	ctx := context.Background()

	// ── phase 1: the stamp ──────────────────────────────────────────────────
	truncSess := govSession(t, truncProbeSub, []string{truncProbeGroupKept}, true)
	truncRaw, created := mintToken(t, srv, truncSess, "ci-truncated")
	if created.GroupsTruncated == nil || !*created.GroupsTruncated {
		t.Fatalf("mint response groups_truncated = %v, want true — handleCreateAPIToken (apitokens.go:236-243) did not stamp the session's bit", created.GroupsTruncated)
	}
	var col *bool
	if err := pool.QueryRow(ctx, `SELECT groups_truncated FROM api_tokens WHERE id = $1`, created.ID).Scan(&col); err != nil {
		t.Fatalf("read groups_truncated: %v", err)
	}
	if col == nil || !*col {
		t.Fatalf("api_tokens.groups_truncated = %v, want TRUE — the store (store_apitokens.go:48-52) lost the marker", col)
	}

	// ── phase 2: PF-21 scoping — no group-tier row, truncated is served ─────
	// Must run BEFORE the assignment exists. A 403 here would mean the refusal
	// fires on deployments that never adopted group profiles.
	if code, body, name := truncProbeCeiling(t, srv, truncRaw); code != http.StatusOK || name != "" {
		t.Fatalf("truncated token, NO group-tier rows: code=%d profile=%q body=%s; want 200 and the deployment ceiling (PF-21)", code, name, body)
	}

	// ── phase 3: the refusal ────────────────────────────────────────────────
	profile := truncProbeSeedGroupProfile(t, pg)
	code, body, _ := truncProbeCeiling(t, srv, truncRaw)
	if code != http.StatusForbidden {
		t.Fatalf("truncated token with a group-tier row present: code=%d body=%s; want 403 — the tier evaporated (governance.go:607/656)", code, body)
	}
	if !strings.Contains(body, "groups_snapshot_stale") {
		t.Errorf("403 body does not name the condition: %s", body)
	}
	// The same refusal must reach a run create, the lane that actually spends
	// the ceiling (runs_create_validate.go:425). No Runner is wired, so a
	// non-403 here means the ceiling resolved and the create went on to fail
	// LATER for an unrelated reason — which is exactly the widening.
	if w := do(t, srv, http.MethodPost, "/api/v1/runs", truncRaw, `{"agent":"claude-code","task":"t"}`); w.Code != http.StatusForbidden {
		t.Errorf("POST /runs with the truncated token: code=%d body=%s; want 403 groups_snapshot_stale", w.Code, w.Body.String())
	}

	// ── phase 4: the 0.6-era row (NULL marker) ──────────────────────────────
	legacyRaw := truncProbeInsertLegacyRow(t, pool, []string{truncProbeGroupKept})
	legacy, err := pg.GetAPITokenByRaw(ctx, legacyRaw)
	if err != nil {
		t.Fatalf("lookup legacy row: %v", err)
	}
	if legacy.GroupsTruncated != nil {
		t.Fatalf("legacy row GroupsTruncated = %v, want nil — the store collapsed NULL into a bool (three-valued contract, types.go:835-846)", *legacy.GroupsTruncated)
	}
	if code, body, _ := truncProbeCeiling(t, srv, legacyRaw); code != http.StatusForbidden || !strings.Contains(body, "groups_snapshot_stale") {
		t.Fatalf("legacy NULL-marker token: code=%d body=%s; want 403 groups_snapshot_stale — NULL read as false (apitokens.go:129-130)", code, body)
	}
	// A legacy row whose snapshot DOES contain the walled group is refused too:
	// NULL means unknown, and unknown is not answered by a lucky snapshot.
	legacyWithGroup := truncProbeInsertLegacyRow(t, pool, []string{truncProbeGroupKept, truncProbeGroupWalled})
	if code, body, _ := truncProbeCeiling(t, srv, legacyWithGroup); code != http.StatusForbidden {
		t.Errorf("legacy NULL-marker token carrying the walled group: code=%d body=%s; want 403 (completeness unknown)", code, body)
	}

	// ── phase 5: controls ───────────────────────────────────────────────────
	// 5a. a COMPLETE snapshot that carries the group resolves the profile.
	fullSess := govSession(t, truncProbeSub+"-full", []string{truncProbeGroupKept, truncProbeGroupWalled}, false)
	fullRaw, fullCreated := mintToken(t, srv, fullSess, "ci-complete")
	if fullCreated.GroupsTruncated == nil || *fullCreated.GroupsTruncated {
		t.Fatalf("complete mint groups_truncated = %v, want false (non-nil)", fullCreated.GroupsTruncated)
	}
	if err := pool.QueryRow(ctx, `SELECT groups_truncated FROM api_tokens WHERE id = $1`, fullCreated.ID).Scan(&col); err != nil || col == nil || *col {
		t.Fatalf("complete row groups_truncated = %v (err %v), want FALSE not NULL — a 0.7 mint must never leave the column NULL", col, err)
	}
	if code, body, name := truncProbeCeiling(t, srv, fullRaw); code != http.StatusOK || name != profile.Name {
		t.Fatalf("complete token: code=%d profile=%q body=%s; want 200 under %q", code, name, body, profile.Name)
	}
	// 5b. a COMPLETE snapshot WITHOUT the group is the deployment ceiling — so
	// the 403 above was about completeness, not about group membership.
	partialSess := govSession(t, truncProbeSub+"-other", []string{truncProbeGroupKept}, false)
	partialRaw, _ := mintToken(t, srv, partialSess, "ci-other")
	if code, body, name := truncProbeCeiling(t, srv, partialRaw); code != http.StatusOK || name != "" {
		t.Fatalf("complete token, not in the walled group: code=%d profile=%q body=%s; want 200 deployment", code, name, body)
	}
	// 5c. PF-25: a USER-tier row names the truncated human — served, not refused.
	if _, err := pg.UpsertGovernanceAssignment(ctx, types.GovernanceAssignment{
		ID: uuid.New(), SubjectType: types.CapabilitySubjectUser,
		Subject: truncProbeSub, ProfileID: profile.ID, CreatedBy: "probe",
	}); err != nil {
		t.Fatalf("seed user assignment: %v", err)
	}
	if code, body, name := truncProbeCeiling(t, srv, truncRaw); code != http.StatusOK || name != profile.Name {
		t.Fatalf("truncated token with a user-tier row: code=%d profile=%q body=%s; want 200 under %q (PF-25)", code, name, body, profile.Name)
	}
}

// TestPG_APIToken_TruncatedSnapshot_CapabilityDenyEvaporates — EXPECTED RED on
// fa910735 (trace hypothesis H2). The governance resolver treats a truncated
// snapshot as unanswerable (governance.go:607); the CAPABILITY resolver does
// not (capabilities.go:223 discards `stale` and never reads the truncation
// bit). A group DENY grant written against the group that fell off the cap
// therefore matches nothing for this token, and the seam answers "allowed".
//
// The seam under test is capAllowed itself, reached through the real
// apiTokenAuth context — the same path approvals.go:430 (member decides an
// egress approval), inline_policy.go:298/350 and secrets.go:384 take. None of
// those seams is preceded by an effectiveCeiling call, so on a deployment with
// group DENY grants but no group governance assignments there is no 403
// anywhere.
func TestPG_APIToken_TruncatedSnapshot_CapabilityDenyEvaporates(t *testing.T) {
	srv, pg, _ := truncProbeServer(t)
	ctx := context.Background()
	const deniedHost = "exfil.example"

	if _, err := pg.UpsertCapabilityGrant(ctx, types.CapabilityGrant{
		ID: uuid.New(), SubjectType: types.CapabilitySubjectGroup, Subject: truncProbeGroupWalled,
		Capability: capEgressHost, Value: deniedHost, Effect: types.CapabilityDeny, CreatedBy: "probe",
	}); err != nil {
		t.Fatalf("seed deny grant: %v", err)
	}

	// capture runs the real token auth branch and hands back the request
	// context it publishes — exactly what every seam downstream reads.
	capture := func(bearer string) context.Context {
		var got context.Context
		next := http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { got = r.Context() })
		r := httptest.NewRequest(http.MethodGet, "/api/v1/me", nil)
		r.Header.Set("Authorization", "Bearer "+bearer)
		srv.apiTokenAuth(next, http.NotFoundHandler()).ServeHTTP(httptest.NewRecorder(), r)
		if got == nil {
			t.Fatalf("token %q never authenticated", bearer[:8])
		}
		return got
	}

	// Control: the SAME human with a COMPLETE snapshot is denied.
	fullRaw, _ := mintToken(t, srv, govSession(t, truncProbeSub, []string{truncProbeGroupKept, truncProbeGroupWalled}, false), "full")
	if ok, err := srv.capAllowed(capture(fullRaw), capEgressHost, deniedHost); err != nil || ok {
		t.Fatalf("control: complete snapshot capAllowed(%s) = %v, %v; want false (the deny must bite)", deniedHost, ok, err)
	}

	// The walled group fell off the cap; the bit says so. Any answer other
	// than "refused" (false, or an errGroupsSnapshotStale-class error) means
	// the deny evaporated for this credential.
	truncRaw, _ := mintToken(t, srv, govSession(t, truncProbeSub, []string{truncProbeGroupKept}, true), "trunc")
	ok, err := srv.capAllowed(capture(truncRaw), capEgressHost, deniedHost)
	if err == nil && ok {
		t.Fatalf("H2 CONFIRMED: truncated snapshot capAllowed(%s) = true — the group DENY grant on %q evaporated because capScan (capabilities.go:223) never reads the truncation bit", deniedHost, truncProbeGroupWalled)
	}

	// And the NULL-marker (0.6-era) token: same question, same expectation.
	// (Seeded through the store with a nil marker — the API cannot mint one;
	// nil binds as SQL NULL, store_apitokens.go:36-41.)
	legacyRaw := apiTokenPrefix + "legacy-" + strings.ReplaceAll(uuid.NewString(), "-", "")
	if _, err := pg.CreateAPIToken(ctx, types.APIToken{
		ID: uuid.New(), Principal: truncProbeSub, Email: truncProbeSub + "@corp.example",
		Role: oidc.RoleMember, Groups: []string{truncProbeGroupKept}, GroupsTruncated: nil,
		Name: "legacy", CreatedAt: time.Now().UTC(),
	}, legacyRaw); err != nil {
		t.Fatalf("seed legacy token: %v", err)
	}
	if ok, err := srv.capAllowed(capture(legacyRaw), capEgressHost, deniedHost); err == nil && ok {
		t.Fatalf("H2 CONFIRMED (legacy row): NULL-marker token capAllowed(%s) = true — the group DENY on %q is not enforced for a 0.6-era token", deniedHost, truncProbeGroupWalled)
	}
}
