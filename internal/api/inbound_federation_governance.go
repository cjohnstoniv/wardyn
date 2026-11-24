// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Four-eyes on inbound federation. With WARDYN_GOVERNANCE_SECOND_HUMAN on, a person's turning the switch on and
// adding a trust are held as one governance change of kind inbound_federation and apply only when a distinct human
// on the security tier, signed in, approves them. Turning it off, deleting a trust and a sponsor's confirm apply
// directly, and turning it off cancels a pending turn-on.
package api

import (
	"errors"
	"net/http"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

const govKindInboundFederation = types.GovernanceTargetInboundFederation

// inboundFederationKey is the switch's target key. A trust's key is its name, which never holds '_'.
const inboundFederationKey = types.InboundFederationSwitchKey

// The kind is registered here, beside its apply, and not in the table in governance_change_gate.go, so the one
// table and this file change independently.
func init() {
	governanceChangeKinds[govKindInboundFederation] = govKind{approver: securityApprover, apply: applyInboundFederationChange}
}

// holdInboundFederationOn stores a turn-on as a pending change. A dry run of the store write first shows it would
// apply.
func (s *Server) holdInboundFederationOn(w http.ResponseWriter, r *http.Request, _ types.InboundFederation) {
	by := principalFromRequest(r)
	err := s.readGovernanceState(r, func(q store.Querier) error {
		_, err := store.SetInboundFederationQ(r.Context(), q, true, by)
		return err
	})
	if err != nil {
		writeServerError(w, r, "check inbound federation", err)
		return
	}
	s.proposeGovernanceChange(w, r, govProposal{
		kind: govKindInboundFederation, op: "update", key: inboundFederationKey, payload: map[string]bool{"enabled": true},
		before: map[string]bool{"enabled": false}, after: map[string]bool{"enabled": true}, changed: []string{"enabled"},
		baseState: map[string]bool{"enabled": false},
	})
}

// holdInboundTrustCreate stores a trust create as a pending change. A dry run of the store write first, with the
// confirmation set as the direct create sets it, so a taken name, a missing identity or a reserved audience is the
// direct write's own refusal and is never held.
func (s *Server) holdInboundTrustCreate(w http.ResponseWriter, r *http.Request, t types.InboundTrust, now time.Time) {
	dry := t
	if err := s.confirmNewTrust(r, &dry, principalFromRequest(r), now); err != nil {
		writeServerError(w, r, "read the revocation sequence", err)
		return
	}
	err := s.readGovernanceState(r, func(q store.Querier) error {
		_, _, err := store.CreateInboundTrustQ(r.Context(), q, dry)
		return err
	})
	if writeInboundTrustCreateError(w, r, err) {
		return
	}
	req := inboundTrustRequestOf(t)
	after := &req
	s.proposeGovernanceChange(w, r, govProposal{
		kind: govKindInboundFederation, op: "create", key: t.Name, payload: req,
		after: after, changed: changedPaths(nil, after), baseState: map[string]any{"name": t.Name, "absent": true},
	})
}

// applyInboundFederationChange applies a held turn-on or trust create inside the decision transaction. The approver
// must be a signed-in person: the admin token skips the approver predicate, so this is the check that covers it.
// A trust create applies only while it still validates against this deployment's addresses, the name and triple
// are free and the identity is usable; anything else is store.ErrGovernanceChangeStale. ErrInboundTrustInvalid,
// which ends the transaction, is returned as it is, so the decision rolls back.
func applyInboundFederationChange(s *Server, r *http.Request, q store.Querier, ch types.GovernanceChange) (govApplied, error) {
	if !inboundPerson(r.Context()) {
		return govApplied{}, &govRefusal{why: "inbound federation approver is not a signed-in person", write: func(w http.ResponseWriter, r *http.Request) {
			writeErrorReason(w, http.StatusForbidden, reasonInboundFederationNeedsPerson, inboundFederationNeedsPersonMsg)
		}}
	}
	ctx := r.Context()
	if err := store.LockGovernanceTarget(ctx, q, govKindInboundFederation, ch.TargetKey); err != nil {
		return govApplied{}, err
	}
	switch ch.Op {
	case "update":
		// No base to compare: a switch-off after the proposal cancelled it, so an approval that gets here was not
		// overtaken by an off.
		if _, err := store.SetInboundFederationQ(ctx, q, true, ch.ProposedBy); err != nil {
			return govApplied{}, err
		}
		return govApplied{action: "inbound_federation.update", target: inboundFederationKey,
			data: map[string]any{"enabled": true, "tokens_revoked": 0, "changes_cancelled": []string{}}}, nil
	case "create":
		return applyInboundTrustCreate(s, r, q, ch)
	}
	return govApplied{}, writeRefusal(http.StatusBadRequest, reasonInvalidRequestBody, "unknown inbound federation change op %q", ch.Op)
}

// applyInboundTrustCreate is applyInboundFederationChange's create.
func applyInboundTrustCreate(s *Server, r *http.Request, q store.Querier, ch types.GovernanceChange) (govApplied, error) {
	var req inboundTrustRequest
	if err := decodeHeldPayload(ch.Payload, &req); err != nil {
		return govApplied{}, writeRefusal(http.StatusBadRequest, reasonInvalidRequestBody, "invalid inbound trust: %v", err)
	}
	// Validated again: this deployment's own addresses may have changed since the proposal.
	t, reason, _ := s.validateInboundTrust(req)
	if reason != "" {
		return govApplied{}, store.ErrGovernanceChangeStale
	}
	if err := s.confirmNewTrust(r, &t, ch.ProposedBy, s.cfg.Now().UTC()); err != nil {
		return govApplied{}, err
	}
	created, sponsor, err := store.CreateInboundTrustQ(r.Context(), q, t)
	if errors.Is(err, store.ErrNotFound) || errors.Is(err, store.ErrServiceIdentityInactive) ||
		errors.Is(err, store.ErrInboundTrustExists) || errors.Is(err, store.ErrInboundTrustAudienceReserved) {
		return govApplied{}, store.ErrGovernanceChangeStale
	}
	if err != nil {
		return govApplied{}, err
	}
	return govApplied{action: "inbound_trust.create", target: created.ID.String(), data: inboundTrustCreateData(created, sponsor)}, nil
}
