// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package scim

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

var userFilterAttrs = []string{"userName", "externalId", "emails.value"}

func TestParseFilter(t *testing.T) {
	good := []struct {
		in   string
		want Filter
	}{
		{`userName eq "bjensen"`, Filter{"userName", "bjensen"}},
		{`  USERNAME   EQ   "bjensen"  `, Filter{"userName", "bjensen"}},
		{"externalId\teq\t\"a b\"", Filter{"externalId", "a b"}},
		{`emails.value eq "a@example.com"`, Filter{"emails.value", "a@example.com"}},
		{`userName eq "say \"hi\" é"`, Filter{"userName", `say "hi" é`}},
		{`userName eq ""`, Filter{"userName", ""}},
	}
	for _, c := range good {
		got, err := ParseFilter(c.in, userFilterAttrs...)
		if err != nil || got != c.want {
			t.Errorf("%q: got %+v, %v; want %+v", c.in, got, err, c.want)
		}
	}
	bad := []string{
		``, `userName`, `userName eq`, `userName eq bjensen`, `userName eq "bjensen`, `userName eq 'bjensen'`,
		`userName ne "x"`, `userName co "x"`, `userName pr`, `displayName eq "x"`,
		`userName eq "a" and externalId eq "b"`, `userName eq "a" or userName eq "b"`,
		`not (userName eq "a")`, `(userName eq "a")`, `userName eq "a" "b"`, `userName eq"a"`,
		`userName eq "bad \q escape"`, `emails[type eq "work"].value eq "x"`,
	}
	for _, in := range bad {
		_, err := ParseFilter(in, userFilterAttrs...)
		if err == nil || err.ScimType != TypeInvalidFilter || err.Status != 400 {
			t.Errorf("%q: want 400 invalidFilter, got %v", in, err)
		}
	}
}

func TestPatchEnvelopeIsInvalidSyntax(t *testing.T) {
	for _, body := range []string{
		``, `{`, `not json`, `[]`, `{"Operations":`, `{"schemas":["x"],"Operations":[{"op":"add","path":"nickName","value":"a"}]}`,
		`{"schemas":["urn:ietf:params:scim:api:messages:2.0:PatchOp"]}`,
		`{"schemas":["urn:ietf:params:scim:api:messages:2.0:PatchOp"],"Operations":[]}`,
		`{"schemas":["urn:ietf:params:scim:api:messages:2.0:PatchOp"],"Operations":[{"op":"add","path":1}]}`,
		`{"schemas":["urn:ietf:params:scim:api:messages:2.0:PatchOp"],"Operations":[]} {}`,
	} {
		ops, perr := ParsePatch([]byte(body))
		if ops != nil || perr == nil || perr.ScimType != TypeInvalidSyntax || perr.Status != 400 || len(perr.Invalid) != 0 {
			t.Errorf("%q: want invalidSyntax with no ops, got %v, %+v", body, ops, perr)
		}
	}
}

func patchBody(ops string) []byte {
	return []byte(`{"schemas":["urn:ietf:params:scim:api:messages:2.0:PatchOp"],"Operations":[` + ops + `]}`)
}

// TestPatchRefusesWholeRequestWithoutPartialResult: one bad operation among good ones yields no ops and
// names the bad one, so the caller can still act on the rest (suspend alongside a refused externalId change).
func TestPatchRefusesWholeRequestWithoutPartialResult(t *testing.T) {
	ops, perr := ParsePatch(patchBody(`
		{"op":"replace","path":"active","value":"False"},
		{"op":"replace","path":"active","value":"maybe"},
		{"op":"replace","path":"userName","value":"a@example.com"},
		{"op":"remove"}`))
	if ops != nil {
		t.Fatalf("partial result: %+v", ops)
	}
	if perr == nil || len(perr.Invalid) != 2 || perr.Invalid[0].Index != 1 || perr.Invalid[1].Index != 3 {
		t.Fatalf("want operations 1 and 3 reported, got %+v", perr)
	}
	if perr.Invalid[0].ScimType != TypeInvalidValue || perr.Invalid[1].ScimType != TypeNoTarget {
		t.Fatalf("per-operation scimType: %+v", perr.Invalid)
	}
	if perr.ScimType != TypeInvalidValue || !strings.Contains(perr.Detail, "[1 3]") {
		t.Fatalf("request-level error: %+v", perr.Error)
	}
}

func TestPatchOperationRules(t *testing.T) {
	cases := []struct {
		name, op string
		want     []Op
		scimType string
	}{
		{"string True", `{"op":"ADD","path":"ACTIVE","value":"TRUE"}`, []Op{{0, OpAdd, AttrActive, true}}, ""},
		{"bool true", `{"op":"Replace","path":"active","value":true}`, []Op{{0, OpReplace, AttrActive, true}}, ""},
		{"path case", `{"op":"replace","path":"USERNAME","value":"a"}`, []Op{{0, OpReplace, AttrUserName, "a"}}, ""},
		{"core urn prefix", `{"op":"replace","path":"urn:ietf:params:scim:schemas:core:2.0:User:userName","value":"a"}`,
			[]Op{{0, OpReplace, AttrUserName, "a"}}, ""},
		{"members add", `{"op":"add","path":"members","value":[{"value":"m1"},{"value":"m2","$ref":null,"display":"x"}]}`,
			[]Op{{0, OpAdd, AttrMembers, []Member{{Value: "m1"}, {Value: "m2", Display: "x"}}}}, ""},
		{"members filter with colon and bracket", `{"op":"remove","path":"members[value eq \"a:b]c\"]"}`,
			[]Op{{0, OpRemove, AttrMembers, []Member{{Value: "a:b]c"}}}}, ""},
		{"remove scalar", `{"op":"remove","path":"externalId"}`, []Op{{0, OpRemove, AttrExternalID, nil}}, ""},
		{"unknown op", `{"op":"move","path":"userName","value":"a"}`, nil, TypeInvalidSyntax},
		{"remove without path", `{"op":"remove"}`, nil, TypeNoTarget},
		{"replace without value", `{"op":"replace","path":"userName"}`, nil, TypeInvalidValue},
		{"replace null value", `{"op":"replace","path":"userName","value":null}`, nil, TypeInvalidValue},
		{"empty userName", `{"op":"replace","path":"userName","value":""}`, nil, TypeInvalidValue},
		{"active number", `{"op":"replace","path":"active","value":1}`, nil, TypeInvalidValue},
		{"pathless scalar value", `{"op":"replace","value":"x"}`, nil, TypeInvalidValue},
		{"pathless empty object", `{"op":"replace","value":{}}`, nil, TypeInvalidValue},
		{"pathless bad key value", `{"op":"replace","value":{"active":"nope"}}`, nil, TypeInvalidValue},
		{"members replace", `{"op":"replace","path":"members","value":[{"value":"m"}]}`, nil, TypeInvalidValue},
		{"members add without value", `{"op":"add","path":"members"}`, nil, TypeInvalidValue},
		{"members add empty array", `{"op":"add","path":"members","value":[]}`, nil, TypeInvalidValue},
		{"members add blank ref", `{"op":"add","path":"members","value":[{"value":""}]}`, nil, TypeInvalidValue},
		{"members add not array", `{"op":"add","path":"members","value":{"value":"m"}}`, nil, TypeInvalidValue},
		{"members add filter path", `{"op":"add","path":"members[value eq \"m\"]","value":[{"value":"m"}]}`, nil, TypeInvalidPath},
		{"members filter plus value", `{"op":"remove","path":"members[value eq \"m\"]","value":[{"value":"n"}]}`, nil, TypeInvalidPath},
		{"members sub-attribute", `{"op":"remove","path":"members[value eq \"m\"].display"}`, nil, TypeInvalidPath},
		{"unclosed bracket", `{"op":"remove","path":"members[value eq \"m\""}`, nil, TypeInvalidPath},
		{"unsupported path filter", `{"op":"remove","path":"members[value co \"m\"]"}`, nil, TypeInvalidFilter},
		{"compound path filter", `{"op":"remove","path":"members[value eq \"m\" or value eq \"n\"]"}`, nil, TypeInvalidFilter},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, perr := ParsePatch(patchBody(c.op))
			if c.scimType != "" {
				if perr == nil || perr.ScimType != c.scimType || got != nil {
					t.Fatalf("want %s, got %v, %+v", c.scimType, got, perr)
				}
				return
			}
			if perr != nil || !reflect.DeepEqual(got, c.want) {
				t.Fatalf("got %#v, %+v\nwant %#v", got, perr, c.want)
			}
		})
	}
}

func TestParseResourcesRefuseBadInput(t *testing.T) {
	for _, body := range []string{``, `{`, `[]`, `{"userName":"a","externalId":"e"} x`} {
		if _, err := ParseUser([]byte(body)); err == nil || err.ScimType != TypeInvalidSyntax {
			t.Errorf("user %q: want invalidSyntax, got %v", body, err)
		}
		if _, err := ParseGroup([]byte(body)); err == nil || err.ScimType != TypeInvalidSyntax {
			t.Errorf("group %q: want invalidSyntax, got %v", body, err)
		}
	}
	for _, body := range []string{`{"externalId":"e"}`, `{"userName":"a"}`, `{"userName":"a","externalId":"e","active":"maybe"}`} {
		_, err := ParseUser([]byte(body))
		if err == nil || (err.ScimType != TypeInvalidValue && err.ScimType != TypeInvalidSyntax) {
			t.Errorf("user %q: want a 400, got %v", body, err)
		}
	}
	for _, body := range []string{`{"externalId":"e"}`, `{"displayName":"g"}`, `{"displayName":"g","externalId":"e","members":[{"value":""}]}`} {
		if _, err := ParseGroup([]byte(body)); err == nil || err.ScimType != TypeInvalidValue {
			t.Errorf("group %q: want invalidValue, got %v", body, err)
		}
	}
}

func TestParseUserActive(t *testing.T) {
	for body, want := range map[string]bool{
		`{"userName":"a","externalId":"e"}`:                    true,
		`{"userName":"a","externalId":"e","active":false}`:     false,
		`{"userName":"a","externalId":"e","active":"False"}`:   false,
		`{"userName":"a","externalId":"e","active":"True"}`:    true,
		`{"userName":"a","externalId":"e","id":"x","meta":{}}`: true,
	} {
		u, err := ParseUser([]byte(body))
		if err != nil || u.Active != want || u.ID != "" {
			t.Errorf("%s: got %+v, %v; want active=%v", body, u, err, want)
		}
	}
}

func TestErrorEnvelope(t *testing.T) {
	b, err := json.Marshal(NewError(400, TypeInvalidFilter, "nope"))
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	want := map[string]any{"schemas": []any{SchemaError}, "scimType": "invalidFilter", "detail": "nope", "status": "400"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %s", b)
	}
}
