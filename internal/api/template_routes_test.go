// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/pkg/client"
)

const templateIntentEgress = `{"inline_policy":{"allowed_domains":["pypi.org"]}}`

// templateDocBody is a valid partial document holding intent.
func templateDocBody(intent string) string {
	return `{"api_version":"wardyn/v1","kind":"RunTemplate","coverage":"partial","intent":` + intent + `}`
}

func templateSaveBody(scope, group, name string) string {
	b, _ := json.Marshal(map[string]any{"scope": scope, "group_id": group, "name": name, "document": json.RawMessage(templateDocBody(templateIntentEgress))})
	return string(b)
}

// templateCallers are the six callers of the C-templates storage matrix: the
// owner of the personal template, another member, an organisation
// administrator, a member of group eng, eng's template manager, and the
// template manager of another group.
type templateCallerCase struct {
	name     string
	sub      string
	role     string
	groups   []string
	grant    string
	inEng    bool
	engAdmin bool
	orgAdmin bool
}

var templateCallerCases = []templateCallerCase{
	{name: "owner", sub: "alice", role: oidc.RoleUser, groups: []string{}},
	{name: "other member", sub: "bob", role: oidc.RoleUser, groups: []string{}},
	{name: "org admin", sub: "root", role: oidc.RoleAdmin, groups: []string{}, orgAdmin: true},
	{name: "group member", sub: "gina", role: oidc.RoleUser, groups: []string{"eng"}, inEng: true},
	{name: "group admin", sub: "gary", role: oidc.RoleUser, groups: []string{"eng"}, grant: "eng", inEng: true, engAdmin: true},
	{name: "other group's admin", sub: "olga", role: oidc.RoleUser, groups: []string{"ops"}, grant: "ops"},
}

// tierAllows reads one TemplateRoutes tier for a caller: the table, not this
// test, says who may act.
func tierAllows(tier types.TemplateTier, c templateCallerCase) bool {
	switch tier {
	case types.TemplateTierOwner:
		return c.sub == "alice"
	case types.TemplateTierMember:
		return true
	case types.TemplateTierOrgAdmin:
		return c.orgAdmin
	case types.TemplateTierGroupMember:
		return c.inEng
	case types.TemplateTierGroupAdmin:
		return c.engAdmin
	}
	return false
}

func templateRouteRow(t *testing.T, route string) types.TemplateRoute {
	t.Helper()
	for _, r := range types.TemplateRoutes {
		if r.Route == route {
			return r
		}
	}
	t.Fatalf("%s is not in TemplateRoutes", route)
	return types.TemplateRoute{}
}

func (r templateCallerCase) tier(row types.TemplateRoute, scope types.TemplateScope) types.TemplateTier {
	return map[types.TemplateScope]types.TemplateTier{types.TemplateScopePerson: row.Person, types.TemplateScopeOrg: row.Org, types.TemplateScopeGroup: row.Group}[scope]
}

var templateOwners = map[types.TemplateScope]types.TemplateOwner{
	types.TemplateScopePerson: {Scope: types.TemplateScopePerson, Person: "alice"},
	types.TemplateScopeOrg:    {Scope: types.TemplateScopeOrg},
	types.TemplateScopeGroup:  {Scope: types.TemplateScopeGroup, Group: "eng"},
}

// TestTemplateRouteScopeCallerMatrix walks every {id} route x scope x caller
// through the real router with SSO sessions, and holds each answer to what
// types.TemplateRoutes says that caller may do: allowed acts succeed, a write
// a reader may not make is 403, and anything on a template the caller cannot
// read is the 404 a missing one gets.
func TestTemplateRouteScopeCallerMatrix(t *testing.T) {
	routes := []struct {
		route, method, suffix string
		ok                    int
		body                  func(types.TemplateOwner) string
	}{
		{"GET /api/v1/templates/{id}", http.MethodGet, "", http.StatusOK, nil},
		{"GET /api/v1/templates/{id}/revisions/{revision}", http.MethodGet, "/revisions/1", http.StatusOK, nil},
		{"PUT /api/v1/templates/{id}", http.MethodPut, "", http.StatusOK, func(types.TemplateOwner) string {
			return `{"name":"renamed","expected_revision":1,"document":` + templateDocBody(templateIntentEgress) + `}`
		}},
		{"DELETE /api/v1/templates/{id}", http.MethodDelete, "", http.StatusNoContent, nil},
		{"POST /api/v1/templates/{id}/copy", http.MethodPost, "/copy", http.StatusCreated, func(o types.TemplateOwner) string {
			return `{"scope":"` + string(o.Scope) + `","group_id":"` + o.Group + `","name":"a copy"}`
		}},
	}
	for _, rt := range routes {
		row := templateRouteRow(t, rt.route)
		for scope, owner := range templateOwners {
			for _, c := range templateCallerCases {
				t.Run(rt.route+"/"+string(scope)+"/"+c.name, func(t *testing.T) {
					e := newTemplateEnv(t)
					for _, x := range templateCallerCases {
						if x.grant != "" {
							e.grant(x.grant, x.sub)
						}
					}
					tpl := e.seed(owner, types.TemplateKindTemplate, "seeded", templateIntentEgress)
					body := ""
					if rt.body != nil {
						body = rt.body(owner)
					}
					status, resp := e.call(templateCookie(t, c.sub, c.role, false, c.groups, false), rt.method, "/api/v1/templates/"+tpl.ID.String()+rt.suffix, body)
					readRow := templateRouteRow(t, "GET /api/v1/templates/{id}")
					canRead := tierAllows(c.tier(readRow, scope), c)
					want := http.StatusNotFound
					switch {
					case canRead && tierAllows(c.tier(row, scope), c):
						want = rt.ok
					case canRead:
						want = http.StatusForbidden
					}
					if status != want {
						t.Fatalf("status = %d, want %d; body=%s", status, want, resp)
					}
					if status >= 400 && strings.Contains(resp, "eng") {
						t.Errorf("a refusal names the group: %s", resp)
					}
				})
			}
		}
	}
}

// TestTemplateCreateAndListFollowTheTable: POST /templates per target scope and
// caller, and GET /templates per caller, against TemplateRoutes.
func TestTemplateCreateAndListFollowTheTable(t *testing.T) {
	create := templateRouteRow(t, "POST /api/v1/templates")
	list := templateRouteRow(t, "GET /api/v1/templates")
	for _, c := range templateCallerCases {
		t.Run(c.name, func(t *testing.T) {
			e := newTemplateEnv(t)
			for _, x := range templateCallerCases {
				if x.grant != "" {
					e.grant(x.grant, x.sub)
				}
			}
			ids := map[types.TemplateScope]uuid.UUID{}
			for scope, owner := range templateOwners {
				ids[scope] = e.seed(owner, types.TemplateKindTemplate, "seeded "+string(scope), templateIntentEgress).ID
			}
			cookie := templateCookie(t, c.sub, c.role, false, c.groups, false)
			for scope, owner := range templateOwners {
				status, body := e.call(cookie, http.MethodPost, "/api/v1/templates", templateSaveBody(string(scope), owner.Group, "new "+string(scope)))
				readable := tierAllows(c.tier(list, scope), c) || scope == types.TemplateScopePerson
				want := map[bool]int{true: http.StatusCreated, false: http.StatusForbidden}[scope == types.TemplateScopePerson || tierAllows(c.tier(create, scope), c)]
				if !readable && want != http.StatusCreated {
					want = http.StatusNotFound
				}
				if status != want {
					t.Errorf("create %s: status = %d, want %d; body=%s", scope, status, want, body)
				}
			}
			status, body := e.call(cookie, http.MethodGet, "/api/v1/templates", "")
			if status != http.StatusOK {
				t.Fatalf("list = %d %s", status, body)
			}
			var got client.TemplateList
			_ = json.Unmarshal([]byte(body), &got)
			for scope, id := range ids {
				listed := slices.ContainsFunc(got.Templates, func(s client.TemplateSummary) bool { return s.ID == id })
				if listed != tierAllows(c.tier(list, scope), c) {
					t.Errorf("%s template listed = %v, the table says %v", scope, listed, !listed)
				}
			}
		})
	}
}

// TestTemplateForeignIDIsTheMissingID404: on every {id} route, a template the
// caller cannot read answers byte for byte what a missing id answers.
func TestTemplateForeignIDIsTheMissingID404(t *testing.T) {
	e := newTemplateEnv(t)
	routes := []struct{ method, suffix, body string }{
		{http.MethodGet, "", ""},
		{http.MethodGet, "/revisions/1", ""},
		{http.MethodPut, "", `{"name":"x","expected_revision":1,"document":` + templateDocBody(templateIntentEgress) + `}`},
		{http.MethodDelete, "", ""},
		{http.MethodPost, "/copy", `{"scope":"person"}`},
	}
	foreign := map[string]types.RunTemplate{
		"another person's":      e.seed(types.TemplateOwner{Scope: types.TemplateScopePerson, Person: "alice"}, types.TemplateKindTemplate, "mine", templateIntentEgress),
		"a group's, non-member": e.seed(types.TemplateOwner{Scope: types.TemplateScopeGroup, Group: "eng"}, types.TemplateKindTemplate, "eng's", templateIntentEgress),
	}
	callers := map[string]*http.Cookie{
		"member":           templateCookie(t, "bob", oidc.RoleUser, false, []string{"ops"}, false),
		"org admin":        templateCookie(t, "root", oidc.RoleAdmin, false, []string{}, false),
		"truncated member": templateCookie(t, "gina", oidc.RoleUser, false, []string{"eng"}, true),
	}
	for _, rt := range routes {
		for cname, cookie := range callers {
			_, missing := e.call(cookie, rt.method, "/api/v1/templates/"+uuid.NewString()+rt.suffix, rt.body)
			for fname, tpl := range foreign {
				if cname == "truncated member" && tpl.Owner.Scope == types.TemplateScopeGroup && (rt.method == http.MethodPut || rt.method == http.MethodDelete) {
					continue // a write the snapshot cannot answer is 403 (TestTemplateUnverifiedSnapshotWritesAre403)
				}
				status, body := e.call(cookie, rt.method, "/api/v1/templates/"+tpl.ID.String()+rt.suffix, rt.body)
				if status != http.StatusNotFound || body != missing {
					t.Errorf("%s %s as %s on %s: %d %q, want the missing id's 404 %q", rt.method, rt.suffix, cname, fname, status, body, missing)
				}
			}
		}
	}
}

// TestTemplateEffectiveRoleClamp: an administrator looking through the User
// view is a member here. They are not offered the organisation and may not
// write it; the same person in the Admin view may.
func TestTemplateEffectiveRoleClamp(t *testing.T) {
	e := newTemplateEnv(t)
	for _, tc := range []struct {
		name       string
		memberMode bool
		org        bool
		status     int
	}{
		{"user view", true, false, http.StatusForbidden},
		{"admin view", false, true, http.StatusCreated},
	} {
		cookie := templateCookie(t, "root", oidc.RoleAdmin, tc.memberMode, []string{}, false)
		status, body := e.call(cookie, http.MethodPost, "/api/v1/templates", templateSaveBody("org", "", "org "+tc.name))
		if status != tc.status {
			t.Errorf("%s: org write = %d, want %d; %s", tc.name, status, tc.status, body)
		}
		if tc.status == http.StatusForbidden && !strings.Contains(body, reasonTemplateScopeForbidden) {
			t.Errorf("%s: refusal %s, want %s", tc.name, body, reasonTemplateScopeForbidden)
		}
		_, body = e.call(cookie, http.MethodGet, "/api/v1/templates", "")
		var got client.TemplateList
		_ = json.Unmarshal([]byte(body), &got)
		if got.Publish.Org != tc.org {
			t.Errorf("%s: publish.org = %v, want %v", tc.name, got.Publish.Org, tc.org)
		}
	}
}

// TestTemplatePublishOffersOnlyWritableAudiences (DR-0066): a group is offered
// only to a verified member who also holds its grant; a missing or truncated
// snapshot offers no group and says so.
func TestTemplatePublishOffersOnlyWritableAudiences(t *testing.T) {
	e := newTemplateEnv(t)
	e.grant("eng", "gary")
	e.grant("qa", "gary") // a grant on a group gary is not in
	for _, tc := range []struct {
		name       string
		groups     []string
		truncated  bool
		want       []string
		unverified bool
	}{
		{"verified", []string{"eng", "ops"}, false, []string{"eng"}, false},
		{"truncated", []string{"eng"}, true, []string{}, true},
		{"no snapshot", nil, false, []string{}, true},
	} {
		_, body := e.call(templateCookie(t, "gary", oidc.RoleUser, false, tc.groups, tc.truncated), http.MethodGet, "/api/v1/templates", "")
		var got client.TemplateList
		if err := json.Unmarshal([]byte(body), &got); err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(got.Publish.Groups, tc.want) || got.Publish.GroupsUnverified != tc.unverified || got.Publish.Org {
			t.Errorf("%s: publish = %+v, want groups %v unverified %v", tc.name, got.Publish, tc.want, tc.unverified)
		}
	}
}

// TestNoTemplateRouteAnswersUnavailable: with a template store, every template
// route answers on its own merits, never 501.
func TestNoTemplateRouteAnswersUnavailable(t *testing.T) {
	e := newTemplateEnv(t)
	tpl := e.seed(types.TemplateOwner{Scope: types.TemplateScopeOrg}, types.TemplateKindTemplate, "org", templateIntentEgress)
	admin := templateCookie(t, "root", oidc.RoleAdmin, false, []string{}, false)
	for _, row := range types.TemplateRoutes {
		method, path, _ := strings.Cut(row.Route, " ")
		path = strings.ReplaceAll(strings.ReplaceAll(path, "{id}", tpl.ID.String()), "{revision}", "1")
		if status, body := e.call(admin, method, path, `{}`); status == http.StatusNotImplemented {
			t.Errorf("%s answers 501: %s", row.Route, body)
		}
	}
}
