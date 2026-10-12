// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/audit"
	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// queueAuthority is the identity a launch was admitted under, kept on its run_queue row so the start,
// which has no session, can act as the owner with what the owner held at launch (pool-queue.md section
// 15, Q8-B): the sub, email, user type and role the session carried, the group snapshot with its
// truncation bit, the authority epoch, and the portal a delegated launch came through. It is personal
// data: it goes with the row, and the leaver purge deletes it.
type queueAuthority struct {
	// Sub is the verified human (an IdP sub, a service identity's principal); empty for the admin token
	// and for local mode, which has Local instead.
	Sub   string `json:"sub,omitempty"`
	Local string `json:"local,omitempty"`
	Email string `json:"email,omitempty"`
	Role  string `json:"role,omitempty"`
	// UserType and Groups are the snapshot the ceiling and the capability rows resolve against. Groups
	// keeps nil (no snapshot: unanswerable) apart from empty (a complete, empty one).
	UserType        string   `json:"user_type,omitempty"`
	Groups          []string `json:"groups"`
	GroupsTruncated bool     `json:"groups_truncated,omitempty"`
	// Epoch is the session's authority epoch; absent for an API token or the admin token.
	Epoch *int64 `json:"epoch,omitempty"`
	// Service and Sponsor are set when a service identity made the launch.
	Service string `json:"service,omitempty"`
	Sponsor string `json:"sponsor,omitempty"`
	// Delegate and Grant are the portal and delegated token of a portal launch. Grant only fills the
	// audit rows' via; the start never reads the token's ten-minute expiry.
	Delegate *uuid.UUID `json:"delegate,omitempty"`
	Grant    *uuid.UUID `json:"grant,omitempty"`
}

// captureQueueAuthority reads the launch's identity off the request context.
func captureQueueAuthority(ctx context.Context) queueAuthority {
	a := queueAuthority{
		Sub: oidcHumanFromContext(ctx), Local: localPrincipalFromContext(ctx), Email: oidcEmailFromContext(ctx),
		Role: oidcRoleFromContext(ctx), UserType: oidcUserTypeFromContext(ctx), Groups: oidcGroupsFromContext(ctx),
		GroupsTruncated: oidcGroupsTruncatedFromContext(ctx),
	}
	if e, ok := oidc.AuthorityEpochFromContext(ctx); ok {
		a.Epoch = &e
	}
	if name, sponsor, ok := serviceIdentityFromContext(ctx); ok {
		a.Service, a.Sponsor = name, sponsor
	}
	if via, ok := audit.DelegationFrom(ctx); ok {
		a.Delegate, a.Grant = &via.Delegate, &via.Grant
	}
	return a
}

// context puts the identity back on ctx, so everything the fold reads off a request (the ceiling, the
// capability rows, the pool catalogue, the secret namespace) resolves for the owner as it did at launch.
func (a queueAuthority) context(ctx context.Context) context.Context {
	switch {
	case a.Local != "":
		ctx = withLocalPrincipal(ctx, a.Local)
	case a.Sub != "":
		ctx = withHumanIdentity(ctx, a.Sub, a.Email, a.Role, a.UserType, a.Groups, a.GroupsTruncated)
	}
	if a.Service != "" {
		ctx = withServiceIdentity(ctx, a.Service, a.Sponsor)
	}
	if v, ok := a.via(); ok {
		ctx = audit.WithDelegation(ctx, v)
	}
	return ctx
}

// via is the portal a delegated launch came through.
func (a queueAuthority) via() (types.DelegationVia, bool) {
	if a.Delegate == nil || a.Grant == nil {
		return types.DelegationVia{}, false
	}
	return types.DelegationVia{Delegate: *a.Delegate, Grant: *a.Grant}, true
}
