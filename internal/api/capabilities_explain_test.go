// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"sort"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// explainGrid runs capExplain for a group subject over a fake store holding
// grants and enf.
func explainGrid(t *testing.T, grants []types.CapabilityGrant, enf map[string]bool, group string, kinds []string) []capExplainRow {
	t.Helper()
	rows, err := capServer(&capStore{grants: grants, enf: enf}).capExplain(context.Background(), nil, []string{group}, kinds)
	if err != nil {
		t.Fatalf("capExplain: %v", err)
	}
	return rows
}

func explainState(rows []capExplainRow, kind, value string) capExplainState {
	for _, r := range rows {
		if r.Kind == kind && r.Value == value {
			return r.State
		}
	}
	return ""
}

// TestCapExplainDefaults: a kind with no row at all for a subject (or "all")
// reports the kind's own direction default — Everyone for the six narrowing
// kinds, Admins only for the one widening kind (image) — exactly the posture
// capBatch.decide falls back to on an absent row.
func TestCapExplainDefaults(t *testing.T) {
	got := explainGrid(t, nil, nil, "eng", capabilityKinds)
	if len(got) != len(capabilityKinds) {
		t.Fatalf("capExplain(no grants) = %d rows, want one per kind (%d)", len(got), len(capabilityKinds))
	}
	for _, row := range got {
		want := capExplainEveryone
		if capKinds[row.Kind].direction == capWidening {
			want = capExplainAdminsOnly
		}
		if row.Value != capWildcard || row.State != want {
			t.Errorf("%s: got {%s %s}, want {%s %s}", row.Kind, row.Value, row.State, capWildcard, want)
		}
	}
}

// TestCapExplainWildcardRows: a blanket allow or deny for the subject (or
// "all") replaces the synthesized default with the real row — This type or
// Blocked — and is not duplicated.
func TestCapExplainWildcardRows(t *testing.T) {
	tests := []struct {
		name  string
		grant types.CapabilityGrant
		want  capExplainState
	}{
		{"subject wildcard allow", grant(types.CapabilitySubjectGroup, "eng", capSecret, capWildcard, types.CapabilityAllow), capExplainThisType},
		{"subject wildcard deny", grant(types.CapabilitySubjectGroup, "eng", capSecret, capWildcard, types.CapabilityDeny), capExplainBlocked},
		{"all wildcard allow", grant(types.CapabilitySubjectAll, "", capSecret, capWildcard, types.CapabilityAllow), capExplainThisType},
		{"all wildcard deny", grant(types.CapabilitySubjectAll, "", capSecret, capWildcard, types.CapabilityDeny), capExplainBlocked},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rows := explainGrid(t, []types.CapabilityGrant{tc.grant}, nil, "eng", []string{capSecret})
			if len(rows) != 1 || rows[0].Value != capWildcard || rows[0].State != tc.want {
				t.Fatalf("capExplain = %+v, want one row {%s %s %s}", rows, capSecret, capWildcard, tc.want)
			}
		})
	}
}

// TestCapExplainPerValueRows: a specific-value grant renders as its own row
// alongside the default, at the exact value an admin wrote — "Available to"
// granularity, not kind granularity — and a grant on another subject does not
// appear.
func TestCapExplainPerValueRows(t *testing.T) {
	grants := []types.CapabilityGrant{
		grant(types.CapabilitySubjectGroup, "eng", capWorkspace, "ws-allowed", types.CapabilityAllow),
		grant(types.CapabilitySubjectGroup, "eng", capWorkspace, "ws-blocked", types.CapabilityDeny),
		grant(types.CapabilitySubjectUser, "someone-else", capWorkspace, "ws-not-mine", types.CapabilityAllow),
	}
	rows := explainGrid(t, grants, nil, "eng", []string{capWorkspace})
	want := map[string]capExplainState{
		capWildcard:  capExplainEveryone,
		"ws-allowed": capExplainThisType,
		"ws-blocked": capExplainBlocked,
	}
	if len(rows) != len(want) {
		t.Fatalf("capExplain = %+v, want %d rows (a grant on another subject must not appear)", rows, len(want))
	}
	for _, row := range rows {
		if got, ok := want[row.Value]; !ok || got != row.State {
			t.Errorf("value %q: state = %s, want %s (ok=%v)", row.Value, row.State, want[row.Value], ok)
		}
	}
}

// TestCapExplainDenyWinsAcrossSubjects: a subject-scoped allow and an
// "all"-scoped deny on the SAME value; deny wins, exactly as capBatch.scan
// reads two subjects for one caller — no user-over-group, no type-over-all
// precedence.
func TestCapExplainDenyWinsAcrossSubjects(t *testing.T) {
	grants := []types.CapabilityGrant{
		grant(types.CapabilitySubjectGroup, "eng", capEgressHost, "api.example.com", types.CapabilityAllow),
		grant(types.CapabilitySubjectAll, "", capEgressHost, "api.example.com", types.CapabilityDeny),
	}
	for _, order := range [][]types.CapabilityGrant{grants, {grants[1], grants[0]}} {
		rows := explainGrid(t, order, nil, "eng", []string{capEgressHost})
		if got := explainState(rows, capEgressHost, "api.example.com"); got != capExplainBlocked {
			t.Fatalf("deny-wins order %v: state = %s, want %s", order, got, capExplainBlocked)
		}
	}
}

// TestCapExplainOverlapAndEnforcement pins the cells where a row-by-row
// reading disagrees with the resolver: a broader deny walls a narrower allow
// (decide step 2), and the enforcement switch is part of the answer — an
// enforced narrowing kind with no allow is Not available (step 7), and an
// image allow while image is off is still Admins only (step 4; UT-10's
// restriction bit is what will let it through).
func TestCapExplainOverlapAndEnforcement(t *testing.T) {
	allow, deny := types.CapabilityAllow, types.CapabilityDeny
	tests := []struct {
		name        string
		grants      []types.CapabilityGrant
		enf         map[string]bool
		kind, value string
		want        capExplainState
		wantDefault capExplainState
	}{
		{"all wildcard deny walls a group value allow",
			[]types.CapabilityGrant{
				grant(types.CapabilitySubjectAll, "", capSecret, capWildcard, deny),
				grant(types.CapabilitySubjectGroup, "eng", capSecret, "prod-db", allow),
			}, nil, capSecret, "prod-db", capExplainBlocked, capExplainBlocked},
		{"all host-suffix deny walls a group host allow",
			[]types.CapabilityGrant{
				grant(types.CapabilitySubjectAll, "", capEgressHost, "*.example.com", deny),
				grant(types.CapabilitySubjectGroup, "eng", capEgressHost, "api.example.com", allow),
			}, nil, capEgressHost, "api.example.com", capExplainBlocked, capExplainEveryone},
		{"enforced narrowing kind, no allow",
			[]types.CapabilityGrant{grant(types.CapabilitySubjectGroup, "eng", capSecret, "prod-db", allow)},
			map[string]bool{capSecret: true}, capSecret, "prod-db", capExplainThisType, capExplainNotAvailable},
		{"image allow while image is unenforced",
			[]types.CapabilityGrant{grant(types.CapabilitySubjectGroup, "eng", capImage, "ghcr.io/org/img:1", allow)},
			nil, capImage, "ghcr.io/org/img:1", capExplainAdminsOnly, capExplainAdminsOnly},
		{"image allow while image is enforced",
			[]types.CapabilityGrant{grant(types.CapabilitySubjectGroup, "eng", capImage, "ghcr.io/org/img:1", allow)},
			map[string]bool{capImage: true}, capImage, "ghcr.io/org/img:1", capExplainThisType, capExplainAdminsOnly},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rows := explainGrid(t, tc.grants, tc.enf, "eng", []string{tc.kind})
			if got := explainState(rows, tc.kind, tc.value); got != tc.want {
				t.Errorf("%s %q = %s, want %s (rows %+v)", tc.kind, tc.value, got, tc.want, rows)
			}
			if got := explainState(rows, tc.kind, capWildcard); got != tc.wantDefault {
				t.Errorf("%s default = %s, want %s (rows %+v)", tc.kind, got, tc.wantDefault, rows)
			}
		})
	}
}

// TestCapExplainUnknownKindSkipped: an unknown kind name is dropped rather
// than producing a row — the HTTP handler is what validates kinds and refuses
// the request.
func TestCapExplainUnknownKindSkipped(t *testing.T) {
	if rows := explainGrid(t, nil, nil, "eng", []string{"no_such_kind"}); len(rows) != 0 {
		t.Fatalf("capExplain(unknown kind) = %+v, want no rows", rows)
	}
}

// ─── the property: every cell equals Allowed ─────────────────────────────────

// TestCapExplainAgreesWithResolver is K4's acceptance test (authz design §5):
// every grid cell equals the resolver's own answer for a synthetic principal
// who is exactly the subject. Generated over every kind × every grant set of
// up to three rows drawn from {wildcard, specific values, a host-set suffix} ×
// {the subject, `all`, the same name at the other subject type} × {allow,
// deny}, × the kind's switch on and off, × all three subject types.
//
// The resolver side is the live path, not the grid's own: a member ctx built
// the way humanOrAdminAuth builds one, answered by capAllowed through
// capabilitySubjects and ListCapabilityGrantsFor. A specific-value row is
// compared at its value; the default row at a probe value no generated row
// names or covers.
func TestCapExplainAgreesWithResolver(t *testing.T) {
	type subjectCase struct {
		typ   types.CapabilitySubjectType
		name  string
		ctx   context.Context
		scope []types.CapabilitySubjectType // the subject types a candidate row may carry
	}
	const who = "eng"
	member := func(sub string, groups []string) context.Context {
		return withOIDCGroups(operatorCtx(sub, "", oidc.RoleMember), groups)
	}
	subjects := []subjectCase{
		{types.CapabilitySubjectUser, who, member(who, []string{}), []types.CapabilitySubjectType{types.CapabilitySubjectUser, types.CapabilitySubjectAll, types.CapabilitySubjectGroup}},
		{types.CapabilitySubjectGroup, who, member("sub-synthetic-nobody", []string{who}), []types.CapabilitySubjectType{types.CapabilitySubjectGroup, types.CapabilitySubjectAll, types.CapabilitySubjectUser}},
		{capabilitySubjectUserType, who, member("sub-synthetic-nobody", []string{}), []types.CapabilitySubjectType{types.CapabilitySubjectAll, types.CapabilitySubjectUser, types.CapabilitySubjectGroup}},
	}
	cells := 0
	for _, kind := range capabilityKinds {
		values, probe := []string{capWildcard, "v-one", "v-two"}, "v-unnamed"
		if capKinds[kind].hostSet {
			values, probe = []string{capWildcard, "*.example.com", "api.example.com", "other.org"}, "unnamed.example.net"
		}
		for _, sc := range subjects {
			var cands []types.CapabilityGrant
			for _, v := range values {
				for _, st := range sc.scope {
					subject := sc.name
					if st == types.CapabilitySubjectAll {
						subject = ""
					}
					for _, eff := range []types.CapabilityEffect{types.CapabilityAllow, types.CapabilityDeny} {
						cands = append(cands, grant(st, subject, kind, v, eff))
					}
				}
			}
			for _, set := range grantSubsets(cands, 3) {
				for _, on := range []bool{false, true} {
					st := &capStore{grants: set, enf: map[string]bool{kind: on}}
					srv := capServer(st)
					subject, users, groups, err := explainPrincipal(sc.typ, sc.name)
					if err != nil {
						t.Fatalf("explainPrincipal(%s, %q): %v", sc.typ, sc.name, err)
					}
					rows, err := srv.capExplain(context.Background(), users, groups, []string{kind})
					if err != nil {
						t.Fatalf("capExplain: %v", err)
					}
					if want := explainValues(set, sc.typ, subject); !slices.Equal(rowValues(rows), want) {
						t.Fatalf("%s %s=%s enforced=%v %s: rows %v, want values %v", kind, sc.typ, subject, on, fmtGrants(set), rows, want)
					}
					for _, row := range rows {
						v := row.Value
						if v == capWildcard {
							v = probe
						}
						want, err := srv.capAllowed(sc.ctx, kind, v)
						if err != nil {
							t.Fatalf("capAllowed: %v", err)
						}
						if got := row.State == capExplainEveryone || row.State == capExplainThisType; got != want {
							t.Fatalf("%s %s=%s enforced=%v %s: cell %q = %s, resolver allowed(%q) = %v",
								kind, sc.typ, subject, on, fmtGrants(set), row.Value, row.State, v, want)
						}
						cells++
					}
				}
			}
		}
	}
	t.Logf("%d cells agree with capAllowed", cells)
}

// grantSubsets is every subset of cands of size <= max that capability_grants'
// natural key (subject_type, subject, capability, value) could hold.
func grantSubsets(cands []types.CapabilityGrant, max int) [][]types.CapabilityGrant {
	out := [][]types.CapabilityGrant{nil}
	var walk func(start int, cur []types.CapabilityGrant)
	walk = func(start int, cur []types.CapabilityGrant) {
		if len(cur) == max {
			return
		}
		for i := start; i < len(cands); i++ {
			c := cands[i]
			if slices.ContainsFunc(cur, func(g types.CapabilityGrant) bool {
				return g.SubjectType == c.SubjectType && g.Subject == c.Subject && g.Value == c.Value
			}) {
				continue
			}
			next := append(slices.Clone(cur), c)
			out = append(out, next)
			walk(i+1, next)
		}
	}
	walk(0, nil)
	return out
}

// explainValues is the row set the grid must list: the default, then every
// specific value named by a row of the subject or `all`, sorted.
func explainValues(set []types.CapabilityGrant, typ types.CapabilitySubjectType, subject string) []string {
	var vs []string
	for _, g := range set {
		mine := g.SubjectType == types.CapabilitySubjectAll || (g.SubjectType == typ && g.Subject == subject)
		if mine && g.Value != capWildcard && !slices.Contains(vs, g.Value) {
			vs = append(vs, g.Value)
		}
	}
	sort.Strings(vs)
	return append([]string{capWildcard}, vs...)
}

func rowValues(rows []capExplainRow) []string {
	vs := make([]string, len(rows))
	for i, r := range rows {
		vs[i] = r.Value
	}
	return vs
}

func fmtGrants(set []types.CapabilityGrant) string {
	b, _ := json.Marshal(set)
	return string(b)
}

// ─── HTTP surface ────────────────────────────────────────────────────────────

// TestHandleExplainCapabilities: securityOps only, validates its query, and
// returns kinds_version alongside the grid so a client can tell its own copy
// of the kind table is stale (the same field GET /me/capabilities gained in
// #737).
func TestHandleExplainCapabilities(t *testing.T) {
	srv, st := permServer(t)
	admin := permAdmin(t)
	st.grants = []types.CapabilityGrant{
		grant(types.CapabilitySubjectGroup, "eng", capSecret, "prod-db", types.CapabilityDeny),
	}

	w := doSSO(t, srv, http.MethodGet, "/api/v1/permissions/explain?subject_type=group&subject=eng&kinds=secret", admin, "")
	if w.Code != http.StatusOK {
		t.Fatalf("GET /permissions/explain = %d: %s", w.Code, w.Body.String())
	}
	var body explainResponse
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v: %s", err, w.Body.String())
	}
	if body.KindsVersion != capKindsVersion {
		t.Errorf("kinds_version = %d, want %d", body.KindsVersion, capKindsVersion)
	}
	if len(body.Rows) != 2 {
		t.Fatalf("rows = %+v, want 2 (the default + the deny)", body.Rows)
	}

	member := permMember(t)
	if w := doSSO(t, srv, http.MethodGet, "/api/v1/permissions/explain?subject_type=group&subject=eng", member, ""); w.Code != http.StatusForbidden {
		t.Fatalf("member: status = %d, want 403: %s", w.Code, w.Body.String())
	}

	for _, tc := range []struct {
		name, query string
	}{
		{"missing subject_type", "?subject=eng"},
		{"bad subject_type", "?subject_type=all&subject=x"},
		{"missing subject", "?subject_type=group"},
		{"blank subject", "?subject_type=user&subject=%20%20"},
		{"non-ASCII group", "?subject_type=group&subject=%C3%A9ng"},
		{"unknown kind", "?subject_type=group&subject=eng&kinds=no_such_kind"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := doSSO(t, srv, http.MethodGet, "/api/v1/permissions/explain"+tc.query, admin, "")
			if w.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400: %s", w.Code, w.Body.String())
			}
		})
	}

	// user_type is accepted even though no grant can name one yet (UT-3):
	// Explain answers the defaults, not a refusal.
	w = doSSO(t, srv, http.MethodGet, "/api/v1/permissions/explain?subject_type=user_type&subject=portfolio-manager", admin, "")
	if w.Code != http.StatusOK {
		t.Fatalf("user_type subject: status = %d, want 200: %s", w.Code, w.Body.String())
	}
}

// TestHandleExplainCanonicalizesSubject: the subject is folded the way the
// grant write boundary folds it, so an admin checking "Alice@Corp.com" or
// " alice@corp.com" sees the deny stored for alice@corp.com rather than
// "everyone". Repeated kinds answer once.
func TestHandleExplainCanonicalizesSubject(t *testing.T) {
	srv, st := permServer(t)
	admin := permAdmin(t)
	st.grants = []types.CapabilityGrant{
		grant(types.CapabilitySubjectUser, "alice@corp.com", capSecret, capWildcard, types.CapabilityDeny),
		grant(types.CapabilitySubjectGroup, "eng", capSecret, capWildcard, types.CapabilityDeny),
	}
	for _, q := range []string{
		"subject_type=user&subject=Alice@Corp.com&kinds=secret",
		"subject_type=user&subject=%20alice@corp.com%20&kinds=secret",
		"subject_type=group&subject=%20ENG&kinds=secret",
		"subject_type=user&subject=alice@corp.com&kinds=secret,secret",
	} {
		t.Run(q, func(t *testing.T) {
			w := doSSO(t, srv, http.MethodGet, "/api/v1/permissions/explain?"+q, admin, "")
			if w.Code != http.StatusOK {
				t.Fatalf("status = %d: %s", w.Code, w.Body.String())
			}
			var body explainResponse
			if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
				t.Fatalf("decode: %v: %s", err, w.Body.String())
			}
			if len(body.Rows) != 1 || body.Rows[0].State != capExplainBlocked {
				t.Fatalf("rows = %+v, want one secret row, blocked", body.Rows)
			}
			if body.Subject != "alice@corp.com" && body.Subject != "eng" {
				t.Errorf("subject = %q, want the canonical spelling", body.Subject)
			}
		})
	}
}
