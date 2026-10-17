// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/scim"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

const scimCovGroupExternal = "Eng-Group"

type scimCovGroupBody struct {
	ID          string        `json:"id"`
	ExternalID  string        `json:"externalId"`
	DisplayName string        `json:"displayName"`
	Members     []scim.Member `json:"members"`
}

func scimCovGroupAdd(ids ...string) string {
	return scimCovMembersOp("Add", ids...)
}

func scimCovGroupRemove(ids ...string) string {
	return scimCovMembersOp("Remove", ids...)
}

func scimCovMembersOp(op string, ids ...string) string {
	var ms []string
	for _, id := range ids {
		ms = append(ms, fmt.Sprintf(`{"value":%q}`, id))
	}
	return fmt.Sprintf(`{"op":%q,"path":"members","value":[%s]}`, op, strings.Join(ms, ","))
}

func scimCovPerson(st *scimCovStore, principal string) store.PrincipalIdentity {
	return st.addIdentity(store.PrincipalIdentity{Principal: principal, Issuer: scimCovIssuer, EmailLower: principal + "@corp.example"})
}

func scimCovTokenIn(st *scimCovStore, principal string, groups []string, truncated *bool) uuid.UUID {
	return st.addToken(types.APIToken{Principal: principal, Groups: groups, GroupsTruncated: truncated, Name: "t"})
}

func scimCovBoolPtr(v bool) *bool { return &v }

func TestSCIMCovGroupsListAndGet(t *testing.T) {
	st := newSCIMCovStore()
	pat := scimCovPerson(st, "sub-p")
	g := st.addGroup(scimCovGroupExternal, "Engineering", pat.ID)
	e := newSCIMCovEnv(t, st)

	for _, c := range []struct{ attr, value string }{{"displayName", "ENGINEERING"}, {"externalId", " eng-group "}} {
		w := e.scim(http.MethodGet, "/scim/v2/Groups?filter="+url.QueryEscape(c.attr+` eq "`+c.value+`"`), "")
		list := scimCovDecode[struct {
			TotalResults int                `json:"totalResults"`
			Resources    []scimCovGroupBody `json:"Resources"`
		}](t, w)
		if w.Code != http.StatusOK || list.TotalResults != 1 || list.Resources[0].ID != g.ID.String() ||
			len(list.Resources[0].Members) != 1 || list.Resources[0].Members[0].Value != pat.ID.String() {
			t.Errorf("filter %s = %d %s, want the group with its member's identity id", c.attr, w.Code, w.Body.String())
		}
	}
	w := e.scim(http.MethodGet, "/scim/v2/Groups/"+g.ID.String(), "")
	if got := scimCovDecode[scimCovGroupBody](t, w); w.Code != http.StatusOK || got.ExternalID != scimCovGroupExternal || got.DisplayName != "Engineering" {
		t.Errorf("GET = %d %s", w.Code, w.Body.String())
	}
}

func TestSCIMCovGroupsReadRefusals(t *testing.T) {
	st := newSCIMCovStore()
	g := st.addGroup(scimCovGroupExternal, "Engineering")
	e := newSCIMCovEnv(t, st)
	for name, path := range map[string]string{
		"no filter":      "/scim/v2/Groups",
		"a Users filter": "/scim/v2/Groups?filter=" + url.QueryEscape(`userName eq "x"`),
		"an unsupported": "/scim/v2/Groups?filter=" + url.QueryEscape(`displayName sw "x"`),
		"a blank filter": "/scim/v2/Groups?filter=%20",
	} {
		t.Run(name, func(t *testing.T) {
			scimCovWantError(t, e.scim(http.MethodGet, path, ""), http.StatusBadRequest, scim.TypeInvalidFilter)
		})
	}
	for _, id := range []string{"not-a-uuid", uuid.NewString()} {
		w := e.scim(http.MethodGet, "/scim/v2/Groups/"+id, "")
		scimCovWantError(t, w, http.StatusNotFound, "")
		if !strings.Contains(w.Body.String(), "no such group") {
			t.Errorf("GET /Groups/%s: %s", id, w.Body.String())
		}
	}
	listPath := "/scim/v2/Groups?filter=" + url.QueryEscape(`displayName eq "engineering"`)
	for _, c := range []struct{ name, method, path string }{
		{"searching", "SearchScimGroups", listPath},
		{"reading the group", "GetScimGroup", "/scim/v2/Groups/" + g.ID.String()},
		{"reading the members of a group", "ScimGroupMembers", "/scim/v2/Groups/" + g.ID.String()},
		{"reading the members of a listed group", "ScimGroupMembers", listPath},
	} {
		t.Run("a failure "+c.name, func(t *testing.T) {
			st.failNext(c.method, errSCIMCovBoom, 1)
			scimCovWantRetry(t, e.scim(http.MethodGet, c.path, ""), "secret-dsn")
		})
	}
}

func TestSCIMCovCreateGroup(t *testing.T) {
	st := newSCIMCovStore()
	pat := scimCovPerson(st, "sub-p")
	e := newSCIMCovEnv(t, st)

	body := fmt.Sprintf(`{"externalId":" Eng-Group ","displayName":"Engineering","members":[{"value":%q},{"value":%q},{"value":"u1091"},{"value":%q}]}`,
		pat.ID, pat.ID, uuid.New())
	w := e.scim(http.MethodPost, "/scim/v2/Groups", body)
	got := scimCovDecode[scimCovGroupBody](t, w)
	if w.Code != http.StatusCreated || got.ExternalID != "Eng-Group" || got.DisplayName != "Engineering" {
		t.Fatalf("POST = %d %s", w.Code, w.Body.String())
	}
	if len(got.Members) != 1 || got.Members[0].Value != pat.ID.String() {
		t.Errorf("members = %+v, want only the identity Wardyn knows, once", got.Members)
	}
	if rows := e.h.audit.snapshot(); len(rows) != 0 {
		t.Errorf("creating a group that grants nothing wrote audit rows: %+v", rows)
	}

	scimCovWantError(t, e.scim(http.MethodPost, "/scim/v2/Groups", `{"externalId":"eng-group","displayName":"Again"}`), http.StatusConflict, "uniqueness")
	creates := st.callCount("CreateScimGroup")
	huge := `{"externalId":"x","displayName":"` + strings.Repeat("a", scimMaxBody) + `"}`
	scimCovWantError(t, e.scim(http.MethodPost, "/scim/v2/Groups", huge), http.StatusRequestEntityTooLarge, "")
	for _, c := range []struct{ name, body, scimType string }{
		{"not JSON", `{`, scim.TypeInvalidSyntax},
		{"no displayName", `{"externalId":"x"}`, scim.TypeInvalidValue},
		{"no externalId", `{"displayName":"x"}`, scim.TypeInvalidValue},
		{"a member with no id", `{"externalId":"x","displayName":"x","members":[{"value":""}]}`, scim.TypeInvalidValue},
	} {
		t.Run(c.name, func(t *testing.T) {
			scimCovWantError(t, e.scim(http.MethodPost, "/scim/v2/Groups", c.body), http.StatusBadRequest, c.scimType)
		})
	}
	if got := st.callCount("CreateScimGroup"); got != creates {
		t.Errorf("a refused POST reached CreateScimGroup %d more times", got-creates)
	}
}

func TestSCIMCovCreateGroupStoreFailures(t *testing.T) {
	for _, method := range []string{"CreateScimGroup", "AddScimGroupMembers", "ScimGroupMembers"} {
		t.Run(method, func(t *testing.T) {
			st := newSCIMCovStore()
			e := newSCIMCovEnv(t, st)
			st.failNext(method, errSCIMCovBoom, -1)
			scimCovWantRetry(t, e.scim(http.MethodPost, "/scim/v2/Groups", `{"externalId":"g1","displayName":"G"}`), "secret-dsn")
		})
	}
}

func TestSCIMCovPatchGroupRefusals(t *testing.T) {
	st := newSCIMCovStore()
	g := st.addGroup(scimCovGroupExternal, "Engineering")
	e := newSCIMCovEnv(t, st)
	path := "/scim/v2/Groups/" + g.ID.String()
	for _, c := range []struct {
		name, path, body, scimType string
		status                     int
	}{
		{"an unknown group", "/scim/v2/Groups/" + uuid.NewString(), scimCovPatch(scimCovGroupAdd(uuid.NewString())), "", http.StatusNotFound},
		{"an id that is not a UUID", "/scim/v2/Groups/sub-p", scimCovPatch(scimCovGroupAdd(uuid.NewString())), "", http.StatusNotFound},
		{"an envelope with no schema", path, `{"Operations":[]}`, scim.TypeInvalidSyntax, http.StatusBadRequest},
		{"a members replace", path, scimCovPatch(`{"op":"Replace","path":"members","value":[{"value":"x"}]}`), scim.TypeInvalidValue, http.StatusBadRequest},
		{"a body over the cap", path, `{"x":"` + strings.Repeat("a", scimMaxBody) + `"}`, "", http.StatusRequestEntityTooLarge},
	} {
		t.Run(c.name, func(t *testing.T) {
			scimCovWantError(t, e.scim(http.MethodPatch, c.path, c.body), c.status, c.scimType)
		})
	}
	if st.callCount("AddScimGroupMembers")+st.callCount("RenameScimGroup")+st.callCount("StartGroupRemoval") != 0 {
		t.Errorf("a refused PATCH wrote: %v", st.calls)
	}
}

func TestSCIMCovPatchGroupRenamesAddsAndRefusesAnExternalIDChange(t *testing.T) {
	st := newSCIMCovStore()
	pat := scimCovPerson(st, "sub-p")
	g := st.addGroup(scimCovGroupExternal, "Engineering")
	e := newSCIMCovEnv(t, st)
	path := "/scim/v2/Groups/" + g.ID.String()

	w := e.scim(http.MethodPatch, path, scimCovPatch(
		scimCovReplace("displayName", `"Platform"`), scimCovReplace("externalId", `" ENG-GROUP "`), scimCovGroupAdd(pat.ID.String(), uuid.NewString(), "u1091")))
	got := scimCovDecode[scimCovGroupBody](t, w)
	if w.Code != http.StatusOK || got.DisplayName != "Platform" || len(got.Members) != 1 || got.Members[0].Value != pat.ID.String() {
		t.Fatalf("PATCH = %d %s, want the rename and the known member added", w.Code, w.Body.String())
	}
	if members := st.groupMembers(g.ID); !slices.Equal(members, []uuid.UUID{pat.ID}) {
		t.Errorf("stored members = %v", members)
	}

	w = e.scim(http.MethodPatch, path, scimCovPatch(scimCovReplace("displayName", `"Renamed"`), scimCovReplace("externalId", `"another"`)))
	scimCovWantError(t, w, http.StatusBadRequest, scim.TypeInvalidValue)
	if st.groups[g.ID].DisplayName != "Platform" {
		t.Errorf("display name = %q, want the refused request to leave it alone", st.groups[g.ID].DisplayName)
	}
	if rows := e.h.audit.snapshot(); len(rows) != 0 {
		t.Errorf("a rename wrote audit rows: %+v", rows)
	}
}

func TestSCIMCovPatchGroupStoreFailures(t *testing.T) {
	for _, c := range []struct {
		name, method, body string
	}{
		{"adding members", "AddScimGroupMembers", scimCovGroupAdd(uuid.NewString())},
		{"renaming", "RenameScimGroup", scimCovReplace("displayName", `"X"`)},
		{"listing everyone to remove", "GroupRemovalIdentities", `{"op":"Remove","path":"members"}`},
	} {
		t.Run(c.name, func(t *testing.T) {
			st := newSCIMCovStore()
			g := st.addGroup(scimCovGroupExternal, "Engineering", scimCovPerson(st, "sub-p").ID)
			e := newSCIMCovEnv(t, st)
			st.failNext(c.method, errSCIMCovBoom, -1)
			w := e.scim(http.MethodPatch, "/scim/v2/Groups/"+g.ID.String(), scimCovPatch(c.body))
			scimCovWantRetry(t, w, "secret-dsn")
			if c.method == "GroupRemovalIdentities" && (st.callCount("StartGroupRemoval") != 0 || len(e.h.audit.snapshot()) != 0) {
				t.Error("a failed read of who to remove was followed by a removal")
			}
		})
	}
}

// A member removed is a mover: the sessions are cut without touching the other credentials, the tokens whose
// group snapshot holds the group (or cannot say) are revoked, and one row records it.
func TestSCIMCovRemovingAMemberCutsSessionsAndRevokesOnlyTheTokensItAffects(t *testing.T) {
	st := newSCIMCovStore()
	p := scimCovPerson(st, "sub-p")
	g := st.addGroup(scimCovGroupExternal, "Engineering", p.ID)
	holds := scimCovTokenIn(st, "sub-p", []string{" ENG-group "}, scimCovBoolPtr(false))
	other := scimCovTokenIn(st, "sub-p", []string{"another-group"}, scimCovBoolPtr(false))
	never := scimCovTokenIn(st, "sub-p", nil, nil)
	truncated := scimCovTokenIn(st, "sub-p", []string{"another-group"}, scimCovBoolPtr(true))
	bystander := scimCovTokenIn(st, "sub-bystander", []string{scimCovGroupExternal}, scimCovBoolPtr(false))
	e := newSCIMCovEnv(t, st)

	w := e.scim(http.MethodPatch, "/scim/v2/Groups/"+g.ID.String(), scimCovPatch(scimCovGroupRemove(p.ID.String())))
	if w.Code != http.StatusOK || len(scimCovDecode[scimCovGroupBody](t, w).Members) != 0 {
		t.Fatalf("PATCH = %d %s", w.Code, w.Body.String())
	}
	for name, c := range map[string]struct {
		id      uuid.UUID
		revoked bool
	}{
		"a token whose snapshot holds the group (case and space insensitive)": {holds, true},
		"a token whose snapshot does not":                                     {other, false},
		"a token whose snapshot was never recorded":                           {never, true},
		"a token whose snapshot was truncated":                                {truncated, true},
		"a bystander's token":                                                 {bystander, false},
	} {
		if got := st.tokenRevoked(c.id); got != c.revoked {
			t.Errorf("%s: revoked = %t, want %t", name, got, c.revoked)
		}
	}
	revoked, cut := e.rev.snapshot()
	if !slices.Equal(cut, []string{"sub-p", "sub-p@corp.example"}) || len(revoked) != 0 {
		t.Errorf("sessions cut %v revoked %v, want both forms cut and nothing revoked (the other credentials keep working)", cut, revoked)
	}
	rows := e.auditRows("scim.group.member_remove")
	if len(rows) != 1 || rows[0].Target != p.ID.String() || rows[0].Actor != scimActor || rows[0].Outcome != "success" {
		t.Fatalf("scim.group.member_remove rows = %+v", rows)
	}
	if d := e.auditData(rows[0]); d["slot"] != scimSlotPrimary || d["group"] != g.ID.String() || d["group_external_id"] != scimCovGroupExternal ||
		d["sessions_cut"] != float64(2) || d["tokens_revoked"] != float64(3) {
		t.Errorf("row data = %v", d)
	}
	for _, j := range st.ledger(p.ID, store.JobKindGroupRemove) {
		if !j.Done || j.Target != g.ID.String() {
			t.Errorf("ledger row %s/%s done %t", j.Step, j.Target, j.Done)
		}
	}
}

func TestSCIMCovRepeatedRemovalIsFreeAndAReaddedMemberIsRemovedAgain(t *testing.T) {
	st := newSCIMCovStore()
	p := scimCovPerson(st, "sub-p")
	g := st.addGroup(scimCovGroupExternal, "Engineering", p.ID)
	e := newSCIMCovEnv(t, st)
	path := "/scim/v2/Groups/" + g.ID.String()

	e.scim(http.MethodPatch, path, scimCovPatch(scimCovGroupRemove(p.ID.String())))
	_, cut := e.rev.snapshot()
	e.scim(http.MethodPatch, path, scimCovPatch(scimCovGroupRemove(p.ID.String())))
	if _, again := e.rev.snapshot(); len(again) != len(cut) || len(e.auditRows("scim.group.member_remove")) != 1 {
		t.Errorf("a repeated removal cut %d more sessions and wrote %d rows in all", len(again)-len(cut), len(e.auditRows("scim.group.member_remove")))
	}

	e.scim(http.MethodPatch, path, scimCovPatch(scimCovGroupAdd(p.ID.String())))
	e.scim(http.MethodPatch, path, scimCovPatch(scimCovGroupRemove(p.ID.String())))
	if _, again := e.rev.snapshot(); len(again) != 2*len(cut) || len(e.auditRows("scim.group.member_remove")) != 2 {
		t.Errorf("after a re-add and a removal: %d sessions cut, %d rows, want the removal done again", len(again), len(e.auditRows("scim.group.member_remove")))
	}
}

func TestSCIMCovAFailedRemovalStepStaysPendingAndIsRetried(t *testing.T) {
	for _, c := range []struct {
		name    string
		arm     func(*scimCovEnv)
		disarm  func(*scimCovEnv)
		pending string
		lastErr string
		revoked bool
	}{
		{"cutting sessions", func(e *scimCovEnv) { e.rev.set(nil, errors.New("cut unavailable")) }, func(e *scimCovEnv) { e.rev.set(nil, nil) },
			store.GroupStepSessions, "cut unavailable", true},
		{"revoking tokens", func(e *scimCovEnv) { e.st.failNext("RevokeAPIToken", errors.New("revoke unavailable"), -1) }, func(e *scimCovEnv) { e.st.failNext("RevokeAPIToken", nil, 0) },
			store.GroupStepTokens, "revoke unavailable", false},
		{"writing the audit row", func(e *scimCovEnv) { e.audit.fail("scim.group.member_remove", true) }, func(e *scimCovEnv) { e.audit.fail("scim.group.member_remove", false) },
			store.GroupStepAudit, "audit sink unavailable", true},
	} {
		t.Run(c.name, func(t *testing.T) {
			st := newSCIMCovStore()
			p := scimCovPerson(st, "sub-p")
			g := st.addGroup(scimCovGroupExternal, "Engineering", p.ID)
			tok := scimCovTokenIn(st, "sub-p", nil, nil)
			e := newSCIMCovEnv(t, st)
			c.arm(e)
			path := "/scim/v2/Groups/" + g.ID.String()

			w := e.scim(http.MethodPatch, path, scimCovPatch(scimCovGroupRemove(p.ID.String())))
			scimCovWantRetry(t, w, c.lastErr)
			if !strings.Contains(w.Body.String(), "deprovisioning is incomplete") {
				t.Errorf("the 500 does not tell the provider to retry: %s", w.Body.String())
			}
			var pending []store.DeprovisionJob
			for _, j := range st.ledger(p.ID, store.JobKindGroupRemove) {
				if !j.Done {
					pending = append(pending, j)
				}
			}
			if len(pending) == 0 || pending[0].Step != c.pending || !strings.Contains(pending[0].LastError, c.lastErr) {
				t.Errorf("pending rows = %+v, want %s with %q", pending, c.pending, c.lastErr)
			}
			if got := len(e.auditRows("scim.group.member_remove")); got != 0 {
				t.Errorf("the audit row was written with a step pending: %d", got)
			}
			if got := st.tokenRevoked(tok); got != c.revoked {
				t.Errorf("token revoked = %t, want %t: a failed step must not stop the others, and a failed revoke leaves it live", got, c.revoked)
			}

			c.disarm(e)
			if w := e.scim(http.MethodPatch, path, scimCovPatch(scimCovGroupRemove(p.ID.String()))); w.Code != http.StatusOK {
				t.Fatalf("retry = %d %s", w.Code, w.Body.String())
			}
			if left := st.pendingSteps(p.ID, store.JobKindGroupRemove); len(left) != 0 || !st.tokenRevoked(tok) {
				t.Errorf("after the retry pending %v, token revoked %t", left, st.tokenRevoked(tok))
			}
			if got := len(e.auditRows("scim.group.member_remove")); got != 1 {
				t.Errorf("scim.group.member_remove rows = %d, want one", got)
			}
		})
	}
}

func TestSCIMCovARemovalStoreFailureStopsLaterStepsAndIsRetried(t *testing.T) {
	for _, c := range []struct {
		method      string
		skip        int
		wantCut     bool
		wantRevoked bool
	}{
		{"GetIdentity", 0, false, false},
		{"StartGroupRemoval", 0, false, false},
		{"IdentityAliasValues", 0, false, false},
		{"ListDeprovisionJobs:group_remove", 0, false, false},
		{"FinishDeprovisionJob:group_remove", 0, true, true},
		{"ListDeprovisionJobs:group_remove", 1, true, true},
	} {
		t.Run(fmt.Sprintf("%s after %d", c.method, c.skip), func(t *testing.T) {
			st := newSCIMCovStore()
			p := scimCovPerson(st, "sub-p")
			g := st.addGroup(scimCovGroupExternal, "Engineering", p.ID)
			tok := scimCovTokenIn(st, "sub-p", nil, nil)
			e := newSCIMCovEnv(t, st)
			st.failAfter(c.method, errSCIMCovBoom, c.skip)
			path := "/scim/v2/Groups/" + g.ID.String()
			body := scimCovPatch(scimCovGroupRemove(p.ID.String()))

			scimCovWantRetry(t, e.scim(http.MethodPatch, path, body), "secret-dsn")
			if _, cut := e.rev.snapshot(); (len(cut) > 0) != c.wantCut {
				t.Errorf("sessions cut = %v, want cut %t", cut, c.wantCut)
			}
			if got := st.tokenRevoked(tok); got != c.wantRevoked {
				t.Errorf("token revoked = %t, want %t", got, c.wantRevoked)
			}
			if got := len(e.auditRows("scim.group.member_remove")); got != 0 {
				t.Errorf("%d audit rows written after a failed %s", got, c.method)
			}

			if w := e.scim(http.MethodPatch, path, body); w.Code != http.StatusOK {
				t.Fatalf("retry = %d %s", w.Code, w.Body.String())
			}
			if got := len(e.auditRows("scim.group.member_remove")); got != 1 || !st.tokenRevoked(tok) || len(st.pendingSteps(p.ID, store.JobKindGroupRemove)) != 0 {
				t.Errorf("after the retry: %d audit rows, token revoked %t, pending %v", got, st.tokenRevoked(tok), st.pendingSteps(p.ID, store.JobKindGroupRemove))
			}
		})
	}
}

func TestSCIMCovRemovingSomeoneWardynDoesNotKnowIsAnEmptyRemoval(t *testing.T) {
	st := newSCIMCovStore()
	g := st.addGroup(scimCovGroupExternal, "Engineering")
	e := newSCIMCovEnv(t, st)
	ghost := uuid.New()

	w := e.scim(http.MethodPatch, "/scim/v2/Groups/"+g.ID.String(), scimCovPatch(scimCovGroupRemove(ghost.String(), "u1091")))
	if w.Code != http.StatusOK || st.callCount("StartGroupRemoval") != 0 || len(e.h.audit.snapshot()) != 0 {
		t.Errorf("PATCH = %d, ledger opens %d, audit %d, want a plain 200 that opened nothing", w.Code, st.callCount("StartGroupRemoval"), len(e.h.audit.snapshot()))
	}
}

func TestSCIMCovPatchRemoveAllRemovesEveryoneAndStillRefusesTheExternalID(t *testing.T) {
	st := newSCIMCovStore()
	m1, m2 := scimCovPerson(st, "sub-1"), scimCovPerson(st, "sub-2")
	g := st.addGroup(scimCovGroupExternal, "Engineering", m1.ID, m2.ID)
	e := newSCIMCovEnv(t, st)

	w := e.scim(http.MethodPatch, "/scim/v2/Groups/"+g.ID.String(), scimCovPatch(`{"op":"Remove","path":"members"}`, scimCovReplace("externalId", `"moved"`)))
	scimCovWantError(t, w, http.StatusBadRequest, scim.TypeInvalidValue)
	if len(st.groupMembers(g.ID)) != 0 || len(e.auditRows("scim.group.member_remove")) != 2 {
		t.Errorf("members %v rows %d, want both removed although the externalId change was refused", st.groupMembers(g.ID), len(e.auditRows("scim.group.member_remove")))
	}
}

func TestSCIMCovDeleteGroupRemovesEveryoneThenTheGroup(t *testing.T) {
	st := newSCIMCovStore()
	m1, m2, pending := scimCovPerson(st, "sub-1"), scimCovPerson(st, "sub-2"), scimCovPerson(st, "sub-3")
	g := st.addGroup(scimCovGroupExternal, "Engineering", m1.ID, m2.ID)
	// A removal begun earlier and never finished is still owed, though the person is no longer a member.
	for _, step := range store.GroupRemovalSteps {
		st.jobs = append(st.jobs, &store.DeprovisionJob{IdentityID: pending.ID, Kind: store.JobKindGroupRemove, Step: step, Target: g.ID.String()})
	}
	e := newSCIMCovEnv(t, st)

	w := e.scim(http.MethodDelete, "/scim/v2/Groups/"+g.ID.String(), "")
	if w.Code != http.StatusNoContent || w.Body.Len() != 0 {
		t.Fatalf("DELETE = %d %q, want 204 with no body", w.Code, w.Body.String())
	}
	if _, ok := st.groups[g.ID]; ok {
		t.Error("the group is still stored")
	}
	targets := map[string]bool{}
	for _, ev := range e.auditRows("scim.group.member_remove") {
		targets[ev.Target] = true
	}
	if len(targets) != 3 || !targets[m1.ID.String()] || !targets[m2.ID.String()] || !targets[pending.ID.String()] {
		t.Errorf("removal rows for %v, want the two members and the person whose removal was owed", targets)
	}
}

func TestSCIMCovDeleteGroupFailures(t *testing.T) {
	t.Run("an unknown group", func(t *testing.T) {
		e := newSCIMCovEnv(t, newSCIMCovStore())
		scimCovWantError(t, e.scim(http.MethodDelete, "/scim/v2/Groups/"+uuid.NewString(), ""), http.StatusNotFound, "")
	})
	t.Run("the group already gone is a success", func(t *testing.T) {
		st := newSCIMCovStore()
		g := st.addGroup(scimCovGroupExternal, "Engineering")
		st.failNext("DeleteScimGroup", store.ErrNotFound, 1)
		e := newSCIMCovEnv(t, st)
		if w := e.scim(http.MethodDelete, "/scim/v2/Groups/"+g.ID.String(), ""); w.Code != http.StatusNoContent {
			t.Errorf("DELETE = %d %s, want 204", w.Code, w.Body.String())
		}
	})
	t.Run("the group delete failing", func(t *testing.T) {
		st := newSCIMCovStore()
		g := st.addGroup(scimCovGroupExternal, "Engineering")
		st.failNext("DeleteScimGroup", errSCIMCovBoom, 1)
		e := newSCIMCovEnv(t, st)
		scimCovWantRetry(t, e.scim(http.MethodDelete, "/scim/v2/Groups/"+g.ID.String(), ""), "secret-dsn")
	})
	t.Run("listing who to remove failing", func(t *testing.T) {
		st := newSCIMCovStore()
		g := st.addGroup(scimCovGroupExternal, "Engineering", scimCovPerson(st, "sub-1").ID)
		st.failNext("GroupRemovalIdentities", errSCIMCovBoom, 1)
		e := newSCIMCovEnv(t, st)
		scimCovWantRetry(t, e.scim(http.MethodDelete, "/scim/v2/Groups/"+g.ID.String(), ""), "secret-dsn")
		if st.callCount("DeleteScimGroup") != 0 || st.callCount("StartGroupRemoval") != 0 {
			t.Errorf("calls after the failed listing: %v", st.calls)
		}
	})
	t.Run("a failed removal keeps the group and does not stop the others", func(t *testing.T) {
		st := newSCIMCovStore()
		m1, m2 := scimCovPerson(st, "sub-1"), scimCovPerson(st, "sub-2")
		g := st.addGroup(scimCovGroupExternal, "Engineering", m1.ID, m2.ID)
		e := newSCIMCovEnv(t, st)
		e.rev.set(nil, errors.New("cut unavailable"))
		scimCovWantRetry(t, e.scim(http.MethodDelete, "/scim/v2/Groups/"+g.ID.String(), ""), "cut unavailable")
		if st.callCount("DeleteScimGroup") != 0 {
			t.Error("the group was deleted while a removal was pending")
		}
		if st.callCount("StartGroupRemoval") != 2 {
			t.Errorf("StartGroupRemoval ran %d times, want both members attempted", st.callCount("StartGroupRemoval"))
		}
	})
}

func TestSCIMCovGroupSnapshotMatch(t *testing.T) {
	answerable := func(groups []string) types.APIToken {
		f := false
		return types.APIToken{Groups: groups, GroupsTruncated: &f}
	}
	for _, c := range []struct {
		name  string
		match string
		tok   types.APIToken
		want  bool
	}{
		{"a snapshot holding the id", "Eng-Group", answerable([]string{"eng-group"}), true},
		{"a snapshot holding it with padding", " eng-group ", answerable([]string{"  ENG-GROUP"}), true},
		{"a snapshot holding the display name only", "eng-group", answerable([]string{"Engineering"}), false},
		{"an empty answerable snapshot", "eng-group", answerable([]string{}), false},
		{"a snapshot never recorded", "eng-group", types.APIToken{}, true},
	} {
		if got := groupSnapshotMatch(c.match)(c.tok); got != c.want {
			t.Errorf("%s: match = %t, want %t", c.name, got, c.want)
		}
	}
	if err := (&Server{}).removeGroupMembers(context.Background(), nil, store.ScimGroup{}, nil, "x"); err != nil {
		t.Errorf("removing no one = %v, want nil", err)
	}
}
