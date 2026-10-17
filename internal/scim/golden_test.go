// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package scim

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

const fixtureDir = "testdata/entra"

type fixture struct {
	Source   string          `json:"source"`
	Captured bool            `json:"captured"`
	Body     json.RawMessage `json:"body"`
}

func load(t *testing.T, name string) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(fixtureDir, name))
	if err != nil {
		t.Fatal(err)
	}
	var f fixture
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return f.Body
}

func raw(s string) json.RawMessage { return json.RawMessage(s) }

const (
	enterpriseEmployee = "urn:ietf:params:scim:schemas:extension:enterprise:2.0:User:employeeNumber"
	filterMember       = "7f4bc1a3-285e-48ae-8202-5accb43efb0e"
)

// goldenPatches is every PATCH fixture and the typed operations it must parse into.
var goldenPatches = map[string][]Op{
	"entra-doc-patch-user-active-string-false.json":         {{0, OpReplace, AttrActive, false}},
	"entra-doc-patch-user-active-false-bool.json":           {{0, OpReplace, AttrActive, false}},
	"entra-doc-patch-user-active-bool-lowercase-op.json":    {{0, OpReplace, AttrActive, false}},
	"entra-doc-patch-user-add-nickname.json":                {{0, OpAdd, "nickName", raw(`"Babs"`)}},
	"entra-doc-patch-user-username.json":                    {{0, OpReplace, AttrUserName, "5b50642d-79fc-4410-9e90-4c077cdd1a59@testuser.com"}},
	"entra-doc-patch-group-displayname.json":                {{0, OpReplace, AttrDisplayName, "1879db59-3bdf-4490-ad68-ab880a269474updatedDisplayName"}},
	"entra-doc-patch-group-add-members.json":                {{0, OpAdd, AttrMembers, []Member{{Value: "f648f8d5ea4e4cd38e9c"}}}},
	"entra-doc-patch-group-remove-members.json":             {{0, OpRemove, AttrMembers, []Member{{Value: "f648f8d5ea4e4cd38e9c"}}}},
	"entra-doc-patch-group-remove-members-value-array.json": {{0, OpRemove, AttrMembers, []Member{{Value: "u1091"}}}},
	"entra-doc-patch-group-remove-members-filter-path.json": {{0, OpRemove, AttrMembers, []Member{{Value: filterMember}}}},
	"rfc-patch-group-remove-member-filter-path.json":        {{0, OpRemove, AttrMembers, []Member{{Value: "2819c223-7f76-...413861904646"}}}},
	"rfc-patch-group-remove-all-members.json":               {{0, OpRemove, AttrMembers, nil}},
	"entra-doc-patch-user-email-name.json": {
		{0, OpReplace, AttrEmail, "updatedEmail@microsoft.com"},
		{1, OpReplace, "name.familyName", raw(`"updatedFamilyName"`)},
	},
	"entra-doc-patch-user-multi-replace.json": {
		{0, OpReplace, AttrDisplayName, "Pvlo"},
		{1, OpReplace, AttrEmail, "TestBcwqnm@test.microsoft.com"},
		{2, OpReplace, "name.givenName", raw(`"Gtfd"`)},
		{3, OpReplace, "name.familyName", raw(`"Pkqf"`)},
		{4, OpReplace, AttrExternalID, "Eqpj"},
		{5, OpReplace, enterpriseEmployee, raw(`"Eqpj"`)},
	},
	"entra-doc-patch-user-pathless-replace.json": {
		{0, OpReplace, AttrEmail, "TestMhvaes@test.microsoft.com"},
		{1, OpReplace, AttrDisplayName, "Bjfe"},
		{1, OpReplace, "name.familyName", raw(`"Unua"`)},
		{1, OpReplace, "name.givenName", raw(`"Kkom"`)},
		{1, OpReplace, enterpriseEmployee, raw(`"Aklq"`)},
	},
}

func TestGoldenPatches(t *testing.T) {
	for name, want := range goldenPatches {
		t.Run(name, func(t *testing.T) {
			got, perr := ParsePatch(load(t, name))
			if perr != nil {
				t.Fatalf("ParsePatch: %v", perr.Error)
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("ops\n got %#v\nwant %#v", got, want)
			}
		})
	}
}

func TestGoldenResources(t *testing.T) {
	u, err := ParseUser(load(t, "entra-doc-post-user.json"))
	if err != nil {
		t.Fatal(err)
	}
	if u.ExternalID != "0a21f0f2-8d2a-4f8e-bf98-7363c4aed4ef" || !u.Active || len(u.Emails) != 1 || u.Emails[0].Type != "work" {
		t.Fatalf("post user: %+v", u)
	}
	put, err := ParseUser(load(t, "rfc-put-user.json"))
	if err != nil {
		t.Fatal(err)
	}
	if put.UserName != "bjensen" || len(put.Emails) != 2 || put.ID != "" {
		t.Fatalf("put user: %+v (client id must be dropped)", put)
	}
	g, err := ParseGroup(load(t, "entra-doc-post-group.json"))
	if err != nil {
		t.Fatal(err)
	}
	if g.DisplayName != "displayName" || g.ExternalID != "8aa1a0c0-c4c3-4bc0-b4a5-2ef676900159" {
		t.Fatalf("post group: %+v", g)
	}
}

func TestGoldenResponses(t *testing.T) {
	var list ListResponse[User]
	if err := json.Unmarshal(load(t, "entra-doc-response-list-users.json"), &list); err != nil {
		t.Fatal(err)
	}
	if len(list.Resources) != 1 || list.Resources[0].ID != "2441309d85324e7793ae" || !list.Resources[0].Active {
		t.Fatalf("list: %+v", list)
	}
	b, err := json.Marshal(NewListResponse[User](nil))
	if err != nil || !strings.Contains(string(b), `"Resources":[]`) || !strings.Contains(string(b), SchemaListResponse) {
		t.Fatalf("empty list renders %s (%v)", b, err)
	}
	want := load(t, "entra-doc-response-error-404.json")
	got, err := json.Marshal(NewError(404, "", "Resource 23B51B0E5D7AE9110A49411D@7cca31655d49f3640a494224 not found"))
	if err != nil {
		t.Fatal(err)
	}
	var w, g map[string]any
	if json.Unmarshal(want, &w) != nil || json.Unmarshal(got, &g) != nil || !reflect.DeepEqual(w, g) {
		t.Fatalf("error envelope\n got %s\nwant %s", got, want)
	}
}

// TestFixturesAreRecorded keeps the record honest: every fixture cites its source, is marked not captured
// unless it is a live capture, is covered by a test above, and is listed in RECORD.md.
func TestFixturesAreRecorded(t *testing.T) {
	record, err := os.ReadFile(filepath.Join(fixtureDir, "RECORD.md"))
	if err != nil {
		t.Fatal(err)
	}
	files, _ := filepath.Glob(filepath.Join(fixtureDir, "*.json"))
	covered := map[string]bool{
		"entra-doc-post-user.json": true, "entra-doc-post-group.json": true, "rfc-put-user.json": true,
		"entra-doc-response-list-users.json": true, "entra-doc-response-error-404.json": true,
	}
	for name := range goldenPatches {
		covered[name] = true
	}
	for _, path := range files {
		name := filepath.Base(path)
		var f fixture
		b, _ := os.ReadFile(path)
		if err := json.Unmarshal(b, &f); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		live := strings.HasPrefix(name, "entra-live-")
		switch {
		case !strings.HasPrefix(f.Source, "https://"):
			t.Errorf("%s: source must be a URL, got %q", name, f.Source)
		case strings.HasPrefix(name, "rfc-") && !strings.HasPrefix(f.Source, "https://www.rfc-editor.org/rfc/rfc7644"):
			t.Errorf("%s: an rfc-* fixture must cite RFC 7644", name)
		case strings.HasPrefix(name, "entra-doc-") && !strings.HasPrefix(f.Source, "https://learn.microsoft.com/"):
			t.Errorf("%s: an entra-doc-* fixture must cite Microsoft Learn", name)
		case f.Captured != live:
			t.Errorf("%s: captured=%v, but only entra-live-* files are captures", name, f.Captured)
		case !covered[name] && !live:
			t.Errorf("%s: no test covers this fixture", name)
		case !strings.Contains(string(record), "`"+name+"`"):
			t.Errorf("%s: not listed in RECORD.md", name)
		}
	}
	for name := range covered {
		if _, err := os.Stat(filepath.Join(fixtureDir, name)); err != nil {
			t.Errorf("test covers %s, which is not on disk", name)
		}
	}
}
