// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"net/http"
	"slices"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// assistantActorCtxKey marks a context built by assistantToolCtx. neverOperator reads it, so a bug that
// loses the person reads as a member, never as the admin token.
type assistantActorCtxKey struct{}

func withAssistantActor(ctx context.Context) context.Context {
	return context.WithValue(ctx, assistantActorCtxKey{}, true)
}

func assistantActorFromContext(ctx context.Context) bool {
	v, _ := ctx.Value(assistantActorCtxKey{}).(bool)
	return v
}

// assistantSnapshot is who the person was when the request that carries it was made: the six identity
// values that request published. A session stores one at create; every tool call acts from one.
type assistantSnapshot struct {
	Principal       string
	Email           string
	Role            string
	UserType        string
	Groups          []string
	GroupsTruncated bool
}

// assistantSnapshotOf reads the six values off the request's context, exactly as the queued-run authority
// reads them, whichever auth branch published them.
func assistantSnapshotOf(ctx context.Context) assistantSnapshot {
	return assistantSnapshot{
		Principal: oidcHumanFromContext(ctx), Email: oidcEmailFromContext(ctx), Role: oidcRoleFromContext(ctx),
		UserType: oidcUserTypeFromContext(ctx), Groups: oidcGroupsFromContext(ctx), GroupsTruncated: oidcGroupsTruncatedFromContext(ctx),
	}
}

func assistantSnapshotOfSession(sess types.AssistantSession) assistantSnapshot {
	return assistantSnapshot{
		Principal: sess.Principal, Email: sess.Email, Role: sess.Role, UserType: sess.UserType,
		Groups: sess.Groups, GroupsTruncated: sess.GroupsTruncated,
	}
}

// errAssistantRoleDenied is a refusal of the person: no principal, an erased one, or a role mapping that
// no longer lets them in. The caller ends the session as role_denied and answers 403.
var errAssistantRoleDenied = errors.New("api: the assistant's person is no longer allowed")

// assistantNoPerson reports a snapshot that names nobody: empty, or the erased marker.
func assistantNoPerson(snap assistantSnapshot) bool {
	return snap.Principal == "" || snap.Principal == types.AssistantErasedPrincipal
}

// assistantEffectiveRole is the role a tool call acts at: the security tier when the snapshot and the
// fresh role both hold it, else user. Both are asked of the tier predicates, never compared as strings.
// denied is true when sign-in would now refuse the person.
func (s *Server) assistantEffectiveRole(snapRole, freshRole string, snapTruncated bool) (role string, denied bool) {
	if freshRole == accessDeniedRole {
		return "", true
	}
	if snapTruncated || freshRole == roleUnknown {
		return oidc.RoleUser, false
	}
	if s.isSecurityOperator(roleSnapshotCtx(snapRole)) && s.isSecurityOperator(roleSnapshotCtx(freshRole)) {
		return oidc.RoleSecurityAdmin, false
	}
	return oidc.RoleUser, false
}

// assistantGroups is the groups a tool call acts with: those both the snapshot and the person's latest
// sign-in hold. When either side is truncated there are none, and the context says so.
func assistantGroups(snap, fresh []string, snapTruncated, freshTruncated bool) (groups []string, truncated bool) {
	if snapTruncated || freshTruncated {
		return []string{}, true
	}
	groups = []string{}
	for _, g := range snap {
		if slices.Contains(fresh, g) {
			groups = append(groups, g)
		}
	}
	return groups, false
}

// assistantFreshRole re-derives the person's role and groups from their last sign-in record, as
// GET /people derives them, through personDerivationResolver. A person with no record is unknown. It returns the
// role, the fresh groups and whether they are truncated.
func (s *Server) assistantFreshRole(r *http.Request, snap assistantSnapshot) (role string, groups []string, truncated bool, err error) {
	resolve, err := s.personDerivationResolver(r)
	if err != nil {
		return "", nil, false, err
	}
	rs, ok := s.cfg.Store.(principalGroupsReader)
	if !ok {
		return roleUnknown, nil, true, nil
	}
	rec, err := rs.GetPrincipalGroups(r.Context(), snap.Principal, "")
	switch {
	case errors.Is(err, store.ErrNotFound):
		return roleUnknown, nil, true, nil
	case err != nil:
		return "", nil, false, err
	}
	listing := store.PersonListing{Principal: snap.Principal, Email: rec.Email, Groups: rec.Groups,
		GroupsTruncated: rec.Truncated, GroupsVerified: true}
	return resolve(listing).Role, rec.Groups, rec.Truncated, nil
}

// assistantToolCtx is the context every tool call, preview and context read runs under: the person at the
// security tier at most, never the operator. It is built on a fresh base, never on r's context, which for a
// run-token request carries the run's claims and live run; the base is cancelled when r's context ends. The
// release func must be called when the work is done.
//
// Steps: the snapshot must name a person; the fresh role is re-derived and a denied one refuses; the role is
// the lower of the snapshot and the fresh one, capped at the security tier; the groups are the intersection.
// The identity is published through withHumanIdentity, so the user type and the e-mail answer as they do on
// the person's own request. Rows written through the assistant do not carry the member-view mark: it is not part
// of the snapshot, and the role the snapshot holds is already the clamped one.
func (s *Server) assistantToolCtx(r *http.Request, snap assistantSnapshot) (context.Context, func(), error) {
	if assistantNoPerson(snap) {
		return nil, nil, errAssistantRoleDenied
	}
	fresh, freshGroups, freshTruncated, err := s.assistantFreshRole(r, snap)
	if err != nil {
		return nil, nil, err
	}
	effective, denied := s.assistantEffectiveRole(snap.Role, fresh, snap.GroupsTruncated)
	if denied {
		return nil, nil, errAssistantRoleDenied
	}
	groups, truncated := assistantGroups(snap.Groups, freshGroups, snap.GroupsTruncated, freshTruncated)
	base, cancel := context.WithCancel(context.Background())
	stop := context.AfterFunc(r.Context(), cancel)
	ctx := withHumanIdentity(base, snap.Principal, snap.Email, effective, snap.UserType, groups, truncated)
	return withAssistantActor(ctx), func() { stop(); cancel() }, nil
}
