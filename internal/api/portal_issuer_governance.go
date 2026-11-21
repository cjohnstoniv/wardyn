// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// The two writes that change an issuer portal's authority after it is registered: retiring a binding that
// admitted nobody (DELETE /admin/portal-issuers/{k}) and changing a portal's admission rule (PUT
// /admin/delegates/{id}/admission). Both are super-admin writes held under WARDYN_GOVERNANCE_SECOND_HUMAN as
// their own governance kinds; the rows are written only in the decision transaction, so a rejected, withdrawn
// or expired change leaves the earlier rule in force.
package api

import (
	"errors"
	"net/http"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

const (
	govKindDelegateAdmission  = "delegate_admission"
	govKindPortalIssuerRetire = "portal_issuer_retire"
)

// The kinds are registered here, beside their apply functions, and not in the table in
// governance_change_gate.go, so the one table and this file change independently.
func init() {
	governanceChangeKinds[govKindDelegateAdmission] = govKind{approver: operatorApprover, apply: applyDelegateAdmissionChange}
	governanceChangeKinds[govKindPortalIssuerRetire] = govKind{approver: operatorApprover, apply: applyPortalIssuerRetire}
}

var issuerKeyRE = regexp.MustCompile(`^[a-z2-7]{26}$`)

const (
	portalIssuerNotFoundMsg = "No issuer binding has that key."
	portalIssuerInUseMsg    = "That issuer binding is in use: a portal, a person, a grant or a stored rule still names it. Only a binding that never admitted anyone can be retired."
	admissionStaleMsg       = "The portal's admission rule changed since you read it. Read it again and resubmit."
	admissionNotPortalMsg   = "Only an issuer portal has an admission rule."
	admissionGroupOrFlagMsg = "Name a group, or set portal_managed_admission to say the portal chooses its own users."
	admissionFlagGroupMsg   = "portal_managed_admission cannot be set while a group is named."
	admissionClaimReserved  = "That claim name already has a meaning and cannot be matched."
	admissionClaimHalfMsg   = "A claim match needs both a claim and a value."
	admissionClaimShapeMsg  = "The claim must be 1-64 characters of letters, digits and _ . : -, and the value 1-256 bytes of printable text."
	admissionExpectedMsg    = "Send expected, the admission rule you read."
)

// portalRetireView is the reviewer's view of a held retirement.
type portalRetireView struct {
	IssuerKey string `json:"issuer_key"`
}

// retirePortalIssuer is the one transaction a retirement runs in: the registry lock first, then the store's
// checks under the binding row's lock.
func retirePortalIssuer(s *Server, r *http.Request, q store.Querier, key string) error {
	if err := store.LockPortalIssuerRegistry(r.Context(), q); err != nil {
		return err
	}
	pg, ok := s.cfg.Store.(store.PortalIssuerRegistryStore)
	if !ok {
		return errGovernanceTxUnsupported
	}
	return pg.RetirePortalIssuerQ(r.Context(), q, key)
}

// portalRetireRefusal maps a retirement's store error to the refusal a direct write answers and a decision
// applies, or nil for any other error.
func portalRetireRefusal(err error) *unitWriteError {
	switch {
	case errors.Is(err, store.ErrNotFound):
		return writeRefusal(http.StatusNotFound, reasonPortalIssuerNotFound, "%s", portalIssuerNotFoundMsg)
	case errors.Is(err, store.ErrIssuerInUse):
		return writeRefusal(http.StatusConflict, reasonPortalIssuerInUse, "%s", portalIssuerInUseMsg)
	}
	return nil
}

// handleRetirePortalIssuer is DELETE /api/v1/admin/portal-issuers/{k}.
func (s *Server) handleRetirePortalIssuer(w http.ResponseWriter, r *http.Request) {
	key := chi.URLParam(r, "k")
	if !issuerKeyRE.MatchString(key) {
		writeErrorReason(w, http.StatusNotFound, reasonPortalIssuerNotFound, portalIssuerNotFoundMsg)
		return
	}
	mode, ok := s.governanceWriteMode(w, r)
	if !ok {
		return
	}
	if mode == govQueue {
		// A dry run first: a key nobody bound, or one in use, is the direct write's own refusal and is never held.
		err := s.readGovernanceState(r, func(q store.Querier) error { return retirePortalIssuer(s, r, q, key) })
		if ref := portalRetireRefusal(err); ref != nil {
			writeErrorReason(w, ref.status, ref.reason, ref.msg)
			return
		}
		if err != nil {
			writeServerError(w, r, "check the issuer binding retirement", err)
			return
		}
		before := portalRetireView{IssuerKey: key}
		s.proposeGovernanceChange(w, r, govProposal{
			kind: govKindPortalIssuerRetire, op: "delete", key: key, payload: before,
			before: before, changed: []string{"binding"}, baseState: before,
		})
		return
	}
	err := s.writeGovernance(r.Context(), func(q store.Querier) error { return retirePortalIssuer(s, r, q, key) })
	if ref := portalRetireRefusal(err); ref != nil {
		writeErrorReason(w, ref.status, ref.reason, ref.msg)
		return
	}
	if err != nil {
		writeServerError(w, r, "retire the issuer binding", err)
		return
	}
	actorType, actor := actorFromRequest(r)
	s.recordAudit(r.Context(), s.auditEvent(nil, actorType, actor, "portal_issuer.retire", key, "success", mustJSON(map[string]any{"issuer_key": key})))
	if mode == govBypass {
		s.recordGovernanceBypass(r, govKindPortalIssuerRetire, key, "success", nil)
	}
	w.WriteHeader(http.StatusNoContent)
}

// applyPortalIssuerRetire is the held retirement's apply, inside the decision transaction. The store
// re-checks every condition under the binding row's lock; a binding that went in use meanwhile is a 409 that
// leaves the change pending.
func applyPortalIssuerRetire(s *Server, r *http.Request, q store.Querier, ch types.GovernanceChange) (govApplied, error) {
	if err := retirePortalIssuer(s, r, q, ch.TargetKey); err != nil {
		if ref := portalRetireRefusal(err); ref != nil {
			return govApplied{}, ref
		}
		return govApplied{}, err
	}
	return govApplied{action: "portal_issuer.retire", target: ch.TargetKey, data: map[string]any{"issuer_key": ch.TargetKey}}, nil
}

// putAdmissionRequest is PUT /admin/delegates/{id}/admission. Expected is the rule the caller read; the write
// applies only while it still is the stored one, so two edits cannot cross.
type putAdmissionRequest struct {
	Group                  string                     `json:"group"`
	PortalManagedAdmission bool                       `json:"portal_managed_admission"`
	RequiredClaim          *types.PortalRequiredClaim `json:"required_claim"`
	Expected               *types.PortalAdmission     `json:"expected"`
}

// admissionChangePayload is the held change: the portal, the rule it is read at and the rule to apply.
// GroupsClaim is the portal's own, kept so the apply judges a claim name as the proposal did.
type admissionChangePayload struct {
	Delegate    uuid.UUID             `json:"delegate"`
	GroupsClaim string                `json:"groups_claim"`
	Expected    types.PortalAdmission `json:"expected"`
	Next        types.PortalAdmission `json:"next"`
}

// reservedAdmissionClaims are the claim names that already mean something to the exchange (D5).
var reservedAdmissionClaims = []string{"iss", "sub", "aud", "exp", "iat", "nbf", "jti", "cid", "uid", "scp", "scope", "client_id", "azp", "auth_time", "ver", "groups"}

var admissionClaimNameRE = regexp.MustCompile(`^[A-Za-z0-9_.:-]{1,64}$`)

// validatePortalAdmission is the admission rules of an issuer portal (D5): a group or the explicit
// portal-managed flag but not both, and one claim match that is whole, well named and not reserved. It
// returns the refusal's status, reason and sentence, or ok.
func validatePortalAdmission(a types.PortalAdmission, groupsClaim string) (status int, reason, msg string, ok bool) {
	switch {
	case a.Group == "" && !a.PortalManaged:
		return http.StatusUnprocessableEntity, reasonDelegateGroupOrPortalManaged, admissionGroupOrFlagMsg, false
	case a.Group != "" && a.PortalManaged:
		return http.StatusUnprocessableEntity, reasonDelegatePortalManagedWithGroup, admissionFlagGroupMsg, false
	}
	c := a.RequiredClaim
	if c == nil {
		return 0, "", "", true
	}
	if c.Claim == "" || c.Value == "" {
		return http.StatusUnprocessableEntity, reasonDelegateClaimIncomplete, admissionClaimHalfMsg, false
	}
	if !admissionClaimNameRE.MatchString(c.Claim) || len(c.Value) > 256 || !utf8.ValidString(c.Value) || strings.IndexFunc(c.Value, func(r rune) bool { return !unicode.IsPrint(r) }) >= 0 {
		return http.StatusBadRequest, reasonInvalidRequestBody, admissionClaimShapeMsg, false
	}
	for _, name := range append([]string{groupsClaim}, reservedAdmissionClaims...) {
		if name != "" && c.Claim == name {
			return http.StatusUnprocessableEntity, reasonDelegateClaimReserved, admissionClaimReserved, false
		}
	}
	return 0, "", "", true
}

func admissionOf(d types.Delegate) types.PortalAdmission {
	return types.PortalAdmission{Group: d.Group, PortalManaged: d.PortalManagedAdmission, RequiredClaim: d.RequiredClaim}
}

// admissionView is the reviewer's view of a rule: the claim's value is shown, because the approver must see
// what they approve. An audit row never carries it (admissionAudit).
func admissionView(a types.PortalAdmission) map[string]any {
	v := map[string]any{"group": a.Group, "portal_managed_admission": a.PortalManaged}
	if a.RequiredClaim != nil {
		v["required_claim"] = map[string]string{"claim": a.RequiredClaim.Claim, "value": a.RequiredClaim.Value}
	}
	return v
}

// admissionAudit is the audit data of a change: whether a group is set, and the claim names, never a value.
func admissionAudit(id uuid.UUID, was, now types.PortalAdmission) map[string]any {
	name := func(c *types.PortalRequiredClaim) any {
		if c == nil {
			return nil
		}
		return c.Claim
	}
	return map[string]any{"delegate": id.String(),
		"group_before": was.Group != "", "group_after": now.Group != "",
		"portal_managed_before": was.PortalManaged, "portal_managed_after": now.PortalManaged,
		"claim_before": name(was.RequiredClaim), "claim_after": name(now.RequiredClaim)}
}

// handlePutDelegateAdmission is PUT /api/v1/admin/delegates/{id}/admission.
func (s *Server) handlePutDelegateAdmission(w http.ResponseWriter, r *http.Request) {
	id, ok := parseIDParam(w, r, "id", "delegate")
	if !ok {
		return
	}
	var req putAdmissionRequest
	if !decodeStrict(w, r, &req) {
		return
	}
	if req.Expected == nil {
		writeErrorReason(w, http.StatusBadRequest, reasonInvalidRequestBody, admissionExpectedMsg)
		return
	}
	dr, ok := s.cfg.Store.(store.DelegateReader)
	if !ok {
		writeErrorReason(w, http.StatusNotImplemented, reasonDelegationStoreUnavailable, "delegation requires the Postgres store backend")
		return
	}
	d, err := dr.GetDelegateByID(r.Context(), id)
	if notFoundIf(w, err, "delegate", reasonDelegateNotFound) {
		return
	}
	if err != nil {
		writeServerError(w, r, "read the portal", err)
		return
	}
	if d.RevokedAt != nil {
		writeErrorReason(w, http.StatusNotFound, reasonDelegateNotFound, "delegate not found")
		return
	}
	if d.Issuer == "" {
		writeErrorReason(w, http.StatusUnprocessableEntity, reasonDelegatePortalManagedNeedsIssuer, admissionNotPortalMsg)
		return
	}
	next := types.PortalAdmission{Group: strings.TrimSpace(req.Group), PortalManaged: req.PortalManagedAdmission, RequiredClaim: req.RequiredClaim}
	if status, reason, msg, valid := validatePortalAdmission(next, d.GroupsClaim); !valid {
		writeErrorReason(w, status, reason, msg)
		return
	}
	cur := admissionOf(d)
	if computeETag(cur) != computeETag(*req.Expected) {
		writeErrorReason(w, http.StatusConflict, reasonDelegateAdmissionStale, admissionStaleMsg)
		return
	}
	mode, ok := s.governanceWriteMode(w, r)
	if !ok {
		return
	}
	payload := admissionChangePayload{Delegate: id, GroupsClaim: d.GroupsClaim, Expected: cur, Next: next}
	if mode == govQueue {
		before, after := admissionView(cur), admissionView(next)
		s.proposeGovernanceChange(w, r, govProposal{
			kind: govKindDelegateAdmission, op: "update", key: id.String(), payload: payload,
			before: before, after: after, changed: changedPaths(before, after), baseState: cur,
		})
		return
	}
	err = s.writeGovernance(r.Context(), func(q store.Querier) error { return updateAdmission(s, r, q, payload) })
	switch {
	case errors.Is(err, store.ErrStaleCompare):
		writeErrorReason(w, http.StatusConflict, reasonDelegateAdmissionStale, admissionStaleMsg)
		return
	case err != nil:
		writeServerError(w, r, "change the portal's admission rule", err)
		return
	}
	actorType, actor := actorFromRequest(r)
	s.recordAudit(r.Context(), s.auditEvent(nil, actorType, actor, "delegate.admission.change", id.String(), "success", mustJSON(admissionAudit(id, cur, next))))
	if mode == govBypass {
		s.recordGovernanceBypass(r, govKindDelegateAdmission, id.String(), "success", nil)
	}
	writeJSON(w, http.StatusOK, map[string]any{"delegate": id, "admission": admissionView(next)})
}

// updateAdmission applies a change on the transaction q: the same rules, then the store's compare-and-set.
func updateAdmission(s *Server, r *http.Request, q store.Querier, p admissionChangePayload) error {
	if _, _, _, valid := validatePortalAdmission(p.Next, p.GroupsClaim); !valid {
		return store.ErrGovernanceChangeStale
	}
	pg, ok := s.cfg.Store.(store.PortalIssuerRegistryStore)
	if !ok {
		return errGovernanceTxUnsupported
	}
	return pg.UpdateDelegateAdmissionQ(r.Context(), q, p.Delegate, p.Expected, p.Next)
}

// applyDelegateAdmissionChange is the held change's apply, inside the decision transaction. A rule that is no
// longer the one the change was proposed against is stale: nothing is written and the change stays pending.
func applyDelegateAdmissionChange(s *Server, r *http.Request, q store.Querier, ch types.GovernanceChange) (govApplied, error) {
	var p admissionChangePayload
	if err := decodeHeldPayload(ch.Payload, &p); err != nil {
		return govApplied{}, writeRefusal(http.StatusBadRequest, reasonInvalidRequestBody, "invalid admission change: %v", err)
	}
	if err := store.LockGovernanceTarget(r.Context(), q, govKindDelegateAdmission, ch.TargetKey); err != nil {
		return govApplied{}, err
	}
	if err := updateAdmission(s, r, q, p); err != nil {
		if errors.Is(err, store.ErrStaleCompare) {
			return govApplied{}, store.ErrGovernanceChangeStale
		}
		return govApplied{}, err
	}
	return govApplied{action: "delegate.admission.change", target: p.Delegate.String(), data: admissionAudit(p.Delegate, p.Expected, p.Next)}, nil
}
