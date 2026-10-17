// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/pkg/client"
)

const twoSecretDef = `{"hosts":["api.example.com"],"secrets":[` +
	`{"secret_name":"stripe-key","delivery":{"mode":"header","host":"api.example.com","header":"X-Stripe-Auth"}},` +
	`{"secret_name":"stripe-key","delivery":{"mode":"env","var":"STRIPE_KEY"}},` +
	`{"secret_name":"webhook-secret","delivery":{"mode":"file","file":"webhook"}}],"config":{"REGION":"eu-confidential"}}`

// TestRequirements_ComponentSaveReportsPresenceInTheSaversNamespace: the save
// door lists each secret once, present or missing in the caller's own rows, and
// a later save sees a secret stored in between.
func TestRequirements_ComponentSaveReportsPresenceInTheSaversNamespace(t *testing.T) {
	srv, _, _ := componentsServer(t)
	member := ssoSession(t, "sub-member", "m@corp.example", oidc.RoleUser)

	w := doSSO(t, srv, http.MethodPost, "/api/v1/me/components", member, saveComponentBody("Stripe", twoSecretDef))
	if w.Code != http.StatusCreated {
		t.Fatalf("save = %d: %s", w.Code, w.Body.String())
	}
	saved := decodeSaved(t, w)
	want := []client.ComponentRequirement{
		{Kind: "secret", Name: "stripe-key", Status: "missing", Fix: "add_secret"},
		{Kind: "secret", Name: "webhook-secret", Status: "missing", Fix: "add_secret"},
	}
	if len(saved.Requirements) != len(want) || saved.Requirements[0] != want[0] || saved.Requirements[1] != want[1] {
		t.Fatalf("requirements = %+v, want %+v", saved.Requirements, want)
	}

	secrets := srv.cfg.Secrets.(*memSecrets)
	secrets.owned["sub-member"] = map[string][]byte{"stripe-key": []byte("v")}
	w = doSSO(t, srv, http.MethodPut, "/api/v1/me/components/"+saved.ID.String(), member, saveComponentBody("Stripe", twoSecretDef))
	if w.Code != http.StatusOK {
		t.Fatalf("update = %d: %s", w.Code, w.Body.String())
	}
	got := decodeSaved(t, w).Requirements
	if len(got) != 2 || got[0] != (client.ComponentRequirement{Kind: "secret", Name: "stripe-key", Status: "present"}) || got[1].Status != "missing" {
		t.Fatalf("after storing stripe-key, requirements = %+v, want it present and webhook-secret still missing", got)
	}
}

// TestRequirements_MemberBodyDisclosesNothingAdminOwned (A18/A28): a person's
// save answers from their own rows only. An operator secret of the same name does
// not make theirs read "present" (no existence oracle), and the list carries no
// config value, header name or delivery detail, and nothing of another person's.
func TestRequirements_MemberBodyDisclosesNothingAdminOwned(t *testing.T) {
	srv, ast, _ := componentsServer(t)
	member := ssoSession(t, "sub-member", "m@corp.example", oidc.RoleUser)
	other := ast.seedComponent("sub-other")

	// "shared-key" is the operator's, stored by componentsServer.
	def := `{"hosts":["api.example.com"],"secrets":[{"secret_name":"shared-key","delivery":{"mode":"header","host":"api.example.com","header":"X-Hidden-Header"}}],"config":{"REGION":"eu-confidential"}}`
	w := doSSO(t, srv, http.MethodPost, "/api/v1/me/components", member, saveComponentBody("Mine", def))
	if w.Code != http.StatusCreated {
		t.Fatalf("save = %d: %s", w.Code, w.Body.String())
	}
	reqs := decodeSaved(t, w).Requirements
	if len(reqs) != 1 || reqs[0].Status != "missing" || reqs[0].Fix != "add_secret" {
		t.Fatalf("requirements = %+v, want the operator-held name still MISSING for a member who has none of their own", reqs)
	}
	body := w.Body.String()
	arr := body[strings.Index(body, `"requirements"`):]
	for _, forbidden := range []string{"eu-confidential", "X-Hidden-Header", "STRIPE_KEY", other.String(), "sub-other"} {
		if strings.Contains(arr, forbidden) {
			t.Errorf("requirements %s carries %q, which a save answer must not", arr, forbidden)
		}
	}
}

// TestRequirements_OrgSaveReadsTheOperatorNamespace: an organisation row's
// secrets, shared or not, are looked up in the operator namespace; the answer is
// the admin's, and a member cannot reach the door.
func TestRequirements_OrgSaveReadsTheOperatorNamespace(t *testing.T) {
	srv, _, _ := componentsServer(t)
	admin := ssoSession(t, "sub-admin", "adm@corp.example", oidc.RoleAdmin)
	member := ssoSession(t, "sub-member", "m@corp.example", oidc.RoleUser)
	org := `{"hosts":["api.example.com"],"secrets":[` +
		`{"secret_name":"shared-key","shared":true,"delivery":{"mode":"header","host":"api.example.com"}},` +
		`{"secret_name":"per-person-key","delivery":{"mode":"env","var":"PERSON_KEY"}}]}`

	w := doSSO(t, srv, http.MethodPut, "/api/v1/components/"+uuid.NewString(), admin, saveComponentBody("Org", org))
	if w.Code != http.StatusCreated {
		t.Fatalf("org save = %d: %s", w.Code, w.Body.String())
	}
	got := decodeSaved(t, w).Requirements
	if len(got) != 2 || got[0] != (client.ComponentRequirement{Kind: "secret", Name: "shared-key", Status: "present"}) ||
		got[1] != (client.ComponentRequirement{Kind: "secret", Name: "per-person-key", Status: "missing", Fix: "add_secret"}) {
		t.Fatalf("requirements = %+v, want shared-key present (operator) and per-person-key missing", got)
	}
	if w := doSSO(t, srv, http.MethodPut, "/api/v1/components/"+uuid.NewString(), member, saveComponentBody("Org", org)); w.Code != http.StatusForbidden {
		t.Fatalf("member org save = %d, want 403: the list is the admin's only", w.Code)
	}
}

// TestRequirements_ReservedNameIsRefusedAndNeverListed (G-3): every name the
// sinks reserve is refused at save by the component door, and the refusal body
// carries no requirements, so none can reach the list.
func TestRequirements_ReservedNameIsRefusedAndNeverListed(t *testing.T) {
	srv, _, _ := componentsServer(t)
	member := ssoSession(t, "sub-member", "m@corp.example", oidc.RoleUser)
	for _, name := range []string{
		bedrockAccessKeyIDSecret, bedrockSecretAccessKeySecret, bedrockSessionTokenSecret,
		types.SubscriptionOAuthSecret, types.ManagedOAuthSecret,
	} {
		if !sinkReservedSecret(name) {
			t.Fatalf("%q is not sinkReserved: the test names the wrong set", name)
		}
		def := `{"hosts":["api.example.com"],"secrets":[{"secret_name":` + jsonString(name) + `,"delivery":{"mode":"header","host":"api.example.com"}}]}`
		w := doSSO(t, srv, http.MethodPost, "/api/v1/me/components", member, saveComponentBody("R", def))
		if w.Code != http.StatusUnprocessableEntity || strings.Contains(w.Body.String(), "requirements") {
			t.Errorf("%q = %d %s, want a 422 refusal with no requirements list", name, w.Code, w.Body.String())
		}
	}
}

// TestRequirements_ComponentRequirementsListsEachSecretOnce pins the pure
// helper: a name is one row however many deliveries or namespaces use it (the
// wire row has no shared flag), a name whose reads disagree is listed missing,
// and an empty definition is [] not nil.
func TestRequirements_ComponentRequirementsListsEachSecretOnce(t *testing.T) {
	def := types.ComponentDefinition{Secrets: []types.ComponentSecret{
		{SecretName: "a"}, {SecretName: "a"}, {SecretName: "a", Shared: true}, {SecretName: "b"}, {SecretName: "c"}, {SecretName: "c", Shared: true},
	}}
	own := func(n string) bool { return n == "a" || n == "c" }
	operator := func(n string) bool { return n == "c" }
	got := componentRequirements(def, own, operator)
	if len(got) != 3 || got[0].Name != "a" || got[0].Status != "missing" || got[1].Name != "b" || got[1].Status != "missing" ||
		got[2].Name != "c" || got[2].Status != "present" {
		t.Fatalf("got %+v, want a missing (own present, shared missing: disagree), b missing, c present (both read present), each once", got)
	}
	if empty := componentRequirements(types.ComponentDefinition{}, own, operator); empty == nil || len(empty) != 0 {
		t.Fatalf("empty = %#v, want a non-nil empty list", empty)
	}
}

// TestRequirements_OrgRowNonSharedSecretReadsTheGatesNamespace (C19 review F1):
// a per-person secret on an organisation row is read the way the run gate reads
// it (callerOwnsSecret): the operator's names for an operator-owned request,
// otherwise the caller's own rows. A shared secret keeps the operator namespace.
func TestRequirements_OrgRowNonSharedSecretReadsTheGatesNamespace(t *testing.T) {
	org := `{"hosts":["api.example.com"],"secrets":[` +
		`{"secret_name":"mine-only","delivery":{"mode":"env","var":"MINE"}},` +
		`{"secret_name":"shared-key","delivery":{"mode":"env","var":"OPERATOR"}},` +
		`{"secret_name":"shared-key","shared":true,"delivery":{"mode":"header","host":"api.example.com"}}]}`
	statuses := func(t *testing.T, w *httptest.ResponseRecorder) map[string]string {
		t.Helper()
		if w.Code != http.StatusCreated {
			t.Fatalf("org save = %d: %s", w.Code, w.Body.String())
		}
		return requirementStatuses(decodeSaved(t, w).Requirements)
	}

	t.Run("admin token reads the operator namespace", func(t *testing.T) {
		srv, _, _ := componentsServer(t)
		// "shared-key" is the operator's; "mine-only" is nobody's.
		got := statuses(t, do(t, srv, http.MethodPut, "/api/v1/components/"+uuid.NewString(), adminToken, saveComponentBody("Org", org)))
		if got["shared-key"] != "present" || got["mine-only"] != "missing" {
			t.Fatalf("statuses = %+v, want the operator-held name present and the unheld one missing", got)
		}
	})

	t.Run("SSO admin reads their own rows, not the operator's", func(t *testing.T) {
		srv, _, _ := componentsServer(t)
		admin := ssoSession(t, "sub-admin", "adm@corp.example", oidc.RoleAdmin)
		srv.cfg.Secrets.(*memSecrets).owned["sub-admin"] = map[string][]byte{"mine-only": []byte("v")}
		// "shared-key" non-shared is held only by the operator: for this caller the
		// gate says it is not theirs, and the shared use of the same name stays the
		// operator's, so the one row reads missing.
		got := statuses(t, doSSO(t, srv, http.MethodPut, "/api/v1/components/"+uuid.NewString(), admin, saveComponentBody("Org", org)))
		if got["mine-only"] != "present" || got["shared-key"] != "missing" {
			t.Fatalf("statuses = %+v, want the caller's own name present and the operator-only name missing", got)
		}
	})

	t.Run("a shared secret stays the operator's for an SSO admin", func(t *testing.T) {
		srv, _, _ := componentsServer(t)
		admin := ssoSession(t, "sub-admin", "adm@corp.example", oidc.RoleAdmin)
		shared := `{"hosts":["api.example.com"],"secrets":[{"secret_name":"shared-key","shared":true,"delivery":{"mode":"header","host":"api.example.com"}}]}`
		got := statuses(t, doSSO(t, srv, http.MethodPut, "/api/v1/components/"+uuid.NewString(), admin, saveComponentBody("Org", shared)))
		if got["shared-key"] != "present" {
			t.Fatalf("statuses = %+v, want the operator-held shared secret present", got)
		}
	})
}

// TestRequirements_NoSecretIsAnEmptyList: a definition naming no secret answers
// [] and never null, the shape the console can iterate.
func TestRequirements_NoSecretIsAnEmptyList(t *testing.T) {
	srv, _, _ := componentsServer(t)
	member := ssoSession(t, "sub-member", "m@corp.example", oidc.RoleUser)
	w := doSSO(t, srv, http.MethodPost, "/api/v1/me/components", member, saveComponentBody("Plain", `{"hosts":["plain.example.com"]}`))
	if w.Code != http.StatusCreated || !strings.Contains(w.Body.String(), `"requirements":[]`) {
		t.Fatalf("= %d %s, want 201 with requirements as []", w.Code, w.Body.String())
	}
}
