// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// A component withdrawn from a running run: the live credential doors (liveWithholding) ask again,
// for the run's owner, the component doors the run was launched through, as a revive asks them
// (ownerCapabilityRefusal), with the owner's groups taken from the record of their last sign-in through
// the door that launched the run.

// principalGroupsReader is the store that keeps the record of each person's last sign-in through each
// door: portal "" for an SSO sign-in to Wardyn itself, a portal's id for its token exchange.
type principalGroupsReader interface {
	GetPrincipalGroups(ctx context.Context, principal, portal string) (store.PrincipalGroups, error)
}

// The Postgres store keeps the record, so a deployment never skips the check for want of it.
var _ principalGroupsReader = store.PG{}

// componentWithdrawnMsgs is what a mint or redeem refused by componentWithdrawn tells the run, by reason.
var componentWithdrawnMsgs = map[string]string{
	reasonOwnerComponentGone: "wardyn: this run is no longer given credentials: a component it carries no longer exists; start a new run",
	reasonOwnerCapabilityComponent: "wardyn: this run is no longer given credentials: its owner no longer holds a component it carries. " +
		"If they should, they sign in again so Wardyn sees their groups, through the portal if a portal started the run; otherwise start a new run without the component",
}

// componentWithdrawn is why the run is no longer given credentials over a component in comps (its
// snapshot), or "": reasonOwnerComponentGone when an organisation component in it was deleted since
// launch, asked of every run as a revive asks it (componentGoneRefusal); reasonOwnerCapabilityComponent
// when the run's owner no longer holds one of the component doors the run was launched through
// (persistedLaunchDoors: an organisation component's grant, or the custom_component feature for one
// the owner defined). An operator-owned run has no person to ask. An owner whose groups are not on
// record is judged by sub and user type alone (capAllowedForSub), as an admin's revive of their run
// is: a grant held through a group is not seen, and any deny of the component refuses. A store that
// keeps no sign-in record checks nothing, as one that keeps no components does. A read error is
// returned, never an answer.
func (s *Server) componentWithdrawn(ctx context.Context, run types.AgentRun, comps []types.RunComponent) (string, error) {
	rs, ok := s.cfg.Store.(principalGroupsReader)
	if !ok || len(comps) == 0 {
		return "", nil
	}
	if ref, err := s.componentGoneRefusal(ctx, comps); err != nil || ref != nil {
		if err != nil {
			return "", err
		}
		return ref.reason, nil
	}
	if run.OperatorOwned {
		return "", nil
	}
	a, known, err := runOwnerAuthority(ctx, rs, run)
	if err != nil {
		return "", err
	}
	// Read as a display read: a user type deleted since launch withholds below, and its refusal row
	// (callerSubjects) would name the owner, who asked for nothing, once per mint. The door that asked
	// writes its own row. The mark's other readers on this path only withhold an audit row; none changes a
	// decision.
	owner := withDisplayRead(a.context(ctx))
	// The owner's own batch, never the request's: the request is the run's, not the owner's.
	b := s.newCapBatch(owner)
	// The zero run and no provider rows leave the component doors alone.
	for _, d := range persistedLaunchDoors(types.AgentRun{}, nil, comps) {
		var allowed bool
		if known {
			allowed, err = b.allowed(owner, d.kind, d.value)
		} else {
			allowed, err = s.capAllowedForSub(ctx, run.CreatedBy, run.UserType, d.kind, d.value)
		}
		if errors.Is(err, errUserTypeUnknown) {
			allowed, err = false, nil
		}
		if err != nil {
			return "", err
		}
		if allowed {
			continue
		}
		exempt, err := s.ownerExemptAsAdmin(ctx, run, a)
		if err != nil || exempt {
			return "", err
		}
		return reasonOwnerCapabilityComponent, nil
	}
	return "", nil
}

// runOwnerAuthority is the identity the run's owner resolves with at the live doors, in the shape a
// queued start replays (queueAuthority.context), always at the user role, so never the operator. A
// service identity has the complete, empty group snapshot and the sponsor recorded at launch, as its
// token publishes them; a device is its id alone and carries no person. A person has the email and
// groups of the record of their last sign-in through the door that launched the run (runPortal): a
// portal's token exchange for a run it launched, their sign-in to Wardyn itself for any other, so one
// door's wider claims never stand in for the other's. known is false for a person with no record there,
// whose groups and email cannot be known. The owner's prefix can be trusted: no human door admits a principal that
// spells one (isReservedPrincipal).
func runOwnerAuthority(ctx context.Context, rs principalGroupsReader, run types.AgentRun) (a queueAuthority, known bool, err error) {
	if name, ok := strings.CutPrefix(run.CreatedBy, types.ServicePrincipalPrefix); ok {
		return queueAuthority{Sub: run.CreatedBy, Role: oidc.RoleUser, UserType: run.UserType, Groups: []string{},
			Service: name, Sponsor: run.HumanAuthor}, true, nil
	}
	if raw, ok := strings.CutPrefix(run.CreatedBy, "device:"); ok {
		id, err := uuid.Parse(raw)
		if err != nil {
			return queueAuthority{}, false, fmt.Errorf("read the run's device owner: %w", err)
		}
		return queueAuthority{Device: &id}, true, nil
	}
	a = queueAuthority{Sub: run.CreatedBy, Role: oidc.RoleUser, UserType: run.UserType}
	rec, err := rs.GetPrincipalGroups(ctx, run.CreatedBy, runPortal(run))
	switch {
	case errors.Is(err, store.ErrNotFound):
		return a, false, nil
	case err != nil:
		return queueAuthority{}, false, fmt.Errorf("read the run owner's sign-in record: %w", err)
	}
	a.Email, a.Groups, a.GroupsTruncated = rec.Email, rec.Groups, rec.Truncated
	return a, true, nil
}

// runPortal is the door the run was launched through, as the sign-in record keys it: the portal's id for
// a run a portal launched, "" for any other.
func runPortal(run types.AgentRun) string {
	if run.CreatedVia == nil {
		return ""
	}
	return run.CreatedVia.String()
}

// ownerExemptAsAdmin reports whether the run's owner is an admin whose runs stand outside governance now
// (runUngoverned): by the role the current role mappings derive from the email and groups of their last
// sign-in, as a queued start (queueCurrentRole) and GET /people derive it. A run a portal launched was the
// person's at the user role; a service identity, a device and a person with no record are never an admin
// here. A partial group list can only lack groups, so it still makes an admin of one whom a mapping or
// the allowlist matches in what it holds, never of one whose admin role is the default role: a group it
// lacks may map them lower (roleIsTheDefault).
func (s *Server) ownerExemptAsAdmin(ctx context.Context, run types.AgentRun, a queueAuthority) (bool, error) {
	if s.cfg.OIDC == nil || run.CreatedVia != nil || a.Sub == "" || a.Service != "" || a.Groups == nil {
		return false, nil
	}
	rows, err := s.cfg.Store.ListRoleMappings(ctx)
	if err != nil {
		return false, fmt.Errorf("read the role mappings: %w", err)
	}
	userTypes, err := s.cfg.Store.ListUserTypes(ctx)
	if err != nil {
		return false, fmt.Errorf("read the user types: %w", err)
	}
	d := s.cfg.OIDC.PreviewRoleAgainst(toOIDCRoleMappings(rows), userTypes, nil, a.Groups, a.Email)
	if !d.OK() || (a.GroupsTruncated && roleIsTheDefault(d)) {
		return false, nil
	}
	// The derived role decides through runUngoverned (isOperator), so only an admin is ever spared.
	a.Role = d.Role
	return s.runUngoverned(a.context(ctx)), nil
}

// roleIsTheDefault says whether d's role is the default role, which nothing the person holds matched.
func roleIsTheDefault(d oidc.Derivation) bool {
	return slices.ContainsFunc(d.Matches, func(m oidc.Match) bool { return m.Source == oidc.MatchSourceDefaultRole })
}
