// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package types

import "slices"

// Run templates (0.9): reusable run setups a person keeps for themselves, an
// administrator publishes to the organisation, or a group administrator
// publishes to one group. This file is the authority half of that contract. A
// template names who may read it and who may write it; it never grants the
// access a run resolves, which the launcher's own current policy, credentials,
// components, pools and drives decide at use.

// TemplateScope is who a template is for.
type TemplateScope string

const (
	// TemplateScopePerson is private to the person who owns it.
	TemplateScopePerson TemplateScope = "person"
	// TemplateScopeOrg is offered to everyone signed in to the organisation.
	TemplateScopeOrg TemplateScope = "org"
	// TemplateScopeGroup is offered to the members of one group.
	TemplateScopeGroup TemplateScope = "group"
)

// Valid reports whether s is one of the three scopes.
func (s TemplateScope) Valid() bool {
	return s == TemplateScopePerson || s == TemplateScopeOrg || s == TemplateScopeGroup
}

// TemplateOwner is the trusted catalogue key of one template: the scope and
// the identity that scope is bound to. The server assigns it from the
// authenticated request. A template document never carries one, so imported
// text cannot name its own owner or audience.
type TemplateOwner struct {
	Scope TemplateScope `json:"scope"`
	// Person is the owning person's canonical subject; set for person scope only.
	Person string `json:"owner_id,omitempty"`
	// Group is the canonical group subject the authenticated group snapshot
	// carries (CanonicalGroupSubject); set for group scope only. Authority
	// follows this identifier, never a display name.
	Group string `json:"group_id,omitempty"`
}

// Valid reports whether the owner is well formed for its scope: person scope
// names a person and no group, group scope a group and no person, org scope
// neither.
func (o TemplateOwner) Valid() bool {
	switch o.Scope {
	case TemplateScopePerson:
		return o.Person != "" && o.Group == ""
	case TemplateScopeOrg:
		return o.Person == "" && o.Group == ""
	case TemplateScopeGroup:
		_, ok := CanonicalGroupSubject(o.Group)
		return ok && o.Person == ""
	}
	return false
}

// TemplateGroupAdmin is a bounded template-management grant: Person may
// publish and edit templates for Group, and nothing else. An organisation
// administrator writes and revokes it through an audited route. It is not a
// general administrator role, and it is not implied by group membership.
type TemplateGroupAdmin struct {
	Group     string `json:"group_id"`
	Person    string `json:"person"`
	GrantedBy string `json:"granted_by,omitempty"`
}

// TemplateActor is the caller, as the server verified them for this request.
type TemplateActor struct {
	// Person is the authenticated principal's canonical subject.
	Person string
	// OrgAdmin is the current organisation-administrator tier (the one that
	// writes the organisation's presets, policies and components).
	OrgAdmin bool
	// Groups is the verified group snapshot, each entry canonical. Nil means
	// the snapshot is unavailable (a cookie from before groups were recorded).
	Groups []string
	// GroupsTruncated reports a snapshot that dropped entries or never arrived
	// in full (cookie byte cap, an IdP groups overage, a token whose
	// completeness was not recorded). Group authority cannot be answered from
	// it, in either direction.
	GroupsTruncated bool
	// GroupAdminGrants are the groups the store holds a TemplateGroupAdmin
	// grant for this person on.
	GroupAdminGrants []string
}

// TemplateAction is what a caller asks to do with a template.
type TemplateAction string

const (
	// TemplateRead is list, direct and versioned lookup, and use.
	TemplateRead TemplateAction = "read"
	// TemplateWrite is create, update, delete, and the target side of a copy
	// or publish.
	TemplateWrite TemplateAction = "write"
)

// TemplateDenial is why TemplateAuthorize refused. The zero value is allowed.
type TemplateDenial string

const (
	TemplateAllowed TemplateDenial = ""
	// TemplateDenyNotOwner: someone else's personal template. A read answers
	// it as a missing template, so the id is no existence oracle.
	TemplateDenyNotOwner TemplateDenial = "not_owner"
	// TemplateDenyNotOrgAdmin: org scope is written by an organisation administrator only.
	TemplateDenyNotOrgAdmin TemplateDenial = "not_org_admin"
	// TemplateDenyNotGroupMember: group scope is read by the group's members only.
	TemplateDenyNotGroupMember TemplateDenial = "not_group_member"
	// TemplateDenyNotGroupAdmin: group scope is written by that group's administrators only.
	TemplateDenyNotGroupAdmin TemplateDenial = "not_group_admin"
	// TemplateDenyGroupUnverified: the group snapshot is unavailable or
	// truncated, so membership and administration cannot be established.
	TemplateDenyGroupUnverified TemplateDenial = "group_unverified"
	// TemplateDenyBadOwner: the owner is malformed (a refusal, never a default).
	TemplateDenyBadOwner TemplateDenial = "bad_owner"
)

// TemplateAuthorize decides one action on a template of the given owner.
//
//   - Person scope: the owner reads and writes; nobody else does, an
//     organisation administrator included.
//   - Org scope: any signed-in person reads; an organisation administrator
//     writes.
//   - Group scope: a verified member reads; a verified member who holds the
//     group's template-management grant writes. A grant on group A says
//     nothing about group B, and a grant outlives nothing: once the person
//     leaves the group in the verified snapshot, they are no administrator of
//     it either. An organisation administrator is not implicitly a group
//     administrator; they delegate through the grant.
//
// Group answers need a complete snapshot. Nil or truncated gives
// TemplateDenyGroupUnverified, never an empty-groups reading.
func TemplateAuthorize(actor TemplateActor, owner TemplateOwner, action TemplateAction) TemplateDenial {
	if !owner.Valid() || actor.Person == "" || (action != TemplateRead && action != TemplateWrite) {
		return TemplateDenyBadOwner
	}
	switch owner.Scope {
	case TemplateScopePerson:
		if actor.Person != owner.Person {
			return TemplateDenyNotOwner
		}
	case TemplateScopeOrg:
		if action == TemplateWrite && !actor.OrgAdmin {
			return TemplateDenyNotOrgAdmin
		}
	case TemplateScopeGroup:
		group, _ := CanonicalGroupSubject(owner.Group)
		if actor.Groups == nil || actor.GroupsTruncated {
			return TemplateDenyGroupUnverified
		}
		if !slices.Contains(actor.Groups, group) {
			return TemplateDenyNotGroupMember
		}
		if action == TemplateWrite && !slices.Contains(actor.GroupAdminGrants, group) {
			return TemplateDenyNotGroupAdmin
		}
	}
	return TemplateAllowed
}

// TemplateCopyAuthorize decides copying or publishing a template from one
// owner to another: the caller must read the source and write the target.
// Owning a personal template does not by itself let its content reach a wider
// audience; the store still re-checks every reference the copy discloses.
func TemplateCopyAuthorize(actor TemplateActor, from, to TemplateOwner) TemplateDenial {
	if d := TemplateAuthorize(actor, from, TemplateRead); d != TemplateAllowed {
		return d
	}
	return TemplateAuthorize(actor, to, TemplateWrite)
}

// TemplateTier is the authority a template route asks for, per scope.
type TemplateTier string

const (
	// TemplateTierNone: the route does not act on that scope.
	TemplateTierNone TemplateTier = ""
	// TemplateTierOwner: the person who owns the template.
	TemplateTierOwner TemplateTier = "owner"
	// TemplateTierMember: any signed-in person.
	TemplateTierMember TemplateTier = "member"
	// TemplateTierOrgAdmin: an organisation administrator.
	TemplateTierOrgAdmin TemplateTier = "org_admin"
	// TemplateTierGroupMember: a verified member of that group.
	TemplateTierGroupMember TemplateTier = "group_member"
	// TemplateTierGroupAdmin: a verified administrator of that group, and of no other.
	TemplateTierGroupAdmin TemplateTier = "group_admin"
)

// TemplateRoute classifies one future template route: the router tier that
// gates reaching the handler, and the per-scope authority the handler then
// enforces with TemplateAuthorize. RouterClass uses authz_test.go's route
// class names. The routes are not registered yet; the storage lane registers
// them, moves each into routeMatrix, and deletes
// TestTemplateRoutesAreNotRegisteredYet.
type TemplateRoute struct {
	Route       string
	RouterClass string
	Person      TemplateTier
	Org         TemplateTier
	Group       TemplateTier
}

// TemplateRoutes is the route and authority table for the template surface.
var TemplateRoutes = []TemplateRoute{
	{Route: "GET /api/v1/templates", RouterClass: "member", Person: TemplateTierOwner, Org: TemplateTierMember, Group: TemplateTierGroupMember},
	{Route: "GET /api/v1/templates/{id}", RouterClass: "member", Person: TemplateTierOwner, Org: TemplateTierMember, Group: TemplateTierGroupMember},
	{Route: "GET /api/v1/templates/{id}/revisions/{revision}", RouterClass: "member", Person: TemplateTierOwner, Org: TemplateTierMember, Group: TemplateTierGroupMember},
	{Route: "POST /api/v1/templates", RouterClass: "member", Person: TemplateTierOwner, Org: TemplateTierOrgAdmin, Group: TemplateTierGroupAdmin},
	{Route: "PUT /api/v1/templates/{id}", RouterClass: "member", Person: TemplateTierOwner, Org: TemplateTierOrgAdmin, Group: TemplateTierGroupAdmin},
	{Route: "DELETE /api/v1/templates/{id}", RouterClass: "member", Person: TemplateTierOwner, Org: TemplateTierOrgAdmin, Group: TemplateTierGroupAdmin},
	{Route: "POST /api/v1/templates/{id}/copy", RouterClass: "member", Person: TemplateTierOwner, Org: TemplateTierOrgAdmin, Group: TemplateTierGroupAdmin},
	{Route: "POST /api/v1/templates/import", RouterClass: "member"},
	{Route: "GET /api/v1/admin/template-group-admins", RouterClass: "admin"},
	{Route: "PUT /api/v1/admin/template-group-admins", RouterClass: "admin"},
	{Route: "DELETE /api/v1/admin/template-group-admins", RouterClass: "admin"},
}
