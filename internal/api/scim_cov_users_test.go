// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/scim"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

const scimCovUserSchema = `"schemas":["urn:ietf:params:scim:schemas:core:2.0:User"]`

func scimCovBound() store.PrincipalIdentity {
	return store.PrincipalIdentity{
		Principal: "sub-pat", Issuer: scimCovIssuer, TenantID: scimCovTenant, ObjectID: scimCovOID,
		EmailLower: "pat@corp.example",
	}
}

func scimCovPatch(ops ...string) string {
	return `{"schemas":["urn:ietf:params:scim:api:messages:2.0:PatchOp"],"Operations":[` + strings.Join(ops, ",") + `]}`
}

func scimCovReplace(path, value string) string {
	return fmt.Sprintf(`{"op":"Replace","path":%q,"value":%s}`, path, value)
}

func scimCovUserBody(externalID, userName string, active bool, emails ...string) string {
	var list []string
	for _, e := range emails {
		list = append(list, fmt.Sprintf(`{"value":%q,"type":"work","primary":true}`, e))
	}
	return fmt.Sprintf(`{%s,"externalId":%q,"userName":%q,"active":%t,"emails":[%s]}`,
		scimCovUserSchema, externalID, userName, active, strings.Join(list, ","))
}

// Every route answers 501 in the SCIM envelope when the store is not the Postgres backend, and writes nothing.
func TestSCIMCovRoutesNeedTheSCIMStore(t *testing.T) {
	e := newSCIMCovEnv(t, rbacStore{})
	id := uuid.NewString()
	for _, c := range []struct{ method, path, body string }{
		{http.MethodGet, "/scim/v2/Users?filter=" + url.QueryEscape(`userName eq "x"`), ""},
		{http.MethodGet, "/scim/v2/Users/" + id, ""},
		{http.MethodPost, "/scim/v2/Users", scimCovUserBody(scimCovOID, "x", true)},
		{http.MethodPatch, "/scim/v2/Users/" + id, scimCovPatch(scimCovReplace("active", "false"))},
		{http.MethodPut, "/scim/v2/Users/" + id, scimCovUserBody(scimCovOID, "x", true)},
		{http.MethodDelete, "/scim/v2/Users/" + id, ""},
		{http.MethodGet, "/scim/v2/Groups?filter=" + url.QueryEscape(`displayName eq "x"`), ""},
		{http.MethodGet, "/scim/v2/Groups/" + id, ""},
		{http.MethodPost, "/scim/v2/Groups", `{"externalId":"g","displayName":"G"}`},
		{http.MethodPatch, "/scim/v2/Groups/" + id, scimCovPatch(scimCovReplace("displayName", `"G"`))},
		{http.MethodDelete, "/scim/v2/Groups/" + id, ""},
	} {
		t.Run(c.method+" "+strings.SplitN(c.path, "?", 2)[0], func(t *testing.T) {
			w := e.scim(c.method, c.path, c.body)
			scimCovWantError(t, w, http.StatusNotImplemented, "")
			if !strings.Contains(w.Body.String(), "SCIM requires the Postgres store backend") {
				t.Errorf("the 501 does not say why: %s", w.Body.String())
			}
		})
	}
	if rows := e.h.audit.snapshot(); len(rows) != 0 {
		t.Errorf("a refused request wrote audit rows: %+v", rows)
	}
}

func TestSCIMCovResourceRendering(t *testing.T) {
	id := uuid.MustParse("00000000-0000-4000-8000-000000000001")
	gone := scimCovNow
	for _, c := range []struct {
		name string
		in   store.PrincipalIdentity
		want scimCovUser
	}{
		{"an unbound projection", store.PrincipalIdentity{ID: id, ObjectID: scimCovOID, ScimExternalID: "ext", ScimUserName: "Pat", EmailLower: "pat@corp.example"},
			scimCovUser{ID: id.String(), ExternalID: "ext", UserName: "Pat", Active: true, Emails: []scim.Email{{Value: "pat@corp.example", Type: "work", Primary: true}}}},
		{"no projection falls back to the email and object id", store.PrincipalIdentity{ID: id, ObjectID: scimCovOID, EmailLower: "pat@corp.example"},
			scimCovUser{ID: id.String(), ExternalID: scimCovOID, UserName: "pat@corp.example", Active: true, Emails: []scim.Email{{Value: "pat@corp.example", Type: "work", Primary: true}}}},
		{"only an object id", store.PrincipalIdentity{ID: id, ObjectID: scimCovOID},
			scimCovUser{ID: id.String(), ExternalID: scimCovOID, UserName: scimCovOID, Active: true}},
		{"a deactivated identity is inactive", store.PrincipalIdentity{ID: id, ObjectID: scimCovOID, DeactivatedAt: &gone},
			scimCovUser{ID: id.String(), ExternalID: scimCovOID, UserName: scimCovOID}},
		{"a purged identity is inactive", store.PrincipalIdentity{ID: id, ObjectID: scimCovOID, PurgedAt: &gone},
			scimCovUser{ID: id.String(), ExternalID: scimCovOID, UserName: scimCovOID}},
	} {
		t.Run(c.name, func(t *testing.T) {
			got := scimResource(c.in)
			if got.ID != c.want.ID || got.ExternalID != c.want.ExternalID || got.UserName != c.want.UserName || got.Active != c.want.Active ||
				!slices.Equal(got.Emails, c.want.Emails) {
				t.Errorf("scimResource = %+v, want %+v", got, c.want)
			}
			if got.Meta == nil || got.Meta.ResourceType != "User" || got.Meta.Created != c.in.CreatedAt.UTC().Format(time.RFC3339) {
				t.Errorf("meta = %+v", got.Meta)
			}
			if len(got.Schemas) != 1 || got.Schemas[0] != scim.SchemaUser {
				t.Errorf("schemas = %v", got.Schemas)
			}
		})
	}
}

func TestSCIMCovListUsers(t *testing.T) {
	st := newSCIMCovStore()
	pat := st.addIdentity(store.PrincipalIdentity{Issuer: scimCovIssuer, ObjectID: scimCovOID, ScimUserName: "Pat", EmailLower: "pat@corp.example"}, "pat")
	st.addIdentity(store.PrincipalIdentity{Issuer: "https://other.example", ObjectID: "other", EmailLower: "pat@corp.example"})
	e := newSCIMCovEnv(t, st)

	path := "/scim/v2/Users?filter=" + url.QueryEscape(`emails.value eq "PAT@corp.example"`)
	w := e.scim(http.MethodGet, path, "")
	if w.Code != http.StatusOK || w.Header().Get("Content-Type") != scim.MediaType {
		t.Fatalf("list = %d %q: %s", w.Code, w.Header().Get("Content-Type"), w.Body.String())
	}
	list := scimCovDecode[struct {
		TotalResults int           `json:"totalResults"`
		Resources    []scimCovUser `json:"Resources"`
	}](t, w)
	if list.TotalResults != 1 || len(list.Resources) != 1 || list.Resources[0].ID != pat.ID.String() || list.Resources[0].UserName != "Pat" {
		t.Errorf("list = %+v, want only the identity under this deployment's issuer", list)
	}
	if got := st.searches; len(got) != 1 || got[0] != (scimCovSearch{Issuer: scimCovIssuer, Attr: store.SearchEmail, Value: "PAT@corp.example", Limit: scimListLimit}) {
		t.Errorf("the store was asked %+v, want one search of the configured issuer, capped at %d", got, scimListLimit)
	}

	empty := e.scim(http.MethodGet, "/scim/v2/Users?filter="+url.QueryEscape(`userName eq "nobody"`), "")
	if got := scimCovDecode[struct {
		TotalResults int `json:"totalResults"`
		Resources    []any
	}](t, empty); empty.Code != http.StatusOK || got.TotalResults != 0 || got.Resources == nil {
		t.Errorf("no match = %d %s, want 200 with an empty Resources array", empty.Code, empty.Body.String())
	}
}

func TestSCIMCovListUsersRefusals(t *testing.T) {
	st := newSCIMCovStore()
	e := newSCIMCovEnv(t, st)
	for name, filter := range map[string]string{
		"no filter":      "",
		"a blank filter": "%20%20",
		"an unsupported": url.QueryEscape(`displayName eq "x"`),
	} {
		t.Run(name, func(t *testing.T) {
			path := "/scim/v2/Users"
			if filter != "" {
				path += "?filter=" + filter
			}
			scimCovWantError(t, e.scim(http.MethodGet, path, ""), http.StatusBadRequest, scim.TypeInvalidFilter)
		})
	}
	if len(st.searches) != 0 {
		t.Errorf("a refused filter reached the store: %+v", st.searches)
	}

	st.failNext("SearchIdentities", errSCIMCovBoom, -1)
	w := e.scim(http.MethodGet, "/scim/v2/Users?filter="+url.QueryEscape(`userName eq "x"`), "")
	scimCovWantRetry(t, w, "secret-dsn")
}

func TestSCIMCovGetUser(t *testing.T) {
	st := newSCIMCovStore()
	pat := st.addIdentity(scimCovBound())
	e := newSCIMCovEnv(t, st)

	w := e.scim(http.MethodGet, "/scim/v2/Users/"+pat.ID.String(), "")
	if got := scimCovDecode[scimCovUser](t, w); w.Code != http.StatusOK || got.ID != pat.ID.String() || !got.Active {
		t.Errorf("GET = %d %+v", w.Code, got)
	}
	for _, id := range []string{"not-a-uuid", "sub-pat", uuid.NewString()} {
		w := e.scim(http.MethodGet, "/scim/v2/Users/"+id, "")
		scimCovWantError(t, w, http.StatusNotFound, "")
		if !strings.Contains(w.Body.String(), "no such user") {
			t.Errorf("GET /Users/%s: %s", id, w.Body.String())
		}
	}
	st.failNext("GetIdentity", errSCIMCovBoom, 1)
	scimCovWantRetry(t, e.scim(http.MethodGet, "/scim/v2/Users/"+pat.ID.String(), ""), "secret-dsn")
}

func TestSCIMCovCreateRefusals(t *testing.T) {
	st := newSCIMCovStore()
	e := newSCIMCovEnv(t, st)
	for _, c := range []struct {
		name, body, scimType string
		status               int
	}{
		{"not JSON", `{`, scim.TypeInvalidSyntax, http.StatusBadRequest},
		{"no userName", `{"externalId":"` + scimCovOID + `"}`, scim.TypeInvalidValue, http.StatusBadRequest},
		{"no externalId", `{"userName":"x"}`, scim.TypeInvalidValue, http.StatusBadRequest},
		{"an externalId that is not a GUID", scimCovUserBody("u1091", "x", true), scim.TypeInvalidValue, http.StatusBadRequest},
		{"a body over the cap", `{"userName":"` + strings.Repeat("a", scimMaxBody) + `"}`, "", http.StatusRequestEntityTooLarge},
	} {
		t.Run(c.name, func(t *testing.T) {
			scimCovWantError(t, e.scim(http.MethodPost, "/scim/v2/Users", c.body), c.status, c.scimType)
		})
	}
	if len(st.creates) != 0 || len(st.updates) != 0 || len(e.h.audit.snapshot()) != 0 {
		t.Errorf("a refused POST wrote: creates %v updates %v audit %v", st.creates, st.updates, e.h.audit.snapshot())
	}
}

func TestSCIMCovCreateStoresAnUnboundIdentity(t *testing.T) {
	st := newSCIMCovStore()
	e := newSCIMCovEnv(t, st)
	body := scimCovUserBody(strings.ToUpper(scimCovOID), "Pat.Jones@corp.example", true, "Pat.Jones@corp.example")

	w := e.scim(http.MethodPost, "/scim/v2/Users", body)
	got := scimCovDecode[scimCovUser](t, w)
	if w.Code != http.StatusCreated || !got.Active || got.ExternalID != scimCovOID || got.UserName != "Pat.Jones@corp.example" {
		t.Fatalf("POST = %d %+v", w.Code, got)
	}
	want := scimCovCreate{Issuer: scimCovIssuer, Tenant: scimCovTenant, Object: scimCovOID, U: store.IdentityUpdate{
		ExternalID: scimCovOID, UserName: "Pat.Jones@corp.example", Emails: []string{"Pat.Jones@corp.example"},
	}}
	if len(st.creates) != 1 || !scimCovSameCreate(st.creates[0], want) {
		t.Errorf("CreateScimIdentity got %+v, want exactly %+v (the object id normalised to lower case)", st.creates, want)
	}
	row := st.identity(uuid.MustParse(got.ID))
	if row.Principal != "" || row.DeactivatedAt != nil {
		t.Errorf("a created user must be unbound and active: %+v", row)
	}
	rows := e.auditRows("scim.user.write")
	if len(rows) != 1 || rows[0].Actor != scimActor || rows[0].ActorType != types.ActorSystem || rows[0].Target != got.ID || rows[0].Outcome != "success" {
		t.Fatalf("scim.user.write rows = %+v", rows)
	}
	if d := e.auditData(rows[0]); d["op"] != "create" || d["slot"] != scimSlotPrimary {
		t.Errorf("row data = %v, want op create under the primary slot", d)
	}
	if st.callCount("SuspendIdentity") != 0 {
		t.Error("an active user was suspended")
	}
}

func scimCovSameCreate(a, b scimCovCreate) bool {
	return a.Issuer == b.Issuer && a.Tenant == b.Tenant && a.Object == b.Object &&
		a.U.ExternalID == b.U.ExternalID && a.U.UserName == b.U.UserName && slices.Equal(a.U.Emails, b.U.Emails)
}

func TestSCIMCovCreateLinksTheIdentityHoldingTheObjectID(t *testing.T) {
	for _, c := range []struct {
		name       string
		projection string
		wantExt    string
		wantStatus int
	}{
		{"an unprojected identity gets the externalId", "", scimCovOID, http.StatusCreated},
		{"the same externalId in another case is left as it is", strings.ToUpper(scimCovOID), "", http.StatusCreated},
		{"another externalId is a conflict", "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb", "", http.StatusConflict},
	} {
		t.Run(c.name, func(t *testing.T) {
			st := newSCIMCovStore()
			row := scimCovBound()
			row.ScimExternalID = c.projection
			ident := st.addIdentity(row)
			e := newSCIMCovEnv(t, st)

			w := e.scim(http.MethodPost, "/scim/v2/Users", scimCovUserBody(scimCovOID, "Pat", true, "pat@corp.example"))
			if c.wantStatus == http.StatusConflict {
				scimCovWantError(t, w, http.StatusConflict, "uniqueness")
				if len(st.updates) != 0 || len(e.h.audit.snapshot()) != 0 {
					t.Errorf("a refused link wrote: updates %v audit %v", st.updates, e.h.audit.snapshot())
				}
				return
			}
			if w.Code != c.wantStatus || scimCovDecode[scimCovUser](t, w).ID != ident.ID.String() {
				t.Fatalf("POST = %d %s, want the existing identity %s", w.Code, w.Body.String(), ident.ID)
			}
			if len(st.creates) != 0 {
				t.Errorf("a linked identity was created again: %+v", st.creates)
			}
			if len(st.updates) != 1 || st.updates[0].ID != ident.ID || st.updates[0].U.ExternalID != c.wantExt || st.updates[0].U.UserName != "Pat" {
				t.Errorf("update = %+v, want the projection of %s with externalId %q", st.updates, ident.ID, c.wantExt)
			}
			if rows := e.auditRows("scim.user.write"); len(rows) != 1 || e.auditData(rows[0])["op"] != "link" {
				t.Errorf("rows = %+v, want one link", rows)
			}
		})
	}
}

func TestSCIMCovCreateLinksThroughThePersonOrAnAlias(t *testing.T) {
	personKey := entraPrincipalPrefix + scimCovTenant + ":" + scimCovOID

	t.Run("the person set up as entra:tenant:oid", func(t *testing.T) {
		st := newSCIMCovStore()
		bound := st.addIdentity(store.PrincipalIdentity{Principal: personKey, Issuer: scimCovIssuer})
		st.addIdentity(store.PrincipalIdentity{Principal: personKey, Issuer: "https://other.example"})
		e := newSCIMCovEnv(t, scimCovPeople{scimCovStore: st, people: map[string]types.Person{personKey: {Principal: personKey}}})
		w := e.scim(http.MethodPost, "/scim/v2/Users", scimCovUserBody(scimCovOID, "Pat", true))
		if w.Code != http.StatusCreated || len(st.creates) != 0 || len(st.updates) != 1 || st.updates[0].ID != bound.ID {
			t.Errorf("POST = %d creates %v updates %+v, want the row of the configured issuer linked", w.Code, st.creates, st.updates)
		}
	})
	t.Run("a person whose only row is on another issuer links nothing", func(t *testing.T) {
		st := newSCIMCovStore()
		st.addIdentity(store.PrincipalIdentity{Principal: personKey, Issuer: "https://other.example"})
		e := newSCIMCovEnv(t, scimCovPeople{scimCovStore: st, people: map[string]types.Person{personKey: {Principal: personKey}}})
		if w := e.scim(http.MethodPost, "/scim/v2/Users", scimCovUserBody(scimCovOID, "Pat", true)); w.Code != http.StatusCreated || len(st.creates) != 1 {
			t.Errorf("POST = %d creates %v, want a new identity", w.Code, st.creates)
		}
	})
	t.Run("the one row with no object id seen under the email", func(t *testing.T) {
		st := newSCIMCovStore()
		old := st.addIdentity(store.PrincipalIdentity{Principal: "sub-old", Issuer: scimCovIssuer, EmailLower: "pat@corp.example"})
		e := newSCIMCovEnv(t, st)
		w := e.scim(http.MethodPost, "/scim/v2/Users", scimCovUserBody(scimCovOID, "Pat", true, "Pat@corp.example"))
		if w.Code != http.StatusCreated || len(st.creates) != 0 || len(st.updates) != 1 || st.updates[0].ID != old.ID {
			t.Errorf("POST = %d creates %v updates %+v, want the alias row linked", w.Code, st.creates, st.updates)
		}
	})
	t.Run("two rows under the email link nothing", func(t *testing.T) {
		st := newSCIMCovStore()
		st.addIdentity(store.PrincipalIdentity{Principal: "sub-1", Issuer: scimCovIssuer, EmailLower: "pat@corp.example"})
		st.addIdentity(store.PrincipalIdentity{Principal: "sub-2", Issuer: scimCovIssuer, EmailLower: "pat@corp.example"})
		e := newSCIMCovEnv(t, st)
		if w := e.scim(http.MethodPost, "/scim/v2/Users", scimCovUserBody(scimCovOID, "Pat", true, "pat@corp.example")); w.Code != http.StatusCreated || len(st.creates) != 1 || len(st.updates) != 0 {
			t.Errorf("POST = %d creates %v updates %v, want a new identity and no rebinding", w.Code, st.creates, st.updates)
		}
	})
}

func TestSCIMCovCreateAnswers500WhenAStoreStepFails(t *testing.T) {
	personKey := entraPrincipalPrefix + scimCovTenant + ":" + scimCovOID
	for _, c := range []struct {
		name, method string
		seed         func(*scimCovStore)
		withPerson   bool
		isRead       bool
	}{
		{"the object lookup", "GetIdentityByObject", nil, false, true},
		{"the person lookup", "GetPerson", nil, true, true},
		{"the person's identity rows", "IdentitiesByPrincipal", func(s *scimCovStore) {
			s.addIdentity(store.PrincipalIdentity{Principal: personKey, Issuer: scimCovIssuer})
		}, true, true},
		{"the alias lookup", "IdentitiesByAlias", nil, false, true},
		{"the create", "CreateScimIdentity", nil, false, false},
		{"the link", "ApplyIdentityUpdate", func(s *scimCovStore) { s.addIdentity(scimCovBound()) }, false, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			st := newSCIMCovStore()
			if c.seed != nil {
				c.seed(st)
			}
			people := scimCovPeople{scimCovStore: st}
			if c.withPerson {
				people.people = map[string]types.Person{personKey: {Principal: personKey}}
			}
			st.failNext(c.method, errSCIMCovBoom, -1)
			e := newSCIMCovEnv(t, people)
			w := e.scim(http.MethodPost, "/scim/v2/Users", scimCovUserBody(scimCovOID, "Pat", true, "pat@corp.example"))
			scimCovWantRetry(t, w, "secret-dsn")
			if rows := e.auditRows("scim.user.write"); len(rows) != 0 {
				t.Errorf("a failed write was audited as done: %+v", rows)
			}
			if c.isRead && (len(st.creates) != 0 || len(st.updates) != 0) {
				t.Errorf("a failed read was followed by a write: creates %+v updates %+v", st.creates, st.updates)
			}
		})
	}
}

func TestSCIMCovCreateInactiveSuspendsTheNewUser(t *testing.T) {
	st := newSCIMCovStore()
	e := newSCIMCovEnv(t, st)

	w := e.scim(http.MethodPost, "/scim/v2/Users", scimCovUserBody(scimCovOID, "Pat", false, "pat@corp.example"))
	got := scimCovDecode[scimCovUser](t, w)
	if w.Code != http.StatusCreated || got.Active {
		t.Fatalf("POST active=false = %d %+v, want 201 with the user inactive", w.Code, got)
	}
	id := uuid.MustParse(got.ID)
	if st.identity(id).DeactivatedAt == nil || len(st.plans) != 1 || st.plans[0].IdentityID != id {
		t.Errorf("the identity was not suspended: %+v plans %+v", st.identity(id), st.plans)
	}
	if rows := e.auditRows("scim.user.write"); len(rows) != 1 {
		t.Errorf("scim.user.write rows = %d, want the create recorded once", len(rows))
	}
	if rows := e.auditRows("scim.user.deactivate"); len(rows) != 1 || e.auditData(rows[0])["slot"] != scimSlotPrimary {
		t.Errorf("scim.user.deactivate rows = %+v, want one naming the primary slot", rows)
	}

	// A suspension that cannot finish answers 5xx so the identity provider retries, after the create stuck.
	st2 := newSCIMCovStore()
	st2.failNext("SuspendIdentity", errSCIMCovBoom, -1)
	e2 := newSCIMCovEnv(t, st2)
	scimCovWantRetry(t, e2.scim(http.MethodPost, "/scim/v2/Users", scimCovUserBody(scimCovOID, "Pat", false)), "secret-dsn")
	if len(st2.creates) != 1 || len(e2.auditRows("scim.user.write")) != 1 {
		t.Errorf("the create must stand while the suspension retries: creates %v", st2.creates)
	}
}

func TestSCIMCovPatchRefusals(t *testing.T) {
	st := newSCIMCovStore()
	pat := st.addIdentity(scimCovBound())
	e := newSCIMCovEnv(t, st)
	path := "/scim/v2/Users/" + pat.ID.String()
	for _, c := range []struct {
		name, path, body, scimType string
		status                     int
	}{
		{"an unknown id", "/scim/v2/Users/" + uuid.NewString(), scimCovPatch(scimCovReplace("active", "false")), "", http.StatusNotFound},
		{"an id that is not a UUID", "/scim/v2/Users/sub-pat", scimCovPatch(scimCovReplace("active", "false")), "", http.StatusNotFound},
		{"an envelope with no schema", path, `{"Operations":[{"op":"Replace","path":"active","value":false}]}`, scim.TypeInvalidSyntax, http.StatusBadRequest},
		{"an invalid operation", path, scimCovPatch(scimCovReplace("active", `"maybe"`)), scim.TypeInvalidValue, http.StatusBadRequest},
		{"a body over the cap", path, `{"x":"` + strings.Repeat("a", scimMaxBody) + `"}`, "", http.StatusRequestEntityTooLarge},
	} {
		t.Run(c.name, func(t *testing.T) {
			scimCovWantError(t, e.scim(http.MethodPatch, c.path, c.body), c.status, c.scimType)
		})
	}
	if len(st.updates) != 0 || len(st.plans) != 0 || len(e.h.audit.snapshot()) != 0 {
		t.Errorf("a refused PATCH wrote: updates %v plans %v audit %v", st.updates, st.plans, e.h.audit.snapshot())
	}
}

func TestSCIMCovPatchProjectsNamesAndAuditsOnlyAChange(t *testing.T) {
	st := newSCIMCovStore()
	pat := st.addIdentity(scimCovBound(), "pat@corp.example")
	e := newSCIMCovEnv(t, st)
	path := "/scim/v2/Users/" + pat.ID.String()

	w := e.scim(http.MethodPatch, path, scimCovPatch(
		scimCovReplace("userName", `"Pat.Renamed@corp.example"`), scimCovReplace("emails.value", `"pat.new@corp.example"`),
		`{"op":"Remove","path":"userName"}`))
	if got := scimCovDecode[scimCovUser](t, w); w.Code != http.StatusOK || got.UserName != "Pat.Renamed@corp.example" {
		t.Fatalf("PATCH = %d %s", w.Code, w.Body.String())
	}
	if len(st.updates) != 1 || st.updates[0].U.UserName != "Pat.Renamed@corp.example" || !slices.Equal(st.updates[0].U.Emails, []string{"pat.new@corp.example"}) || st.updates[0].U.Reactivate {
		t.Errorf("update = %+v, want the replaced names and no reactivation (a remove changes nothing)", st.updates)
	}
	if rows := e.auditRows("scim.user.write"); len(rows) != 1 || e.auditData(rows[0])["op"] != "projection" || rows[0].Target != pat.ID.String() {
		t.Errorf("rows = %+v, want one projection row for the identity", rows)
	}

	// The same projection again changes nothing, so nothing is audited.
	before := len(e.auditRows("scim.user.write"))
	e.scim(http.MethodPatch, path, scimCovPatch(scimCovReplace("userName", `"Pat.Renamed@corp.example"`), scimCovReplace("emails.value", `"pat.new@corp.example"`)))
	if after := len(e.auditRows("scim.user.write")); after != before {
		t.Errorf("an unchanged projection wrote %d new rows", after-before)
	}
	if len(st.plans) != 0 {
		t.Error("a projection suspended the user")
	}
}

func TestSCIMCovPatchActiveFalseSuspendsAndTrueReactivates(t *testing.T) {
	st := newSCIMCovStore()
	pat := st.addIdentity(scimCovBound())
	e := newSCIMCovEnv(t, st)
	path := "/scim/v2/Users/" + pat.ID.String()

	w := e.scim(http.MethodPatch, path, scimCovPatch(scimCovReplace("active", "false")))
	if got := scimCovDecode[scimCovUser](t, w); w.Code != http.StatusOK || got.Active {
		t.Fatalf("active=false = %d %s", w.Code, w.Body.String())
	}
	if len(st.plans) != 1 || !slices.Equal(st.plans[0].Principals, []string{"sub-pat", entraPrincipalPrefix + scimCovTenant + ":" + scimCovOID}) {
		t.Fatalf("plans = %+v, want one suspension of the principal and its object-id form", st.plans)
	}

	w = e.scim(http.MethodPatch, path, scimCovPatch(scimCovReplace("active", "true")))
	if got := scimCovDecode[scimCovUser](t, w); w.Code != http.StatusOK || !got.Active {
		t.Fatalf("active=true = %d %s", w.Code, w.Body.String())
	}
	last := st.updates[len(st.updates)-1]
	if !last.U.Reactivate || !slices.Equal(last.U.PeoplePrincipals, st.plans[0].Principals) {
		t.Errorf("reactivation update = %+v, want Reactivate with the bound principals", last.U)
	}
	rows := e.auditRows("scim.user.write")
	if len(rows) != 1 || e.auditData(rows[0])["op"] != "reactivate" {
		t.Errorf("rows = %+v, want one reactivate", rows)
	}
	if len(st.plans) != 1 {
		t.Errorf("a reactivation suspended again: %d plans", len(st.plans))
	}
}

func TestSCIMCovPatchExternalIDIsImmutable(t *testing.T) {
	other := "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"
	setup := func(t *testing.T) (*scimCovStore, *scimCovEnv, string) {
		st := newSCIMCovStore()
		pat := st.addIdentity(scimCovBound())
		return st, newSCIMCovEnv(t, st), "/scim/v2/Users/" + pat.ID.String()
	}

	t.Run("a change is refused and audited", func(t *testing.T) {
		st, e, path := setup(t)
		scimCovWantError(t, e.scim(http.MethodPatch, path, scimCovPatch(scimCovReplace("externalId", `"`+other+`"`))), http.StatusBadRequest, scim.TypeInvalidValue)
		rows := e.auditRows("scim.user.write")
		if len(rows) != 1 || rows[0].Outcome != "denied" || e.auditData(rows[0])["reason"] != "external_id_immutable" {
			t.Errorf("rows = %+v, want one denied external_id_immutable", rows)
		}
		if len(st.updates) != 0 || len(st.plans) != 0 {
			t.Errorf("a refused change wrote: updates %v plans %v", st.updates, st.plans)
		}
	})
	t.Run("removal outranks the refusal", func(t *testing.T) {
		st, e, path := setup(t)
		w := e.scim(http.MethodPatch, path, scimCovPatch(scimCovReplace("externalId", `"`+other+`"`), scimCovReplace("active", "false")))
		scimCovWantError(t, w, http.StatusBadRequest, scim.TypeInvalidValue)
		if len(st.plans) != 1 || len(st.updates) != 0 {
			t.Errorf("plans %v updates %v, want the suspension done and no projection written", st.plans, st.updates)
		}
	})
	t.Run("a suspension that cannot finish is a 5xx, not the 400", func(t *testing.T) {
		st, e, path := setup(t)
		st.failNext("SuspendIdentity", errSCIMCovBoom, -1)
		scimCovWantRetry(t, e.scim(http.MethodPatch, path, scimCovPatch(scimCovReplace("externalId", `"`+other+`"`), scimCovReplace("active", "false"))), "secret-dsn")
	})
	t.Run("the bound id in another case is allowed", func(t *testing.T) {
		st, e, path := setup(t)
		w := e.scim(http.MethodPatch, path, scimCovPatch(scimCovReplace("externalId", `"`+strings.ToUpper(scimCovOID)+`"`)))
		if w.Code != http.StatusOK || len(st.updates) != 1 || st.updates[0].U.ExternalID != scimCovOID {
			t.Errorf("PATCH = %d updates %+v, want the projection set to the bound id", w.Code, st.updates)
		}
	})
}

func TestSCIMCovExternalIDAllowed(t *testing.T) {
	for _, c := range []struct {
		name      string
		ident     store.PrincipalIdentity
		requested string
		want      bool
	}{
		{"the bound object id", store.PrincipalIdentity{ObjectID: scimCovOID}, " " + strings.ToUpper(scimCovOID) + " ", true},
		{"another GUID than the bound one", store.PrincipalIdentity{ObjectID: scimCovOID}, "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb", false},
		{"the projected id in another case", store.PrincipalIdentity{ScimExternalID: "EXT-1"}, "ext-1", true},
		{"another id than the projected one", store.PrincipalIdentity{ScimExternalID: "ext-1"}, "ext-2", false},
		{"an identity with neither", store.PrincipalIdentity{}, "anything", true},
		{"a bound identity and a non-GUID", store.PrincipalIdentity{ObjectID: scimCovOID}, "u1091", false},
	} {
		if got := externalIDAllowed(c.ident, c.requested); got != c.want {
			t.Errorf("%s: externalIDAllowed(%+v, %q) = %t, want %t", c.name, c.ident, c.requested, got, c.want)
		}
	}
}

func TestSCIMCovPatchRefusesReactivatingAPurgedIdentity(t *testing.T) {
	st := newSCIMCovStore()
	row := scimCovBound()
	gone := scimCovNow
	row.DeactivatedAt, row.PurgedAt = &gone, &gone
	pat := st.addIdentity(row)
	e := newSCIMCovEnv(t, st)

	w := e.scim(http.MethodPatch, "/scim/v2/Users/"+pat.ID.String(), scimCovPatch(scimCovReplace("active", "true")))
	scimCovWantError(t, w, http.StatusBadRequest, scim.TypeInvalidValue)
	if !strings.Contains(w.Body.String(), "purged") {
		t.Errorf("the refusal does not say the identity was purged: %s", w.Body.String())
	}
	rows := e.auditRows("scim.user.write")
	if len(rows) != 1 || rows[0].Outcome != "denied" || e.auditData(rows[0])["reason"] != "purged" {
		t.Errorf("rows = %+v, want one denied purged", rows)
	}
	if st.identity(pat.ID).DeactivatedAt == nil {
		t.Error("the tombstone was reactivated")
	}
}

func TestSCIMCovPatchAnswers500WhenAStoreStepFails(t *testing.T) {
	for _, c := range []struct {
		name, method string
		body         string
		noPrincipal  bool
	}{
		{"the alias read", "IdentityAliasValues", scimCovPatch(scimCovReplace("userName", `"x"`)), false},
		{"the update", "ApplyIdentityUpdate", scimCovPatch(scimCovReplace("userName", `"x"`)), false},
		{"the forms a reactivation clears", "PrincipalsByEmail", scimCovPatch(scimCovReplace("active", "true")), true},
	} {
		t.Run(c.name, func(t *testing.T) {
			st := newSCIMCovStore()
			row := scimCovBound()
			if c.noPrincipal {
				row.Principal = ""
			}
			pat := st.addIdentity(row, "pat@corp.example")
			st.failNext(c.method, errSCIMCovBoom, -1)
			e := newSCIMCovEnv(t, st)
			w := e.scim(http.MethodPatch, "/scim/v2/Users/"+pat.ID.String(), c.body)
			scimCovWantRetry(t, w, "secret-dsn")
			if rows := e.auditRows("scim.user.write"); len(rows) != 0 {
				t.Errorf("a failed write was audited as done: %+v", rows)
			}
			if c.method != "ApplyIdentityUpdate" && len(st.updates) != 0 {
				t.Errorf("a failed read was followed by a write: %+v", st.updates)
			}
		})
	}
}

func TestSCIMCovPutReplacesTheProjectionAndSuspends(t *testing.T) {
	st := newSCIMCovStore()
	row := scimCovBound()
	gone := scimCovNow
	row.DeactivatedAt = &gone
	pat := st.addIdentity(row)
	e := newSCIMCovEnv(t, st)
	path := "/scim/v2/Users/" + pat.ID.String()

	w := e.scim(http.MethodPut, path, scimCovUserBody(scimCovOID, "Pat.Put@corp.example", true, "pat@corp.example"))
	if got := scimCovDecode[scimCovUser](t, w); w.Code != http.StatusOK || !got.Active || got.UserName != "Pat.Put@corp.example" {
		t.Fatalf("PUT active=true = %d %s", w.Code, w.Body.String())
	}
	if len(st.updates) != 1 || !st.updates[0].U.Reactivate || st.updates[0].U.UserName != "Pat.Put@corp.example" || !slices.Equal(st.updates[0].U.Emails, []string{"pat@corp.example"}) {
		t.Errorf("update = %+v, want the full replacement with a reactivation", st.updates)
	}

	w = e.scim(http.MethodPut, path, scimCovUserBody(scimCovOID, "Pat.Put@corp.example", false))
	if got := scimCovDecode[scimCovUser](t, w); w.Code != http.StatusOK || got.Active || len(st.plans) != 1 {
		t.Errorf("PUT active=false = %d %s plans %d, want a suspension", w.Code, w.Body.String(), len(st.plans))
	}

	updates, plans := len(st.updates), len(st.plans)
	big := `{"userName":"` + strings.Repeat("a", scimMaxBody) + `"}`
	for _, c := range []struct {
		name, path, body, scimType string
		status                     int
	}{
		{"no userName", path, `{"externalId":"` + scimCovOID + `"}`, scim.TypeInvalidValue, http.StatusBadRequest},
		{"another externalId", path, scimCovUserBody("bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb", "x", true), scim.TypeInvalidValue, http.StatusBadRequest},
		{"a body that is not JSON", path, `[`, scim.TypeInvalidSyntax, http.StatusBadRequest},
		{"an unknown id", "/scim/v2/Users/" + uuid.NewString(), scimCovUserBody(scimCovOID, "x", true), "", http.StatusNotFound},
		{"a body over the cap", path, big, "", http.StatusRequestEntityTooLarge},
	} {
		t.Run(c.name, func(t *testing.T) {
			scimCovWantError(t, e.scim(http.MethodPut, c.path, c.body), c.status, c.scimType)
		})
	}
	if len(st.updates) != updates || len(st.plans) != plans {
		t.Errorf("a refused PUT wrote: updates %d->%d plans %d->%d", updates, len(st.updates), plans, len(st.plans))
	}
}

func TestSCIMCovNormaliseHelpers(t *testing.T) {
	if got, ok := normalizeExternalID("  " + strings.ToUpper(scimCovOID) + " "); !ok || got != scimCovOID {
		t.Errorf("normalizeExternalID = %q %t, want the lower-case GUID", got, ok)
	}
	if _, ok := normalizeExternalID("u1091"); ok {
		t.Error("a non-GUID was accepted as an object id")
	}
	if got := scimNames(" Pat@Corp.example ", []string{"pat@corp.example", "", "Other@corp.example"}); !slices.Equal(got, []string{"pat@corp.example", "other@corp.example"}) {
		t.Errorf("scimNames = %v, want the lower-cased distinct names", got)
	}
	if got := emailValues([]scim.Email{{Value: "a@x"}, {Value: "b@x"}}); !slices.Equal(got, []string{"a@x", "b@x"}) {
		t.Errorf("emailValues = %v", got)
	}
}
