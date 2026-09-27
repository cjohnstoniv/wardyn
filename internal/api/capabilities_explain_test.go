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

// explainGrid runs capExplain for one subject over a fake store holding
// grants, enf and restricted.
func explainGrid(t *testing.T, st *capStore, typ types.CapabilitySubjectType, who string, kinds []string) []capExplainRow {
	t.Helper()
	_, subj, err := explainPrincipal(typ, who)
	if err != nil {
		t.Fatalf("explainPrincipal(%s, %q): %v", typ, who, err)
	}
	rows, err := capServer(st).capExplain(context.Background(), subj, kinds)
	if err != nil {
		t.Fatalf("capExplain: %v", err)
	}
	return rows
}

// groupGrid is explainGrid for the group "eng".
func groupGrid(t *testing.T, grants []types.CapabilityGrant, enf map[string]bool, kinds []string) []capExplainRow {
	t.Helper()
	return explainGrid(t, &capStore{grants: grants, enf: enf}, types.CapabilitySubjectGroup, "eng", kinds)
}

func explainRow(rows []capExplainRow, kind, value string) (capExplainRow, bool) {
	i := slices.IndexFunc(rows, func(r capExplainRow) bool { return r.Kind == kind && r.Value == value })
	if i < 0 {
		return capExplainRow{}, false
	}
	return rows[i], true
}

func explainState(rows []capExplainRow, kind, value string) capExplainState {
	r, _ := explainRow(rows, kind, value)
	return r.State
}

// TestCapExplainDefaults: a kind with no row at all for a subject (or "all")
// reports the kind's own direction default — Everyone for the narrowing
// kinds, Admins only for the one widening kind (image) — exactly the posture
// capBatch.decide falls back to on an absent row.
func TestCapExplainDefaults(t *testing.T) {
	got := groupGrid(t, nil, nil, capabilityKinds)
	if len(got) != len(capabilityKinds) {
		t.Fatalf("capExplain(no grants) = %d rows, want one per kind (%d)", len(got), len(capabilityKinds))
	}
	for _, row := range got {
		want := capExplainEveryone
		if capKinds[row.Kind].direction == capWidening {
			want = capExplainAdminsOnly
		}
		if row.Value != capWildcard || row.State != want || row.Restricted {
			t.Errorf("%s: got %+v, want {%s %s}", row.Kind, row, capWildcard, want)
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
			rows := groupGrid(t, []types.CapabilityGrant{tc.grant}, nil, []string{capSecret})
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
		grant(types.CapabilitySubjectUserType, utPM, capWorkspace, "ws-a-type", types.CapabilityAllow),
	}
	rows := groupGrid(t, grants, nil, []string{capWorkspace})
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
		rows := groupGrid(t, order, nil, []string{capEgressHost})
		if got := explainState(rows, capEgressHost, "api.example.com"); got != capExplainBlocked {
			t.Fatalf("deny-wins order %v: state = %s, want %s", order, got, capExplainBlocked)
		}
	}
}

// TestCapExplainDefaultRowIsDecidedOverWildcardRowsOnly: the default row
// answers "a value no specific row names", so it is decided over the kind's
// "*" rows alone. Decided over every row, an egress host-suffix deny would
// wall the default too — "*" as a WANT overlaps every host deny
// (capValueOverlaps) — and the grid would say Blocked for every host while
// the resolver lets an unnamed host through.
func TestCapExplainDefaultRowIsDecidedOverWildcardRowsOnly(t *testing.T) {
	st := &capStore{grants: []types.CapabilityGrant{
		grant(types.CapabilitySubjectAll, "", capEgressHost, "*.example.com", types.CapabilityDeny),
		grant(types.CapabilitySubjectGroup, "eng", capEgressHost, "api.example.com", types.CapabilityAllow),
	}}
	rows := explainGrid(t, st, types.CapabilitySubjectGroup, "eng", []string{capEgressHost})
	if got := explainState(rows, capEgressHost, capWildcard); got != capExplainEveryone {
		t.Errorf("egress_host default = %s, want %s (rows %+v)", got, capExplainEveryone, rows)
	}
	if got := explainState(rows, capEgressHost, "api.example.com"); got != capExplainBlocked {
		t.Errorf("api.example.com = %s, want %s: the broader deny walls the narrower allow", got, capExplainBlocked)
	}
	allowed, err := capServer(st).capAllowed(withOIDCGroups(operatorCtx("sub-nobody", "", oidc.RoleUser), []string{"eng"}), capEgressHost, "unnamed.example.net")
	if err != nil || !allowed {
		t.Fatalf("resolver on an unnamed host = %v, %v; want allowed — the default row must say what it says", allowed, err)
	}
}

// TestCapExplainOverlapAndEnforcement pins the cells where a row-by-row
// reading disagrees with the resolver: a broader deny walls a narrower allow
// (decide step 2), and the enforcement switch is part of the answer — an
// enforced narrowing kind with no allow is Not available (step 7), and an
// image allow while image is off is still Admins only (step 4).
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
			rows := groupGrid(t, tc.grants, tc.enf, []string{tc.kind})
			if got := explainState(rows, tc.kind, tc.value); got != tc.want {
				t.Errorf("%s %q = %s, want %s (rows %+v)", tc.kind, tc.value, got, tc.want, rows)
			}
			if got := explainState(rows, tc.kind, capWildcard); got != tc.wantDefault {
				t.Errorf("%s default = %s, want %s (rows %+v)", tc.kind, got, tc.wantDefault, rows)
			}
		})
	}
}

// TestCapExplainUserTypeGrants: a user_type subject reads that type's own
// rows (UT-3), the way the resolver reads them for a person of the type — a
// type deny is Blocked, a type allow is This type — and never another type's.
// A group or user subject does not pick up a type's rows.
func TestCapExplainUserTypeGrants(t *testing.T) {
	grants := []types.CapabilityGrant{
		grant(types.CapabilitySubjectUserType, utPM, capFeature, featureSSHKey, types.CapabilityDeny),
		grant(types.CapabilitySubjectUserType, utPM, capSecret, "prod-db", types.CapabilityAllow),
		grant(types.CapabilitySubjectUserType, utDev, capSecret, "dev-db", types.CapabilityAllow),
	}
	st := &capStore{grants: grants, enf: map[string]bool{capSecret: true}, userTypes: utKnown}
	rows := explainGrid(t, st, types.CapabilitySubjectUserType, utPM, []string{capFeature, capSecret})
	want := []capExplainRow{
		{Kind: capFeature, Value: capWildcard, State: capExplainEveryone},
		{Kind: capFeature, Value: featureSSHKey, State: capExplainBlocked},
		{Kind: capSecret, Value: capWildcard, State: capExplainNotAvailable},
		{Kind: capSecret, Value: "prod-db", State: capExplainThisType},
	}
	if !slices.Equal(rows, want) {
		t.Fatalf("user_type grid = %+v, want %+v", rows, want)
	}
	// The resolver agrees for a person of the type.
	ctx := utCtx(oidc.RoleUser, utPM, []string{})
	for _, c := range []struct {
		kind, value string
		want        bool
	}{{capFeature, featureSSHKey, false}, {capSecret, "prod-db", true}, {capSecret, "dev-db", false}} {
		if got, err := capServer(st).capAllowed(ctx, c.kind, c.value); err != nil || got != c.want {
			t.Errorf("resolver %s %q for a %s = %v, %v; want %v", c.kind, c.value, utPM, got, err, c.want)
		}
	}
	for _, typ := range []types.CapabilitySubjectType{types.CapabilitySubjectGroup, types.CapabilitySubjectUser} {
		if rows := explainGrid(t, st, typ, utPM, []string{capFeature}); len(rows) != 1 || rows[0].State != capExplainEveryone {
			t.Errorf("%s %q grid = %+v, want only the default, everyone: a type's rows are not theirs", typ, utPM, rows)
		}
	}
}

// TestCapExplainRestrictedValues: a value an admin restricted ("Available to:
// Only...", UT-10) gets its own row with restricted set, even when no grant
// names it, and reads Not available unless an allow naming the value lists
// this subject — a wildcard allow does not, a deny still wins, and on an
// image the restriction lets a listed subject in while image is off.
func TestCapExplainRestrictedValues(t *testing.T) {
	const img = "ghcr.io/org/img:1"
	allow, deny := types.CapabilityAllow, types.CapabilityDeny
	st := &capStore{
		grants: []types.CapabilityGrant{
			grant(types.CapabilitySubjectUserType, utPM, capAgent, capWildcard, allow),
			grant(types.CapabilitySubjectUserType, utPM, capAgent, "listed", allow),
			grant(types.CapabilitySubjectUserType, utPM, capAgent, "walled", deny),
			grant(types.CapabilitySubjectUserType, utPM, capAgent, "open", allow),
			grant(types.CapabilitySubjectUserType, utPM, capImage, img, allow),
		},
		restricted: map[string]map[string]bool{
			capAgent:  {"listed": true, "walled": true, "unlisted": true},
			capImage:  {img: true, "ghcr.io/org/other:1": true},
			capSecret: {"not-restrictable": true},
		},
		userTypes: utKnown,
	}
	rows := explainGrid(t, st, types.CapabilitySubjectUserType, utPM, []string{capAgent, capImage, capSecret})
	want := []capExplainRow{
		{Kind: capAgent, Value: capWildcard, State: capExplainThisType},
		{Kind: capAgent, Value: "listed", State: capExplainThisType, Restricted: true},
		{Kind: capAgent, Value: "open", State: capExplainThisType},
		{Kind: capAgent, Value: "unlisted", State: capExplainNotAvailable, Restricted: true},
		{Kind: capAgent, Value: "walled", State: capExplainBlocked, Restricted: true},
		{Kind: capImage, Value: capWildcard, State: capExplainAdminsOnly},
		{Kind: capImage, Value: img, State: capExplainThisType, Restricted: true},
		{Kind: capImage, Value: "ghcr.io/org/other:1", State: capExplainNotAvailable, Restricted: true},
		{Kind: capSecret, Value: capWildcard, State: capExplainEveryone},
	}
	if !slices.Equal(rows, want) {
		t.Fatalf("grid =\n  %+v\nwant\n  %+v", rows, want)
	}
}

// TestCapExplainWideningNoAllow: image (the one widening kind) with no allow
// row for this subject stays Admins only even when the subject holds a DENY
// at some other value — the default reflects "no grant", not "a grant of the
// opposite effect".
func TestCapExplainWideningNoAllow(t *testing.T) {
	grants := []types.CapabilityGrant{
		grant(types.CapabilitySubjectGroup, "eng", capImage, "ghcr.io/evil/image", types.CapabilityDeny),
	}
	rows := groupGrid(t, grants, nil, []string{capImage})
	if got := explainState(rows, capImage, capWildcard); got != capExplainAdminsOnly {
		t.Errorf("wildcard row = %s, want %s", got, capExplainAdminsOnly)
	}
}

// TestCapExplainUnknownKindSkipped: an unknown kind name is dropped rather
// than producing a row — the HTTP handler is what validates kinds and refuses
// the request.
func TestCapExplainUnknownKindSkipped(t *testing.T) {
	if rows := groupGrid(t, nil, nil, []string{"no_such_kind"}); len(rows) != 0 {
		t.Fatalf("capExplain(unknown kind) = %+v, want no rows", rows)
	}
}

// ─── the property: every cell equals the resolver ───────────────────────────

// TestCapExplainAgreesWithResolver is K4's acceptance test (authz design §5):
// every grid cell equals the resolver's own answer for a person who is exactly
// the subject. Generated over every kind × every grant set of up to three rows
// drawn from {wildcard, specific values, a host-set suffix} × {user, group,
// user_type, all — the subject's own name at each} × {allow, deny}, × the
// kind's switch on and off, × (on a restrictable kind) one value restricted
// or none, × all three subject types.
//
// The resolver side is the live path, not the grid's own: a ctx built the way
// humanOrAdminAuth builds one, answered by capAllowed through callerSubjects
// and ListCapabilityGrantsFor. A specific-value row is compared at its value;
// the default row at a probe value no generated row names or covers. A user
// or group subject's person carries the built-in type, which no generated row
// names.
func TestCapExplainAgreesWithResolver(t *testing.T) {
	const who, nobody = utPM, "sub-synthetic-nobody"
	person := func(sub string, groups []string, userType string) context.Context {
		return withOIDCUserType(withOIDCGroups(operatorCtx(sub, "", oidc.RoleUser), groups), userType)
	}
	subjects := []struct {
		typ types.CapabilitySubjectType
		ctx context.Context
	}{
		{types.CapabilitySubjectUser, person(who, []string{}, "")},
		{types.CapabilitySubjectGroup, person(nobody, []string{who}, "")},
		{types.CapabilitySubjectUserType, person(nobody, []string{}, who)},
	}
	scopes := []types.CapabilitySubjectType{types.CapabilitySubjectUser, types.CapabilitySubjectGroup, types.CapabilitySubjectUserType, types.CapabilitySubjectAll}
	cells := 0
	for _, kind := range capabilityKinds {
		values, probe := []string{capWildcard, "v-one", "v-two"}, "v-unnamed"
		if capKinds[kind].hostSet {
			values, probe = []string{capWildcard, "*.example.com", "api.example.com", "other.org"}, "unnamed.example.net"
		}
		restrictions := []map[string]map[string]bool{nil}
		if capKinds[kind].restrictable {
			restrictions = append(restrictions, map[string]map[string]bool{kind: {"v-one": true}})
		}
		var cands []types.CapabilityGrant
		for _, v := range values {
			for _, st := range scopes {
				subject := who
				if st == types.CapabilitySubjectAll {
					subject = ""
				}
				for _, eff := range []types.CapabilityEffect{types.CapabilityAllow, types.CapabilityDeny} {
					cands = append(cands, grant(st, subject, kind, v, eff))
				}
			}
		}
		for _, sc := range subjects {
			subject, subj, err := explainPrincipal(sc.typ, who)
			if err != nil {
				t.Fatalf("explainPrincipal(%s, %q): %v", sc.typ, who, err)
			}
			for _, set := range grantSubsets(cands, 3) {
				for _, on := range []bool{false, true} {
					for _, restricted := range restrictions {
						st := &capStore{grants: set, enf: map[string]bool{kind: on}, restricted: restricted, userTypes: utKnown}
						srv := capServer(st)
						rows, err := srv.capExplain(context.Background(), subj, []string{kind})
						if err != nil {
							t.Fatalf("capExplain: %v", err)
						}
						if want := explainValues(set, sc.typ, subject, restricted[kind]); !slices.Equal(rowValues(rows), want) {
							t.Fatalf("%s %s=%s enforced=%v restricted=%v %s: rows %v, want values %v", kind, sc.typ, subject, on, restricted, fmtGrants(set), rows, want)
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
							if got := row.State == capExplainEveryone || row.State == capExplainThisType; got != want || row.Restricted != restricted[kind][row.Value] {
								t.Fatalf("%s %s=%s enforced=%v restricted=%v %s: cell %+v, resolver allowed(%q) = %v",
									kind, sc.typ, subject, on, restricted, fmtGrants(set), row, v, want)
							}
							cells++
						}
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
// specific value named by a row of the subject or `all`, or restricted,
// sorted.
func explainValues(set []types.CapabilityGrant, typ types.CapabilitySubjectType, subject string, restricted map[string]bool) []string {
	var vs []string
	for _, g := range set {
		mine := g.SubjectType == types.CapabilitySubjectAll || (g.SubjectType == typ && g.Subject == subject)
		if mine && g.Value != capWildcard && !slices.Contains(vs, g.Value) {
			vs = append(vs, g.Value)
		}
	}
	for v := range restricted {
		if !slices.Contains(vs, v) {
			vs = append(vs, v)
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
// of the kind table is stale (the same field GET /me/capabilities carries).
func TestHandleExplainCapabilities(t *testing.T) {
	srv, st := permServer(t)
	admin := permAdmin(t)
	st.userTypes = utKnown
	st.grants = []types.CapabilityGrant{
		grant(types.CapabilitySubjectGroup, "eng", capSecret, "prod-db", types.CapabilityDeny),
		grant(types.CapabilitySubjectUserType, utPM, capSecret, "pm-db", types.CapabilityDeny),
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
		{"malformed user type", "?subject_type=user_type&subject=Portfolio%20Manager"},
		{"unknown user type", "?subject_type=user_type&subject=no-such-type"},
		{"unknown kind", "?subject_type=group&subject=eng&kinds=no_such_kind"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := doSSO(t, srv, http.MethodGet, "/api/v1/permissions/explain"+tc.query, admin, "")
			if w.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400: %s", w.Code, w.Body.String())
			}
		})
	}

	// A user type that exists (and the built-in one, which always does) is
	// answered from its own rows.
	for _, tc := range []struct {
		subject string
		want    int
	}{{utPM, 2}, {types.UserTypeStandard, 1}} {
		w = doSSO(t, srv, http.MethodGet, "/api/v1/permissions/explain?subject_type=user_type&kinds=secret&subject="+tc.subject, admin, "")
		body = explainResponse{}
		if err := json.Unmarshal(w.Body.Bytes(), &body); w.Code != http.StatusOK || err != nil || len(body.Rows) != tc.want {
			t.Fatalf("user_type %q: status = %d, rows = %+v, want 200 and %d rows: %s", tc.subject, w.Code, body.Rows, tc.want, w.Body.String())
		}
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
