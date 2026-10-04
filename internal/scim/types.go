// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Package scim is the pure SCIM 2.0 wire layer Wardyn serves to an identity provider's provisioning
// client: types, the one filter form, PatchOp parsing (including the non-RFC shapes Entra sends) and the
// RFC 7644 error envelope. It has no HTTP handlers, no store and no routes.
package scim

// Schema URNs and the media type of every SCIM body.
const (
	SchemaUser         = "urn:ietf:params:scim:schemas:core:2.0:User"
	SchemaGroup        = "urn:ietf:params:scim:schemas:core:2.0:Group"
	SchemaListResponse = "urn:ietf:params:scim:api:messages:2.0:ListResponse"
	SchemaPatchOp      = "urn:ietf:params:scim:api:messages:2.0:PatchOp"
	SchemaError        = "urn:ietf:params:scim:api:messages:2.0:Error"
	MediaType          = "application/scim+json"
)

// Email is one entry of a User's emails.
type Email struct {
	Value   string `json:"value"`
	Type    string `json:"type,omitempty"`
	Primary bool   `json:"primary,omitempty"`
}

// Meta is the resource metadata block. A client's meta is ignored on input.
type Meta struct {
	ResourceType string `json:"resourceType,omitempty"`
	Created      string `json:"created,omitempty"`
	LastModified string `json:"lastModified,omitempty"`
	Location     string `json:"location,omitempty"`
}

// User is the SCIM User subset Wardyn serves. Attributes outside it are dropped on parse.
//
// ExternalID is required: Entra's default mapping sends the Entra objectId as externalId, and Wardyn
// binds the projection to a sign-in identity through it. ParseUser refuses a User without one.
type User struct {
	Schemas     []string `json:"schemas"`
	ID          string   `json:"id,omitempty"`
	ExternalID  string   `json:"externalId"`
	UserName    string   `json:"userName"`
	DisplayName string   `json:"displayName,omitempty"`
	Emails      []Email  `json:"emails,omitempty"`
	Active      bool     `json:"active"`
	Meta        *Meta    `json:"meta,omitempty"`
}

// Member is one entry of a Group's members or of a members PatchOp value. Ref is `$ref`, which Entra
// sends as null.
type Member struct {
	Value   string `json:"value"`
	Ref     string `json:"$ref,omitempty"`
	Display string `json:"display,omitempty"`
}

// Group is the SCIM Group subset Wardyn serves. ExternalID is required for the same reason as User's.
type Group struct {
	Schemas     []string `json:"schemas"`
	ID          string   `json:"id,omitempty"`
	ExternalID  string   `json:"externalId"`
	DisplayName string   `json:"displayName"`
	Members     []Member `json:"members,omitempty"`
	Meta        *Meta    `json:"meta,omitempty"`
}

// ListResponse is the RFC 7644 list envelope. Wardyn does no paging, so StartIndex is always 1.
type ListResponse[T any] struct {
	Schemas      []string `json:"schemas"`
	TotalResults int      `json:"totalResults"`
	StartIndex   int      `json:"startIndex"`
	ItemsPerPage int      `json:"itemsPerPage"`
	Resources    []T      `json:"Resources"`
}

// NewListResponse renders resources as one page; a nil slice becomes `[]`, never null.
func NewListResponse[T any](resources []T) ListResponse[T] {
	if resources == nil {
		resources = []T{}
	}
	return ListResponse[T]{
		Schemas: []string{SchemaListResponse}, TotalResults: len(resources), StartIndex: 1,
		ItemsPerPage: len(resources), Resources: resources,
	}
}
