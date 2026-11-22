// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/audit"
	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/authz"
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
	Delegate *uuid.UUID  `json:"delegate,omitempty"`
	Grant    *queueGrant `json:"grant,omitempty"`
	// Actor is the agent the portal named on that token; the start's rows replay it beside the portal.
	Actor *types.DelegationActor `json:"actor,omitempty"`
	// Device is the laptop that submitted the launch, which carries no person: the start replays the fold
	// as that device, so the rules a device keeps at the create door hold when the run is promoted.
	Device *uuid.UUID `json:"device,omitempty"`
	// BreakGlass is the reason an admin-token launch carried as a break-glass run, so the start
	// resolves as the break-glass run it was tagged at launch.
	BreakGlass string `json:"break_glass_reason,omitempty"`
	// WorkloadOwner and WorkloadName are the workload the launch was for, so the start mints the run
	// under that workload's SPIFFE ID as an unqueued launch would (withLaunchWorkload); empty otherwise.
	WorkloadOwner string `json:"workload_owner,omitempty"`
	WorkloadName  string `json:"workload_name,omitempty"`
	// Binding is the namespace binding a custom resource's launch was authorised under: the start holds
	// only while that binding exists (store.QueueStart.Binding).
	Binding *uuid.UUID `json:"binding,omitempty"`
	// PolicyPin is the workload's policy pin the launch carried (policyPinOf): the start holds the replayed
	// fold to it, as the create was held.
	PolicyPin string `json:"policy_pin,omitempty"`
}

// queuePortalGrantPrefix marks the grant of an issuer-portal launch in the stored authority: the field holds
// "portal:<uuid>" there and a bare uuid for a console portal. The prefix is 7 characters, so the whole is 43,
// a length uuid.UUID's own text decoding refuses: a 0.10.1 worker decodes the field into a *uuid.UUID, fails,
// and ends the run before any authority is bound (run_queue_start.go, newQueuedStart), never starting it
// under a console user type. The prefix must never be 9 characters ("urn:uuid:"), which that decoder reads.
const queuePortalGrantPrefix = "portal:"

// queueGrant is the delegated token a portal launch carried, and whether it came through an issuer portal.
type queueGrant struct {
	ID     uuid.UUID
	Portal bool
}

// MarshalJSON writes the bare uuid for a console portal and "portal:<uuid>" for an issuer portal.
func (g queueGrant) MarshalJSON() ([]byte, error) {
	text := g.ID.String()
	if g.Portal {
		text = queuePortalGrantPrefix + text
	}
	return json.Marshal(text)
}

// UnmarshalJSON reads either form.
func (g *queueGrant) UnmarshalJSON(b []byte) error {
	var text string
	if err := json.Unmarshal(b, &text); err != nil {
		return err
	}
	rest, portal := strings.CutPrefix(text, queuePortalGrantPrefix)
	id, err := uuid.Parse(rest)
	if err != nil {
		return fmt.Errorf("queue authority grant: %w", err)
	}
	g.ID, g.Portal = id, portal
	return nil
}

// portalIssuerLaunch reports whether a is an issuer-portal person's launch: its grant is encoded as one, or
// its principal is a portal person. Either marks the launch, so a row missing one mark is still never run
// under the console's derivation.
func (a queueAuthority) portalIssuerLaunch() bool {
	return (a.Grant != nil && a.Grant.Portal) || types.IsPortalPersonPrincipal(a.Sub)
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
		a.Delegate, a.Grant, a.Actor = &via.Delegate, &queueGrant{ID: via.Grant, Portal: types.IsPortalPersonPrincipal(a.Sub)}, via.Actor
	}
	if d, ok := deviceFromContext(ctx); ok {
		a.Device = &d.ID
	}
	a.BreakGlass = breakGlassReason(ctx)
	w := launchWorkloadFrom(ctx)
	a.WorkloadOwner, a.WorkloadName = w.Owner, w.Name
	if b, ok := launchBindingFrom(ctx); ok {
		a.Binding = &b
	}
	if p := policyPinOf(ctx); p != nil {
		a.PolicyPin = p.want
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
	if a.Device != nil {
		ctx = context.WithValue(ctx, deviceCtxKey{}, types.Device{ID: *a.Device})
	}
	if v, ok := a.via(); ok {
		ctx = audit.WithDelegation(ctx, v)
	}
	if a.BreakGlass != "" {
		ctx = context.WithValue(ctx, breakGlassKey{}, a.BreakGlass)
	}
	if a.PolicyPin != "" {
		ctx, _ = withPolicyPin(ctx, a.PolicyPin)
	}
	return ctx
}

// via is the portal a delegated launch came through.
func (a queueAuthority) via() (types.DelegationVia, bool) {
	if a.Delegate == nil || a.Grant == nil {
		return types.DelegationVia{}, false
	}
	return types.DelegationVia{Delegate: *a.Delegate, Grant: a.Grant.ID, Actor: a.Actor}, true
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
	if a.portalIssuerLaunch() {
		return s.queuePortalIssuerRole(st)
	}
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

// The ends a portal launch's start gives that no console launch has.
const (
	queueEndUserTypeUnknown   = string(authz.ReasonUserTypeUnknown)
	queueEndPortalUnitScope   = "portal_unit_scope"
	queueEndPortalRunScope    = "portal_run_scope"
	queueUserTypeUnknownHint  = "The user type this portal gives its people no longer exists, so this run was not started."
	queuePortalUnitScopeHint  = "This run's unit is no longer under the portal's unit, so it was not started."
	queuePortalPrincipalError = "Queue portal start: the principal is not one of the portal's people"
)

// queuePortalRunScope ends a run that mounted a portal's drive unless it starts through the portal that created it: the
// row's creator (immutable) must be the portal the stored authority names, or the replay would resolve one portal's
// drive and attribute it to the other. It runs for every queued start, ahead of queueCurrentRole, because the fold reads
// the authority's delegate whether or not the authority marks itself a portal launch (portalIssuerLaunch), so a row whose
// marks and delegate disagree is judged by the delegate too. A run with no portal drive is not touched.
func queuePortalRunScope(run types.AgentRun, a queueAuthority) *queueEnd {
	if run.PortalDrives && (run.CreatedVia == nil || a.Delegate == nil || *run.CreatedVia != *a.Delegate) {
		return &queueEnd{token: queueEndPortalRunScope, hint: portalRunScopeMsg}
	}
	return nil
}

// queuePortalIssuerRole is queueCurrentRole for an issuer-portal person's launch (the stored authority marks
// it). The role stays user and is never derived. No role mapping or user-type-from-mapping is read: the user
// type is the live portal row's, and must still exist. The portal row must be live (queueOwnerStanding), the
// principal must be one of that portal's people (the reverse check), and the unit the launch froze must still
// be under the portal's pin in the tree as it stands (the resolved context is then held to it by sameUnit).
func (s *Server) queuePortalIssuerRole(st *queuedStart) *queueEnd {
	a := st.auth
	failed := &queueEnd{hint: queueStartFailedHint}
	via, ok := a.via()
	dr, drOK := s.cfg.Store.(store.DelegateReader)
	if !ok || !drOK {
		return failed
	}
	d, err := dr.GetDelegateByID(st.ctx, via.Delegate)
	switch {
	case errors.Is(err, store.ErrNotFound):
		return &queueEnd{token: string(types.RunEndQueuePortalRevoked), hint: types.RunEndQueuePortalRevoked.Sentence()}
	case err != nil:
		return failed
	case d.RevokedAt != nil:
		return &queueEnd{token: string(types.RunEndQueuePortalRevoked), hint: types.RunEndQueuePortalRevoked.Sentence()}
	}
	if d.Issuer == "" || d.IssuerKey == "" || !strings.HasPrefix(a.Sub, types.PortalPersonPrefix+d.IssuerKey+":") {
		slog.ErrorContext(st.base, queuePortalPrincipalError, slog.String("run_id", st.run.ID.String()), slog.String("delegate", d.ID.String()))
		return failed
	}
	userTypes, err := s.cfg.Store.ListUserTypes(st.ctx)
	if err != nil {
		return failed
	}
	found := false
	for _, t := range userTypes {
		if t.ID == d.UserType {
			found = true
			break
		}
	}
	if !found {
		return &queueEnd{token: queueEndUserTypeUnknown, hint: queueUserTypeUnknownHint}
	}
	pin, refusal := s.portalPinOf(st.ctx)
	switch {
	case refusal != nil:
		return failed
	case !pin.holds(st.launch.UnitID): // the unit frozen at enqueue; the row's own unit is stamped by the start
		return &queueEnd{token: queueEndPortalUnitScope, hint: queuePortalUnitScopeHint}
	}
	a.Role, a.UserType = oidc.RoleUser, d.UserType
	if err := st.bind(a); err != nil {
		return failed
	}
	return nil
}
