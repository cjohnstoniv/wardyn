// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package types

import "time"

// ServiceIdentityState is where a service identity stands. It is derived from the row and the sponsor's
// session, never stored.
type ServiceIdentityState string

const (
	ServiceIdentityActive          ServiceIdentityState = "active"
	ServiceIdentitySuspended       ServiceIdentityState = "suspended"
	ServiceIdentityRetired         ServiceIdentityState = "retired"
	ServiceIdentitySponsorInactive ServiceIdentityState = "sponsor_inactive"
)

// ServicePrincipalPrefix begins the principal of a service identity.
const ServicePrincipalPrefix = "service:"

// ServiceIdentity is a named, sponsored non-human principal (migration 0157, #1508). Its principal is
// ServicePrincipalPrefix + Name, and its tokens are ordinary api_tokens rows.
type ServiceIdentity struct {
	Name         string               `json:"name"`
	Description  string               `json:"description,omitempty"`
	Sponsor      string               `json:"sponsor"`
	SponsorSince time.Time            `json:"sponsor_since"`
	UserType     string               `json:"user_type"`
	CreatedBy    string               `json:"created_by"`
	CreatedAt    time.Time            `json:"created_at"`
	SuspendedAt  *time.Time           `json:"suspended_at,omitempty"`
	SuspendedBy  string               `json:"suspended_by,omitempty"`
	RetiredAt    *time.Time           `json:"retired_at,omitempty"`
	RetiredBy    string               `json:"retired_by,omitempty"`
	State        ServiceIdentityState `json:"state"`
}

// Principal is the identity's principal, "service:<name>".
func (s ServiceIdentity) Principal() string { return ServicePrincipalPrefix + s.Name }

// Usable reports whether the record alone lets a token authenticate: neither suspended nor retired. The
// sponsor's session is a second, separate check.
func (s ServiceIdentity) Usable() bool { return s.SuspendedAt == nil && s.RetiredAt == nil }

// BaseState is the state the record alone gives: retired, suspended or active. A sponsor whose session is
// not live makes an active identity sponsor_inactive, which only the API layer can read.
func (s ServiceIdentity) BaseState() ServiceIdentityState {
	switch {
	case s.RetiredAt != nil:
		return ServiceIdentityRetired
	case s.SuspendedAt != nil:
		return ServiceIdentitySuspended
	}
	return ServiceIdentityActive
}
