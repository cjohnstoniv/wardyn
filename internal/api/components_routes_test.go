// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/pkg/client"
)

// componentsServer is the matrix server with a secret store that holds the
// operator's "shared-key" and a component store behind the saved-component
// routes, plus the recorder the audit assertions read.
func componentsServer(t *testing.T) (*Server, *authzStore, *recRecorder) {
	t.Helper()
	ast := newAuthzStore()
	h := newHarness(t)
	cfg := baseTestConfig(h, ast)
	cfg.OIDC = &oidc.Authenticator{}
	cfg.Secrets = &memSecrets{m: map[string][]byte{"shared-key": []byte("v")}, owned: map[string]map[string][]byte{}}
	cfg.SessionRevocations = fakeAuthzSessionRevocations{}
	return matrixServer(cfg), ast, h.audit
}

func componentBody(name, def string) string {
	return `{"name":` + jsonString(name) + `,"definition":` + def + `}`
}

func jsonString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

const stripeDef = `{"hosts":["api.example.com"],"secrets":[{"secret_name":"stripe-key","delivery":{"mode":"header","host":"api.example.com"}}],"config":{"REGION":"eu"}}`

func decodeSaved(t *testing.T, w *httptest.ResponseRecorder) client.ComponentSaved {
	t.Helper()
	var got client.ComponentSaved
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode %q: %v", w.Body.String(), err)
	}
	return got
}

// TestComponents_MemberSavesUpdatesDeletesOwnRow: the whole life of a person's
// row through the routes, with the version moving and the list following.
func TestComponents_MemberSavesUpdatesDeletesOwnRow(t *testing.T) {
	srv, _, rec := componentsServer(t)
	member := ssoSession(t, "sub-member", "m@corp.example", oidc.RoleUser)

	w := doSSO(t, srv, http.MethodPost, "/api/v1/me/components", member, componentBody("Stripe", stripeDef))
	if w.Code != http.StatusCreated {
		t.Fatalf("save = %d: %s", w.Code, w.Body.String())
	}
	saved := decodeSaved(t, w)
	if saved.Owner != "sub-member" || saved.Version != 1 || saved.Name != "Stripe" || saved.Requirements == nil || len(saved.Requirements) != 0 {
		t.Fatalf("saved = %+v, want the caller's row at version 1 with an empty requirements list", saved)
	}
	if !strings.Contains(w.Body.String(), `"requirements":[]`) {
		t.Errorf("body = %s, want requirements as [] not null", w.Body.String())
	}

	w = doSSO(t, srv, http.MethodPut, "/api/v1/me/components/"+saved.ID.String(), member, componentBody("Stripe live", stripeDef))
	if w.Code != http.StatusOK {
		t.Fatalf("update = %d: %s", w.Code, w.Body.String())
	}
	if up := decodeSaved(t, w); up.Version != 2 || up.Name != "Stripe live" {
		t.Fatalf("updated = %+v, want version 2 under the new name", up)
	}

	w = doSSO(t, srv, http.MethodGet, "/api/v1/me/components", member, "")
	var list client.MyComponents
	if err := json.Unmarshal(w.Body.Bytes(), &list); err != nil || w.Code != http.StatusOK {
		t.Fatalf("list = %d %s (%v)", w.Code, w.Body.String(), err)
	}
	if !list.MayDefine || !list.ResidentDeliveryAllowed || list.AutonomyCap != "" || len(list.Mine) != 1 || list.Mine[0].Name != "Stripe live" || list.Org == nil || len(list.Org) != 0 {
		t.Fatalf("list = %+v, want one saved row, may_define, no org rows, no cap", list)
	}

	if w := doSSO(t, srv, http.MethodDelete, "/api/v1/me/components/"+saved.ID.String(), member, ""); w.Code != http.StatusNoContent {
		t.Fatalf("delete = %d: %s", w.Code, w.Body.String())
	}
	if w := doSSO(t, srv, http.MethodDelete, "/api/v1/me/components/"+saved.ID.String(), member, ""); w.Code != http.StatusNotFound {
		t.Fatalf("second delete = %d, want 404", w.Code)
	}

	var actions []string
	for _, ev := range rec.snapshot() {
		if strings.HasPrefix(ev.Action, "component.") {
			actions = append(actions, ev.Action)
		}
	}
	if strings.Join(actions, ",") != "component.write,component.write,component.delete" {
		t.Errorf("component audit rows = %v, want write, write, delete", actions)
	}
}

// TestComponents_SaveRefusals: what the shared validator and the api layer
// refuse at save, each as a 422 component_definition_invalid naming the field.
func TestComponents_SaveRefusals(t *testing.T) {
	srv, _, _ := componentsServer(t)
	member := ssoSession(t, "sub-member", "m@corp.example", oidc.RoleUser)
	hdr := func(secret string, extra string) string {
		return `{"hosts":["api.example.com"],"secrets":[{"secret_name":"` + secret + `",` + extra + `"delivery":{"mode":"header","host":"api.example.com"}}]}`
	}
	cases := []struct {
		name, def, wantIn string
	}{
		{"an IP literal host (A2)", `{"hosts":["10.0.0.5"]}`, "hosts"},
		{"a decimal IP literal host (A2)", `{"hosts":["2130706433"]}`, "hosts"},
		{"a port on the header host (A9)", `{"hosts":["api.example.com:8443"],"secrets":[{"secret_name":"k","delivery":{"mode":"header","host":"api.example.com:8443"}}]}`, "delivery"},
		{"shared on a person's row", hdr("stripe-key", `"shared":true,`), "shared"},
		{"plain http on a person's row", `{"hosts":["api.example.com"],"secrets":[{"secret_name":"k","delivery":{"mode":"header","host":"api.example.com","plain_http":true}}]}`, "plain_http"},
		{"a secret name Wardyn manages (G-3)", hdr("wardyn-signing-key", ""), "managed by Wardyn"},
		{"a harness sign-in blob (G-3)", hdr("wardyn-harness-anthropic-oauth", ""), "managed by Wardyn"},
		{"a model-provider credential into the environment", `{"secrets":[{"secret_name":"` + providerSecretPrefix + `abc-key","delivery":{"mode":"env","var":"MY_KEY"}}]}`, "managed by Wardyn"},
		{"file delivery, not available in this release", `{"secrets":[{"secret_name":"k","delivery":{"mode":"file","file":"token"}}]}`, "file"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			w := doSSO(t, srv, http.MethodPost, "/api/v1/me/components", member, componentBody("x", c.def))
			if w.Code != http.StatusUnprocessableEntity || errorReason(w) != "component_definition_invalid" || !strings.Contains(w.Body.String(), c.wantIn) {
				t.Fatalf("= %d %s, want 422 component_definition_invalid naming %q", w.Code, w.Body.String(), c.wantIn)
			}
		})
	}
	// A bad name, and an unknown field, are refused before the definition is read.
	for _, body := range []string{componentBody("", `{}`), componentBody(" padded ", `{}`), `{"name":"x","definition":{},"owner":""}`} {
		if w := doSSO(t, srv, http.MethodPost, "/api/v1/me/components", member, body); w.Code != http.StatusUnprocessableEntity && w.Code != http.StatusBadRequest {
			t.Errorf("%s = %d, want a refusal", body, w.Code)
		}
	}
}

// TestComponents_NamesAreUniquePerOwnerWithoutCase (C1-R7): "Stripe" and "stripe"
// read the same in a list, so they are one name; another person may use either.
func TestComponents_NamesAreUniquePerOwnerWithoutCase(t *testing.T) {
	srv, _, _ := componentsServer(t)
	alice := ssoSession(t, "sub-alice", "a@corp.example", oidc.RoleUser)
	bob := ssoSession(t, "sub-bob", "b@corp.example", oidc.RoleUser)

	first := doSSO(t, srv, http.MethodPost, "/api/v1/me/components", alice, componentBody("Stripe", stripeDef))
	if first.Code != http.StatusCreated {
		t.Fatalf("first = %d: %s", first.Code, first.Body.String())
	}
	for _, name := range []string{"Stripe", "stripe", "STRIPE"} {
		w := doSSO(t, srv, http.MethodPost, "/api/v1/me/components", alice, componentBody(name, stripeDef))
		if w.Code != http.StatusConflict || errorReason(w) != "component_name_conflict" {
			t.Errorf("%q = %d %s, want 409 component_name_conflict", name, w.Code, w.Body.String())
		}
	}
	if w := doSSO(t, srv, http.MethodPost, "/api/v1/me/components", bob, componentBody("stripe", stripeDef)); w.Code != http.StatusCreated {
		t.Errorf("another person's same name = %d, want 201", w.Code)
	}
	// Renaming a row onto a name its owner already uses is refused; keeping its own name is not.
	other := decodeSaved(t, doSSO(t, srv, http.MethodPost, "/api/v1/me/components", alice, componentBody("Other", stripeDef)))
	if w := doSSO(t, srv, http.MethodPut, "/api/v1/me/components/"+other.ID.String(), alice, componentBody("sTRIPE", stripeDef)); w.Code != http.StatusConflict {
		t.Errorf("rename onto a taken name = %d, want 409", w.Code)
	}
	if w := doSSO(t, srv, http.MethodPut, "/api/v1/me/components/"+other.ID.String(), alice, componentBody("OTHER", stripeDef)); w.Code != http.StatusOK {
		t.Errorf("recasing its own name = %d, want 200: %s", w.Code, w.Body.String())
	}
}

// TestComponents_AnotherPersonsRowIsAbsent: a row that is someone else's answers
// exactly as an id that exists nowhere, on every by-id route and in the list.
func TestComponents_AnotherPersonsRowIsAbsent(t *testing.T) {
	srv, ast, _ := componentsServer(t)
	alice := ssoSession(t, "sub-alice", "a@corp.example", oidc.RoleUser)
	bob := ssoSession(t, "sub-bob", "b@corp.example", oidc.RoleUser)
	admin := ssoSession(t, "sub-admin", "adm@corp.example", oidc.RoleAdmin)
	row := ast.seedComponent("sub-bob")
	absent := uuid.New()

	for _, method := range []string{http.MethodPut, http.MethodDelete} {
		for who, sess := range map[string]*http.Cookie{"another member": alice, "an admin": admin} {
			foreign := doSSO(t, srv, method, "/api/v1/me/components/"+row.String(), sess, componentBody("x", stripeDef))
			missing := doSSO(t, srv, method, "/api/v1/me/components/"+absent.String(), sess, componentBody("x", stripeDef))
			if foreign.Code != http.StatusNotFound || foreign.Body.String() != missing.Body.String() {
				t.Errorf("%s %s on someone else's row = %d %s, want the bytes an absent id gets (%d %s)",
					who, method, foreign.Code, foreign.Body.String(), missing.Code, missing.Body.String())
			}
		}
	}
	if got, _ := ast.GetComponent(context.Background(), row, "sub-bob"); got.ID != row {
		t.Fatal("a refused request changed or removed someone else's row")
	}
	var list client.MyComponents
	_ = json.Unmarshal(doSSO(t, srv, http.MethodGet, "/api/v1/me/components", alice, "").Body.Bytes(), &list)
	if len(list.Mine) != 0 {
		t.Errorf("alice's list = %+v, want none of bob's rows", list.Mine)
	}
	_ = json.Unmarshal(doSSO(t, srv, http.MethodGet, "/api/v1/me/components", bob, "").Body.Bytes(), &list)
	if len(list.Mine) != 1 {
		t.Errorf("bob's list = %+v, want his one row", list.Mine)
	}
}

// TestComponents_MemberCannotTouchOrgRoutes: the organisation's routes are the
// admin tier's, whatever a member sends.
func TestComponents_MemberCannotTouchOrgRoutes(t *testing.T) {
	srv, _, _ := componentsServer(t)
	member := ssoSession(t, "sub-member", "m@corp.example", oidc.RoleUser)
	secadmin := ssoSession(t, "sub-sec", "sec@corp.example", oidc.RoleSecurityAdmin)
	id := uuid.NewString()
	for _, c := range []struct{ method, path, body string }{
		{http.MethodGet, "/api/v1/components", ""},
		{http.MethodPut, "/api/v1/components/" + id, componentBody("Org", stripeDef)},
		{http.MethodDelete, "/api/v1/components/" + id, ""},
	} {
		for who, sess := range map[string]*http.Cookie{"member": member, "security admin": secadmin} {
			if w := doSSO(t, srv, c.method, c.path, sess, c.body); w.Code != http.StatusForbidden {
				t.Errorf("%s %s %s = %d, want 403: %s", who, c.method, c.path, w.Code, w.Body.String())
			}
		}
	}
}

// TestComponents_OrgCreateIsRestrictedAndNamesAreAudited: an admin's PUT creates
// the row restricted (the same transaction, proved on Postgres) and audits both
// writes; a person's row audit carries counts only (A22), an org row its name.
func TestComponents_OrgCreateIsRestrictedAndNamesAreAudited(t *testing.T) {
	srv, ast, rec := componentsServer(t)
	admin := ssoSession(t, "sub-admin", "adm@corp.example", oidc.RoleAdmin)
	member := ssoSession(t, "sub-member", "m@corp.example", oidc.RoleUser)
	id := uuid.New()

	org := `{"hosts":["api.example.com"],"secrets":[{"secret_name":"shared-key","shared":true,"delivery":{"mode":"header","host":"api.example.com","plain_http":true}}]}`
	w := doSSO(t, srv, http.MethodPut, "/api/v1/components/"+id.String(), admin, componentBody("Org Stripe", org))
	if w.Code != http.StatusCreated {
		t.Fatalf("create = %d: %s", w.Code, w.Body.String())
	}
	if !ast.restricted["component/"+id.String()] {
		t.Fatal("a new org component was created without its restriction")
	}
	if up := doSSO(t, srv, http.MethodPut, "/api/v1/components/"+id.String(), admin, componentBody("Org Stripe", org)); up.Code != http.StatusOK || decodeSaved(t, up).Version != 2 {
		t.Fatalf("second PUT = %d %s, want 200 at version 2", up.Code, up.Body.String())
	}
	doSSO(t, srv, http.MethodPost, "/api/v1/me/components", member, componentBody("My Secret Host", `{"hosts":["internal-billing.example.com"],"secrets":[{"secret_name":"my-token-name","delivery":{"mode":"env","var":"MY_TOKEN"}}],"config":{"MY_CONFIG_KEY":"v"}}`))

	var orgRow, personRow map[string]any
	for _, ev := range rec.snapshot() {
		var d map[string]any
		_ = json.Unmarshal(ev.Data, &d)
		switch {
		case ev.Action == "component.write" && d["source"] == "org" && d["op"] == "create":
			orgRow = d
		case ev.Action == "component.write" && d["source"] == "self":
			personRow = d
		}
		if ev.Action == "capability.availability.write" && !strings.Contains(string(ev.Data), id.String()) {
			t.Errorf("availability row = %s, want it to name the new component", ev.Data)
		}
	}
	if orgRow == nil || orgRow["name"] != "Org Stripe" || orgRow["kind"] != "custom" || orgRow["version"] != float64(1) || orgRow["hosts"] != float64(1) {
		t.Errorf("org audit datum = %v, want its name, kind, version and counts", orgRow)
	}
	if personRow == nil || personRow["kind"] != "custom" || personRow["secrets"] != float64(1) || personRow["config_keys"] != float64(1) {
		t.Fatalf("person audit datum = %v, want kind and counts", personRow)
	}
	for _, ev := range rec.snapshot() {
		for _, leak := range []string{"internal-billing", "my-token-name", "MY_TOKEN", "MY_CONFIG_KEY", "My Secret Host"} {
			if strings.Contains(string(ev.Data), leak) || strings.Contains(ev.Target, leak) {
				t.Errorf("audit row %s carries %q from a person's component (A22): %s", ev.Action, leak, ev.Data)
			}
		}
	}
}

// TestComponents_OrgSharedSecretMustBeInTheOperatorNamespace: a shared secret an
// admin names must already be the operator's.
func TestComponents_OrgSharedSecretMustBeInTheOperatorNamespace(t *testing.T) {
	srv, _, _ := componentsServer(t)
	admin := ssoSession(t, "sub-admin", "adm@corp.example", oidc.RoleAdmin)
	def := func(secret string) string {
		return `{"hosts":["api.example.com"],"secrets":[{"secret_name":"` + secret + `","shared":true,"delivery":{"mode":"header","host":"api.example.com"}}]}`
	}
	w := doSSO(t, srv, http.MethodPut, "/api/v1/components/"+uuid.NewString(), admin, componentBody("Org", def("not-stored")))
	if w.Code != http.StatusUnprocessableEntity || errorReason(w) != "component_secret_missing" {
		t.Fatalf("an unstored shared secret = %d %s, want 422 component_secret_missing", w.Code, w.Body.String())
	}
	if w := doSSO(t, srv, http.MethodPut, "/api/v1/components/"+uuid.NewString(), admin, componentBody("Org", def("shared-key"))); w.Code != http.StatusCreated {
		t.Fatalf("a stored shared secret = %d %s, want 201", w.Code, w.Body.String())
	}
	// An org row may name an address; a person's may not.
	if w := doSSO(t, srv, http.MethodPut, "/api/v1/components/"+uuid.NewString(), admin, componentBody("Lab", `{"hosts":["10.0.0.5"]}`)); w.Code != http.StatusCreated {
		t.Errorf("an org row naming an address = %d %s, want 201", w.Code, w.Body.String())
	}
	if w := doSSO(t, srv, http.MethodPut, "/api/v1/components/"+uuid.Nil.String(), admin, componentBody("Nil", `{}`)); w.Code != http.StatusBadRequest {
		t.Errorf("the nil id = %d, want 400", w.Code)
	}
}
