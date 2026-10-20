// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// The local_credential_delivery governance document (design 0.9 §7.5): per credential class, how an
// org-held credential may reach a run on a person's own runner. Absent means refuse. One writer and one
// gate: securityOps, held for a second human under WARDYN_GOVERNANCE_SECOND_HUMAN, audited
// credential.delivery.write with before and after. Writable while runners are off, so the policy is in
// place before the first runner is.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/placement"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// errCredentialDeliveryStale is the If-Match check refusing the document it was handed.
var errCredentialDeliveryStale = errors.New("credential delivery policy changed")

const (
	govKindCredentialDelivery = types.GovernanceTargetCredentialDelivery
	govCredentialDeliveryKey  = "local_credential_delivery"
)

// deliveryPolicyOf decodes a stored document: never a nil class map.
func deliveryPolicyOf(raw json.RawMessage) (placement.DeliveryPolicy, error) {
	var p placement.DeliveryPolicy
	if err := json.Unmarshal(raw, &p); err != nil {
		return p, fmt.Errorf("decode credential delivery policy: %w", err)
	}
	if p.Classes == nil {
		p.Classes = map[string]placement.ClassPolicy{}
	}
	return p, nil
}

// storedDeliveryPolicy reads the document every local eligibility decision is judged on. A store
// without it is the default: every class refuses.
func (s *Server) storedDeliveryPolicy(ctx context.Context) (placement.DeliveryPolicy, error) {
	st, ok := s.cfg.Store.(interface {
		GetCredentialDeliveryPolicy(context.Context) (types.CredentialDeliveryPolicyDoc, error)
	})
	if !ok {
		return placement.DeliveryPolicy{Classes: map[string]placement.ClassPolicy{}}, nil
	}
	doc, err := st.GetCredentialDeliveryPolicy(ctx)
	if err != nil {
		return placement.DeliveryPolicy{}, err
	}
	p, err := deliveryPolicyOf(doc.Policy)
	if err != nil {
		return placement.DeliveryPolicy{}, err
	}
	// Both writers validate, but a document written out of band or by another version is judged
	// before it decides anything: an invalid one fails every door closed.
	if err := p.Validate(); err != nil {
		return placement.DeliveryPolicy{}, fmt.Errorf("stored credential delivery policy: %w", err)
	}
	return p, nil
}

// handleGetCredentialDelivery returns the stored document. securityOps (routes.go).
func (s *Server) handleGetCredentialDelivery(w http.ResponseWriter, r *http.Request) {
	st, ok := s.cfg.Store.(store.RunnerStore)
	if !ok {
		writeErrorReason(w, http.StatusNotImplemented, reasonCredentialDeliveryUnavailable, "the credential delivery policy requires the Postgres store")
		return
	}
	doc, err := st.GetCredentialDeliveryPolicy(r.Context())
	if err != nil {
		writeServerError(w, r, "get credential delivery policy", err)
		return
	}
	// The ETag is the hash a held change records as its base (applyCredentialDeliveryChange), so a console that
	// sends it back as If-Match is judged against the very state an approval would be.
	if p, err := deliveryPolicyOf(doc.Policy); err == nil {
		w.Header().Set("ETag", computeETag(p))
	}
	// buildable_modes sits beside policy, never inside it: PUT decodes the policy strictly, and a client
	// that sends GET's policy back must not carry it.
	resp := struct {
		types.CredentialDeliveryPolicyDoc
		BuildableModes map[string][]placement.Mode `json:"buildable_modes"`
	}{doc, map[string][]placement.Mode{}}
	for _, class := range placement.DeliveryClasses() {
		resp.BuildableModes[class] = placement.BuildableModes(class)
	}
	writeJSON(w, http.StatusOK, resp)
}

// handlePutCredentialDelivery replaces the whole document. securityOps (routes.go). If-Match is optional
// optimistic concurrency against GET's ETag, as PUT /governance/egress-baseline does.
func (s *Server) handlePutCredentialDelivery(w http.ResponseWriter, r *http.Request) {
	st, ok := s.cfg.Store.(store.RunnerStore)
	if !ok {
		writeErrorReason(w, http.StatusNotImplemented, reasonCredentialDeliveryUnavailable, "the credential delivery policy requires the Postgres store")
		return
	}
	mode, ok := s.governanceWriteMode(w, r)
	if !ok {
		return
	}
	var after placement.DeliveryPolicy
	if !decodeStrict(w, r, &after) {
		return
	}
	if after.Classes == nil {
		after.Classes = map[string]placement.ClassPolicy{}
	}
	if err := after.Validate(); err != nil {
		writeErrorReason(w, http.StatusBadRequest, reasonCredentialDeliveryInvalid, err.Error())
		return
	}
	if err := after.CheckBuildable(); err != nil {
		writeErrorReason(w, http.StatusUnprocessableEntity, reasonCredentialDeliveryInvalid, err.Error())
		return
	}
	staleIfMatch := func(cur types.CredentialDeliveryPolicyDoc) (placement.DeliveryPolicy, error) {
		before, err := deliveryPolicyOf(cur.Policy)
		if err != nil {
			return before, err
		}
		if !ifMatchSatisfied(r, computeETag(before)) {
			return before, errCredentialDeliveryStale
		}
		return before, nil
	}
	writeStale := func() {
		writeErrorReason(w, http.StatusPreconditionFailed, reasonCredentialDeliveryStale,
			"If-Match does not match the current credential delivery policy — GET /governance/credential-delivery again and retry")
	}
	var before placement.DeliveryPolicy
	if mode == govQueue {
		doc, err := st.GetCredentialDeliveryPolicy(r.Context())
		if err != nil {
			writeServerError(w, r, "get credential delivery policy", err)
			return
		}
		if before, err = staleIfMatch(doc); errors.Is(err, errCredentialDeliveryStale) {
			writeStale()
			return
		} else if err != nil {
			writeServerError(w, r, "get credential delivery policy", err)
			return
		}
		s.proposeGovernanceChange(w, r, govProposal{
			kind: govKindCredentialDelivery, op: "replace", key: govCredentialDeliveryKey, payload: after,
			before: before, after: after, changed: changedPaths(before, after), baseState: before,
		})
		return
	}
	// The If-Match check and the write share one transaction under the target lock, so a stale
	// ETag answers 412 even when another write landed between a read and this one.
	stored, err := st.PutCredentialDeliveryPolicyIf(r.Context(), mustJSON(after), principalFromRequest(r), time.Now().UTC(),
		func(cur types.CredentialDeliveryPolicyDoc) (err error) {
			before, err = staleIfMatch(cur)
			return err
		})
	if errors.Is(err, errCredentialDeliveryStale) {
		writeStale()
		return
	}
	if err != nil {
		writeServerError(w, r, "put credential delivery policy", err)
		return
	}
	s.recordAudit(r.Context(), s.auditEvent(nil, actorTypeFromRequest(r), principalFromRequest(r),
		placement.ActionCredentialDeliveryWrite, govCredentialDeliveryKey, "success", mustJSON(map[string]any{"before": before, "after": after})))
	if mode == govBypass {
		s.recordGovernanceBypass(r, govKindCredentialDelivery, govCredentialDeliveryKey, "success", nil)
	}
	w.Header().Set("ETag", computeETag(after))
	writeJSON(w, http.StatusOK, stored)
}

// applyCredentialDeliveryChange applies a held replacement inside the decision transaction: the target
// lock, the document read as it now stands and compared with what the proposal reviewed, then the write.
func applyCredentialDeliveryChange(s *Server, r *http.Request, q store.Querier, ch types.GovernanceChange) (govApplied, error) {
	ctx := r.Context()
	var after placement.DeliveryPolicy
	if err := decodeHeldPayload(ch.Payload, &after); err != nil {
		return govApplied{}, fmt.Errorf("governance change %s: payload: %w", ch.ID, err)
	}
	if after.Classes == nil {
		after.Classes = map[string]placement.ClassPolicy{}
	}
	if err := after.Validate(); err != nil {
		return govApplied{}, writeRefusal(http.StatusBadRequest, reasonCredentialDeliveryInvalid, "%s", err.Error())
	}
	if err := after.CheckBuildable(); err != nil {
		return govApplied{}, writeRefusal(http.StatusUnprocessableEntity, reasonCredentialDeliveryInvalid, "%s", err.Error())
	}
	if err := store.LockGovernanceTarget(ctx, q, govKindCredentialDelivery); err != nil {
		return govApplied{}, err
	}
	doc, err := store.GetCredentialDeliveryPolicyQ(ctx, q)
	if err != nil {
		return govApplied{}, err
	}
	before, err := deliveryPolicyOf(doc.Policy)
	if err != nil {
		return govApplied{}, err
	}
	if computeETag(before) != ch.BaseHash {
		return govApplied{}, store.ErrGovernanceChangeStale
	}
	if _, err := store.PutCredentialDeliveryPolicyQ(ctx, q, mustJSON(after), principalFromRequest(r), time.Now().UTC()); err != nil {
		return govApplied{}, err
	}
	return govApplied{action: placement.ActionCredentialDeliveryWrite, target: govCredentialDeliveryKey,
		data: map[string]any{"before": before, "after": after}}, nil
}
