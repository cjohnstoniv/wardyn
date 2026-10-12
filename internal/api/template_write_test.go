// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/erasure"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/pkg/client"
)

func decodeTemplate(t *testing.T, body string) client.Template {
	t.Helper()
	var out client.Template
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatalf("decode %s: %v", body, err)
	}
	return out
}

func saveBodyWith(scope, group, name, kind, intent string) string {
	b, _ := json.Marshal(map[string]any{"scope": scope, "group_id": group, "name": name, "kind": kind, "document": json.RawMessage(templateDocBody(intent))})
	return string(b)
}

// TestTemplatePolicyKeysArePinned: the keys a policy may hold are derived from
// the registry (run-wide network, credential and access-rule parts) and are
// exactly these.
func TestTemplatePolicyKeysArePinned(t *testing.T) {
	want := []string{"allow_all_egress", "allowed_domains", "allowed_methods", "denied_domains", "eligible_grants",
		"first_use_approval", "first_use_hold_seconds", "llm_inspection", "max_holds"}
	got := templatePolicyKeys()
	slices.Sort(got)
	if !slices.Equal(got, want) {
		t.Fatalf("policy keys = %v, want %v", got, want)
	}
}

// TestTemplatePolicyKindRefusesEveryOtherKey: a policy holds only those keys.
// Every other request field and every other policy field is refused with
// template_policy_field_refused at its path.
func TestTemplatePolicyKindRefusesEveryOtherKey(t *testing.T) {
	e := newTemplateEnv(t)
	alice := templateCookie(t, "alice", oidc.RoleUser, false, []string{}, false)
	if status, body := e.call(alice, http.MethodPost, "/api/v1/templates", saveBodyWith("person", "", "egress", "policy", templateIntentEgress)); status != http.StatusCreated {
		t.Fatalf("a policy of allowed keys = %d %s", status, body)
	}
	allowed := templatePolicyKeys()
	for _, r := range templatePolicyRules {
		if slices.Contains(allowed, r.Name) {
			continue
		}
		doc := client.TemplateDocument{APIVersion: "wardyn/v1", Kind: "RunTemplate", Coverage: client.TemplateCoveragePartial}
		if err := doc.Intent.Set("inline_policy", map[string]any{"allowed_domains": []string{"pypi.org"}, r.Name: true}); err != nil {
			t.Fatal(err)
		}
		diags := templatePolicyKindRefusals(doc)
		if len(diags) != 1 || diags[0].Path != "intent.inline_policy."+r.Name || diags[0].Reason != reasonTemplatePolicyFieldRefused {
			t.Errorf("inline_policy.%s in a policy: %+v", r.Name, diags)
		}
	}
	status, body := e.call(alice, http.MethodPost, "/api/v1/templates", saveBodyWith("person", "", "with agent", "policy", `{"agent":"claude-code","inline_policy":{"allowed_domains":["pypi.org"]}}`))
	if status != http.StatusUnprocessableEntity || !strings.Contains(body, `"path":"intent.agent","reason":"template_policy_field_refused"`) {
		t.Errorf("intent.agent in a policy: %d %s", status, body)
	}
}

// TestTemplateRevisionConflictChangesNothing (DR-0055): an update against a
// stale revision is a 409 and leaves the template as it was; one without
// expected_revision is refused.
func TestTemplateRevisionConflictChangesNothing(t *testing.T) {
	e := newTemplateEnv(t)
	alice := templateCookie(t, "alice", oidc.RoleUser, false, []string{}, false)
	tpl := e.seed(types.TemplateOwner{Scope: types.TemplateScopePerson, Person: "alice"}, types.TemplateKindTemplate, "mine", templateIntentEgress)
	path := "/api/v1/templates/" + tpl.ID.String()
	put := func(name string, rev int) (int, string) {
		b, _ := json.Marshal(map[string]any{"name": name, "expected_revision": rev, "document": json.RawMessage(templateDocBody(templateIntentEgress))})
		return e.call(alice, http.MethodPut, path, string(b))
	}
	if status, body := put("second", 1); status != http.StatusOK || decodeTemplate(t, body).Revision != 2 {
		t.Fatalf("update = %d %s", status, body)
	}
	status, body := put("stale edit", 1)
	if status != http.StatusConflict || !strings.Contains(body, reasonTemplateRevisionConflict) || !strings.Contains(body, "revision 2") {
		t.Fatalf("stale update = %d %s", status, body)
	}
	if cur, _ := e.ts.GetTemplate(context.Background(), tpl.ID); cur.Name != "second" || cur.Revision != 2 {
		t.Fatalf("after the conflict the row is %q rev %d, want second rev 2", cur.Name, cur.Revision)
	}
	if status, body := e.call(alice, http.MethodPut, path, `{"name":"x","document":`+templateDocBody(templateIntentEgress)+`}`); status != http.StatusBadRequest || !strings.Contains(body, `"path":"expected_revision"`) {
		t.Errorf("update without expected_revision = %d %s", status, body)
	}
	// The old revision stays readable and unchanged.
	if status, body := e.call(alice, http.MethodGet, path+"/revisions/1", ""); status != http.StatusOK || decodeTemplate(t, body).Name != "mine" {
		t.Errorf("revision 1 = %d %s", status, body)
	}
}

// TestTemplateNameTakenAndLimitReached: a name is unique per audience,
// case-insensitively, and an audience holds at most its limit.
func TestTemplateNameTakenAndLimitReached(t *testing.T) {
	e := newTemplateEnv(t)
	e.grant("eng", "gary")
	gary := templateCookie(t, "gary", oidc.RoleUser, false, []string{"eng"}, false)
	if status, body := e.call(gary, http.MethodPost, "/api/v1/templates", templateSaveBody("person", "", "Deploy")); status != http.StatusCreated {
		t.Fatalf("first = %d %s", status, body)
	}
	if status, body := e.call(gary, http.MethodPost, "/api/v1/templates", templateSaveBody("person", "", "deploy")); status != http.StatusConflict || !strings.Contains(body, reasonTemplateNameTaken) {
		t.Errorf("same name, other case = %d %s", status, body)
	}
	if status, body := e.call(gary, http.MethodPost, "/api/v1/templates", templateSaveBody("group", "eng", "deploy")); status != http.StatusCreated {
		t.Errorf("same name, another audience = %d %s", status, body)
	}
	for i := 1; i < store.TemplatesMaxPerPerson; i++ {
		e.seed(types.TemplateOwner{Scope: types.TemplateScopePerson, Person: "gary"}, types.TemplateKindTemplate, "filler "+uuid.NewString(), templateIntentEgress)
	}
	status, body := e.call(gary, http.MethodPost, "/api/v1/templates", templateSaveBody("person", "", "one too many"))
	if status != http.StatusConflict || !strings.Contains(body, reasonTemplateLimitReached) || !strings.Contains(body, "holds 200 templates") {
		t.Errorf("over the limit = %d %s", status, body)
	}
}

// TestTemplateAuthorshipOnlyForWriters: the people a template is offered to are
// not told who published it.
func TestTemplateAuthorshipOnlyForWriters(t *testing.T) {
	e := newTemplateEnv(t)
	tpl := e.seed(types.TemplateOwner{Scope: types.TemplateScopeOrg}, types.TemplateKindTemplate, "org", templateIntentEgress)
	path := "/api/v1/templates/" + tpl.ID.String()
	_, body := e.call(templateCookie(t, "bob", oidc.RoleUser, false, []string{}, false), http.MethodGet, path, "")
	if got := decodeTemplate(t, body); got.CreatedBy != "" || got.UpdatedBy != "" || strings.Contains(body, tpl.CreatedBy) {
		t.Errorf("a reader sees the publisher: %s", body)
	}
	_, body = e.call(templateCookie(t, "root", oidc.RoleAdmin, false, []string{}, false), http.MethodGet, path, "")
	if got := decodeTemplate(t, body); got.CreatedBy != tpl.CreatedBy {
		t.Errorf("a writer reads created_by %q, want %q", got.CreatedBy, tpl.CreatedBy)
	}
}

// TestTemplateCopyRerunsTheScopeDoor: a copy passes the door again for its
// target, so a field only one person has (runner_id) never reaches the
// organisation, whatever the source holds.
func TestTemplateCopyRerunsTheScopeDoor(t *testing.T) {
	e := newTemplateEnv(t)
	root := templateCookie(t, "root", oidc.RoleAdmin, false, []string{}, false)
	mine := e.seed(types.TemplateOwner{Scope: types.TemplateScopePerson, Person: "root"}, types.TemplateKindTemplate, "on my runner",
		`{"runner_id":"`+uuid.NewString()+`"}`)
	status, body := e.call(root, http.MethodPost, "/api/v1/templates/"+mine.ID.String()+"/copy", `{"scope":"org"}`)
	if status < 400 || !strings.Contains(body, `"path":"intent.runner_id","reason":"template_shared_field_refused"`) {
		t.Fatalf("copy to org = %d %s", status, body)
	}
	clean := e.seed(types.TemplateOwner{Scope: types.TemplateScopePerson, Person: "root"}, types.TemplateKindTemplate, "egress", templateIntentEgress)
	if status, body = e.call(root, http.MethodPost, "/api/v1/templates/"+clean.ID.String()+"/copy", `{"scope":"org","name":"published"}`); status != http.StatusCreated {
		t.Errorf("copy of a clean template to org = %d %s", status, body)
	}
}

// TestGroupSharedTemplateHoldsReferencesNeverValues (DR-0079, A17): a group
// template keeps the names of what it uses (a secret's name) and never a
// value. A value-shaped string is refused before anything is stored, and no
// response, row or audit record carries it.
func TestGroupSharedTemplateHoldsReferencesNeverValues(t *testing.T) {
	const canary = "ghp_A1b2C3d4E5f6G7h8I9j0K1l2M3n4O5p6Q7r8"
	e := newTemplateEnv(t)
	e.grant("eng", "gary")
	gary := templateCookie(t, "gary", oidc.RoleUser, false, []string{"eng"}, false)
	gina := templateCookie(t, "gina", oidc.RoleUser, false, []string{"eng"}, false)
	refs := `{"inline_policy":{"allowed_domains":["api.example.com"],"llm_inspection":{"mode":"block","workspace_secret_names":["TEAM_TOKEN"]}}}`
	var bodies []string
	for _, tc := range []struct {
		name string
		body string
	}{
		{"a value under a secret's name", saveBodyWith("group", "eng", "values", "policy",
			`{"inline_policy":{"llm_inspection":{"mode":"block","workspace_secret_values":["`+canary+`"]}}}`)},
		{"a value in the description", func() string {
			b, _ := json.Marshal(map[string]any{"scope": "group", "group_id": "eng", "name": "desc",
				"document": json.RawMessage(`{"api_version":"wardyn/v1","kind":"RunTemplate","coverage":"partial","description":"use ` + canary + `","intent":` + refs + `}`)})
			return string(b)
		}()},
		{"a value as the name", saveBodyWith("group", "eng", "deploy "+canary, "template", refs)},
	} {
		status, body := e.call(gary, http.MethodPost, "/api/v1/templates", tc.body)
		if status < 400 || status >= 500 {
			t.Errorf("%s: %d %s, want a refusal", tc.name, status, body)
		}
		bodies = append(bodies, body)
	}
	status, body := e.call(gary, http.MethodPost, "/api/v1/templates", saveBodyWith("group", "eng", "team refs", "policy", refs))
	if status != http.StatusCreated {
		t.Fatalf("references = %d %s", status, body)
	}
	id := decodeTemplate(t, body).ID.String()
	_, read := e.call(gina, http.MethodGet, "/api/v1/templates/"+id, "")
	_, list := e.call(gina, http.MethodGet, "/api/v1/templates", "")
	if !strings.Contains(read, "TEAM_TOKEN") {
		t.Errorf("a member does not see the reference's name: %s", read)
	}
	bodies = append(bodies, body, read, list)
	for _, b := range bodies {
		if strings.Contains(b, canary) {
			t.Errorf("a response carries the value: %s", b)
		}
	}
	for _, ev := range e.audit.snapshot() {
		if strings.Contains(string(ev.Data), canary) {
			t.Errorf("audit %s carries the value", ev.Action)
		}
	}
	rows, _ := e.ts.ListTemplatesVisible(context.Background(), "gary", []string{"eng"})
	for _, r := range rows {
		if strings.Contains(string(r.Document)+r.Name+r.Description, canary) {
			t.Errorf("stored row %s carries the value", r.Name)
		}
	}
}

// TestTemplateGroupAdminRoutesStoreCanonicalGrants: the grant route stores the
// canonical group and person, so a grant written as "DevTeam" authorises the
// group the sign-in calls "devteam"; revoking it ends the person's writes.
func TestTemplateGroupAdminRoutesStoreCanonicalGrants(t *testing.T) {
	e := newTemplateEnv(t)
	root := templateCookie(t, "root", oidc.RoleAdmin, false, []string{}, false)
	bob := templateCookie(t, "bob", oidc.RoleUser, false, []string{"devteam"}, false)
	grant := `{"group_id":"DevTeam","person":" Bob ","label":"Developers"}`
	status, body := e.call(root, http.MethodPut, "/api/v1/admin/template-group-admins", grant)
	if status != http.StatusOK || !strings.Contains(body, `"group_id":"devteam","person":"bob"`) || !strings.Contains(body, `"label":"Developers"`) {
		t.Fatalf("grant = %d %s", status, body)
	}
	if status, body = e.call(bob, http.MethodPut, "/api/v1/admin/template-group-admins", grant); status != http.StatusForbidden {
		t.Errorf("a member grants = %d %s", status, body)
	}
	_, list := e.call(bob, http.MethodGet, "/api/v1/templates", "")
	if !strings.Contains(list, `"groups":["devteam"]`) || !strings.Contains(list, `"devteam":"Developers"`) {
		t.Errorf("bob's publish block = %s", list)
	}
	if status, body = e.call(bob, http.MethodPost, "/api/v1/templates", templateSaveBody("group", "DEVTEAM", "team")); status != http.StatusCreated {
		t.Fatalf("bob publishes = %d %s", status, body)
	}
	if status, body = e.call(root, http.MethodDelete, "/api/v1/admin/template-group-admins", grant); status != http.StatusNoContent {
		t.Fatalf("revoke = %d %s", status, body)
	}
	if status, body = e.call(bob, http.MethodPost, "/api/v1/templates", templateSaveBody("group", "devteam", "after")); status != http.StatusForbidden || !strings.Contains(body, "template managers") {
		t.Errorf("bob after the revoke = %d %s", status, body)
	}
	var actions []string
	for _, ev := range e.audit.snapshot() {
		actions = append(actions, ev.Action)
	}
	for _, a := range []string{"template_group_admin.grant", "template.create", "template_group_admin.revoke"} {
		if !slices.Contains(actions, a) {
			t.Errorf("no %s audit row in %v", a, actions)
		}
	}
}

// TestEraseRunSetupsScope: run_setups removes the person's own templates and
// grants and blanks their name on shared templates; the SCIM purge asks for it.
func TestEraseRunSetupsScope(t *testing.T) {
	e := newTemplateEnv(t)
	mine := e.seed(types.TemplateOwner{Scope: types.TemplateScopePerson, Person: "alice"}, types.TemplateKindTemplate, "mine", templateIntentEgress)
	shared, _ := e.ts.CreateTemplate(context.Background(), types.RunTemplate{ID: uuid.New(), Owner: types.TemplateOwner{Scope: types.TemplateScopeOrg},
		Kind: types.TemplateKindTemplate, Name: "shared", Document: json.RawMessage(templateDocBody(templateIntentEgress)), CreatedBy: "alice"}, nil)
	e.grant("eng", "alice")
	e.grant("eng", "bob")
	rep, err := e.srv.erasureOrchestrator(nil).Orchestrate(context.Background(), " Alice", []erasure.Scope{erasure.RunSetups})
	if err != nil {
		t.Fatal(err)
	}
	if got := rep.Details[erasure.RunSetups].(store.RunSetupsErased); got.Templates != 1 || got.Grants != 1 || got.AuthorshipCleared < 1 {
		t.Errorf("erased = %+v", got)
	}
	if _, err := e.ts.GetTemplate(context.Background(), mine.ID); err == nil {
		t.Error("the person's own template survived")
	}
	if cur, _ := e.ts.GetTemplate(context.Background(), shared.ID); cur.CreatedBy != "" || cur.UpdatedBy != "" {
		t.Errorf("the shared template still names the person: %+v", cur)
	}
	if groups, _ := e.ts.TemplateGroupAdminGroups(context.Background(), []string{"bob"}); !slices.Equal(groups, []string{"eng"}) {
		t.Errorf("another person's grant went: %v", groups)
	}
	if !slices.Contains(scimPurgeScopes, erasure.RunSetups) {
		t.Error("the SCIM purge does not erase run setups")
	}
}

// TestTemplateGrantPersonResolvesOrIsRefused: the grant route resolves the
// person as ?owner= does. An email no principal pairs with, and one two
// principals share, are refused; a known email is stored as its principal, so
// the revoke and erasure that name the person find the grant.
func TestTemplateGrantPersonResolvesOrIsRefused(t *testing.T) {
	e := newTemplateEnv(t)
	root := templateCookie(t, "root", oidc.RoleAdmin, false, []string{}, false)
	put := func(method, person string) (int, string) {
		return e.call(root, method, "/api/v1/admin/template-group-admins", `{"group_id":"eng","person":"`+person+`"}`)
	}
	if status, body := put(http.MethodPut, "nobody@corp.example"); status != http.StatusUnprocessableEntity || !strings.Contains(body, reasonOwnerUnresolved) {
		t.Errorf("an unknown email = %d %s, want 422 %s", status, body, reasonOwnerUnresolved)
	}
	e.signIn("bob", "shared@corp.example")
	e.signIn("mallory", "shared@corp.example")
	if status, body := put(http.MethodPut, "shared@corp.example"); status != http.StatusUnprocessableEntity || !strings.Contains(body, reasonOwnerAmbiguous) {
		t.Errorf("a shared email = %d %s, want 422 %s", status, body, reasonOwnerAmbiguous)
	}
	if grants, _ := e.ts.ListTemplateGroupAdmins(context.Background()); len(grants) != 0 {
		t.Fatalf("a refused grant was stored: %+v", grants)
	}

	// Granted by subject before the person ever signed in, revoked by email after.
	carol := templateCookie(t, "carol", oidc.RoleUser, false, []string{"eng"}, false)
	if status, body := put(http.MethodPut, "carol"); status != http.StatusOK || !strings.Contains(body, `"person":"carol"`) {
		t.Fatalf("grant by subject = %d %s", status, body)
	}
	e.signIn("carol", "carol@corp.example")
	if status, body := e.call(carol, http.MethodPost, "/api/v1/templates", templateSaveBody("group", "eng", "carol's")); status != http.StatusCreated {
		t.Fatalf("carol publishes = %d %s", status, body)
	}
	if status, body := put(http.MethodDelete, "carol@corp.example"); status != http.StatusNoContent {
		t.Fatalf("revoke by email = %d %s", status, body)
	}
	if grants, _ := e.ts.ListTemplateGroupAdmins(context.Background()); len(grants) != 0 {
		t.Fatalf("the revoke left the grant: %+v", grants)
	}
	if status, body := e.call(carol, http.MethodPost, "/api/v1/templates", templateSaveBody("group", "eng", "after")); status != http.StatusForbidden {
		t.Errorf("carol after the revoke = %d %s, want 403", status, body)
	}

	// Granted by email once known: stored as the principal.
	if status, body := put(http.MethodPut, "Carol@corp.example"); status != http.StatusOK || !strings.Contains(body, `"person":"carol"`) {
		t.Errorf("grant by a known email = %d %s, want it stored as carol", status, body)
	}
}

// TestTemplateUnverifiedSnapshotWritesAre403: with a missing or truncated
// group snapshot, a read of a group template is the missing id's 404, and a
// write (PUT, DELETE, create) is 403 template_group_unverified.
func TestTemplateUnverifiedSnapshotWritesAre403(t *testing.T) {
	e := newTemplateEnv(t)
	e.grant("eng", "gary")
	tpl := e.seed(types.TemplateOwner{Scope: types.TemplateScopeGroup, Group: "eng"}, types.TemplateKindTemplate, "eng's", templateIntentEgress)
	path := "/api/v1/templates/" + tpl.ID.String()
	for name, cookie := range map[string]*http.Cookie{
		"truncated":   templateCookie(t, "gary", oidc.RoleUser, false, []string{"eng"}, true),
		"no snapshot": templateCookie(t, "gary", oidc.RoleUser, false, nil, false),
	} {
		_, missing := e.call(cookie, http.MethodGet, "/api/v1/templates/"+uuid.NewString(), "")
		if status, body := e.call(cookie, http.MethodGet, path, ""); status != http.StatusNotFound || body != missing {
			t.Errorf("%s GET = %d %s, want the missing id's 404", name, status, body)
		}
		put := `{"name":"x","expected_revision":1,"document":` + templateDocBody(templateIntentEgress) + `}`
		for _, w := range []struct{ method, path, body string }{
			{http.MethodPut, path, put},
			{http.MethodDelete, path, ""},
			{http.MethodPost, "/api/v1/templates", templateSaveBody("group", "eng", "new")},
		} {
			if status, body := e.call(cookie, w.method, w.path, w.body); status != http.StatusForbidden || !strings.Contains(body, reasonTemplateGroupUnverified) {
				t.Errorf("%s %s = %d %s, want 403 %s", name, w.method, status, body, reasonTemplateGroupUnverified)
			}
		}
	}
}

// TestTemplateGroupLabelIsSecretScanned: a group label is held to the same
// secret scan as a template's name and description.
func TestTemplateGroupLabelIsSecretScanned(t *testing.T) {
	const canary = "ghp_A1b2C3d4E5f6G7h8I9j0K1l2M3n4O5p6Q7r8"
	e := newTemplateEnv(t)
	root := templateCookie(t, "root", oidc.RoleAdmin, false, []string{}, false)
	status, body := e.call(root, http.MethodPut, "/api/v1/admin/template-group-admins", `{"group_id":"eng","person":"gary","label":"team `+canary+`"}`)
	if status != http.StatusBadRequest || !strings.Contains(body, `"path":"label","reason":"template_secret_refused"`) || strings.Contains(body, canary) {
		t.Fatalf("a secret-shaped label = %d %s", status, body)
	}
	if labels, _ := e.ts.TemplateGroupLabels(context.Background(), []string{"eng"}); len(labels) != 0 {
		t.Fatalf("the label was stored: %v", labels)
	}
}
