// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Four-eyes on service identities. With WARDYN_GOVERNANCE_SECOND_HUMAN on, a human's create and any PATCH
// that changes the sponsor are held as one governance change of kind service_identity, the whole PATCH
// included, and apply only when a distinct human on the security tier approves them, against the state
// the proposal saw. A description or suspend PATCH, unsuspend and retire apply directly.
package api

import (
	"errors"
	"net/http"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

const govKindServiceIdentity = types.GovernanceTargetServiceIdentity

// serviceIdentityDiffView is the reviewer's view of a held service identity write.
type serviceIdentityDiffView struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Sponsor     string `json:"sponsor"`
	UserType    string `json:"user_type"`
	Suspended   bool   `json:"suspended,omitempty"`
}

func serviceIdentityViewOf(si types.ServiceIdentity) *serviceIdentityDiffView {
	return &serviceIdentityDiffView{Name: si.Name, Description: si.Description, Sponsor: si.Sponsor,
		UserType: si.UserType, Suspended: si.SuspendedAt != nil}
}

// serviceIdentityBase is the state a held sponsor change is reviewed against: the identity's creation
// (a retired and recreated name differs), its retirement and its sponsor, with the time it was set.
type serviceIdentityBase struct {
	Name         string     `json:"name"`
	CreatedAt    time.Time  `json:"created_at"`
	Sponsor      string     `json:"sponsor"`
	SponsorSince time.Time  `json:"sponsor_since"`
	RetiredAt    *time.Time `json:"retired_at,omitempty"`
}

func serviceIdentityBaseOf(si types.ServiceIdentity) serviceIdentityBase {
	return serviceIdentityBase{Name: si.Name, CreatedAt: si.CreatedAt.UTC(), Sponsor: si.Sponsor,
		SponsorSince: si.SponsorSince.UTC(), RetiredAt: si.RetiredAt}
}

// serviceIdentityAbsent is a held create's base state: the name was free when it was proposed.
type serviceIdentityAbsent struct {
	Name   string `json:"name"`
	Absent bool   `json:"absent"`
}

// holdServiceIdentityCreate stores a create as a pending change. A dry run of the store write first
// shows it would apply, so a taken name, an unbound sponsor or an unknown type is the direct write's own
// refusal and is never held.
func (s *Server) holdServiceIdentityCreate(w http.ResponseWriter, r *http.Request, si types.ServiceIdentity) {
	err := s.readGovernanceState(r, func(q store.Querier) error {
		_, err := store.CreateServiceIdentityQ(r.Context(), q, si)
		return err
	})
	if writeServiceCreateError(w, r, err) {
		return
	}
	after := serviceIdentityViewOf(si)
	s.proposeGovernanceChange(w, r, govProposal{
		kind: govKindServiceIdentity, op: "create", key: si.Name,
		payload: serviceIdentityRequest{Name: si.Name, Description: si.Description, Sponsor: si.Sponsor, UserType: si.UserType},
		after:   after, changed: changedPaths(nil, after), baseState: serviceIdentityAbsent{Name: si.Name, Absent: true},
	})
}

// holdServiceIdentityUpdate stores a PATCH that changes the sponsor as a pending change, whole: none of
// its description or suspend change applies before the approval. A dry run first shows the store would
// accept it.
func (s *Server) holdServiceIdentityUpdate(w http.ResponseWriter, r *http.Request, cur types.ServiceIdentity, req updateServiceIdentityRequest) {
	var changed store.ServiceIdentityChanged
	err := s.readGovernanceState(r, func(q store.Querier) (err error) {
		changed, err = store.UpdateServiceIdentityQ(r.Context(), q, cur.Name, serviceIdentityChangeOf(req, principalFromRequest(r)))
		return err
	})
	if writeServiceUpdateError(w, r, err) {
		return
	}
	before, after := serviceIdentityViewOf(changed.Before), serviceIdentityViewOf(changed.After)
	s.proposeGovernanceChange(w, r, govProposal{
		kind: govKindServiceIdentity, op: "update", key: cur.Name, payload: req,
		before: before, after: after, changed: changedPaths(before, after), baseState: serviceIdentityBaseOf(changed.Before),
	})
}

func serviceIdentityChangeOf(req updateServiceIdentityRequest, by string) store.ServiceIdentityChange {
	return store.ServiceIdentityChange{Description: req.Description, Sponsor: req.Sponsor, Suspended: req.Suspended, By: by}
}

// applyServiceIdentityChange applies a held create or sponsor change inside the decision transaction.
// A create applies only while the name is free, the sponsor bound and active and the type defined; a
// sponsor change only while the identity is the one reviewed: not retired, not recreated, its sponsor
// unchanged. Anything else is store.ErrGovernanceChangeStale.
func applyServiceIdentityChange(s *Server, r *http.Request, q store.Querier, ch types.GovernanceChange) (govApplied, error) {
	ctx := r.Context()
	if err := store.LockGovernanceTarget(ctx, q, govKindServiceIdentity, ch.TargetKey); err != nil {
		return govApplied{}, err
	}
	stale := func(err error) error {
		if errors.Is(err, store.ErrServiceNameTaken) || errors.Is(err, store.ErrServiceSponsorInvalid) ||
			errors.Is(err, store.ErrServiceUserTypeUnknown) || errors.Is(err, store.ErrServiceIdentityInactive) ||
			errors.Is(err, store.ErrNotFound) {
			return store.ErrGovernanceChangeStale
		}
		return err
	}
	if ch.Op == "create" {
		var req serviceIdentityRequest
		if err := decodeHeldPayload(ch.Payload, &req); err != nil {
			return govApplied{}, writeRefusal(http.StatusBadRequest, reasonInvalidRequestBody, "invalid service identity: %v", err)
		}
		created, err := store.CreateServiceIdentityQ(ctx, q, types.ServiceIdentity{Name: req.Name, Description: req.Description,
			Sponsor: req.Sponsor, UserType: req.UserType, CreatedBy: ch.ProposedBy})
		if err != nil {
			return govApplied{}, stale(err)
		}
		return govApplied{action: "service_identity.create", target: created.Principal(),
			data: map[string]any{"sponsor": created.Sponsor, "user_type": created.UserType}}, nil
	}
	var req updateServiceIdentityRequest
	if err := decodeHeldPayload(ch.Payload, &req); err != nil {
		return govApplied{}, writeRefusal(http.StatusBadRequest, reasonInvalidRequestBody, "invalid service identity change: %v", err)
	}
	// The reviewed state is compared before anything is written: a stale decision commits its own state
	// change, so a write made ahead of it would commit too.
	cur, err := store.GetServiceIdentityForUpdateQ(ctx, q, ch.TargetKey)
	if err != nil {
		return govApplied{}, stale(err)
	}
	if computeETag(serviceIdentityBaseOf(cur)) != ch.BaseHash {
		return govApplied{}, store.ErrGovernanceChangeStale
	}
	changed, err := store.UpdateServiceIdentityQ(ctx, q, ch.TargetKey, serviceIdentityChangeOf(req, ch.ProposedBy))
	if err != nil {
		return govApplied{}, stale(err)
	}
	return govApplied{action: "service_identity.update", target: changed.After.Principal(), data: serviceUpdateAuditData(changed),
		afterCommit: func() map[string]any {
			after := changed.After
			if s.withServiceState(r, &after) != nil {
				after.State = after.BaseState()
			}
			return map[string]any{"state": after.State}
		}}, nil
}
