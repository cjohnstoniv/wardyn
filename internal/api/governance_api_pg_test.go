// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/policyref"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// The policy tree's API pins (the TREE-API lane): run_in, the read-only Default unit, membership
// move and promote, the check, the tree dry run and the caller's contexts.

// setLegacy marks memberships as rows from before the tree (legacy=true).
func setLegacy(t *testing.T, e *govEnv, ids ...uuid.UUID) {
	t.Helper()
	if _, err := e.pool.Exec(context.Background(), `UPDATE governance_memberships SET legacy = true WHERE id = ANY($1)`, ids); err != nil {
		t.Fatal(err)
	}
}

// membershipRow is one membership's unit and legacy flag as stored.
func membershipRow(t *testing.T, e *govEnv, id uuid.UUID) (unit uuid.UUID, legacy bool) {
	t.Helper()
	if err := e.pool.QueryRow(context.Background(), `SELECT unit_id, legacy FROM governance_memberships WHERE id = $1`, id).Scan(&unit, &legacy); err != nil {
		t.Fatalf("read membership %s: %v", id, err)
	}
	return unit, legacy
}

func (e *govEnv) check(body string) governanceCheckResponse {
	e.t.Helper()
	w := e.admin(http.MethodPost, "/api/v1/governance/check", body)
	var got governanceCheckResponse
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &got) != nil {
		e.t.Fatalf("check %s = %d %s, want 200", body, w.Code, w.Body)
	}
	return got
}

func contextIDs(cs []checkContext) []uuid.UUID {
	out := make([]uuid.UUID, len(cs))
	for i, c := range cs {
		out[i] = c.UnitID
	}
	return out
}

// runInEnv is a person who is a member of ri-a (home) and ri-b, and a unit ri-c of someone else's.
type runInEnv struct {
	*queueEnv
	a, b, c types.GovernanceUnit
}

func newRunInEnv(t *testing.T) *runInEnv {
	t.Helper()
	e := &runInEnv{queueEnv: newQueueEnv(t)}
	e.a = e.seedProfile("ri-a", "api.anthropic.com", "pypi.org")
	e.b = e.seedProfile("ri-b", "api.anthropic.com", "npmjs.org")
	e.c = e.seedProfile("ri-c", "api.anthropic.com", "github.com")
	treeMember(t, e.govEnv, e.a, types.CapabilitySubjectUser, e.personP, 1, true)
	treeMember(t, e.govEnv, e.b, types.CapabilitySubjectUser, e.personP, 9, false)
	treeMember(t, e.govEnv, e.c, types.CapabilitySubjectUser, e.otherP, 1, false)
	return e
}

func runInField(v string) string { return `,"run_in":"` + v + `"` }

// ranIn checks run id ran in want: the unit its start captured on the run and the policy dispatch
// handed the sandbox. A pooled launch is admitted through the queue, so both are read once it starts.
func (e *runInEnv) ranIn(t *testing.T, what string, id uuid.UUID, want types.GovernanceUnit) {
	t.Helper()
	if got := e.waitStarted(id).UnitID; got == nil || *got != want.ID {
		t.Errorf("%s: run unit_id %v, want %s", what, got, want.Name)
	}
	other := map[uuid.UUID]string{e.a.ID: "npmjs.org", e.b.ID: "pypi.org"}[want.ID]
	if got := e.specFor(id).ProxyConfig.Policy.AllowedDomains; !slices.Contains(got, want.Ceiling.AllowedDomains[1]) || slices.Contains(got, other) {
		t.Errorf("%s: dispatched allowed_domains %v, want %s's %v", what, got, want.Name, want.Ceiling.AllowedDomains)
	}
}

// TestRunInPicksTheContext: run_in naming one of the person's units, by id or by exact name, runs
// there; no run_in runs in the home unit.
func TestRunInPicksTheContext(t *testing.T) {
	e := newRunInEnv(t)
	pool := e.cappedPool("ri-open", 8)
	for _, tc := range []struct {
		what, extra string
		want        types.GovernanceUnit
	}{
		{"run_in by id", runInField(e.b.ID.String()), e.b},
		{"run_in by name", runInField(e.b.Name), e.b},
		{"no run_in", "", e.a},
	} {
		out, w := e.launch(e.person, pool, tc.extra)
		if w.Code != http.StatusCreated || out.State == types.RunQueued {
			t.Fatalf("%s: create = %d %s, want 201 started", tc.what, w.Code, w.Body)
		}
		e.ranIn(t, tc.what, out.ID, tc.want)
	}
}

// TestRunInNotAContext: run_in naming someone else's unit, an unknown id or an unknown name is one
// refusal, byte for byte, on create, preflight and the policy preview.
func TestRunInNotAContext(t *testing.T) {
	e := newRunInEnv(t)
	var first string
	for _, v := range []string{e.c.ID.String(), e.c.Name, uuid.NewString(), "no-such-unit"} {
		for _, door := range placementDoors {
			w := e.call(e.person, http.MethodPost, door, endBody("background", strings.TrimPrefix(runInField(v), ",")))
			if w.Code != http.StatusForbidden || wireReason(t, w) != reasonUnitNotAContext {
				t.Errorf("%s run_in %q = %d %s, want 403 %s", door, v, w.Code, w.Body, reasonUnitNotAContext)
				continue
			}
			if first == "" {
				first = w.Body.String()
			} else if w.Body.String() != first {
				t.Errorf("%s run_in %q answered %s, want the same body as %s", door, v, w.Body, first)
			}
		}
	}
}

// TestRunInOneContextPerRequest: every ceiling read in one create sees the unit run_in chose: the unit
// captured on the run and the dispatched policy, with preflight agreeing.
func TestRunInOneContextPerRequest(t *testing.T) {
	e := newRunInEnv(t)
	pool := e.cappedPool("ri-one", 8)
	extra := `"runner_pool_id":"` + pool.ID.String() + `"` + runInField(e.b.ID.String())
	if w := e.call(e.person, http.MethodPost, "/api/v1/runs/preflight", endBody("background", extra)); w.Code != http.StatusOK {
		t.Fatalf("preflight run_in ri-b = %d %s, want 200", w.Code, w.Body)
	}
	out, w := e.launch(e.person, pool, runInField(e.b.ID.String()))
	if w.Code != http.StatusCreated {
		t.Fatalf("create = %d %s", w.Code, w.Body)
	}
	e.ranIn(t, "run_in ri-b", out.ID, e.b)
}

// TestRunInSurvivesTheQueue: a launch queued with run_in starts in that unit, not the home one.
func TestRunInSurvivesTheQueue(t *testing.T) {
	e := newRunInEnv(t)
	pool := e.cappedPool("ri-full", 1)
	occupant := e.occupy(pool)
	queued := e.queuedLaunch(e.person, pool, runInField(e.b.ID.String()))
	e.free(occupant)
	e.eventually("the queued run to leave the queue", func() bool {
		r := e.run(queued.ID)
		return r.State == types.RunFailed || (r.AdmittedAt != nil && r.SPIFFEID != "" && r.State != types.RunPending)
	})
	if r := e.run(queued.ID); r.State == types.RunFailed {
		t.Fatalf("the queued run_in launch ended FAILED %q, want it started in ri-b", r.StatusDetail)
	}
	e.ranIn(t, "queued run_in ri-b", queued.ID, e.b)
}

// TestDefaultUnitPut: PUT and DELETE on the Default unit are 409 with the read-only reason, with the
// second-human switch off and on; a child of Default is still created.
func TestDefaultUnitPut(t *testing.T) {
	for _, on := range []bool{false, true} {
		var e *govEnv
		if on {
			e = newGovEnv(t)
		} else {
			e = newGovEnvOn(t, throwawayPGPool(t))
			treeChild(t, e, `{"name":"du-child","mode":"pass_through","parent_id":"`+store.DefaultUnitID.String()+`"}`)
		}
		path := "/api/v1/governance/units/" + store.DefaultUnitID.String()
		for method, body := range map[string]string{http.MethodPut: profileBody("Default", "pypi.org"), http.MethodDelete: ""} {
			for name, w := range map[string]*httptest.ResponseRecorder{"admin token": e.admin(method, path, body), "security admin": e.call(e.alice, method, path, body)} {
				if w.Code != http.StatusConflict || wireReason(t, w) != reasonGovernanceUnitDefaultReadonly {
					t.Errorf("switch %v, %s %s Default = %d %s, want 409 %s", on, name, method, w.Code, w.Body, reasonGovernanceUnitDefaultReadonly)
				}
			}
		}
		if n := e.pendingCount(); n != 0 {
			t.Errorf("switch %v: %d changes held for the Default unit, want none", on, n)
		}
	}
}

// TestPromote: with the switch on a promote is held and a second human approves it; the row is then
// not legacy and the person has one more context. With the switch off it is direct and audited.
func TestPromote(t *testing.T) {
	for _, on := range []bool{false, true} { // off first: newGovEnv sets the switch for the rest of the test
		var e *govEnv
		if on {
			e = newGovEnv(t)
		} else {
			e = newGovEnvOn(t, throwawayPGPool(t))
		}
		a := e.seedProfile("pr-a", "a.example")
		b := e.seedProfile("pr-b", "b.example")
		ma := treeMember(t, e, a, types.CapabilitySubjectUser, "sub-pr", 9, false)
		mb := treeMember(t, e, b, types.CapabilitySubjectUser, "sub-pr", 1, false)
		setLegacy(t, e, ma.ID, mb.ID)
		before := e.check(`{"user_subjects":["sub-pr"]}`)
		if len(before.Contexts) != 1 || len(before.Dormant) != 1 || before.Dormant[0].MembershipID != mb.ID {
			t.Fatalf("switch %v: before the promote contexts %v dormant %+v, want pr-a and pr-b dormant", on, contextIDs(before.Contexts), before.Dormant)
		}
		path := "/api/v1/governance/memberships/" + mb.ID.String() + "/promote"
		if on {
			ch := e.pending(e.call(e.alice, http.MethodPost, path, ""))
			if _, legacy := membershipRow(t, e, mb.ID); !legacy {
				t.Fatalf("a held promote changed the row before approval")
			}
			e.approve(ch.ID)
		} else if w := e.call(e.alice, http.MethodPost, path, ""); w.Code != http.StatusOK {
			t.Fatalf("direct promote = %d %s, want 200", w.Code, w.Body)
		}
		if _, legacy := membershipRow(t, e, mb.ID); legacy {
			t.Errorf("switch %v: the promoted row is still legacy", on)
		}
		after := e.check(`{"user_subjects":["sub-pr"]}`)
		if len(after.Contexts) != len(before.Contexts)+1 || len(after.Dormant) != 0 || !slices.Contains(contextIDs(after.Contexts), b.ID) {
			t.Errorf("switch %v: after the promote contexts %v dormant %+v, want pr-a and pr-b", on, contextIDs(after.Contexts), after.Dormant)
		}
		if got := e.audits("governance.membership.promote"); len(got) != 1 || got[0].Target != mb.ID.String() {
			t.Errorf("switch %v: promote audits %+v, want one for the row", on, got)
		}
	}
}

// TestLegacyDeleteSecurityAdmin: deleting the legacy row that wins for someone, with a lower legacy row
// left, names the unit that may take over: in the held change with the switch on, in the audit with it
// off (the delete itself direct, as before).
func TestLegacyDeleteSecurityAdmin(t *testing.T) {
	for _, on := range []bool{false, true} { // off first: newGovEnv sets the switch for the rest of the test
		var e *govEnv
		if on {
			e = newGovEnv(t)
		} else {
			e = newGovEnvOn(t, throwawayPGPool(t))
		}
		a := e.seedProfile("ld-a", "a.example")
		b := e.seedProfile("ld-b", "b.example")
		ma := treeMember(t, e, a, types.CapabilitySubjectUser, "sub-ld", 9, false)
		mb := treeMember(t, e, b, types.CapabilitySubjectUser, "sub-ld", 1, false)
		setLegacy(t, e, ma.ID, mb.ID)
		want := []unitActivation{{UnitID: b.ID, UnitName: b.Name}}
		w := e.call(e.alice, http.MethodDelete, "/api/v1/governance/memberships/"+ma.ID.String(), "")
		var got []unitActivation
		if on {
			var diff governanceDiffBody
			if err := json.Unmarshal(e.pending(w).Diff, &diff); err != nil {
				t.Fatal(err)
			}
			got = diff.Activates
		} else {
			if w.Code != http.StatusNoContent && w.Code != http.StatusOK {
				t.Fatalf("direct delete = %d %s", w.Code, w.Body)
			}
			rows := e.audits("governance.membership.delete")
			if len(rows) != 1 {
				t.Fatalf("delete audits %+v, want one", rows)
			}
			var data struct {
				Activates []unitActivation `json:"activates"`
			}
			if err := json.Unmarshal(rows[0].Data, &data); err != nil {
				t.Fatal(err)
			}
			got = data.Activates
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("switch %v: activates %+v, want %+v", on, got, want)
		}
	}
}

// TestMembershipMove: a move changes the row's unit and keeps its id; with the switch on it is held
// until a second human approves; an unknown destination is 404.
func TestMembershipMove(t *testing.T) {
	for _, on := range []bool{false, true} {
		var e *govEnv
		if on {
			e = newGovEnv(t)
		} else {
			e = newGovEnvOn(t, throwawayPGPool(t))
		}
		a := e.seedProfile("mv-a", "a.example")
		b := e.seedProfile("mv-b", "b.example")
		m := treeMember(t, e, a, types.CapabilitySubjectUser, "sub-mv", 3, false)
		path := "/api/v1/governance/memberships/" + m.ID.String()
		if !on { // a held move is checked at approval; the direct write answers at once
			if w := e.call(e.alice, http.MethodPut, path, `{"unit_id":"`+uuid.NewString()+`"}`); w.Code != http.StatusNotFound || wireReason(t, w) != reasonGovernanceUnitNotFoundByID {
				t.Errorf("move to an unknown unit = %d %s, want 404 %s", w.Code, w.Body, reasonGovernanceUnitNotFoundByID)
			}
		}
		w := e.call(e.alice, http.MethodPut, path, `{"unit_id":"`+b.ID.String()+`"}`)
		if on {
			ch := e.pending(w)
			if unit, _ := membershipRow(t, e, m.ID); unit != a.ID {
				t.Fatalf("a held move changed the row before approval")
			}
			e.approve(ch.ID)
		} else if w.Code != http.StatusOK {
			t.Fatalf("direct move = %d %s, want 200", w.Code, w.Body)
		}
		if unit, _ := membershipRow(t, e, m.ID); unit != b.ID {
			t.Errorf("switch %v: the moved row is in %s, want mv-b", on, unit)
		}
		var n int
		if err := e.pool.QueryRow(context.Background(), `SELECT count(*) FROM governance_memberships WHERE subject = 'sub-mv'`).Scan(&n); err != nil || n != 1 {
			t.Errorf("switch %v: %d rows for the subject after the move (%v), want 1", on, n, err)
		}
	}
}

// TestMembershipMoveOrAddIntoDefault: adding a membership to the Default unit, moving one into it and
// a dry run of that move are 409 governance_membership_default_unit, with the switch off and on, and
// nothing is held or moved.
func TestMembershipMoveOrAddIntoDefault(t *testing.T) {
	for _, on := range []bool{false, true} { // off first: newGovEnv sets the switch for the rest of the test
		var e *govEnv
		if on {
			e = newGovEnv(t)
		} else {
			e = newGovEnvOn(t, throwawayPGPool(t))
		}
		a := e.seedProfile("md-a", "a.example")
		m := treeMember(t, e, a, types.CapabilitySubjectUser, "sub-md", 1, false)
		def := store.DefaultUnitID.String()
		for what, w := range map[string]*httptest.ResponseRecorder{
			"add":     e.call(e.alice, http.MethodPost, "/api/v1/governance/memberships", `{"subject_type":"user","subject":"sub-md2","unit_id":"`+def+`","priority":1}`),
			"move":    e.call(e.alice, http.MethodPut, "/api/v1/governance/memberships/"+m.ID.String(), `{"unit_id":"`+def+`"}`),
			"dry run": e.call(e.alice, http.MethodPost, "/api/v1/governance/dry-run", `{"target":"membership_move","id":"`+m.ID.String()+`","unit_id":"`+def+`"}`),
		} {
			if w.Code != http.StatusConflict || wireReason(t, w) != reasonGovernanceMembershipDefaultUnit {
				t.Errorf("switch %v, %s into Default = %d %s, want 409 %s", on, what, w.Code, w.Body, reasonGovernanceMembershipDefaultUnit)
			}
		}
		if n := e.pendingCount(); n != 0 {
			t.Errorf("switch %v: %d changes held, want none", on, n)
		}
		if unit, _ := membershipRow(t, e, m.ID); unit != a.ID {
			t.Errorf("switch %v: the membership is in %s, want md-a", on, unit)
		}
	}
}

// TestGovernanceCheckEqualsResolve: for every context the check lists, its effective walls equal the
// ceiling a run resolves with run_in naming that unit, and with none, the home one.
func TestGovernanceCheckEqualsResolve(t *testing.T) {
	e := newGovEnvOn(t, throwawayPGPool(t))
	root := e.seedProfile("ck-root", "pypi.org", "npmjs.org", "github.com")
	child := treeChild(t, e, `{"name":"ck-child","parent_id":"`+root.ID.String()+`","overlay":{"allowed_domains":["pypi.org"]}}`)
	sib := e.seedProfile("ck-sib", "github.com")
	l1 := e.seedProfile("ck-l1", "npmjs.org")
	l2 := e.seedProfile("ck-l2", "pypi.org")
	treeMember(t, e, root, types.CapabilitySubjectUser, "sub-ck", 1, true)
	treeMember(t, e, child, types.CapabilitySubjectUser, "sub-ck", 5, false)
	treeMember(t, e, sib, types.CapabilitySubjectUser, "sub-ck", 2, false)
	m1 := treeMember(t, e, l1, types.CapabilitySubjectUser, "sub-ck", 7, false)
	m2 := treeMember(t, e, l2, types.CapabilitySubjectUser, "sub-ck", 0, false)
	setLegacy(t, e, m1.ID, m2.ID)
	all := e.check(`{"user_subjects":["sub-ck"]}`)
	if len(all.Contexts) != 4 || len(all.Dormant) != 1 || all.Dormant[0].MembershipID != m2.ID {
		t.Fatalf("contexts %v dormant %+v, want root, child, sib and ck-l1, with ck-l2 dormant", contextIDs(all.Contexts), all.Dormant)
	}
	for _, runIn := range append([]uuid.UUID{uuid.Nil}, contextIDs(all.Contexts)...) {
		body := `{"user_subjects":["sub-ck"]}`
		if runIn != uuid.Nil {
			body = `{"user_subjects":["sub-ck"],"run_in":"` + runIn.String() + `"}`
		}
		got := e.check(body).Effective
		want := treeCeiling(t, e, withContextChoice(treeCtx("sub-ck", nil, ""), contextChoice{RunIn: runIn}))
		if got == nil || !reflect.DeepEqual(got.Ceiling, redactSpecForRead(want.Spec, true)) || !reflect.DeepEqual(got.Limits, want.Limits) {
			t.Errorf("run_in %s: check effective %+v, want the resolved %+v %+v", runIn, got, want.Spec, want.Limits)
		}
	}
}

// TestDryRunEqualsEffect: a unit edit's and a membership move's dry-run report equals the effect the
// same write has when it is applied.
func TestDryRunEqualsEffect(t *testing.T) {
	e := newGovEnvOn(t, throwawayPGPool(t))
	ctx := context.Background()
	a := e.seedProfile("dr-a", "pypi.org", "npmjs.org")
	b := e.seedProfile("dr-b", "github.com")
	child := treeChild(t, e, `{"name":"dr-child","mode":"pass_through","parent_id":"`+a.ID.String()+`"}`)
	treeMember(t, e, a, types.CapabilitySubjectUser, "sub-d1", 1, false)
	treeMember(t, e, child, types.CapabilitySubjectGroup, "eng", 1, false)
	moved := treeMember(t, e, b, types.CapabilitySubjectUser, "sub-d2", 1, false)
	edit := profileBody("dr-a", "pypi.org")
	for _, tc := range []struct {
		what, dryRun string
		apply        func() *httptest.ResponseRecorder
	}{
		{dryRunTargetUnit, `{"target":"unit","id":"` + a.ID.String() + `",` + strings.TrimPrefix(edit, "{"),
			func() *httptest.ResponseRecorder {
				return e.admin(http.MethodPut, "/api/v1/governance/units/"+a.ID.String(), edit)
			}},
		{dryRunTargetMembershipMove, `{"target":"membership_move","id":"` + moved.ID.String() + `","unit_id":"` + a.ID.String() + `"}`,
			func() *httptest.ResponseRecorder {
				return e.admin(http.MethodPut, "/api/v1/governance/memberships/"+moved.ID.String(), `{"unit_id":"`+a.ID.String()+`"}`)
			}},
	} {
		w := e.admin(http.MethodPost, "/api/v1/governance/dry-run", tc.dryRun)
		var got treeDryRunReport
		if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &got) != nil {
			t.Fatalf("%s dry run = %d %s", tc.what, w.Code, w.Body)
		}
		before, err := readTreeSnapshot(ctx, e.pool)
		if err != nil {
			t.Fatal(err)
		}
		if w := tc.apply(); w.Code != http.StatusOK {
			t.Fatalf("%s applied = %d %s", tc.what, w.Code, w.Body)
		}
		after, err := readTreeSnapshot(ctx, e.pool)
		if err != nil {
			t.Fatal(err)
		}
		want, err := e.srv.treeEffect(ctx, before, after)
		if err != nil {
			t.Fatal(err)
		}
		want.Target = tc.what
		gotJSON, _ := json.Marshal(got)
		wantJSON, _ := json.Marshal(want)
		if len(want.People) == 0 || string(gotJSON) != string(wantJSON) {
			t.Errorf("%s: dry run %s\nthe applied effect %s", tc.what, gotJSON, wantJSON)
		}
	}
}

// TestDefaultPolicyContexts: a member's default policy lists their own units only, answers another
// of them for ?run_in= with its path, and refuses a unit of someone else's.
func TestDefaultPolicyContexts(t *testing.T) {
	e := newGovEnvOn(t, throwawayPGPool(t))
	root := e.seedProfile("dp-root", "pypi.org", "npmjs.org")
	child := treeChild(t, e, `{"name":"dp-child","parent_id":"`+root.ID.String()+`","overlay":{"allowed_domains":["pypi.org"]}}`)
	other := e.seedProfile("dp-other", "github.com")
	treeMember(t, e, root, types.CapabilitySubjectUser, "sub-dp", 1, true)
	treeMember(t, e, child, types.CapabilitySubjectUser, "sub-dp", 1, false)
	treeMember(t, e, other, types.CapabilitySubjectUser, "sub-dp-other", 1, false)
	member := ssoSessionWith(t, "sub-dp", types.UserTypeStandard)
	get := func(c *http.Cookie, query string) defaultPolicyResponse {
		t.Helper()
		var w *httptest.ResponseRecorder
		if c == nil {
			w = e.admin(http.MethodGet, "/api/v1/policies/default"+query, "")
		} else {
			w = e.call(c, http.MethodGet, "/api/v1/policies/default"+query, "")
		}
		var resp defaultPolicyResponse
		if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &resp) != nil {
			t.Fatalf("GET default%s = %d %s", query, w.Code, w.Body)
		}
		return resp
	}
	home := get(member, "")
	want := []defaultPolicyContext{{UnitID: child.ID, UnitName: child.Name}, {UnitID: root.ID, UnitName: root.Name, Home: true}}
	slices.SortFunc(home.Contexts, func(x, y defaultPolicyContext) int { return strings.Compare(x.UnitName, y.UnitName) })
	if !reflect.DeepEqual(home.Contexts, want) || home.UnitName != root.Name {
		t.Errorf("contexts %+v unit %q, want %+v answering dp-root", home.Contexts, home.UnitName, want)
	}
	in := get(member, "?run_in=dp-child")
	if in.UnitName != child.Name || !slices.Equal(in.AllowedDomains, []string{"pypi.org"}) || in.UnitPath != nil {
		t.Errorf("?run_in=dp-child answered %q %v path %+v, want dp-child [pypi.org] and no path: the units above are admin-only", in.UnitName, in.AllowedDomains, in.UnitPath)
	}
	wantPath := []unitPathLink{{ID: child.ID, Name: child.Name}, {ID: root.ID, Name: root.Name}}
	if sec := get(ssoSession(t, "sub-dp", "sub-dp@corp.example", oidc.RoleSecurityAdmin), "?run_in=dp-child"); !reflect.DeepEqual(sec.UnitPath, wantPath) {
		t.Errorf("a security admin's ?run_in=dp-child path %+v, want %+v", sec.UnitPath, wantPath)
	}
	if w := e.call(member, http.MethodGet, "/api/v1/policies/default?run_in="+other.ID.String(), ""); w.Code != http.StatusForbidden || wireReason(t, w) != reasonUnitNotAContext {
		t.Errorf("?run_in= someone else's unit = %d %s, want 403 %s", w.Code, w.Body, reasonUnitNotAContext)
	}
	if admin := get(nil, ""); admin.Contexts != nil {
		t.Errorf("the admin token's default lists contexts %+v, want none", admin.Contexts)
	}
}

// TestDenialPolicyCarriesLocked: a refusal attributed to a knob whose provenance is locked says so on
// its policy; one attributed to an unlocked knob carries no locked key.
func TestDenialPolicyCarriesLocked(t *testing.T) {
	ref := policyref.Project(policyref.SourceUnit, "eng", nil)
	for _, locked := range []bool{true, false} {
		body := errorBody{Error: "refused", Reason: "r", Policy: policyref.Layered(policyref.AttributedTo(ref, policyref.Provenance{Layer: "unit", Name: "eng", Locked: locked}))}
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		if got := strings.Contains(string(b), `"locked":true`); got != locked || (!locked && strings.Contains(string(b), `"locked"`)) {
			t.Errorf("locked %v: refusal %s", locked, b)
		}
	}
	if ref.Locked {
		t.Errorf("AttributedTo changed the ref it was given")
	}
}
