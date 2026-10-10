// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package types

import "testing"

func person(name string) TemplateOwner {
	return TemplateOwner{Scope: TemplateScopePerson, Person: name}
}
func group(id string) TemplateOwner { return TemplateOwner{Scope: TemplateScopeGroup, Group: id} }

var orgOwner = TemplateOwner{Scope: TemplateScopeOrg}

func member(name string, groups ...string) TemplateActor {
	return TemplateActor{Person: name, Groups: append([]string{}, groups...)}
}

func TestTemplateAuthorizePersonScopeIsPrivate(t *testing.T) {
	alice, bob := member("alice"), member("bob")
	admin := TemplateActor{Person: "root", OrgAdmin: true, Groups: []string{}}
	for _, action := range []TemplateAction{TemplateRead, TemplateWrite} {
		if d := TemplateAuthorize(alice, person("alice"), action); d != TemplateAllowed {
			t.Errorf("owner %s: %s", action, d)
		}
		if d := TemplateAuthorize(bob, person("alice"), action); d != TemplateDenyNotOwner {
			t.Errorf("another person %s someone's personal template: %q", action, d)
		}
		if d := TemplateAuthorize(admin, person("alice"), action); d != TemplateDenyNotOwner {
			t.Errorf("an organisation administrator %s a personal template: %q", action, d)
		}
	}
}

func TestTemplateAuthorizeOrgScope(t *testing.T) {
	alice := member("alice")
	admin := TemplateActor{Person: "root", OrgAdmin: true}
	if d := TemplateAuthorize(alice, orgOwner, TemplateRead); d != TemplateAllowed {
		t.Errorf("any signed-in person reads org templates: %q", d)
	}
	if d := TemplateAuthorize(alice, orgOwner, TemplateWrite); d != TemplateDenyNotOrgAdmin {
		t.Errorf("an ordinary member writes an org template: %q", d)
	}
	if d := TemplateAuthorize(admin, orgOwner, TemplateWrite); d != TemplateAllowed {
		t.Errorf("an organisation administrator cannot write: %q", d)
	}
	groupAdmin := TemplateActor{Person: "gina", Groups: []string{"eng"}, GroupAdminGrants: []string{"eng"}}
	if d := TemplateAuthorize(groupAdmin, orgOwner, TemplateWrite); d != TemplateDenyNotOrgAdmin {
		t.Errorf("a group administrator writes org templates: %q", d)
	}
}

func TestTemplateAuthorizeGroupScope(t *testing.T) {
	gina := TemplateActor{Person: "gina", Groups: []string{"eng", "ops"}, GroupAdminGrants: []string{"eng"}}
	tests := []struct {
		name   string
		actor  TemplateActor
		owner  TemplateOwner
		action TemplateAction
		want   TemplateDenial
	}{
		{"admin writes their own group", gina, group("eng"), TemplateWrite, TemplateAllowed},
		{"admin reads their own group", gina, group("eng"), TemplateRead, TemplateAllowed},
		{"group A admin cannot write group B though a member of B", gina, group("ops"), TemplateWrite, TemplateDenyNotGroupAdmin},
		{"group A admin may read group B as a member", gina, group("ops"), TemplateRead, TemplateAllowed},
		{"group A admin cannot read or write a group they are not in", gina, group("finance"), TemplateRead, TemplateDenyNotGroupMember},
		{"nor write it", gina, group("finance"), TemplateWrite, TemplateDenyNotGroupMember},
		{"member without the grant cannot write", member("mia", "eng"), group("eng"), TemplateWrite, TemplateDenyNotGroupAdmin},
		{"member reads", member("mia", "eng"), group("eng"), TemplateRead, TemplateAllowed},
		{"organisation administrator is not implicitly a group administrator",
			TemplateActor{Person: "root", OrgAdmin: true, Groups: []string{"eng"}}, group("eng"), TemplateWrite, TemplateDenyNotGroupAdmin},
		{"a grant outlives nothing: removed from the group, no longer its administrator",
			TemplateActor{Person: "gina", Groups: []string{"ops"}, GroupAdminGrants: []string{"eng"}}, group("eng"), TemplateWrite, TemplateDenyNotGroupMember},
		{"a stale grant on a group the snapshot no longer lists cannot read either",
			TemplateActor{Person: "gina", Groups: []string{}, GroupAdminGrants: []string{"eng"}}, group("eng"), TemplateRead, TemplateDenyNotGroupMember},
		{"no snapshot is unverified, not empty", TemplateActor{Person: "gina", GroupAdminGrants: []string{"eng"}}, group("eng"), TemplateWrite, TemplateDenyGroupUnverified},
		{"a truncated snapshot is unverified in both directions",
			TemplateActor{Person: "gina", Groups: []string{"eng"}, GroupsTruncated: true, GroupAdminGrants: []string{"eng"}}, group("eng"), TemplateRead, TemplateDenyGroupUnverified},
		{"a group name is folded the way the snapshot is", gina, group("  ENG "), TemplateWrite, TemplateAllowed},
		{"a look-alike group name matches nothing",
			gina, group("eng\u212a"), TemplateWrite, TemplateDenyBadOwner},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if d := TemplateAuthorize(tc.actor, tc.owner, tc.action); d != tc.want {
				t.Fatalf("got %q, want %q", d, tc.want)
			}
		})
	}
}

func TestTemplateAuthorizeRefusesMalformedInput(t *testing.T) {
	alice := member("alice")
	for name, owner := range map[string]TemplateOwner{
		"no scope":          {},
		"unknown scope":     {Scope: "team"},
		"person without id": {Scope: TemplateScopePerson},
		"person with group": {Scope: TemplateScopePerson, Person: "alice", Group: "eng"},
		"org with person":   {Scope: TemplateScopeOrg, Person: "alice"},
		"group with person": {Scope: TemplateScopeGroup, Group: "eng", Person: "alice"},
		"group without id":  {Scope: TemplateScopeGroup},
	} {
		if d := TemplateAuthorize(alice, owner, TemplateRead); d != TemplateDenyBadOwner {
			t.Errorf("%s: %q", name, d)
		}
	}
	if d := TemplateAuthorize(TemplateActor{}, orgOwner, TemplateRead); d != TemplateDenyBadOwner {
		t.Errorf("an actor with no identity: %q", d)
	}
	if d := TemplateAuthorize(alice, orgOwner, "delete"); d != TemplateDenyBadOwner {
		t.Errorf("an unknown action: %q", d)
	}
}

func TestTemplateCopyNeedsReadOnTheSourceAndWriteOnTheTarget(t *testing.T) {
	gina := TemplateActor{Person: "gina", Groups: []string{"eng", "ops"}, GroupAdminGrants: []string{"eng"}}
	if d := TemplateCopyAuthorize(gina, person("gina"), group("eng")); d != TemplateAllowed {
		t.Errorf("publishing a personal template to the group the caller administers: %q", d)
	}
	if d := TemplateCopyAuthorize(gina, person("gina"), group("ops")); d != TemplateDenyNotGroupAdmin {
		t.Errorf("publishing to a group the caller only belongs to: %q", d)
	}
	if d := TemplateCopyAuthorize(gina, person("gina"), orgOwner); d != TemplateDenyNotOrgAdmin {
		t.Errorf("publishing to the organisation without the administrator tier: %q", d)
	}
	if d := TemplateCopyAuthorize(gina, person("bob"), group("eng")); d != TemplateDenyNotOwner {
		t.Errorf("copying someone else's personal template into a group: %q", d)
	}
	if d := TemplateCopyAuthorize(gina, group("eng"), person("bob")); d != TemplateDenyNotOwner {
		t.Errorf("copying into someone else's personal scope: %q", d)
	}
}

// TestTemplateRoutesAgreeWithTheAuthority checks the route table against the
// function it describes: for each route and scope, the tier the table names
// admits exactly the callers TemplateAuthorize admits for that action.
func TestTemplateRoutesAgreeWithTheAuthority(t *testing.T) {
	owners := map[TemplateScope]TemplateOwner{
		TemplateScopePerson: person("alice"), TemplateScopeOrg: orgOwner, TemplateScopeGroup: group("eng"),
	}
	callers := map[string]TemplateActor{
		"owner":             member("alice", "ops"),
		"other":             member("bob", "ops"),
		"org admin":         {Person: "root", OrgAdmin: true, Groups: []string{"ops"}},
		"group member":      member("mia", "eng"),
		"group admin":       {Person: "gina", Groups: []string{"eng"}, GroupAdminGrants: []string{"eng"}},
		"other group admin": {Person: "gus", Groups: []string{"ops", "eng"}, GroupAdminGrants: []string{"ops"}},
	}
	// admits says whether a caller holds the authority a tier names for owner.
	admits := func(tier TemplateTier, name string, scope TemplateScope) bool {
		switch tier {
		case TemplateTierOwner:
			return name == "owner"
		case TemplateTierMember:
			return true
		case TemplateTierOrgAdmin:
			return name == "org admin"
		case TemplateTierGroupMember:
			return name == "group member" || name == "group admin" || name == "other group admin"
		case TemplateTierGroupAdmin:
			return name == "group admin"
		}
		return false
	}
	seen := map[string]bool{}
	for _, route := range TemplateRoutes {
		if seen[route.Route] {
			t.Errorf("route %s is listed twice", route.Route)
		}
		seen[route.Route] = true
		if route.RouterClass != "member" && route.RouterClass != "admin" {
			t.Errorf("%s: router class %q", route.Route, route.RouterClass)
		}
		action := TemplateRead
		if route.Org == TemplateTierOrgAdmin {
			action = TemplateWrite
		}
		for scope, tier := range map[TemplateScope]TemplateTier{TemplateScopePerson: route.Person, TemplateScopeOrg: route.Org, TemplateScopeGroup: route.Group} {
			if tier == TemplateTierNone {
				continue
			}
			for name, actor := range callers {
				got := TemplateAuthorize(actor, owners[scope], action) == TemplateAllowed
				if want := admits(tier, name, scope); got != want {
					t.Errorf("%s on %s scope as %s: authority says %v, tier %q says %v", route.Route, scope, name, got, tier, want)
				}
			}
		}
	}
}
