// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

func (e peoplePG) listPeople(t *testing.T, as *http.Cookie, query string) (int, types.PersonList, string) {
	t.Helper()
	path := "/api/v1/people"
	if query != "" {
		path += "?" + query
	}
	w := doSSO(t, e.h.srv, http.MethodGet, path, as, "")
	var out types.PersonList
	if w.Code == http.StatusOK {
		if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
			t.Fatalf("decode %s: %v: %s", path, err, w.Body.String())
		}
	}
	return w.Code, out, w.Body.String()
}

func personRow(t *testing.T, page types.PersonList, principal string) types.PersonSummary {
	t.Helper()
	for _, r := range page.People {
		if r.Principal == principal {
			return r
		}
	}
	t.Fatalf("list has no %q: %+v", principal, page.People)
	return types.PersonSummary{}
}

// Only the security tier reads the directory: a member and a signed-out caller are refused, and a
// security admin and an admin are served.
func TestPeopleList_SecurityTierOnly(t *testing.T) {
	e := newPeoplePG(t)
	for name, c := range map[string]struct {
		as   *http.Cookie
		want int
	}{
		"member": {e.mem, http.StatusForbidden}, "anonymous": {nil, http.StatusUnauthorized},
		"security admin": {e.sec, http.StatusOK}, "admin": {e.super, http.StatusOK},
	} {
		if code, _, body := e.listPeople(t, c.as, ""); code != c.want {
			t.Errorf("%s: GET /people = %d, want %d: %s", name, code, c.want, body)
		}
	}
}

// The listing carries a pre-created person who never signed in, a sub-keyed Entra user with no
// people row, and a deactivated person, each with its role, state and counts.
func TestPeopleList_RowsRolesAndCounts(t *testing.T) {
	e := newPeoplePG(t)
	ctx := context.Background()
	now := time.Now().UTC()
	if w := e.createPerson(t, e.sec, "pre-sub", "sec2@corp.example"); w.Code != http.StatusCreated {
		t.Fatalf("create person: %d %s", w.Code, w.Body.String())
	}
	oid := uuid.NewString()
	tenant := uuid.NewString()
	if _, err := e.st.UpsertLoginIdentity(ctx, store.LoginIdentity{
		Principal: "pairwise-1", Issuer: "https://login.microsoftonline.com/" + tenant + "/v2.0", TenantID: tenant, ObjectID: oid, Email: "boss@corp.example",
	}, now); err != nil {
		t.Fatal(err)
	}
	if _, err := e.st.UpsertLoginIdentity(ctx, store.LoginIdentity{Principal: "gone-sub", Issuer: "https://dex.example", Email: "gone@corp.example"}, now); err != nil {
		t.Fatal(err)
	}
	if _, err := e.pool.Exec(ctx, `UPDATE principal_identities SET deactivated_at = now() WHERE principal = 'gone-sub'`); err != nil {
		t.Fatal(err)
	}
	e.seedLegacyToken(t, "pairwise-1", "admin", "")
	e.seedLegacyToken(t, "pairwise-1", "admin", "")

	code, page, body := e.listPeople(t, e.sec, "")
	if code != http.StatusOK {
		t.Fatalf("GET /people = %d: %s", code, body)
	}
	if len(page.People) != 3 || page.NextCursor != "" {
		t.Fatalf("people = %+v, want the three on one page", page.People)
	}
	if p := personRow(t, page, "pre-sub"); !p.PreCreated || p.Role != "security_admin" || p.IssuerKind != "oidc" || p.LastSignedInAt != nil || p.Email != "sec2@corp.example" {
		t.Errorf("pre-created = %+v", p)
	}
	if p := personRow(t, page, "pairwise-1"); p.PreCreated || p.Role != "admin" || p.IssuerKind != "entra" || p.APITokens != 2 || p.LastSignedInAt == nil || p.ActiveSessions != 1 {
		t.Errorf("entra = %+v", p)
	}
	if p := personRow(t, page, "gone-sub"); p.Role != "user" || p.DeactivatedAt == nil || p.ActiveSessions != 0 {
		t.Errorf("deactivated = %+v", p)
	}
}

// Following next_cursor returns every person exactly once, and bad parameters are 400s.
func TestPeopleList_PagingAndParams(t *testing.T) {
	e := newPeoplePG(t)
	var want []string
	for _, p := range []string{"p-1", "p-2", "p-3", "p-4", "p-5"} {
		want = append(want, p)
		if w := e.createPerson(t, e.sec, p, p+"@corp.example"); w.Code != http.StatusCreated {
			t.Fatalf("create %s: %d %s", p, w.Code, w.Body.String())
		}
	}
	var got []string
	cursor := ""
	for pages := 0; pages < 10; pages++ {
		q := url.Values{"limit": {"2"}}
		if cursor != "" {
			q.Set("cursor", cursor)
		}
		code, page, body := e.listPeople(t, e.sec, q.Encode())
		if code != http.StatusOK {
			t.Fatalf("page %d = %d: %s", pages, code, body)
		}
		for _, p := range page.People {
			got = append(got, p.Principal)
		}
		if cursor = page.NextCursor; cursor == "" {
			break
		}
	}
	if !slices.Equal(got, want) {
		t.Errorf("rows across pages = %v, want %v", got, want)
	}

	if _, page, _ := e.listPeople(t, e.sec, "q=P-3%40"); len(page.People) != 1 || page.People[0].Principal != "p-3" {
		t.Errorf("q by email prefix = %+v, want p-3", page.People)
	}
	for _, bad := range []string{"state=gone", "cursor=!!", "limit=abc", "limit=-1"} {
		if code, _, body := e.listPeople(t, e.sec, bad); code != http.StatusBadRequest {
			t.Errorf("GET /people?%s = %d, want 400: %s", bad, code, body)
		}
	}
	if _, page, _ := e.listPeople(t, e.sec, "limit=500"); len(page.People) != len(want) {
		t.Errorf("limit=500 returned %d rows, want all %d (clamped, not refused)", len(page.People), len(want))
	}
}

// The role shown is the one the person's last verified login derives: their stored groups count, a
// truncated snapshot reads "unknown", and no snapshot is the email alone.
func TestPeopleList_RoleFromVerifiedGroups(t *testing.T) {
	e := newPeoplePG(t)
	ctx := context.Background()
	e.h.srv.cfg.OIDC = newAccessAuth(t, map[string]string{"eng": oidc.RoleAdmin}, oidc.RoleUser, nil, nil)
	e.h.srv.router = e.h.srv.routes()
	for _, p := range []string{"in-eng", "cut", "bare"} {
		if _, err := e.st.UpsertLoginIdentity(ctx, store.LoginIdentity{Principal: p, Issuer: "https://dex.example", Email: p + "@corp.example"}, time.Now()); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := e.pool.Exec(ctx, `INSERT INTO key_domain_login_groups (principal, groups, truncated) VALUES ('in-eng', '["eng"]', false), ('cut', '["eng"]', true)`); err != nil {
		t.Fatal(err)
	}
	code, page, body := e.listPeople(t, e.sec, "")
	if code != http.StatusOK {
		t.Fatalf("GET /people = %d: %s", code, body)
	}
	for p, want := range map[string]string{"in-eng": oidc.RoleAdmin, "cut": roleUnknown, "bare": oidc.RoleUser} {
		if got := personRow(t, page, p).Role; got != want {
			t.Errorf("%s role = %q, want %q", p, got, want)
		}
	}
}
