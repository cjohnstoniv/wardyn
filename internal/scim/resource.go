// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package scim

import "slices"

// ParseUser parses a POST /Users body or a PUT /Users/{id} replacement. userName and externalId are
// required (invalidValue). An omitted active means true; Entra's strings "True" and "False" are accepted.
// The client's id and meta are read-only and dropped.
func ParseUser(body []byte) (*User, *Error) {
	var in struct {
		ExternalID  string   `json:"externalId"`
		UserName    string   `json:"userName"`
		DisplayName string   `json:"displayName"`
		Emails      []Email  `json:"emails"`
		Active      flexBool `json:"active"`
	}
	if err := decodeOne(body, &in); err != nil {
		return nil, err
	}
	if in.UserName == "" {
		return nil, badRequest(TypeInvalidValue, "userName is required")
	}
	if in.ExternalID == "" {
		return nil, badRequest(TypeInvalidValue, "externalId is required: map the Entra objectId to externalId")
	}
	active := !in.Active.set || in.Active.v
	return &User{
		Schemas: []string{SchemaUser}, ExternalID: in.ExternalID, UserName: in.UserName,
		DisplayName: in.DisplayName, Emails: in.Emails, Active: active,
	}, nil
}

// ParseGroup parses a POST /Groups body. displayName and externalId are required (invalidValue).
func ParseGroup(body []byte) (*Group, *Error) {
	var in struct {
		ExternalID  string   `json:"externalId"`
		DisplayName string   `json:"displayName"`
		Members     []Member `json:"members"`
	}
	if err := decodeOne(body, &in); err != nil {
		return nil, err
	}
	if in.DisplayName == "" {
		return nil, badRequest(TypeInvalidValue, "displayName is required")
	}
	if in.ExternalID == "" {
		return nil, badRequest(TypeInvalidValue, "externalId is required: map the Entra objectId to externalId")
	}
	if slices.ContainsFunc(in.Members, func(m Member) bool { return m.Value == "" }) {
		return nil, badRequest(TypeInvalidValue, "every member needs a value")
	}
	return &Group{Schemas: []string{SchemaGroup}, ExternalID: in.ExternalID, DisplayName: in.DisplayName, Members: in.Members}, nil
}
