// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/audit"
	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// queueOwnerCheckTries and queueOwnerCheckBackoff bound how often the start asks an owner check the store
// could not answer before it fails the run: a busy store is not a reason to end a run that waited.
const (
	queueOwnerCheckTries   = 3
	queueOwnerCheckBackoff = 500 * time.Millisecond
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
	// Seq is the revocation sequence the credential that admitted the launch recorded at issue
	// (oidc.RevocationSeqFor); absent when it recorded none, and the owner check compares on time.
	Seq int64 `json:"rseq,omitempty"`
	// Service and Sponsor are set when a service identity made the launch.
	Service string `json:"service,omitempty"`
	Sponsor string `json:"sponsor,omitempty"`
	// Delegate and Grant are the portal and delegated token of a portal launch. Grant only fills the
	// audit rows' via; the start never reads the token's ten-minute expiry.
	Delegate *uuid.UUID `json:"delegate,omitempty"`
	Grant    *uuid.UUID `json:"grant,omitempty"`
	// Device is the laptop that submitted the launch, which carries no person: the start replays the fold
	// as that device, so the rules a device keeps at the create door hold when the run is promoted.
	Device *uuid.UUID `json:"device,omitempty"`
	// BreakGlass is the reason an admin-token launch carried as a break-glass run, so the start
	// resolves as the break-glass run it was tagged at launch.
	BreakGlass string `json:"break_glass_reason,omitempty"`
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
	a.Seq = oidc.RevocationSeqFor(ctx, a.Sub)
	if name, sponsor, ok := serviceIdentityFromContext(ctx); ok {
		a.Service, a.Sponsor = name, sponsor
	}
	if via, ok := audit.DelegationFrom(ctx); ok {
		a.Delegate, a.Grant = &via.Delegate, &via.Grant
	}
	if d, ok := deviceFromContext(ctx); ok {
		a.Device = &d.ID
	}
	a.BreakGlass = breakGlassReason(ctx)
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
	if a.Device != nil {
		ctx = context.WithValue(ctx, deviceCtxKey{}, types.Device{ID: *a.Device})
	}
	if v, ok := a.via(); ok {
		ctx = audit.WithDelegation(ctx, v)
	}
	if a.BreakGlass != "" {
		ctx = context.WithValue(ctx, breakGlassKey{}, a.BreakGlass)
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

// identityBlocker is the store's one-statement identity read (store.PG.IdentityBlocked): it needs no
// connection of its own, so a start never waits on, or is refused for, the identity guard's dials.
type identityBlocker interface {
	IdentityBlocked(ctx context.Context, principal string, epoch int64) (bool, error)
}

// queueOwnerLive reports whether a's owner may still act as they did at queuedAt, the moment the queue
// row was written, which is the issue time of the authority it holds: their identity is not deactivated
// or past the epoch the launch carried, no session revocation since cuts them off (oidc.CheckSession,
// the check every stored credential makes against its issue: by the revocation sequence the launching
// credential recorded, else by queuedAt), and a service identity is not retired,
// still has the sponsor it launched under, and that sponsor's session is live. The admin token and local
// mode have no one to revoke.
func (s *Server) queueOwnerLive(ctx context.Context, a queueAuthority, queuedAt time.Time) (bool, error) {
	if a.Sub == "" {
		return true, nil
	}
	epoch := int64(-1)
	if a.Epoch != nil {
		epoch = *a.Epoch
	}
	if ib, ok := s.cfg.Store.(identityBlocker); ok {
		if blocked, err := ib.IdentityBlocked(ctx, a.Sub, epoch); err != nil || blocked {
			return false, err
		}
	}
	if s.cfg.SessionRevocations != nil {
		if status, err := oidc.CheckSession(ctx, s.cfg.SessionRevocations, a.Sub, a.Email, oidc.Issue{At: queuedAt, Seq: a.Seq}, epoch); err != nil || status != oidc.SessionLive {
			return false, err
		}
	}
	if a.Service == "" {
		return true, nil
	}
	ss, ok := s.cfg.Store.(store.ServiceIdentityStore)
	if !ok || s.cfg.SessionRevocations == nil {
		return false, nil // no store to ask about the identity or its sponsor: fail closed, as its tokens do
	}
	switch still, err := ss.ServiceStillSponsored(ctx, a.Service, a.Sponsor, queuedAt); {
	case errors.Is(err, store.ErrNotFound):
		return false, nil
	case err != nil || !still:
		return false, err
	}
	// a.Seq is the service token's sequence, drawn against the sponsor's cuts when the token was minted.
	status, err := oidc.CheckSession(ctx, s.cfg.SessionRevocations, a.Sponsor, "", oidc.Issue{At: queuedAt, Seq: a.Seq}, -1)
	return err == nil && status == oidc.SessionLive, err
}

// queueCurrentRole judges the start by the owner's role and user type as the current role mappings and
// user types derive them from the email and groups the launch carried, as GET /people derives a person's
// role, so a demotion or a new user type made while the run waited is what the start sees. The role only
// ever narrows (narrowRole). The groups are the launch's own: a group removed at the identity provider is
// not seen before the run's wait ends (pool-queue.md section 17). A derivation that would refuse the
// person's sign-in ends the run.
func (s *Server) queueCurrentRole(st *queuedStart) *queueEnd {
	a := st.auth
	if s.cfg.OIDC == nil || a.Sub == "" || a.Service != "" {
		return nil
	}
	rows, err := s.cfg.Store.ListRoleMappings(st.ctx)
	if err != nil {
		return &queueEnd{hint: queueStartFailedHint}
	}
	userTypes, err := s.cfg.Store.ListUserTypes(st.ctx)
	if err != nil {
		return &queueEnd{hint: queueStartFailedHint}
	}
	d := s.cfg.OIDC.PreviewRoleAgainst(toOIDCRoleMappings(rows), userTypes, nil, a.Groups, a.Email)
	if !d.OK() {
		return &queueEnd{token: string(types.RunEndIdentityDeactivated), hint: types.RunEndIdentityDeactivated.Sentence()}
	}
	a.Role, a.UserType = narrowRole(a.Role, d.Role), d.UserType
	if err := st.bind(a); err != nil {
		return &queueEnd{hint: queueStartFailedHint}
	}
	return nil
}

// narrowRole is the role a start acts with: the launch's when the derivation still gives it, else the
// user tier. A start never gains a tier, and the two admin tiers are not each other's subset.
func narrowRole(launched, derived string) string {
	if derived == launched {
		return launched
	}
	return oidc.RoleUser
}
