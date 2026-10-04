// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/scim"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// The leaver tests: SCIM suspends and reactivates a person, durably and across instances. Everything here
// is Postgres-backed (WARDYN_TEST_PG), with two Server instances over one database where a race is the point.

const (
	oidPat    = "aaaaaaaa-0000-4000-8000-0000000000a1"
	oidLeaver = "aaaaaaaa-0000-4000-8000-0000000000a2"
	oidGhost  = "aaaaaaaa-0000-4000-8000-0000000000a3"
	oidOther  = "aaaaaaaa-0000-4000-8000-0000000000a4"
)

func filterPath(attr, value string) string {
	return "/scim/v2/Users?filter=" + url.QueryEscape(attr+` eq "`+value+`"`)
}

// listIDs returns the ids of the users a filter selects.
func (e *scimEnv) listIDs(n *scimNode, attr, value string) []string {
	e.t.Helper()
	w := e.scim(n, http.MethodGet, filterPath(attr, value), "")
	if w.Code != http.StatusOK {
		e.t.Fatalf("filter %s eq %q = %d: %s", attr, value, w.Code, w.Body.String())
	}
	var list struct {
		TotalResults int        `json:"totalResults"`
		Resources    []scimUser `json:"Resources"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &list); err != nil || list.TotalResults != len(list.Resources) {
		e.t.Fatalf("list %s: %v", w.Body.String(), err)
	}
	ids := make([]string, 0, len(list.Resources))
	for _, u := range list.Resources {
		ids = append(ids, u.ID)
	}
	return ids
}

func (e *scimEnv) idByEmail(n *scimNode, email string) string {
	e.t.Helper()
	ids := e.listIDs(n, "emails.value", email)
	if len(ids) != 1 {
		e.t.Fatalf("emails.value eq %q matched %v, want exactly one identity", email, ids)
	}
	return ids[0]
}

// suspendOn suspends the identity behind email through n's SCIM routes and requires 200.
func (e *scimEnv) suspendOn(n *scimNode, email string) {
	e.t.Helper()
	if w := e.patch(n, e.idByEmail(n, email), patchOf("false")); w.Code != http.StatusOK {
		e.t.Fatalf("suspend %s: %d %s", email, w.Code, w.Body.String())
	}
}

func (e *scimEnv) allJobsDone(id string) bool {
	e.t.Helper()
	jobs, err := e.st.ListDeprovisionJobs(context.Background(), uuid.MustParse(id), store.JobKindSuspend)
	if err != nil || len(jobs) == 0 {
		e.t.Fatalf("ledger for %s: %v, %v", id, jobs, err)
	}
	return !slices.ContainsFunc(jobs, func(j store.DeprovisionJob) bool { return !j.Done })
}

func (e *scimEnv) aliases(id string) []string {
	e.t.Helper()
	v, err := e.st.IdentityAliasValues(context.Background(), uuid.MustParse(id))
	if err != nil {
		e.t.Fatal(err)
	}
	return v
}

// The routes: filter rules, the 100-resource cap with startIndex and count ignored, ids that are never
// principals, the body cap, and what a POST records.
func TestSCIMUsersRoutes(t *testing.T) {
	e := newSCIMEnv(t)
	n := e.a

	for name, path := range map[string]string{
		"no filter":              "/scim/v2/Users",
		"an empty filter":        "/scim/v2/Users?filter=",
		"an unsupported attr":    "/scim/v2/Users?filter=" + url.QueryEscape(`displayName eq "x"`),
		"an unsupported op":      "/scim/v2/Users?filter=" + url.QueryEscape(`userName co "x"`),
		"a compound filter":      "/scim/v2/Users?filter=" + url.QueryEscape(`userName eq "x" and externalId eq "y"`),
		"an unquoted value":      "/scim/v2/Users?filter=" + url.QueryEscape(`userName eq x`),
		"paging does not excuse": "/scim/v2/Users?startIndex=1&count=10",
	} {
		w := e.scim(n, http.MethodGet, path, "")
		if status, typ := scimErrorOf(t, w); w.Code != http.StatusBadRequest || status != "400" || typ != "invalidFilter" {
			t.Errorf("%s: %d %s (%s), want 400 invalidFilter", name, w.Code, typ, w.Body.String())
		}
	}

	if _, code := e.postUser(n, "not-a-guid", "pat@corp.example", "", true); code != http.StatusBadRequest {
		t.Errorf("POST with a non-GUID externalId = %d, want 400", code)
	}
	w := e.scim(n, http.MethodPost, "/scim/v2/Users", `{"schemas":["urn:ietf:params:scim:schemas:core:2.0:User"],"userName":"x"}`)
	if status, typ := scimErrorOf(t, w); w.Code != http.StatusBadRequest || typ != "invalidValue" || status != "400" {
		t.Errorf("POST with no externalId = %d %s, want 400 invalidValue", w.Code, w.Body.String())
	}

	created, code := e.postUser(n, strings.ToUpper(oidPat), "Pat.Jones@corp.example", "Pat.Jones@corp.example", true)
	if code != http.StatusCreated || created.ID == "" || !created.Active || created.ExternalID != oidPat || created.UserName != "Pat.Jones@corp.example" {
		t.Fatalf("POST = %d %+v", code, created)
	}
	row := e.identity(created.ID)
	if row.Principal != "" || row.ObjectID != oidPat || row.TenantID != e.tenant || row.Issuer != e.issuer || row.DeactivatedAt != nil {
		t.Errorf("a created user is an unbound identity keyed by its object id: %+v", row)
	}
	if again, code := e.postUser(n, oidPat, "Pat.Jones@corp.example", "", true); code != http.StatusCreated || again.ID != created.ID {
		t.Errorf("a second POST of the same externalId = %d id %s, want the same identity %s", code, again.ID, created.ID)
	}
	if got := e.rows(n, "scim.user.write"); len(got) != 2 || dataOf(t, got[0])["op"] != "create" || dataOf(t, got[1])["op"] != "link" || dataOf(t, got[0])["slot"] != scimSlotPrimary {
		t.Errorf("scim.user.write rows = %+v, want create then link, naming the slot", got)
	}
	for _, ev := range e.rows(n, "scim.user.write") {
		if ev.Actor != scimActor || ev.Target != created.ID || ev.Outcome != "success" {
			t.Errorf("row %+v", ev)
		}
	}

	if w := e.scim(n, http.MethodGet, "/scim/v2/Users/"+created.ID, ""); w.Code != http.StatusOK || decodeSCIMUser(t, w).ID != created.ID {
		t.Errorf("GET by id = %d %s", w.Code, w.Body.String())
	} else if w.Header().Get("Content-Type") != scim.MediaType {
		t.Errorf("content type = %q", w.Header().Get("Content-Type"))
	}
	for _, id := range []string{"not-a-uuid", uuid.NewString(), "sub-pat", "admin-token"} {
		if w := e.scim(n, http.MethodGet, "/scim/v2/Users/"+id, ""); w.Code != http.StatusNotFound {
			t.Errorf("GET /Users/%s = %d, want 404 (an id is never a principal)", id, w.Code)
		}
	}
	for _, f := range [][2]string{{"externalId", oidPat}, {"externalId", strings.ToUpper(oidPat)}, {"userName", "pat.jones@CORP.example"}, {"emails.value", "PAT.JONES@corp.example"}} {
		if ids := e.listIDs(n, f[0], f[1]); !slices.Equal(ids, []string{created.ID}) {
			t.Errorf("filter %s eq %q = %v, want [%s]", f[0], f[1], ids, created.ID)
		}
	}
	if ids := e.listIDs(n, "externalId", oidOther); len(ids) != 0 {
		t.Errorf("a filter that matches nothing returned %v", ids)
	}

	// startIndex and count are ignored: two identities under one email are both returned.
	e.seedSignIn("sub-dup-1", "dup@corp.example")
	e.seedSignIn("sub-dup-2", "dup@corp.example")
	w = e.scim(n, http.MethodGet, filterPath("emails.value", "dup@corp.example")+"&startIndex=2&count=1", "")
	var list struct {
		TotalResults int `json:"totalResults"`
		StartIndex   int `json:"startIndex"`
		Resources    []scimUser
	}
	if err := json.Unmarshal(w.Body.Bytes(), &list); err != nil || list.TotalResults != 2 || len(list.Resources) != 2 || list.StartIndex != 1 {
		t.Errorf("paging parameters were honoured: %s", w.Body.String())
	}

	huge := `{"schemas":["urn:ietf:params:scim:schemas:core:2.0:User"],"externalId":"` + oidOther + `","userName":"` + strings.Repeat("a", scimMaxBody) + `"}`
	if w := e.scim(n, http.MethodPost, "/scim/v2/Users", huge); w.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("a body over 64 KiB = %d, want 413", w.Code)
	}
}

func scimFixture(t *testing.T, name string) string {
	t.Helper()
	raw, err := os.ReadFile("../scim/testdata/entra/" + name)
	if err != nil {
		t.Fatal(err)
	}
	var f struct {
		Body json.RawMessage `json:"body"`
	}
	if err := json.Unmarshal(raw, &f); err != nil || len(f.Body) == 0 {
		t.Fatalf("fixture %s: %v", name, err)
	}
	return string(f.Body)
}

// Every recorded user payload, replayed through the handlers, has its expected effect: the POST creates,
// each PATCH shape does what the shape says (an active of false as a bool or as Entra's string, a lower-case
// op, a rename, an email by filter path, a path-less replace, an attribute Wardyn ignores), the multi-
// attribute replace that renames the externalId is refused whole, and PUT replaces.
func TestSCIMReplaysRecordedPayloads(t *testing.T) {
	e := newSCIMEnv(t)
	n := e.a

	post := scimFixture(t, "entra-doc-post-user.json")
	w := e.scim(n, http.MethodPost, "/scim/v2/Users", post)
	var posted struct {
		ExternalID string `json:"externalId"`
	}
	_ = json.Unmarshal([]byte(post), &posted)
	if got := decodeSCIMUser(t, w); w.Code != http.StatusCreated || got.ExternalID != posted.ExternalID || !got.Active ||
		got.UserName != "Test_User_00aa00aa-bb11-cc22-dd33-44ee44ee44ee" {
		t.Fatalf("the recorded POST /Users = %d %s", w.Code, w.Body.String())
	}
	if ids := e.listIDs(n, "emails.value", "Test_User_11bb11bb-cc22-dd33-ee44-55ff55ff55ff@testuser.com"); len(ids) != 1 {
		t.Errorf("the recorded POST's email was not recorded: %v", ids)
	}

	fresh := func(oid string) string {
		u, code := e.postUser(n, oid, "orig@corp.example", "orig@corp.example", true)
		if code != http.StatusCreated {
			t.Fatalf("setup POST = %d", code)
		}
		return u.ID
	}
	deactivated := func(t *testing.T, id string) {
		t.Helper()
		if e.identity(id).DeactivatedAt == nil || !e.allJobsDone(id) {
			t.Errorf("identity %s is not suspended with a finished ledger", id)
		}
	}
	for i, c := range []struct {
		fixture string
		check   func(t *testing.T, id string, w int)
	}{
		{"entra-doc-patch-user-active-false-bool.json", func(t *testing.T, id string, _ int) { deactivated(t, id) }},
		{"entra-doc-patch-user-active-string-false.json", func(t *testing.T, id string, _ int) { deactivated(t, id) }},
		{"entra-doc-patch-user-active-bool-lowercase-op.json", func(t *testing.T, id string, _ int) { deactivated(t, id) }},
		{"entra-doc-patch-user-username.json", func(t *testing.T, id string, _ int) {
			if got := e.identity(id).ScimUserName; got != "5b50642d-79fc-4410-9e90-4c077cdd1a59@testuser.com" {
				t.Errorf("userName = %q", got)
			}
			if a := e.aliases(id); !slices.Contains(a, "orig@corp.example") || !slices.Contains(a, "5b50642d-79fc-4410-9e90-4c077cdd1a59@testuser.com") {
				t.Errorf("aliases = %v, want the old and the new userName", a)
			}
		}},
		{"entra-doc-patch-user-email-name.json", func(t *testing.T, id string, _ int) {
			if a := e.aliases(id); !slices.Contains(a, "updatedemail@microsoft.com") || !slices.Contains(a, "orig@corp.example") {
				t.Errorf("aliases = %v, want the new email and the old", a)
			}
		}},
		{"entra-doc-patch-user-pathless-replace.json", func(t *testing.T, id string, _ int) {
			if a := e.aliases(id); !slices.Contains(a, "testmhvaes@test.microsoft.com") {
				t.Errorf("aliases = %v, want the path-less replace's email", a)
			}
		}},
		{"entra-doc-patch-user-add-nickname.json", func(t *testing.T, id string, _ int) {
			if a := e.aliases(id); !slices.Equal(a, []string{"orig@corp.example"}) {
				t.Errorf("an attribute Wardyn ignores changed the aliases: %v", a)
			}
			for _, ev := range e.rows(n, "scim.user.write") {
				if ev.Target == id && dataOf(t, ev)["op"] == "projection" {
					t.Errorf("a PATCH that changed nothing wrote a projection row: %s", ev.Data)
				}
			}
		}},
	} {
		t.Run(strings.TrimSuffix(c.fixture, ".json"), func(t *testing.T) {
			id := fresh(strings.Replace(oidPat, "a1", "b"+string(rune('0'+i)), 1))
			w := e.patch(n, id, scimFixture(t, c.fixture))
			if w.Code != http.StatusOK || decodeSCIMUser(t, w).ID != id {
				t.Fatalf("PATCH = %d %s, want 200", w.Code, w.Body.String())
			}
			c.check(t, id, w.Code)
		})
	}

	t.Run("a multi-attribute replace that renames the externalId is refused whole", func(t *testing.T) {
		id := fresh(oidLeaver)
		before := e.aliases(id)
		w := e.patch(n, id, scimFixture(t, "entra-doc-patch-user-multi-replace.json"))
		if status, typ := scimErrorOf(t, w); w.Code != http.StatusBadRequest || typ != "invalidValue" || status != "400" {
			t.Fatalf("PATCH = %d %s, want 400 invalidValue", w.Code, w.Body.String())
		}
		if after := e.aliases(id); !slices.Equal(before, after) || e.identity(id).DeactivatedAt != nil {
			t.Errorf("a refused PATCH changed the identity (aliases %v -> %v)", before, after)
		}
	})

	t.Run("a PUT replaces userName and email and keeps the old ones as aliases", func(t *testing.T) {
		id := fresh(oidGhost)
		var body map[string]any
		if err := json.Unmarshal([]byte(scimFixture(t, "rfc-put-user.json")), &body); err != nil {
			t.Fatal(err)
		}
		body["externalId"] = oidGhost
		w := e.scim(n, http.MethodPut, "/scim/v2/Users/"+id, string(mustJSON(body)))
		if w.Code != http.StatusOK {
			t.Fatalf("PUT = %d %s", w.Code, w.Body.String())
		}
		if a := e.aliases(id); !slices.Contains(a, "orig@corp.example") || !slices.Contains(a, "bjensen") ||
			!slices.Contains(a, "bjensen@example.com") || !slices.Contains(a, "babs@jensen.org") {
			t.Errorf("aliases = %v", a)
		}
	})

	t.Run("the recorded list response has the shape Wardyn answers with", func(t *testing.T) {
		var want, got map[string]json.RawMessage
		_ = json.Unmarshal([]byte(scimFixture(t, "entra-doc-response-list-users.json")), &want)
		w := e.scim(n, http.MethodGet, filterPath("externalId", posted.ExternalID), "")
		_ = json.Unmarshal(w.Body.Bytes(), &got)
		for k := range want {
			if _, ok := got[k]; !ok {
				t.Errorf("the list response has no %q, which the recorded one does", k)
			}
		}
	})
}

// A rename PATCH, then deactivation addressed by the new userName, suspends the identity and keeps the old
// name and email as aliases; so does a PUT full replacement carrying the new userName and email.
func TestSCIMRenameThenDeactivate(t *testing.T) {
	const old, renamed = "pat.old@corp.example", "pat.new@corp.example"
	for _, via := range []string{"patch", "put"} {
		t.Run(via, func(t *testing.T) {
			e := newSCIMEnv(t)
			row := e.seedEntra("sub-pat", old, oidPat)
			_, tokenRaw := e.seedToken("sub-pat", old)
			id := e.postUserID(e.a, oidPat, old, old)
			if id != row.ID.String() {
				t.Fatalf("the POST linked identity %s, want the signed-in row %s", id, row.ID)
			}
			switch via {
			case "patch":
				rename := `{"schemas":["urn:ietf:params:scim:api:messages:2.0:PatchOp"],"Operations":[` +
					`{"op":"Replace","path":"userName","value":"` + renamed + `"},` +
					`{"op":"Replace","path":"emails[type eq \"work\"].value","value":"` + renamed + `"}]}`
				if w := e.patch(e.a, id, rename); w.Code != http.StatusOK {
					t.Fatalf("rename = %d %s", w.Code, w.Body.String())
				}
			case "put":
				var body map[string]any
				_ = json.Unmarshal([]byte(scimFixture(t, "rfc-put-user.json")), &body)
				body["externalId"], body["userName"] = oidPat, renamed
				body["emails"] = []map[string]any{{"value": renamed, "primary": true}}
				if w := e.scim(e.a, http.MethodPut, "/scim/v2/Users/"+id, string(mustJSON(body))); w.Code != http.StatusOK {
					t.Fatalf("PUT = %d %s", w.Code, w.Body.String())
				}
			}
			ids := e.listIDs(e.b, "userName", renamed)
			if !slices.Equal(ids, []string{id}) {
				t.Fatalf("filter userName eq %q = %v, want [%s]", renamed, ids, id)
			}
			if w := e.patch(e.b, ids[0], patchOf("false")); w.Code != http.StatusOK || decodeSCIMUser(t, w).Active {
				t.Fatalf("deactivate = %d %s", w.Code, w.Body.String())
			}
			if e.identity(id).DeactivatedAt == nil || !e.allJobsDone(id) {
				t.Error("the identity is not suspended")
			}
			if a := e.aliases(id); !slices.Contains(a, old) || !slices.Contains(a, renamed) {
				t.Errorf("aliases = %v, want the old and the new name", a)
			}
			if e.tokenWorks(e.a, tokenRaw) {
				t.Error("the person's token still works on the other instance")
			}
			for _, name := range []string{old, renamed} {
				if ids := e.listIDs(e.a, "userName", name); !slices.Equal(ids, []string{id}) {
					t.Errorf("userName eq %q = %v: an alias must still find the identity", name, ids)
				}
			}
		})
	}
}

// postUserID is postUser for a setup step that must succeed, returning the id.
func (e *scimEnv) postUserID(n *scimNode, externalID, userName, email string) string {
	e.t.Helper()
	u, code := e.postUser(n, externalID, userName, email, true)
	if code != http.StatusCreated {
		e.t.Fatalf("POST /Users = %d", code)
	}
	return u.ID
}

// An externalId change on a bound identity is a 400 invalidValue with a denied scim.user.write row, by PATCH
// and by PUT, and applies nothing; beside active=false in the same request the suspension still lands, and
// the response is still the 400.
func TestSCIMExternalIDIsImmutable(t *testing.T) {
	e := newSCIMEnv(t)
	n := e.a
	const email = "bound@corp.example"
	e.seedEntra("sub-bound", email, oidPat)
	_, tokenRaw := e.seedToken("sub-bound", email)
	id := e.postUserID(n, oidPat, email, email)
	rename := func(extra string) string {
		return `{"schemas":["urn:ietf:params:scim:api:messages:2.0:PatchOp"],"Operations":[` +
			`{"op":"Replace","path":"externalId","value":"` + oidOther + `"},` +
			`{"op":"Replace","path":"userName","value":"changed@corp.example"}` + extra + `]}`
	}
	denied := func() []types.AuditEvent {
		var out []types.AuditEvent
		for _, ev := range e.rows(n, "scim.user.write") {
			if ev.Outcome == "denied" {
				out = append(out, ev)
			}
		}
		return out
	}

	w := e.patch(n, id, rename(""))
	if status, typ := scimErrorOf(t, w); w.Code != http.StatusBadRequest || typ != "invalidValue" || status != "400" {
		t.Fatalf("PATCH = %d %s", w.Code, w.Body.String())
	}
	if d := denied(); len(d) != 1 || dataOf(t, d[0])["reason"] != "external_id_immutable" || d[0].Target != id {
		t.Errorf("denied rows = %+v, want one external_id_immutable", d)
	}
	if got := e.identity(id); got.ScimUserName == "changed@corp.example" || got.DeactivatedAt != nil || got.ObjectID != oidPat {
		t.Errorf("a refused PATCH applied something: %+v", got)
	}

	var put map[string]any
	_ = json.Unmarshal([]byte(scimFixture(t, "rfc-put-user.json")), &put)
	put["externalId"], put["userName"] = oidOther, "changed@corp.example"
	if w := e.scim(n, http.MethodPut, "/scim/v2/Users/"+id, string(mustJSON(put))); w.Code != http.StatusBadRequest {
		t.Fatalf("PUT = %d %s", w.Code, w.Body.String())
	}
	if len(denied()) != 2 || e.identity(id).DeactivatedAt != nil {
		t.Errorf("PUT: denied %d rows, deactivated %v", len(denied()), e.identity(id).DeactivatedAt != nil)
	}

	// With active=false beside it, the suspension still lands and the response is the 400.
	w = e.patch(n, id, rename(`,{"op":"Replace","path":"active","value":false}`))
	if status, typ := scimErrorOf(t, w); w.Code != http.StatusBadRequest || typ != "invalidValue" || status != "400" {
		t.Fatalf("PATCH with active=false = %d %s", w.Code, w.Body.String())
	}
	got := e.identity(id)
	if got.DeactivatedAt == nil || !e.allJobsDone(id) || got.ScimUserName == "changed@corp.example" {
		t.Errorf("the suspension did not land alone: %+v", got)
	}
	if !e.tokenRevoked(mustTokenID(t, e, "sub-bound")) || e.tokenWorks(e.b, tokenRaw) {
		t.Error("the person's token still works")
	}
	if len(denied()) != 3 {
		t.Errorf("denied rows = %d, want 3", len(denied()))
	}

	// The same through PUT.
	e2 := newSCIMEnv(t)
	e2.seedEntra("sub-bound2", email, oidPat)
	id2 := e2.postUserID(e2.a, oidPat, email, email)
	put["externalId"], put["active"] = oidOther, false
	if w := e2.scim(e2.a, http.MethodPut, "/scim/v2/Users/"+id2, string(mustJSON(put))); w.Code != http.StatusBadRequest {
		t.Fatalf("PUT with active=false = %d %s", w.Code, w.Body.String())
	}
	if e2.identity(id2).DeactivatedAt == nil {
		t.Error("PUT: the suspension beside a refused externalId did not land")
	}
}

func mustTokenID(t *testing.T, e *scimEnv, principal string) uuid.UUID {
	t.Helper()
	toks, err := e.st.ListAPITokensByPrincipal(context.Background(), principal)
	if err != nil || len(toks) != 1 {
		t.Fatalf("tokens of %s = %v, %v", principal, toks, err)
	}
	return toks[0].ID
}

// A suspension reaches the person under every form they are known by, and only them.
func TestSCIMSuspendReachesEveryForm(t *testing.T) {
	e := newSCIMEnv(t)
	const email = "leaver@corp.example"
	entraForm := entraPrincipalPrefix + e.tenant + ":" + oidLeaver
	e.seedEntra("sub-leaver", email, oidLeaver)
	id := e.postUserID(e.a, oidLeaver, email, email)

	tokSub, rawSub := e.seedToken("sub-leaver", email)
	tokEmail, rawEmail := e.seedToken(email, email)
	tokEntra, rawEntra := e.seedToken(entraForm, "other@corp.example")
	keys := map[string]string{"sub": "sub-leaver", "email": email, "entra": entraForm}
	for _, p := range keys {
		e.seedKey(p)
	}
	runSub, runEntra := e.seedRun("sub-leaver", types.RunRunning), e.seedRun(entraForm, types.RunPending)
	cookies := map[string]*http.Cookie{
		"sub":   scimCookie(t, "sub-leaver", email, 0),
		"entra": scimCookie(t, entraForm, "x@corp.example", 0),
		"email": scimCookie(t, "idp-other-sub", "LEAVER@Corp.Example", 0),
	}

	_, rawBy := e.seedToken("sub-bystander", "bystander@corp.example")
	e.seedSignIn("sub-bystander", "bystander@corp.example")
	e.seedKey("sub-bystander")
	runBy := e.seedRun("sub-bystander", types.RunRunning)
	cookieBy := scimCookie(t, "sub-bystander", "bystander@corp.example", 0)

	for name, c := range cookies {
		if !e.cookieWorks(e.a, c) {
			t.Fatalf("control: the %s cookie does not work before the suspension", name)
		}
	}
	for _, raw := range []string{rawSub, rawEmail, rawEntra, rawBy} {
		if !e.tokenWorks(e.a, raw) {
			t.Fatal("control: a token does not work before the suspension")
		}
	}

	// The suspension lands on the OTHER instance than the one the credentials are checked on.
	if w := e.patch(e.b, id, patchOf(`"False"`)); w.Code != http.StatusOK || decodeSCIMUser(t, w).Active {
		t.Fatalf("suspend = %d %s", w.Code, w.Body.String())
	}

	for name, c := range cookies {
		if e.cookieWorks(e.a, c) || e.cookieWorks(e.b, c) {
			t.Errorf("the %s-form session still works", name)
		}
	}
	for name, raw := range map[string]string{"sub": rawSub, "email": rawEmail, "entra": rawEntra} {
		if e.tokenWorks(e.a, raw) || e.tokenWorks(e.b, raw) {
			t.Errorf("the %s-form token still works", name)
		}
	}
	for _, tok := range []uuid.UUID{tokSub, tokEmail, tokEntra} {
		if !e.tokenRevoked(tok) {
			t.Errorf("token %s was not revoked", tok)
		}
	}
	for name, p := range keys {
		if e.keyCount(p) != 0 {
			t.Errorf("the %s-form SSH key survived", name)
		}
	}
	for _, r := range []uuid.UUID{runSub, runEntra} {
		if e.runState(r) != types.RunKilled {
			t.Errorf("run %s is %s, want KILLED", r, e.runState(r))
		}
	}
	if e.runner(e.b).killCount() != 2 {
		t.Errorf("the runner was told to kill %d sandboxes, want 2", e.runner(e.b).killCount())
	}
	// Nobody else is touched.
	if !e.tokenWorks(e.a, rawBy) || !e.cookieWorks(e.a, cookieBy) || e.keyCount("sub-bystander") != 1 || e.runState(runBy) != types.RunRunning {
		t.Error("a bystander's token, session, key or run was touched")
	}

	if !e.allJobsDone(id) {
		t.Error("the ledger has pending steps")
	}
	dea, dep := e.rows(e.b, "scim.user.deactivate"), e.rows(e.b, "person.deprovision")
	if len(dea) != 1 || len(dep) != 1 || dea[0].Target != id {
		t.Fatalf("audit rows: %d deactivate, %d deprovision", len(dea), len(dep))
	}
	d := dataOf(t, dep[0])
	if d["tokens_revoked"] != float64(3) || d["keys_deleted"] != float64(3) || d["runs_killed"] != float64(2) || d["sessions_cut"] != float64(3) || d["kind"] != "suspend" {
		t.Errorf("person.deprovision counts = %v", d)
	}

	// An identity provider that repeats the request costs nothing and writes nothing.
	if w := e.patch(e.b, id, patchOf("false")); w.Code != http.StatusOK {
		t.Fatalf("repeat = %d", w.Code)
	}
	if len(e.rows(e.b, "scim.user.deactivate")) != 1 || len(e.rows(e.b, "person.deprovision")) != 1 {
		t.Error("a repeated suspension wrote its audit rows again")
	}
}

func (e *scimEnv) runner(n *scimNode) *scimRunner { return n.runner }

// Suspending someone who has never signed in deactivates only their own row: another person's token, SSH
// key, run and session keep working, and no global cutoff is written.
func TestSCIMSuspendNeverSignedInLeavesOthersAlone(t *testing.T) {
	e := newSCIMEnv(t)
	e.seedSignIn("sub-bystander", "bystander@corp.example")
	_, rawBy := e.seedToken("sub-bystander", "bystander@corp.example")
	e.seedKey("sub-bystander")
	runBy := e.seedRun("sub-bystander", types.RunRunning)
	cookieBy := scimCookie(t, "sub-bystander", "bystander@corp.example", 0)

	ghost, code := e.postUser(e.a, oidGhost, "ghost@corp.example", "ghost@corp.example", false)
	if code != http.StatusCreated || ghost.Active {
		t.Fatalf("POST active=false = %d %+v, want 201 and a suspended user", code, ghost)
	}
	row := e.identity(ghost.ID)
	if row.Principal != "" || row.DeactivatedAt == nil || !e.allJobsDone(ghost.ID) {
		t.Fatalf("ghost row = %+v", row)
	}
	if !e.tokenWorks(e.b, rawBy) || !e.cookieWorks(e.b, cookieBy) || e.keyCount("sub-bystander") != 1 || e.runState(runBy) != types.RunRunning {
		t.Error("a bystander lost a credential to someone else's suspension")
	}
	var global int
	if err := e.pool.QueryRow(context.Background(), `SELECT count(*) FROM oidc_session_revocations WHERE sub = ''`).Scan(&global); err != nil || global != 0 {
		t.Errorf("a global session cutoff was written (%d, %v)", global, err)
	}
	// Their first sign-in, whenever it comes, is refused.
	_, err := e.st.IssueLoginIdentity(context.Background(), store.LoginIdentity{Principal: "sub-ghost", Issuer: e.issuer, TenantID: e.tenant, ObjectID: oidGhost}, time.Now().UTC())
	if !errors.Is(err, store.ErrIdentityDeactivated) {
		t.Errorf("first sign-in of a suspended identity = %v, want ErrIdentityDeactivated", err)
	}
}

// With no identity row binding a principal, the suspension resolves the SCIM user's emails to subs through
// the tokens stamped with them: that person's runs are killed, keys deleted, token revoked. An ambiguous
// owner leaves the step pending and answers 5xx, never done.
func TestSCIMSuspendResolvesEmailsToSubs(t *testing.T) {
	t.Run("a person known only by a token stamped with their email", func(t *testing.T) {
		e := newSCIMEnv(t)
		tok, raw := e.seedToken("sub-x", "x@corp.example")
		e.seedKey("sub-x")
		run := e.seedRun("sub-x", types.RunRunning)
		cookie := scimCookie(t, "sub-x", "x@corp.example", 0)
		_, rawBy := e.seedToken("sub-y", "y@corp.example")
		runBy := e.seedRun("sub-y", types.RunRunning)

		u, code := e.postUser(e.a, oidGhost, "x@corp.example", "x@corp.example", false)
		if code != http.StatusCreated || u.Active {
			t.Fatalf("POST active=false = %d", code)
		}
		if !e.tokenRevoked(tok) || e.tokenWorks(e.b, raw) || e.cookieWorks(e.b, cookie) {
			t.Error("the person's token or session survived")
		}
		if e.keyCount("sub-x") != 0 || e.runState(run) != types.RunKilled {
			t.Error("the person's key or run survived")
		}
		if !e.tokenWorks(e.b, rawBy) || e.runState(runBy) != types.RunRunning {
			t.Error("another person's credentials were touched")
		}
	})

	t.Run("an ambiguous owner leaves the step pending", func(t *testing.T) {
		e := newSCIMEnv(t)
		e.seedToken("sub-t1", "twin@corp.example")
		e.seedToken("sub-t2", "twin@corp.example")
		for attempt := 0; attempt < 2; attempt++ {
			if _, code := e.postUser(e.a, oidGhost, "twin@corp.example", "twin@corp.example", false); code < 500 {
				t.Fatalf("attempt %d answered %d, want 5xx while an owner is ambiguous", attempt+1, code)
			}
		}
		id := e.listIDs(e.a, "externalId", oidGhost)[0]
		if e.identity(id).DeactivatedAt == nil {
			t.Error("step 1 did not commit")
		}
		jobs, _ := e.st.ListDeprovisionJobs(context.Background(), uuid.MustParse(id), store.JobKindSuspend)
		pending := ""
		for _, j := range jobs {
			if !j.Done && j.Step == jobStepSweep {
				pending = j.Target
			}
		}
		if pending != "twin@corp.example" || e.allJobsDone(id) {
			t.Errorf("the ambiguous email's sweep must stay pending, pending target %q", pending)
		}
		if len(e.rows(e.a, "scim.user.deactivate")) != 0 {
			t.Error("a suspension that is not done was recorded as done")
		}
	})
}

// A credential minted by a session admitted before the suspension, whose write lands after step 1, is
// refused at its insert, and stays refused after the reactivation: no token, key or run results.
func TestSCIMMintDuringSuspensionIsRefused(t *testing.T) {
	const sub, email = "sub-m", "m@corp.example"
	e := newSCIMEnv(t)
	e.seedSignIn(sub, email)
	cookie := scimCookie(t, sub, email, 0)

	race := func() { e.suspendOn(e.b, email) }
	e.rev.setLateCheck(race)
	w := doSSO(t, e.a.srv, http.MethodPost, "/api/v1/me/tokens", cookie, `{"name":"racer"}`)
	if w.Code != http.StatusForbidden || errorReason(w) != reasonIdentityDeactivated {
		t.Fatalf("the late mint = %d %s, want 403 %s", w.Code, w.Body.String(), reasonIdentityDeactivated)
	}
	if toks, _ := e.st.ListAPITokensByPrincipal(context.Background(), sub); len(toks) != 0 {
		t.Fatalf("a token was minted for a suspended person: %+v", toks)
	}

	// Reactivate and sign the person in again: the old session is refused, a new one at the new epoch is not.
	id := e.idByEmail(e.b, email)
	if w := e.patch(e.b, id, patchOf("true")); w.Code != http.StatusOK {
		t.Fatalf("reactivate = %d", w.Code)
	}
	if toks, _ := e.st.ListAPITokensByPrincipal(context.Background(), sub); len(toks) != 0 {
		t.Errorf("the reactivation produced a token: %+v", toks)
	}
	if e.cookieWorks(e.a, cookie) {
		t.Error("the session admitted before the suspension works after the reactivation")
	}

	// The same for an SSH key.
	e2 := newSCIMEnv(t)
	e2.seedSignIn(sub, email)
	e2.rev.setLateCheck(func() { e2.suspendOn(e2.b, email) })
	w = doSSO(t, e2.a.srv, http.MethodPost, "/api/v1/me/ssh-keys", scimCookie(t, sub, email, 0),
		`{"name":"k","public_key":"ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIBl3jvXfmZbBd3q5aLKZTv3rIcvKlfz2eYQpuYSGfCPT alice@laptop"}`)
	if w.Code != http.StatusForbidden || errorReason(w) != reasonIdentityDeactivated {
		t.Fatalf("the late key registration = %d %s", w.Code, w.Body.String())
	}
	if e2.keyCount(sub) != 0 {
		t.Error("a key was registered for a suspended person")
	}

	// And a run: the owner guard refuses the row the same way, on either instance.
	ctx := oidc.WithAuthorityEpoch(context.Background(), 0)
	_, err := e2.b.srv.createRun(ctx, types.AgentRun{
		ID: uuid.New(), CreatedBy: sub, Agent: "claude-code", ConfinementClass: types.CC1, State: types.RunPending, RunnerTarget: "docker",
		SPIFFEID: "spiffe://wardyn.local/agent-run/x",
	})
	if !errors.Is(err, store.ErrIdentityDeactivated) {
		t.Errorf("createRun for a suspended owner = %v, want ErrIdentityDeactivated", err)
	}
	if runs, _ := e2.st.ListNonTerminalRunsBy(context.Background(), sub); len(runs) != 0 {
		t.Errorf("a run exists for a suspended owner: %+v", runs)
	}
}

// Reactivation clears the deactivation and nothing else: no token, key, run or old session comes back, a new
// sign-in works, and a second suspension is a new job with its own audit rows.
func TestSCIMReactivationRestoresNothing(t *testing.T) {
	const sub, email = "sub-r", "r@corp.example"
	e := newSCIMEnv(t)
	e.seedEntra(sub, email, oidPat)
	id := e.postUserID(e.a, oidPat, email, email)
	tok, raw := e.seedToken(sub, email)
	e.seedKey(sub)
	run := e.seedRun(sub, types.RunRunning)
	old := scimCookie(t, sub, email, 0)

	e.suspendOn(e.a, email)
	if w := e.patch(e.b, id, patchOf("true")); w.Code != http.StatusOK || !decodeSCIMUser(t, w).Active {
		t.Fatalf("reactivate = %d %s", w.Code, w.Body.String())
	}
	row := e.identity(id)
	if row.DeactivatedAt != nil || row.PurgeAfter != nil || row.AuthorityEpoch != 1 {
		t.Errorf("after reactivation: %+v, want active at epoch 1 (never lowered)", row)
	}
	if !e.tokenRevoked(tok) || e.tokenWorks(e.a, raw) || e.keyCount(sub) != 0 || e.runState(run) != types.RunKilled || e.cookieWorks(e.a, old) {
		t.Error("the reactivation restored a token, key, run or session")
	}
	if got := e.rows(e.b, "scim.user.write"); len(got) == 0 || dataOf(t, got[len(got)-1])["op"] != "reactivate" {
		t.Errorf("no reactivate row: %+v", got)
	}
	// The person signs in again: refused no more, at the new epoch.
	epoch, err := e.st.IssueLoginIdentity(context.Background(), store.LoginIdentity{Principal: sub, Issuer: e.issuer, TenantID: e.tenant, ObjectID: oidPat, Email: email}, time.Now().UTC())
	if err != nil || epoch != 1 {
		t.Fatalf("sign-in after reactivation = %d, %v, want epoch 1", epoch, err)
	}
	if !e.cookieWorks(e.a, scimCookie(t, sub, email, epoch)) {
		t.Error("a session at the new epoch is refused")
	}

	// A new suspension is a new job.
	e.suspendOn(e.b, email)
	if got := e.rows(e.b, "scim.user.deactivate"); len(got) != 1 || len(e.rows(e.a, "scim.user.deactivate")) != 1 {
		t.Errorf("deactivate rows: node a %d, node b %d, want one each", len(e.rows(e.a, "scim.user.deactivate")), len(got))
	}
	if e.identity(id).AuthorityEpoch != 2 {
		t.Errorf("epoch = %d after the second suspension, want 2", e.identity(id).AuthorityEpoch)
	}
}

// A purged identity is a permanent tombstone: active=true is a 400 invalidValue with a denied row, and the
// person cannot sign in.
func TestSCIMPurgedIdentityCannotBeReactivated(t *testing.T) {
	const sub, email = "sub-gone", "gone@corp.example"
	e := newSCIMEnv(t)
	e.seedEntra(sub, email, oidPat)
	id := e.postUserID(e.a, oidPat, email, email)
	e.suspendOn(e.a, email)
	if _, err := e.pool.Exec(context.Background(), `UPDATE principal_identities SET purged_at = now() WHERE id = $1`, id); err != nil {
		t.Fatal(err)
	}
	w := e.patch(e.b, id, patchOf("true"))
	if status, typ := scimErrorOf(t, w); w.Code != http.StatusBadRequest || typ != "invalidValue" || status != "400" {
		t.Fatalf("reactivate a purged identity = %d %s", w.Code, w.Body.String())
	}
	var denied []types.AuditEvent
	for _, ev := range e.rows(e.b, "scim.user.write") {
		if ev.Outcome == "denied" {
			denied = append(denied, ev)
		}
	}
	if len(denied) != 1 || dataOf(t, denied[0])["reason"] != "purged" {
		t.Errorf("denied rows = %+v, want one with reason purged", denied)
	}
	if e.identity(id).DeactivatedAt == nil {
		t.Error("the refused reactivation cleared the deactivation")
	}
	su := e.signIn(e.a, sub, email)
	if sessionCookieOf(su) != nil || authErrorOf(su) != "sign_in_refused" {
		t.Errorf("a purged person signed in: %d %s", su.Code, su.Header().Get("Location"))
	}
	if refused, err := e.st.IdentityRefused(context.Background(), e.issuer, e.tenant, oidPat, ""); err != nil || !refused {
		t.Errorf("IdentityRefused = %v, %v", refused, err)
	}
}

type flipStore struct {
	store.PG
	once sync.Once
	flip func()
}

func (f *flipStore) GetRun(ctx context.Context, id uuid.UUID) (types.AgentRun, error) {
	run, err := f.PG.GetRun(ctx, id)
	f.once.Do(f.flip)
	return run, err
}

// Injected failures: a runner, a revocation and an audit write each fail a suspension with a 5xx, and the
// retry finishes the job, including a run already KILLED whose teardown failed, and a run that moves under
// the kill is killed and confirmed.
func TestSCIMSuspendIsDurable(t *testing.T) {
	t.Run("a runner failure leaves a KILLED run pending and the retry repairs it", func(t *testing.T) {
		const sub, email = "sub-run", "run@corp.example"
		e := newSCIMEnv(t)
		e.seedEntra(sub, email, oidPat)
		id := e.postUserID(e.a, oidPat, email, email)
		run := e.seedRun(sub, types.RunRunning)

		e.a.runner.setFailKills(1)
		if w := e.patch(e.a, id, patchOf("false")); w.Code != http.StatusInternalServerError {
			t.Fatalf("suspend with a failing runner = %d %s, want 500", w.Code, w.Body.String())
		}
		if e.runState(run) != types.RunKilled || e.identity(id).DeactivatedAt == nil || e.allJobsDone(id) {
			t.Fatalf("after the failure: run %s, ledger done=%v", e.runState(run), e.allJobsDone(id))
		}
		if len(e.rows(e.a, "scim.user.deactivate")) != 0 {
			t.Error("an unfinished suspension was recorded as done")
		}
		if w := e.patch(e.b, id, patchOf("false")); w.Code != http.StatusOK {
			t.Fatalf("retry on the other instance = %d %s", w.Code, w.Body.String())
		}
		if e.a.runner.killCount() != 1 || e.b.runner.killCount() != 1 || !e.allJobsDone(id) {
			t.Errorf("kills: %d then %d, done=%v; the KILLED run's teardown must be repeated once", e.a.runner.killCount(), e.b.runner.killCount(), e.allJobsDone(id))
		}
		if len(e.rows(e.b, "scim.user.deactivate")) != 1 {
			t.Error("the finished suspension was not recorded once")
		}
	})

	t.Run("a revocation failure answers 5xx and the retry revokes", func(t *testing.T) {
		e := newSCIMEnv(t)
		e.seedEntra("sub-rev", "rev@corp.example", oidPat)
		id := e.postUserID(e.a, oidPat, "rev@corp.example", "rev@corp.example")
		tok, raw := e.seedToken("sub-rev", "rev@corp.example")
		fp := e.seedKey("sub-rev")
		cookie := scimCookie(t, "sub-rev", "rev@corp.example", 0)
		e.rev.setFail(true)
		if w := e.patch(e.a, id, patchOf("false")); w.Code != http.StatusInternalServerError {
			t.Fatalf("suspend with a failing revocation store = %d, want 500", w.Code)
		}
		if e.tokenRevoked(tok) || e.allJobsDone(id) || e.keyCount("sub-rev") != 1 {
			t.Error("a step that failed was recorded done")
		}
		// The identity is deactivated at once, so the credentials already stop at authentication,
		// each on its own lane, each with the same auth.fail reason.
		if e.tokenWorks(e.a, raw) || e.cookieWorks(e.a, cookie) {
			t.Error("a deactivated person's token or session authenticates while the sweep is pending")
		}
		key, err := e.st.GetSSHKeyByFingerprint(context.Background(), fp)
		if err != nil {
			t.Fatal(err)
		}
		if refusal := e.a.srv.sshKeyRevocationRefusal(context.Background(), key); !strings.Contains(refusal, "deactivated") {
			t.Errorf("the SSH key of a deactivated person is not refused: %q", refusal)
		}
		if got := authFailReasons(e.a.h.audit.snapshot(), adminAuthActor); !slices.Contains(got, authFailedIdentityDeactivated) {
			t.Errorf("auth.fail reasons = %v, want identity_deactivated from the token and session lanes", got)
		}
		e.rev.setFail(false)
		if w := e.patch(e.a, id, patchOf("false")); w.Code != http.StatusOK || !e.tokenRevoked(tok) || !e.allJobsDone(id) {
			t.Fatalf("retry = %d, revoked=%v, done=%v", w.Code, e.tokenRevoked(tok), e.allJobsDone(id))
		}
	})

	t.Run("an audit failure answers 5xx and the retry writes each row once", func(t *testing.T) {
		e := newSCIMEnv(t)
		e.seedEntra("sub-aud", "aud@corp.example", oidPat)
		id := e.postUserID(e.a, oidPat, "aud@corp.example", "aud@corp.example")
		e.a.audit.fail("scim.user.deactivate", true)
		if w := e.patch(e.a, id, patchOf("false")); w.Code != http.StatusInternalServerError {
			t.Fatalf("suspend with a failing audit write = %d, want 500", w.Code)
		}
		if len(e.rows(e.a, "scim.user.deactivate")) != 0 || len(e.rows(e.a, "person.deprovision")) != 1 || e.allJobsDone(id) {
			t.Fatal("after the failure only the row that could be written exists, and the job is open")
		}
		e.a.audit.fail("scim.user.deactivate", false)
		if w := e.patch(e.a, id, patchOf("false")); w.Code != http.StatusOK {
			t.Fatalf("retry = %d", w.Code)
		}
		if len(e.rows(e.a, "scim.user.deactivate")) != 1 || len(e.rows(e.a, "person.deprovision")) != 1 || !e.allJobsDone(id) {
			t.Errorf("rows after the retry: %d deactivate, %d deprovision", len(e.rows(e.a, "scim.user.deactivate")), len(e.rows(e.a, "person.deprovision")))
		}
	})

	t.Run("a run that transitions under the kill is killed and confirmed", func(t *testing.T) {
		const sub, email = "sub-move", "move@corp.example"
		e := newSCIMEnv(t)
		e.seedEntra(sub, email, oidPat)
		run := e.seedRun(sub, types.RunPending)
		fs := &flipStore{PG: e.st, flip: func() {
			if ok, err := e.st.UpdateRunStateIf(context.Background(), run, types.RunPending, types.RunRunning); err != nil || !ok {
				t.Errorf("transition: %v %v", ok, err)
			}
		}}
		n := e.node(func(c *Config) { c.Store = fs })
		id := e.postUserID(n, oidPat, email, email)
		if w := e.patch(n, id, patchOf("false")); w.Code != http.StatusOK {
			t.Fatalf("suspend = %d %s", w.Code, w.Body.String())
		}
		if e.runState(run) != types.RunKilled || n.runner.killCount() != 1 || !e.allJobsDone(id) {
			t.Errorf("run %s, kills %d, done %v: the moved run must be killed once and confirmed", e.runState(run), n.runner.killCount(), e.allJobsDone(id))
		}
	})
}

func adoLoginConfig(e *scimEnv) ADOEntraConfig {
	return ADOEntraConfig{
		RowID: "ado-row-1", TenantID: e.fake.TenantID(), ClientID: e.fake.ClientID(), RedirectURL: e.redirect,
		Scopes:        e.fake.ConsentedScopes()[1:],
		LoginClientID: e.fake.ClientID(), LoginTenantID: e.fake.TenantID(),
		AuthorityOverride: e.fake.URL(), AllowTestEndpoints: true,
	}
}

func (e *scimEnv) captured(sub string) bool {
	e.t.Helper()
	names, err := e.sec.For(sub).List(context.Background())
	if err != nil {
		e.t.Fatal(err)
	}
	return slices.Contains(names, adoEntraSecretName("ado-row-1"))
}

// A suspension whose process dies right after the deactivation commits still leaves its pending work on the
// ledger: the sweeper on another instance, with automatic purge off, finds it and finishes the suspension.
func TestPG_SCIMSuspend_PendingWorkSurvivesCrashAfterCommit(t *testing.T) {
	e := newSCIMEnv(t)
	e.seedEntra("sub-crash", "crash@corp.example", oidPat)
	id := e.postUserID(e.a, oidPat, "crash@corp.example", "crash@corp.example")
	run := e.seedRun("sub-crash", types.RunRunning)
	ctx := context.Background()
	// The crash: every ledger write after the transaction that wrote the cutoff fails.
	if _, err := e.pool.Exec(ctx, `
		CREATE FUNCTION test_fail_after_suspend_commit() RETURNS trigger AS $$ BEGIN
			IF NEW.step <> 'cutoff' AND NOT EXISTS (SELECT 1 FROM deprovision_jobs
				WHERE identity_id = NEW.identity_id AND step = 'cutoff' AND xmin = pg_current_xact_id()::xid) THEN
				RAISE EXCEPTION 'test: the process died after the suspension committed';
			END IF;
			RETURN NEW;
		END; $$ LANGUAGE plpgsql;
		CREATE TRIGGER test_fail_after_suspend_commit BEFORE INSERT ON deprovision_jobs
			FOR EACH ROW EXECUTE FUNCTION test_fail_after_suspend_commit()`); err != nil {
		t.Fatal(err)
	}
	if w := e.patch(e.a, id, patchOf("false")); w.Code/100 != 5 {
		t.Fatalf("suspend = %d %s, want 5xx", w.Code, w.Body.String())
	}
	if e.identity(id).DeactivatedAt == nil {
		t.Fatal("the suspension did not commit")
	}
	if _, err := e.pool.Exec(ctx, `DROP TRIGGER test_fail_after_suspend_commit ON deprovision_jobs`); err != nil {
		t.Fatal(err)
	}
	if _, err := e.pool.Exec(ctx, `UPDATE deprovision_jobs SET updated_at = now() - interval '1 hour' WHERE identity_id = $1`, id); err != nil {
		t.Fatal(err)
	}
	pending, err := e.st.PendingLeavers(ctx, scimResumeIdle, 10)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(pending, func(p store.PendingLeaver) bool { return p.ID.String() == id }) {
		t.Fatalf("pending leavers = %+v, want the committed suspension", pending)
	}
	if err := e.b.srv.SweepSCIMPurge(ctx); err != nil {
		t.Fatal(err)
	}
	if e.runState(run) != types.RunKilled || !e.allJobsDone(id) {
		t.Errorf("after the sweep: run %s, ledger done=%v; want KILLED and done", e.runState(run), e.allJobsDone(id))
	}
}

// A sign-in in flight when a suspension lands, on another instance, ends with no usable session and no
// captured credential, whichever step it was at: admitted (the suspension lands before issuance), or issued
// (it lands before the capture and the cookie's first use).
func TestSCIMSuspensionOvertakesASignIn(t *testing.T) {
	e := newSCIMEnv(t)
	cfg := adoLoginConfig(e)
	for _, n := range []*scimNode{e.a, e.b} {
		n.srv.cfg.ADOEntra = func(context.Context) (ADOEntraConfig, bool, error) { return cfg, true, nil }
	}

	// Control: with no suspension the same sign-in captures the credential and the session works.
	e.a.auth.AttachLoginGrantSink(e.a.srv)
	w := e.signIn(e.a, "sub-control", "control@corp.example")
	if c := sessionCookieOf(w); c == nil || !e.cookieWorks(e.a, c) || !e.captured("sub-control") {
		t.Fatalf("control sign-in: cookie %v, captured %v", sessionCookieOf(w) != nil, e.captured("sub-control"))
	}
	e.a.auth.AttachLoginGrantSink(nil)

	t.Run("admitted, then suspended, then issuance", func(t *testing.T) {
		const sub, email = "sub-p1", "p1@corp.example"
		if w := e.signIn(e.a, sub, email); sessionCookieOf(w) == nil {
			t.Fatal("first sign-in failed")
		}
		e.a.auth.AttachLoginGrantSink(e.a.srv)
		t.Cleanup(func() { e.a.auth.AttachLoginGrantSink(nil) })
		e.a.gate.setAfterRefused(func() { e.suspendOn(e.b, email) })
		w := e.signIn(e.a, sub, email)
		if sessionCookieOf(w) != nil || authErrorOf(w) != "sign_in_refused" {
			t.Fatalf("a sign-in overtaken before issuance: %d %s, want sign_in_refused and no session", w.Code, w.Header().Get("Location"))
		}
		if e.captured(sub) {
			t.Error("a credential was captured for a sign-in that was refused")
		}
		if got := authFailReasons(e.a.h.audit.snapshot(), oidcCallbackActor); !slices.Contains(got, authFailedIdentityDeactivated) {
			t.Errorf("auth.fail reasons = %v, want identity_deactivated", got)
		}
	})

	t.Run("issued, then suspended, then capture", func(t *testing.T) {
		const sub, email = "sub-p2", "p2@corp.example"
		if w := e.signIn(e.a, sub, email); sessionCookieOf(w) == nil {
			t.Fatal("first sign-in failed")
		}
		e.a.auth.AttachLoginGrantSink(e.a.srv)
		t.Cleanup(func() { e.a.auth.AttachLoginGrantSink(nil) })
		e.a.setOnLogin(func() { e.suspendOn(e.b, email) })
		t.Cleanup(func() { e.a.setOnLogin(nil) })
		w := e.signIn(e.a, sub, email)
		cookie := sessionCookieOf(w)
		if cookie == nil {
			t.Fatalf("the sign-in that issued before the suspension has no cookie: %s", w.Header().Get("Location"))
		}
		if e.cookieWorks(e.a, cookie) || e.cookieWorks(e.b, cookie) {
			t.Error("a cookie issued before the suspension works on a second instance")
		}
		if e.captured(sub) {
			t.Error("a credential captured after the suspension is stored")
		}
	})
}

// A fresh sign-in for a person SCIM deactivated is refused on a second instance, with no session, whether
// they are keyed by a pairwise sub with an object id (an Entra user with no people row) or have no object id
// at all (a non-Entra user, reached through their email).
func TestSCIMDeactivatedPersonCannotSignInOnAnotherInstance(t *testing.T) {
	e := newSCIMEnv(t)
	e.seedEntra("fake-sub-entra", "entra.user@corp.example", oidPat)
	e.seedSignIn("fake-sub-other", "other.user@corp.example")

	if _, code := e.postUser(e.a, oidPat, "entra.user@corp.example", "entra.user@corp.example", false); code != http.StatusCreated {
		t.Fatalf("suspend the Entra user = %d", code)
	}
	if _, code := e.postUser(e.a, oidOther, "other.user@corp.example", "other.user@corp.example", false); code != http.StatusCreated {
		t.Fatalf("suspend the non-Entra user = %d", code)
	}
	for _, p := range [][2]string{{"fake-sub-entra", "entra.user@corp.example"}, {"fake-sub-other", "other.user@corp.example"}} {
		w := e.signIn(e.b, p[0], p[1])
		if sessionCookieOf(w) != nil || authErrorOf(w) != "sign_in_refused" {
			t.Errorf("%s signed in: %d %s", p[0], w.Code, w.Header().Get("Location"))
		}
	}
	if got := authFailReasons(e.b.h.audit.snapshot(), oidcCallbackActor); len(got) != 2 || got[0] != authFailedIdentityDeactivated {
		t.Errorf("auth.fail reasons = %v, want two identity_deactivated", got)
	}
	// Control: an unrelated person still signs in.
	if w := e.signIn(e.b, "fake-sub-fine", "fine@corp.example"); sessionCookieOf(w) == nil {
		t.Errorf("an unrelated sign-in was refused: %s", w.Header().Get("Location"))
	}
}

// The delegated portal exchange admits through the same gate: for a deactivated identity it is refused with
// auth.fail reason identity_deactivated, on an instance the suspension did not land on.
func TestSCIMDeactivatedPersonCannotUsePortalExchange(t *testing.T) {
	e := newDelegationPG(t)
	rev := &pgTestRevocations{pool: e.pool, st: e.st}
	auth, err := oidc.New(context.Background(), oidc.Config{
		IssuerURL: e.iss, ClientID: "wardyn-client", ClientSecret: "secret", RedirectURL: "http://localhost/auth/callback",
		DefaultRole: oidc.RoleUser, Revocations: rev, Identities: NewIdentityGate(e.st),
	}, accessTestHMACKey)
	if err != nil {
		t.Fatal(err)
	}
	e.h.srv.cfg.OIDC, e.h.srv.cfg.SessionRevocations = auth, rev
	e.h.srv.router = e.h.srv.routes()
	portal, cred := e.registerPortal(t, delegGroup)

	hb := newHarness(t)
	cfg := baseTestConfig(hb, e.st)
	cfg.OIDC, cfg.SessionRevocations = auth, rev
	cfg.SCIM = &SCIMConfig{Token: scimEnvToken, Issuer: e.iss, Tenant: "11111111-2222-3333-4444-555555555555"}
	other := New(cfg)

	row, err := e.st.UpsertLoginIdentity(context.Background(), store.LoginIdentity{Principal: delegPerson, Issuer: e.iss, Email: delegPersonEmail}, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if w := e.exchange(t, portal, cred, e.subjectToken(t, delegPerson, delegPersonEmail, []string{delegGroup}, nil)); w.Code != http.StatusOK {
		t.Fatalf("control exchange = %d %s", w.Code, w.Body.String())
	}

	scimEnvLike := &scimEnv{t: t, pool: e.pool, st: e.st, issuer: e.iss}
	if w := scimEnvLike.patch(&scimNode{srv: other}, row.ID.String(), patchOf("false")); w.Code != http.StatusOK {
		t.Fatalf("suspend on the other instance = %d %s", w.Code, w.Body.String())
	}

	w := e.exchange(t, portal, cred, e.subjectToken(t, delegPerson, delegPersonEmail, []string{delegGroup}, nil))
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "invalid_grant") {
		t.Fatalf("exchange for a deactivated person = %d %s, want 400 invalid_grant", w.Code, w.Body.String())
	}
	if got := authFailReasons(e.h.audit.snapshot(), oidcCallbackActor); !slices.Equal(got, []string{authFailedIdentityDeactivated}) {
		t.Errorf("auth.fail reasons = %v, want [identity_deactivated]", got)
	}
	var reasons []string
	for _, ev := range e.rows("delegation.exchange") {
		if ev.Outcome == "denied" {
			reasons = append(reasons, dataOf(t, ev)["reason"].(string))
		}
	}
	if !slices.Contains(reasons, "identity_deactivated") {
		t.Errorf("delegation.exchange denied reasons = %v", reasons)
	}
	if got := e.delegatedTokenCount(t); got != 1 {
		t.Errorf("delegated tokens = %d, want only the control exchange's", got)
	}
}
