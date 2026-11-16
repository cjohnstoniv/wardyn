// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/secretstore"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/pkg/client"
)

// templateCheck posts one candidate to the template's check route and decodes a 200's verdict.
func (e *templateEnv) templateCheck(c *http.Cookie, tpl types.RunTemplate, candidate string) (int, client.PolicyCheckResult, string) {
	e.t.Helper()
	status, body := e.call(c, http.MethodPost, "/api/v1/templates/"+tpl.ID.String()+"/policy-check", `{"candidate":`+candidate+`}`)
	var out client.PolicyCheckResult
	if status == http.StatusOK {
		if err := json.Unmarshal([]byte(body), &out); err != nil {
			e.t.Fatalf("verdict is not JSON: %v: %s", err, body)
		}
	}
	return status, out, body
}

// The template names only allowed_domains, so a launch of it takes every other field from the
// caller's source policy, here the deployment default. The check is against that whole policy: the
// template's keys alone, or the default with the template's keys left off, would each answer these
// rows the other way.
func TestTemplatePolicyCheckLaysTheTemplateOntoTheCallersPolicy(t *testing.T) {
	e := newTemplateEnv(t)
	e.srv.cfg.DefaultPolicy = types.RunPolicySpec{
		AllowedDomains:      []string{"pypi.org", "files.corp.example"},
		DeniedDomains:       []string{"evil.example"},
		MinConfinementClass: types.CC2,
	}
	tpl := e.seed(types.TemplateOwner{Scope: types.TemplateScopePerson, Person: "alice"}, types.TemplateKindTemplate, "recorded", templateIntentEgress)
	alice := templateCookie(t, "alice", oidc.RoleUser, false, []string{}, false)
	for _, row := range []struct {
		name, candidate string
		want            client.PolicyCheckResult
	}{
		{"the template as it resolves", `{"allowed_domains":["pypi.org"],"denied_domains":["evil.example"],"min_confinement_class":"CC2"}`,
			client.PolicyCheckResult{Within: true, Widenings: []client.PolicyWidening{}}},
		{"narrower on every field", `{"allowed_domains":[],"denied_domains":["evil.example","worse.example"],"min_confinement_class":"CC3"}`,
			client.PolicyCheckResult{Within: true, Widenings: []client.PolicyWidening{}}},
		{"the template's own keys alone", `{"allowed_domains":["pypi.org"]}`,
			client.PolicyCheckResult{Widenings: []client.PolicyWidening{{Field: "denied_domains", Entries: []string{"evil.example"}}, {Field: "min_confinement_class"}}}},
		{"a host the default allows and the template does not", `{"allowed_domains":["pypi.org","files.corp.example"],"denied_domains":["evil.example"],"min_confinement_class":"CC2"}`,
			client.PolicyCheckResult{Widenings: []client.PolicyWidening{{Field: "allowed_domains", Entries: []string{"files.corp.example"}}}}},
	} {
		status, got, body := e.templateCheck(alice, tpl, row.candidate)
		if status != http.StatusOK || !reflect.DeepEqual(got, row.want) {
			t.Errorf("%s: %d %+v, want 200 %+v; body=%s", row.name, status, got, row.want, body)
		}
	}
}

// Whoever may read the template may check against it, and only they: the route takes the read
// authority types.TemplateRoutes gives it. Anyone else gets the missing template's 404, and a
// session in the Admin view is refused as the policy preview refuses it.
func TestTemplatePolicyCheckFollowsTheReadAuthority(t *testing.T) {
	row := templateRouteRow(t, "POST /api/v1/templates/{id}/policy-check")
	for scope, owner := range templateOwners {
		for _, c := range templateCallerCases {
			e := newTemplateEnv(t)
			tpl := e.seed(owner, types.TemplateKindTemplate, "seeded", templateIntentEgress)
			userView := templateCookie(t, c.sub, c.role, c.role == oidc.RoleAdmin, c.groups, false)
			status, _, body := e.templateCheck(userView, tpl, `{"allowed_domains":["pypi.org"]}`)
			want := http.StatusNotFound
			if tierAllows(c.tier(row, scope), c) {
				want = http.StatusOK
			}
			if status != want {
				t.Errorf("%s template as %s: %d, want %d; body=%s", scope, c.name, status, want, body)
			}
		}
	}
	e := newTemplateEnv(t)
	tpl := e.seed(types.TemplateOwner{Scope: types.TemplateScopeOrg}, types.TemplateKindTemplate, "org", templateIntentEgress)
	if status, _, body := e.templateCheck(templateCookie(t, "root", oidc.RoleAdmin, false, []string{}, false), tpl, `{}`); status != http.StatusConflict {
		t.Errorf("the Admin view: %d, want 409; body=%s", status, body)
	}
}

// A body that names no candidate is refused: read as an empty policy it would be within nearly
// anything. A candidate field this server does not know is refused too, never dropped.
func TestTemplatePolicyCheckRefusesABodyWithNoCandidate(t *testing.T) {
	e := newTemplateEnv(t)
	tpl := e.seed(types.TemplateOwner{Scope: types.TemplateScopePerson, Person: "alice"}, types.TemplateKindTemplate, "recorded", templateIntentEgress)
	alice := templateCookie(t, "alice", oidc.RoleUser, false, []string{}, false)
	for _, body := range []string{`{}`, `{"candidate":null}`, `{"candidate":{"field_from_a_newer_client":true}}`} {
		status, resp := e.call(alice, http.MethodPost, "/api/v1/templates/"+tpl.ID.String()+"/policy-check", body)
		if status != http.StatusBadRequest || !json.Valid([]byte(resp)) {
			t.Errorf("%s: %d %s, want 400", body, status, resp)
		}
	}
}

// A caller below the tier that reads a mount's host path or a secret's name is judged against the
// policy as their own read shows it, so two candidates that differ only in such a value, one the
// true one and one a guess, get the same answer byte for byte.
func TestTemplatePolicyCheckConfirmsNoWithheldValue(t *testing.T) {
	e := newTemplateEnv(t)
	e.srv.cfg.DefaultPolicy = types.RunPolicySpec{
		WorkspaceMounts: []types.WorkspaceMount{{Source: "/srv/operator/creds", Target: "/work"}},
		LLMInspection:   &types.LLMInspectionSpec{WorkspaceSecretNames: []string{"corp-signing-key"}},
	}
	tpl := e.seed(types.TemplateOwner{Scope: types.TemplateScopePerson, Person: "alice"}, types.TemplateKindTemplate, "recorded", templateIntentEgress)
	alice := templateCookie(t, "alice", oidc.RoleUser, false, []string{}, false)
	for _, row := range []struct{ name, right, wrong string }{
		{"a mount's host path",
			`{"workspace_mounts":[{"source":"/srv/operator/creds","target":"/work"}]}`,
			`{"workspace_mounts":[{"source":"/srv/other","target":"/work"}]}`},
		{"a workspace secret name",
			`{"llm_inspection":{"workspace_secret_names":["corp-signing-key"]}}`,
			`{"llm_inspection":{"workspace_secret_names":["other"]}}`},
	} {
		statusR, bodyR := e.call(alice, http.MethodPost, "/api/v1/templates/"+tpl.ID.String()+"/policy-check", `{"candidate":`+row.right+`}`)
		statusW, bodyW := e.call(alice, http.MethodPost, "/api/v1/templates/"+tpl.ID.String()+"/policy-check", `{"candidate":`+row.wrong+`}`)
		if statusR != http.StatusOK || statusW != http.StatusOK || bodyR != bodyW {
			t.Errorf("%s: true value %d %s, wrong value %d %s, want two 200s with one body", row.name, statusR, bodyR, statusW, bodyW)
		}
	}
}

// withheldPolicyStore is the template test store with the policy capability enforced and no grant
// held, so no caller may launch with a stored policy, and with one stored policy to name.
type withheldPolicyStore struct {
	templateTestStore
	policy types.RunPolicy
}

func (withheldPolicyStore) GetCapabilityEnforcement(context.Context) (map[string]bool, error) {
	return map[string]bool{capPolicy: true}, nil
}

func (s withheldPolicyStore) GetPolicy(_ context.Context, id uuid.UUID) (types.RunPolicy, error) {
	if id != s.policy.ID {
		return types.RunPolicy{}, store.ErrNotFound
	}
	return s.policy, nil
}

// A template that names a stored policy the caller may not launch with gets the launch's refusal,
// never a verdict: an answer would tell the caller what that policy holds, which a read of it
// refuses them.
func TestTemplatePolicyCheckRefusesAPolicyTheCallerIsNotGranted(t *testing.T) {
	withheld := types.RunPolicy{ID: uuid.New(), Name: "withheld", Spec: types.RunPolicySpec{DeniedDomains: []string{"secret.example"}}}
	h := newHarness(t)
	ts := newFakeTemplateStore()
	cfg := baseTestConfig(h, withheldPolicyStore{templateTestStore: templateTestStore{fakeTemplateStore: ts}, policy: withheld})
	cfg.OIDC = &oidc.Authenticator{}
	cfg.Secrets = getErrStore{getErr: secretstore.ErrNotFound}
	e := &templateEnv{t: t, srv: New(cfg), ts: ts, audit: h.audit}
	tpl := e.seed(types.TemplateOwner{Scope: types.TemplateScopePerson, Person: "alice"}, types.TemplateKindTemplate, "names a policy", `{"policy_id":"`+withheld.ID.String()+`"}`)
	alice := templateCookie(t, "alice", oidc.RoleUser, false, []string{}, false)
	status, _, body := e.templateCheck(alice, tpl, `{"allowed_domains":["pypi.org"]}`)
	if status != http.StatusForbidden || strings.Contains(body, `"within"`) || strings.Contains(body, "secret.example") {
		t.Errorf("a withheld policy: %d %s, want the launch's 403 and no verdict", status, body)
	}
}
