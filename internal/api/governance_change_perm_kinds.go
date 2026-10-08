// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// The permission-side target kinds gov4-b2 covers: a capability grant (upsert, delete), the
// enforcement map (replace), a value's availability (set) and a user type's priority (update). For
// each, the half that HOLDS a write (the handler has already decoded and validated it as a direct
// write is) and the half that APPLIES a held one inside the decision transaction, which re-validates
// against the state it finds and writes through the store's Querier forms.
//
// No kind here has a narrowing exemption: deleting a deny grant, lifting a restriction and turning
// enforcement on each widen, and nothing proves a given write is not one of those.
package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// decodeHeldPayload decodes a held change's payload by the strictness a request body gets.
func decodeHeldPayload(raw json.RawMessage, into any) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	return dec.Decode(into)
}

// readGovernanceState runs read on a transaction that is rolled back: the target as a proposal sees
// it, read by the same function the approval reads it with.
func (s *Server) readGovernanceState(r *http.Request, read func(q store.Querier) error) error {
	return s.cfg.Store.DryRunGovernance(r.Context(), read)
}

// ── capability grants ────────────────────────────────────────────────────────

// grantChangePayload is a held grant write: the natural key and, for an upsert, the effect. A delete
// also carries the id it resolved from.
type grantChangePayload struct {
	ID          uuid.UUID                   `json:"id,omitzero"`
	SubjectType types.CapabilitySubjectType `json:"subject_type"`
	Subject     string                      `json:"subject"`
	Capability  string                      `json:"capability"`
	Value       string                      `json:"value"`
	Effect      types.CapabilityEffect      `json:"effect,omitempty"`
}

// grantTargetKey is a grant's natural key as target_key: a JSON array, which no subject or value can
// make ambiguous (either may contain any separator).
func grantTargetKey(g types.CapabilityGrant) string {
	return string(mustJSON([]string{string(g.SubjectType), g.Subject, g.Capability, g.Value}))
}

// grantDiffView is a grant as the reviewer reads it.
type grantDiffView struct {
	SubjectType types.CapabilitySubjectType `json:"subject_type"`
	Subject     string                      `json:"subject"`
	Capability  string                      `json:"capability"`
	Value       string                      `json:"value"`
	Effect      types.CapabilityEffect      `json:"effect"`
}

func newGrantDiffView(g types.CapabilityGrant) *grantDiffView {
	return &grantDiffView{SubjectType: g.SubjectType, Subject: g.Subject, Capability: g.Capability, Value: g.Value, Effect: g.Effect}
}

// grantState is the target of a grant change as it stands: the row at its natural key, nil when none.
// A proposal and its approval hash one and the same thing.
func grantState(cur *types.CapabilityGrant) any {
	if cur == nil {
		return map[string]any{"grant": nil}
	}
	return map[string]any{"grant": *cur}
}

// grantAtKeyQ is the grant at one natural key on q, nil when none.
func grantAtKeyQ(r *http.Request, q store.Querier, g types.CapabilityGrant, forUpdate bool) (*types.CapabilityGrant, error) {
	cur, err := store.GetCapabilityGrantByKeyQ(r.Context(), q, string(g.SubjectType), g.Subject, g.Capability, g.Value, forUpdate)
	if errors.Is(err, store.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &cur, nil
}

// holdGrantUpsert stores a validated grant upsert as a pending change.
func (s *Server) holdGrantUpsert(w http.ResponseWriter, r *http.Request, g types.CapabilityGrant) {
	var cur *types.CapabilityGrant
	err := s.readGovernanceState(r, func(q store.Querier) (err error) {
		cur, err = grantAtKeyQ(r, q, g, false)
		return err
	})
	if err != nil {
		writeServerError(w, r, "hold capability grant", err)
		return
	}
	after := newGrantDiffView(g)
	p := govProposal{
		kind: govKindGrant, op: "upsert", key: grantTargetKey(g),
		payload: grantChangePayload{SubjectType: g.SubjectType, Subject: g.Subject, Capability: g.Capability, Value: g.Value, Effect: g.Effect},
		after:   after, baseState: grantState(cur),
	}
	var before *grantDiffView
	if cur != nil {
		before = newGrantDiffView(*cur)
		p.before = before
	}
	p.changed = changedPaths(before, after)
	s.proposeGovernanceChange(w, r, p)
}

// holdGrantDelete stores a grant delete as a pending change. A delete by id is resolved to the
// natural key here, so a pending delete and a pending upsert of one grant collide.
func (s *Server) holdGrantDelete(w http.ResponseWriter, r *http.Request, id uuid.UUID) {
	var cur types.CapabilityGrant
	err := s.readGovernanceState(r, func(q store.Querier) (err error) {
		cur, err = store.GetCapabilityGrantQ(r.Context(), q, id, false)
		return err
	})
	if notFoundIf(w, err, "capability grant", reasonCapabilityGrantNotFound) {
		return
	}
	if err != nil {
		writeServerError(w, r, "hold capability grant delete", err)
		return
	}
	before := newGrantDiffView(cur)
	s.proposeGovernanceChange(w, r, govProposal{
		kind: govKindGrant, op: "delete", key: grantTargetKey(cur),
		payload: grantChangePayload{ID: cur.ID, SubjectType: cur.SubjectType, Subject: cur.Subject, Capability: cur.Capability, Value: cur.Value},
		before:  before, changed: changedPaths(before, nil), baseState: grantState(&cur),
	})
}

// applyGrantChange applies a held grant change inside the decision transaction: the target's lock,
// then the row at the natural key read FOR UPDATE and compared with what the proposal reviewed.
func applyGrantChange(s *Server, r *http.Request, q store.Querier, ch types.GovernanceChange) (govApplied, error) {
	ctx := r.Context()
	var p grantChangePayload
	if err := decodeHeldPayload(ch.Payload, &p); err != nil {
		return govApplied{}, fmt.Errorf("governance change %s: payload: %w", ch.ID, err)
	}
	g := types.CapabilityGrant{SubjectType: p.SubjectType, Subject: p.Subject, Capability: p.Capability, Value: p.Value, Effect: p.Effect}
	if err := store.LockGovernanceTarget(ctx, q, govKindGrant, string(g.SubjectType), g.Subject, g.Capability, g.Value); err != nil {
		return govApplied{}, err
	}
	cur, err := grantAtKeyQ(r, q, g, true)
	if err != nil {
		return govApplied{}, err
	}
	if computeETag(grantState(cur)) != ch.BaseHash {
		return govApplied{}, store.ErrGovernanceChangeStale
	}
	if ch.Op == "delete" {
		if err := store.DeleteCapabilityGrantQ(ctx, q, p.ID); err != nil {
			if errors.Is(err, store.ErrNotFound) {
				return govApplied{}, writeRefusal(http.StatusNotFound, reasonCapabilityGrantNotFound, "capability grant not found")
			}
			return govApplied{}, err
		}
		return govApplied{action: "capability.grant.delete", target: p.ID.String()}, nil
	}
	if err := validateCapabilityGrant(&g); err != nil {
		return govApplied{}, writeRefusal(http.StatusBadRequest, reasonCapabilityGrantInvalid, "invalid grant: %v", err)
	}
	if err := s.checkUserTypeSubject(ctx, q, g.SubjectType, g.Subject); err != nil {
		return govApplied{}, err
	}
	g.ID = uuid.New()
	g.CreatedBy = ch.ProposedBy
	saved, err := store.UpsertCapabilityGrantQ(ctx, q, g)
	if err != nil {
		return govApplied{}, err
	}
	action := "capability.grant.create"
	if saved.ID != g.ID {
		action = "capability.grant.update"
	}
	return govApplied{action: action, target: saved.ID.String(), data: grantAuditData(saved)}, nil
}

// grantAuditData is the capability.grant.create and .update row's data, shared by a direct write and
// an approved one.
func grantAuditData(saved types.CapabilityGrant) map[string]any {
	return map[string]any{
		"subject_type": saved.SubjectType,
		"subject":      saved.Subject,
		"capability":   saved.Capability,
		"value":        saved.Value,
		"effect":       saved.Effect,
	}
}

// ── the enforcement map ──────────────────────────────────────────────────────

// govEnforcementKey is the one target of the whole-map replacement.
const govEnforcementKey = "capability_enforcement"

// holdEnforcement stores a validated enforcement replacement as a pending change. If-Match is judged
// here as it is for a direct write (a stale one is the same 412); the approval then compares the base
// hash, which is the very ETag If-Match is compared with. The approval transaction, not the
// in-process lock a direct write takes, is what serializes it.
func (s *Server) holdEnforcement(w http.ResponseWriter, r *http.Request, body map[string]bool) {
	var existing map[string]bool
	err := s.readGovernanceState(r, func(q store.Querier) (err error) {
		existing, err = store.GetCapabilityEnforcementQ(r.Context(), q)
		return err
	})
	if err != nil {
		writeServerError(w, r, "get existing capability enforcement", err)
		return
	}
	if !ifMatchSatisfied(r, computeETag(existing)) {
		writeErrorReason(w, http.StatusPreconditionFailed, reasonCapabilityEnforcementStale,
			"If-Match does not match the current capability enforcement map — GET /permissions again and retry")
		return
	}
	s.proposeGovernanceChange(w, r, govProposal{
		kind: govKindEnforcement, op: "replace", key: govEnforcementKey, payload: body,
		before: existing, after: body, changed: changedPaths(existing, body), baseState: existing,
	})
}

func applyEnforcementChange(s *Server, r *http.Request, q store.Querier, ch types.GovernanceChange) (govApplied, error) {
	ctx := r.Context()
	var body map[string]bool
	if err := decodeHeldPayload(ch.Payload, &body); err != nil {
		return govApplied{}, fmt.Errorf("governance change %s: payload: %w", ch.ID, err)
	}
	if err := store.LockGovernanceTarget(ctx, q, govKindEnforcement); err != nil {
		return govApplied{}, err
	}
	existing, err := store.GetCapabilityEnforcementQ(ctx, q)
	if err != nil {
		return govApplied{}, err
	}
	if computeETag(existing) != ch.BaseHash {
		return govApplied{}, store.ErrGovernanceChangeStale
	}
	for kind := range body {
		if !validCapabilityKind(kind) {
			return govApplied{}, writeRefusal(http.StatusBadRequest, reasonCapabilityKindUnknown, "unknown capability kind %q", kind)
		}
	}
	saved, err := store.PutCapabilityEnforcementQ(ctx, q, body)
	if err != nil {
		return govApplied{}, err
	}
	data := make(map[string]any, len(saved))
	for k, v := range saved {
		data[k] = v
	}
	return govApplied{action: "capability.enforcement.write", target: govEnforcementKey, data: data}, nil
}

// ── availability ─────────────────────────────────────────────────────────────

// availabilityChangePayload is a held availability write.
type availabilityChangePayload struct {
	Kind       string `json:"kind"`
	Value      string `json:"value"`
	Restricted bool   `json:"restricted"`
}

// availabilityState is what an availability change depends on: the restricted bit and the allow rows
// naming the value (who a restriction lets in, and so whether restricting it is accepted at all).
type availabilityState struct {
	Restricted bool                    `json:"restricted"`
	AllowedBy  []types.CapabilityGrant `json:"allowed_by"`
}

func availabilityStateQ(r *http.Request, q store.Querier, kind, value string) (availabilityState, error) {
	st := availabilityState{AllowedBy: []types.CapabilityGrant{}}
	restricted, err := store.ListCapabilityRestrictionsQ(r.Context(), q)
	if err != nil {
		return st, err
	}
	st.Restricted = restricted[kind][value]
	grants, err := store.ListCapabilityGrantsQ(r.Context(), q)
	if err != nil {
		return st, err
	}
	for _, g := range grants {
		if g.Capability == kind && g.Effect == types.CapabilityAllow && strings.TrimSpace(g.Value) == value {
			st.AllowedBy = append(st.AllowedBy, g)
		}
	}
	return st, nil
}

// holdAvailability stores an availability write that changes the stored restricted bit.
func (s *Server) holdAvailability(w http.ResponseWriter, r *http.Request, kind, value string, restricted bool) {
	var st availabilityState
	err := s.readGovernanceState(r, func(q store.Querier) (err error) {
		st, err = availabilityStateQ(r, q, kind, value)
		return err
	})
	if err != nil {
		writeServerError(w, r, "hold capability availability", err)
		return
	}
	before := availabilityView{Kind: kind, Value: value, Restricted: st.Restricted, AllowedBy: st.AllowedBy}
	after := availabilityView{Kind: kind, Value: value, Restricted: restricted, AllowedBy: st.AllowedBy}
	s.proposeGovernanceChange(w, r, govProposal{
		kind: govKindAvailability, op: "set", key: kind + "/" + value,
		payload: availabilityChangePayload{Kind: kind, Value: value, Restricted: restricted},
		before:  before, after: after, changed: changedPaths(before, after), baseState: st,
	})
}

func applyAvailabilityChange(s *Server, r *http.Request, q store.Querier, ch types.GovernanceChange) (govApplied, error) {
	ctx := r.Context()
	var p availabilityChangePayload
	if err := decodeHeldPayload(ch.Payload, &p); err != nil {
		return govApplied{}, fmt.Errorf("governance change %s: payload: %w", ch.ID, err)
	}
	if err := store.LockGovernanceTarget(ctx, q, govKindAvailability, p.Kind, p.Value); err != nil {
		return govApplied{}, err
	}
	st, err := availabilityStateQ(r, q, p.Kind, p.Value)
	if err != nil {
		return govApplied{}, err
	}
	// The base hash covers the allow rows, so a restriction accepted at proposal (someone is listed)
	// is applied only while that list is what it was: the "Only..." refusal cannot be reached here.
	if computeETag(st) != ch.BaseHash {
		return govApplied{}, store.ErrGovernanceChangeStale
	}
	if !capKinds[p.Kind].restrictable {
		return govApplied{}, writeRefusal(http.StatusBadRequest, reasonAvailabilityKindNotRestrictable,
			"%q can't be restricted to a list; only the resources people are offered can be.", p.Kind)
	}
	if p.Kind == capComponent && !p.Restricted {
		// The component's own seam: an id that is no org component stays closed, and the change stays pending.
		id, err := uuid.Parse(p.Value)
		if err != nil {
			return govApplied{}, err
		}
		if err := store.LiftComponentRestrictionQ(ctx, q, id, ch.ProposedBy); errors.Is(err, store.ErrNotFound) {
			return govApplied{}, writeRefusal(http.StatusNotFound, reasonComponentNotFound, "component not found")
		} else if err != nil {
			return govApplied{}, err
		}
	} else if err := store.SetCapabilityRestrictionQ(ctx, q, p.Kind, p.Value, p.Restricted, ch.ProposedBy); err != nil {
		return govApplied{}, err
	}
	return govApplied{action: "capability.availability.write", target: p.Kind, data: availabilityAuditData(p.Kind, p.Value, p.Restricted)}, nil
}

func availabilityAuditData(kind, value string, restricted bool) map[string]any {
	return map[string]any{"kind": kind, "value": value, "restricted": restricted}
}

// ── user-type priority ───────────────────────────────────────────────────────

// userTypeDiffView is the part of a user type a reviewer compares.
type userTypeDiffView struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Priority    int    `json:"priority"`
}

func newUserTypeDiffView(t types.UserType) *userTypeDiffView {
	return &userTypeDiffView{Name: t.Name, Description: t.Description, Priority: t.Priority}
}

// holdUserTypeUpdate stores a user-type edit that changes its priority. A dry run of the real update
// first shows the store would accept it, so a missing type or a name clash is the direct write's own
// refusal and is never held. A name or description edit with the same priority is metadata and never
// reaches here.
func (s *Server) holdUserTypeUpdate(w http.ResponseWriter, r *http.Request, t types.UserType) {
	var cur types.UserType
	err := s.readGovernanceState(r, func(q store.Querier) (err error) {
		if cur, err = store.GetUserTypeQ(r.Context(), q, t.ID, false); err != nil {
			return err
		}
		_, err = store.UpdateUserTypeQ(r.Context(), q, t)
		return err
	})
	if s.writeUserTypeUpdateError(w, r, err, t) {
		return
	}
	before, after := newUserTypeDiffView(cur), newUserTypeDiffView(t)
	s.proposeGovernanceChange(w, r, govProposal{
		kind: govKindUserType, op: "update", key: t.ID,
		payload: userTypeRequest{ID: t.ID, Name: t.Name, Description: t.Description, Priority: t.Priority},
		before:  before, after: after, changed: changedPaths(before, after), baseState: cur,
	})
}

func applyUserTypeChange(s *Server, r *http.Request, q store.Querier, ch types.GovernanceChange) (govApplied, error) {
	ctx := r.Context()
	var req userTypeRequest
	if err := decodeHeldPayload(ch.Payload, &req); err != nil {
		return govApplied{}, fmt.Errorf("governance change %s: payload: %w", ch.ID, err)
	}
	// The row is locked for the transaction, so a direct edit to it waits for this decision.
	cur, err := store.GetUserTypeQ(ctx, q, ch.TargetKey, true)
	if errors.Is(err, store.ErrNotFound) {
		return govApplied{}, writeRefusal(http.StatusNotFound, reasonUserTypeNotFound, "User type not found.")
	}
	if err != nil {
		return govApplied{}, err
	}
	if computeETag(cur) != ch.BaseHash {
		return govApplied{}, store.ErrGovernanceChangeStale
	}
	t, msg := userTypeFromRequest(req)
	if msg != "" {
		return govApplied{}, writeRefusal(http.StatusBadRequest, reasonUserTypeRequestInvalid, "%s", msg)
	}
	if cur.BuiltIn && t.Priority != 0 {
		return govApplied{}, writeRefusal(http.StatusBadRequest, reasonUserTypeBuiltInNoPriority,
			"%s never wins a tie against another type, so it has no priority.", cur.Name)
	}
	saved, err := store.UpdateUserTypeQ(ctx, q, t)
	switch {
	case errors.Is(err, store.ErrNotFound):
		return govApplied{}, writeRefusal(http.StatusNotFound, reasonUserTypeNotFound, "User type not found.")
	case errors.Is(err, store.ErrConflict):
		return govApplied{}, writeRefusal(http.StatusConflict, reasonUserTypeConflict, "Another user type is already named %q.", t.Name)
	case err != nil:
		return govApplied{}, err
	}
	return govApplied{action: "user_type.write", target: saved.ID, data: userTypeAuditData(saved)}, nil
}
