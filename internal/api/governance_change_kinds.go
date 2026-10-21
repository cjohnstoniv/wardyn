// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// The two target kinds gov4-b1 covers: a governance profile (create, update, delete) and a governance
// assignment (upsert, delete). For each, the half that HOLDS a write (decode and validation are the
// handler's own, unchanged; here the write is run as a dry run, abandoned, and stored as a pending
// change with its diff) and the half that APPLIES a held one inside the decision transaction. Both
// halves call the store's Querier forms, so a held change runs the same SQL a direct write does.
package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/composer"
	"github.com/cjohnstoniv/wardyn/internal/policyref"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// errHoldForApproval is the sentinel a build callback returns to abandon the transaction that would
// have stored a write, because the write is to be held for a second human instead.
var errHoldForApproval = errors.New("api: the write is held for a second human")

// sameJSON reports whether two values have the same JSON form: the comparison a stored row (which has
// round-tripped through the database) and a decoded request need, where nil and empty collapse.
func sameJSON(a, b any) bool {
	ea := computeETag(a)
	return ea != "" && ea == computeETag(b)
}

// ── governance profiles ──────────────────────────────────────────────────────

// unitChangePayload is a held profile write: the validated request body, with each optional member
// present only when the caller sent it (absent keeps the stored value, null clears it, so the two must
// stay distinguishable when the payload is replayed).
type unitChangePayload struct {
	Name          string                 `json:"name"`
	Ceiling       types.RunPolicySpec    `json:"ceiling"`
	Limits        types.GovernanceLimits `json:"limits"`
	Contact       json.RawMessage        `json:"contact,omitempty"`
	ParentID      json.RawMessage        `json:"parent_id,omitempty"`
	Overlay       json.RawMessage        `json:"overlay,omitempty"`
	OverlayLimits json.RawMessage        `json:"overlay_limits,omitempty"`
}

func unitPayloadOf(req governanceUnitRequest) unitChangePayload {
	return unitChangePayload{
		Name: req.Name, Ceiling: req.Ceiling, Limits: req.Limits,
		Contact: req.Contact, ParentID: req.ParentID, Overlay: req.Overlay, OverlayLimits: req.OverlayLimits,
	}
}

// unitRequestFromPayload decodes a held profile write and validates it by the rules a direct write
// is judged by. A message is why it no longer validates.
func unitRequestFromPayload(raw json.RawMessage) (governanceUnitRequest, string) {
	var req governanceUnitRequest
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		return governanceUnitRequest{}, "invalid JSON body: " + err.Error()
	}
	req, msg := finishGovernanceUnitRequest(req)
	if msg != "" {
		return governanceUnitRequest{}, msg
	}
	if msg := req.parseComposition(); msg != "" {
		return governanceUnitRequest{}, msg
	}
	return req, ""
}

// heldUnitWrite is what a profile build proved before it was abandoned: every profile as the
// transaction read them, the row it would have stored and what that composes to.
type heldUnitWrite struct {
	all []types.GovernanceUnit
	row types.GovernanceUnit
	res *ResolvedUnit
}

// unitWriteExempt reports whether a human's profile update applies without a second human. Only an
// UPDATE of an existing profile can be exempt, and only by one of two proofs:
//
//   - metadata only: every stored field other than the name is equal to the current row, including the
//     base, the overlay and the overlay limits;
//   - narrowing: the new effective profile is Leq the current effective one (composer.Leq, which
//     carries the full git_pat dominance contract), and its member CPU and memory cap is no higher
//     (memberCapOf).
//
// A write that changes the contact is held even when Leq holds: a contact is who a refused person is
// told to ask, and nothing proves a new one is not a redirect. So is a rename that can change which
// profile a person is given (renameMaySelect), whichever proof would otherwise admit it. Nothing else
// is exempt.
func (s *Server) unitWriteExempt(ctx context.Context, all []types.GovernanceUnit, p types.GovernanceUnit, res *ResolvedUnit) bool {
	byID := unitsByID(all)
	cur, exists := byID[p.ID]
	if !exists || res == nil {
		return false
	}
	if !sameJSON(nonZeroContact(cur.Contact), nonZeroContact(unitContactAfter(cur, p))) {
		return false
	}
	if cur.Name != p.Name && s.renameMaySelect(ctx, p.ID) {
		return false
	}
	if sameJSON(cur.Ceiling, p.Ceiling) && sameJSON(cur.Limits, p.Limits) && sameJSON(cur.ParentID, p.ParentID) &&
		sameJSON(cur.Overlay, p.Overlay) && sameJSON(cur.OverlayLimits, p.OverlayLimits) {
		return true
	}
	chain, err := chainFromRows(byID, p.ID)
	if err != nil {
		return false
	}
	curRes, err := s.composeChain(chain)
	if err != nil {
		return false
	}
	// Leq reads a maximum set where none was as tighter; the member cap reads it as the size, so the
	// cap is compared too.
	return composer.Leq(res.Ceiling, curRes.Ceiling, res.Limits, curRes.Limits) &&
		len(memberCapRaises(s.resolvedMemberCap(res), s.resolvedMemberCap(curRes))) == 0
}

// renameMaySelect reports whether renaming profile id could change which profile a person resolves
// to. The resolver breaks a tie between two GROUP assignments of equal priority by profile name
// (store's subjectPrecedence), and a person may be in any two groups, so a rename can move the tie
// whenever a group assignment of id shares its priority with a group assignment of another profile.
// The other tiers hold at most one matching row per person. It does not try to tell whether this
// rename actually reorders the pair: the database's collation decides that order, not Go's. A list
// that cannot be read answers true, so the write is held.
func (s *Server) renameMaySelect(ctx context.Context, id uuid.UUID) bool {
	all, err := governanceMembershipsFor(ctx, s)
	if err != nil {
		return true
	}
	for _, a := range all {
		if a.SubjectType != types.CapabilitySubjectGroup || a.UnitID != id {
			continue
		}
		if slices.ContainsFunc(all, func(b types.GovernanceMembership) bool {
			return b.SubjectType == types.CapabilitySubjectGroup && b.UnitID != id && b.Priority == a.Priority
		}) {
			return true
		}
	}
	return false
}

// unitContactAfter is the contact the row holds once p is written over cur.
func unitContactAfter(cur, p types.GovernanceUnit) *policyref.Contact {
	if p.ContactSet {
		return p.Contact
	}
	return cur.Contact
}

// nonZeroContact is c, or nil for an empty contact (the store keeps an empty one as none).
func nonZeroContact(c *policyref.Contact) *policyref.Contact {
	if c == nil || c.IsZero() {
		return nil
	}
	return c
}

// unitDiffStored is the stored fields of a profile the reviewer compares, without its identity or
// provenance (which do not change what the profile permits).
type unitDiffStored struct {
	Name          string                 `json:"name"`
	Ceiling       types.RunPolicySpec    `json:"ceiling"`
	Limits        types.GovernanceLimits `json:"limits"`
	Contact       *policyref.Contact     `json:"contact,omitempty"`
	ParentID      *uuid.UUID             `json:"parent_id,omitempty"`
	Overlay       *types.CeilingOverlay  `json:"overlay,omitempty"`
	OverlayLimits *types.LimitsOverlay   `json:"overlay_limits,omitempty"`
}

// unitDiffView is a profile as the reviewer reads it: the stored fields and what composing them
// gives. Every ceiling in it passes the read-redaction every other read of a ceiling does.
type unitDiffView struct {
	unitDiffStored
	Effective *governanceEffective `json:"effective,omitempty"`
}

func newUnitDiffView(row types.GovernanceUnit, res *ResolvedUnit) *unitDiffView {
	v := &unitDiffView{unitDiffStored: unitDiffStored{
		Name: row.Name, Ceiling: redactSpecForRead(row.Ceiling, true), Limits: row.Limits,
		Contact: nonZeroContact(row.Contact), ParentID: row.ParentID,
		Overlay: redactOverlayForRead(row.Overlay), OverlayLimits: row.OverlayLimits,
	}}
	if res != nil {
		v.Effective = &governanceEffective{
			Ceiling: redactSpecForRead(res.Ceiling, true), Limits: res.Limits, Warnings: res.AdminWarnings,
		}
	}
	return v
}

// redactOverlayForRead is redactSpecForRead for an overlay's llm_inspection block: a raw secret value
// is replaced by a count, on a copy.
func redactOverlayForRead(o *types.CeilingOverlay) *types.CeilingOverlay {
	if o == nil || o.LLMInspection == nil || len(o.LLMInspection.WorkspaceSecretValues) == 0 {
		return o
	}
	cp := *o
	li := *o.LLMInspection
	li.WorkspaceSecretValues = []string{fmt.Sprintf("<%d value(s) redacted>", len(li.WorkspaceSecretValues))}
	cp.LLMInspection = &li
	return &cp
}

// resolveForDiff is what the profile id composes to in all, or nil when it does not compose.
func (s *Server) resolveForDiff(all []types.GovernanceUnit, id uuid.UUID) *ResolvedUnit {
	chain, err := chainFromRows(unitsByID(all), id)
	if err != nil {
		return nil
	}
	res, err := s.composeChain(chain)
	if err != nil {
		return nil
	}
	return res
}

// unitBaseState is the profile graph as a change to profile id depends on it: the row itself (nil
// when absent) and the chain of bases above base. An approval that finds either changed refuses as
// stale, so the profile the reviewer saw, and what it composes on, is what is written.
func unitBaseState(all []types.GovernanceUnit, id uuid.UUID, base *uuid.UUID) any {
	byID := unitsByID(all)
	state := map[string]any{"self": nil}
	if row, ok := byID[id]; ok {
		state["self"] = row
	}
	bases := []types.GovernanceUnit{}
	seen := map[uuid.UUID]bool{id: true}
	for cur := base; cur != nil && !seen[*cur] && len(bases) <= maxUnitDepth; {
		row, ok := byID[*cur]
		if !ok {
			break
		}
		seen[*cur] = true
		bases = append(bases, row)
		cur = row.ParentID
	}
	state["bases"] = bases
	return state
}

// holdUnitWrite stores a profile create or update that build accepted as a pending change.
func (s *Server) holdUnitWrite(w http.ResponseWriter, r *http.Request, id uuid.UUID, req governanceUnitRequest, h heldUnitWrite) {
	cur, exists := unitsByID(h.all)[id]
	row := h.row
	op := "create"
	var before *unitDiffView
	if exists {
		op = "update"
		before = newUnitDiffView(cur, s.resolveForDiff(h.all, id))
		row.Contact = unitContactAfter(cur, h.row)
	}
	after := newUnitDiffView(row, h.res)
	p := govProposal{
		kind: govKindUnit, op: op, key: id.String(), payload: unitPayloadOf(req),
		after: after, baseState: unitBaseState(h.all, id, h.row.ParentID),
	}
	var beforeStored *unitDiffStored
	if before != nil {
		p.before, beforeStored = before, &before.unitDiffStored
	}
	p.changed = changedPaths(beforeStored, after.unitDiffStored)
	// The notes a direct write returns as warnings (writeGovernanceUnit), kept with the change so the
	// proposer and the approver read what the person who saved it directly would have.
	if !row.Composed() {
		p.notes = governanceOmissionWarnings(row.Ceiling, s.cfg.DefaultPolicy, row.Limits)
	}
	p.notes = append(p.notes, h.res.AdminWarnings...)
	if posture := s.rubricPostureAdvisory(r.Context(), h.res); posture != "" {
		p.notes = append(p.notes, posture)
	}
	s.proposeGovernanceChange(w, r, p)
}

// holdUnitDelete stores a profile delete as a pending change, after a dry run of the delete has
// shown the store would accept it (so the refusals are the direct delete's own).
func (s *Server) holdUnitDelete(w http.ResponseWriter, r *http.Request, id uuid.UUID) {
	ctx := r.Context()
	var all []types.GovernanceUnit
	err := s.cfg.Store.DryRunGovernance(ctx, func(q store.Querier) error {
		if err := store.LockGovernanceGraph(ctx, q); err != nil {
			return err
		}
		var err error
		if all, err = store.ListGovernanceUnitsQ(ctx, q); err != nil {
			return err
		}
		return store.DeleteGovernanceUnitQ(ctx, q, id)
	})
	if s.writeUnitDeleteError(w, r, err) {
		return
	}
	cur := unitsByID(all)[id]
	before := newUnitDiffView(cur, s.resolveForDiff(all, id))
	s.proposeGovernanceChange(w, r, govProposal{
		kind: govKindUnit, op: "delete", key: id.String(), payload: map[string]any{"id": id},
		before: before, changed: changedPaths(before.unitDiffStored, nil),
		baseState: unitBaseState(all, id, cur.ParentID),
	})
}

// applyUnitChange applies a held profile change inside the decision transaction.
func applyUnitChange(s *Server, r *http.Request, q store.Querier, ch types.GovernanceChange) (govApplied, error) {
	ctx := r.Context()
	id, err := uuid.Parse(ch.TargetKey)
	if err != nil {
		return govApplied{}, fmt.Errorf("governance change %s: target key %q is not a profile id", ch.ID, ch.TargetKey)
	}
	if ch.Op == "delete" {
		return applyUnitDelete(ctx, q, ch, id)
	}
	req, msg := unitRequestFromPayload(ch.Payload)
	if msg != "" {
		return govApplied{}, writeRefusal(http.StatusBadRequest, reasonGovernanceUnitRequestInvalid, "%s", msg)
	}
	var effective *ResolvedUnit
	saved, err := store.WriteGovernanceUnitQ(ctx, q, id, func(all []types.GovernanceUnit, _ []types.GovernanceMembership) (types.GovernanceUnit, error) {
		base, _, _ := composedFrom(all, id, req.comp)
		if computeETag(unitBaseState(all, id, base)) != ch.BaseHash {
			return types.GovernanceUnit{}, store.ErrGovernanceChangeStale
		}
		p, res, err := s.buildGovernanceUnit(ch.ProposedBy, id, req, all)
		effective = res
		return p, err
	})
	if errors.Is(err, store.ErrConflict) {
		return govApplied{}, writeRefusal(http.StatusConflict, reasonGovernanceUnitNameConflict,
			"a governance profile named %q already exists", req.Name)
	}
	if err != nil {
		return govApplied{}, err
	}
	return govApplied{action: "governance.unit.write", target: saved.ID.String(), data: unitWriteAuditData(saved, effective)}, nil
}

func applyUnitDelete(ctx context.Context, q store.Querier, ch types.GovernanceChange, id uuid.UUID) (govApplied, error) {
	if err := store.LockGovernanceGraph(ctx, q); err != nil {
		return govApplied{}, err
	}
	all, err := store.ListGovernanceUnitsQ(ctx, q)
	if err != nil {
		return govApplied{}, err
	}
	var base *uuid.UUID
	if cur, ok := unitsByID(all)[id]; ok {
		base = cur.ParentID
	}
	if computeETag(unitBaseState(all, id, base)) != ch.BaseHash {
		return govApplied{}, store.ErrGovernanceChangeStale
	}
	err = store.DeleteGovernanceUnitQ(ctx, q, id)
	var children *store.ErrProfileHasChildren
	switch {
	case errors.Is(err, store.ErrNotFound):
		return govApplied{}, writeRefusal(http.StatusNotFound, reasonGovernanceUnitNotFoundByID, "governance profile not found")
	case errors.As(err, &children):
		return govApplied{}, writeRefusal(http.StatusConflict, reasonGovernanceUnitInUse,
			"this governance profile is the base of %s — delete or re-base them first", strings.Join(children.Names, ", "))
	case errors.Is(err, store.ErrConflict):
		return govApplied{}, writeRefusal(http.StatusConflict, reasonGovernanceUnitInUse,
			"this governance profile is still assigned — delete its assignments first")
	case err != nil:
		return govApplied{}, err
	}
	return govApplied{action: "governance.unit.delete", target: id.String()}, nil
}

// ── governance assignments ───────────────────────────────────────────────────

// membershipKey is an assignment's natural key as target_key: the subject type, a colon, the subject.
// A subject type never contains a colon, so the first one splits the two.
func membershipKey(subjectType types.CapabilitySubjectType, subject string) string {
	return string(subjectType) + ":" + subject
}

func splitMembershipKey(key string) (subjectType, subject string, ok bool) {
	return strings.Cut(key, ":")
}

// membershipWriteAuditData is the governance.membership.write row's data, shared by a direct write
// and an approved one.
func membershipWriteAuditData(saved types.GovernanceMembership) map[string]any {
	return map[string]any{
		"subject_type": saved.SubjectType,
		"subject":      saved.Subject,
		"unit_id":      saved.UnitID,
		"priority":     saved.Priority,
	}
}

// readDeleteAudit reads, with the readers a held delete uses, the row a direct delete is about to
// remove and returns the fields its audit row carries: the delete alone would name only an id.
func (s *Server) readDeleteAudit(ctx context.Context, read func(q store.Querier) (map[string]any, error)) (data map[string]any, err error) {
	err = s.cfg.Store.DryRunGovernance(ctx, func(q store.Querier) (err error) {
		data, err = read(q)
		return err
	})
	return data, err
}

type membershipUnitView struct {
	ID      uuid.UUID              `json:"id"`
	Name    string                 `json:"name"`
	Ceiling types.RunPolicySpec    `json:"ceiling"`
	Limits  types.GovernanceLimits `json:"limits"`
	Error   string                 `json:"error,omitempty"`
}

// membershipDiffView is an assignment as the reviewer reads it, with the profile it points at as that
// profile stood at proposal: what the assignment grants is that profile's content, not its name.
type membershipDiffView struct {
	SubjectType types.CapabilitySubjectType `json:"subject_type"`
	Subject     string                      `json:"subject"`
	UnitID      uuid.UUID                   `json:"unit_id"`
	Priority    int                         `json:"priority"`
	Unit        *membershipUnitView         `json:"unit,omitempty"`
}

// newMembershipDiffView renders a with the profile chain (leaf first) it points at. The profile's
// ceiling and limits are its EFFECTIVE ones, and the ceiling passes read-redaction.
func (s *Server) newMembershipDiffView(a types.GovernanceMembership, chain []types.GovernanceUnit) *membershipDiffView {
	v := &membershipDiffView{SubjectType: a.SubjectType, Subject: a.Subject, UnitID: a.UnitID, Priority: a.Priority}
	if len(chain) == 0 {
		return v
	}
	pv := &membershipUnitView{ID: chain[0].ID, Name: chain[0].Name}
	res, err := s.composeChain(chain)
	if err != nil {
		pv.Ceiling, pv.Limits, pv.Error = redactSpecForRead(chain[0].Ceiling, true), chain[0].Limits, adminResolveError(err)
	} else {
		pv.Ceiling, pv.Limits = redactSpecForRead(res.Ceiling, true), res.Limits
	}
	v.Unit = pv
	return v
}

// unitChainQ reads profile id and the bases above it, leaf first, stopping at a missing profile or
// a repeat. forUpdate locks each row for the transaction.
func unitChainQ(ctx context.Context, q store.Querier, id uuid.UUID, forUpdate bool) ([]types.GovernanceUnit, error) {
	chain := []types.GovernanceUnit{}
	seen := map[uuid.UUID]bool{}
	for cur := &id; cur != nil && *cur != uuid.Nil && !seen[*cur] && len(chain) <= maxUnitDepth; {
		row, err := store.GetGovernanceUnitQ(ctx, q, *cur, forUpdate)
		if errors.Is(err, store.ErrNotFound) {
			break
		}
		if err != nil {
			return nil, err
		}
		seen[*cur] = true
		chain = append(chain, row)
		cur = row.ParentID
	}
	return chain, nil
}

// membershipState is the target of an assignment change as it stands: the assignment at its natural
// key (nil when none) and the profile chain it points at (profileID, or the stored row's when
// profileID is nil).
type membershipState struct {
	current *types.GovernanceMembership
	chain   []types.GovernanceUnit
}

// state is what a change's base_hash is computed over. It covers the assignment row AND the profile it
// references, so a rename or a ceiling change to that profile after proposal makes the change stale.
// A proposal and its approval compute one and the same thing.
func (st membershipState) state() any {
	var cur any
	if st.current != nil {
		cur = *st.current
	}
	return map[string]any{"membership": cur, "unit_chain": st.chain}
}

func (st membershipState) hash() string { return computeETag(st.state()) }

func readMembershipState(ctx context.Context, q store.Querier, subjectType, subject string, profileID uuid.UUID, forUpdate bool) (membershipState, error) {
	var st membershipState
	row, err := store.GetGovernanceMembershipByKeyQ(ctx, q, subjectType, subject, forUpdate)
	switch {
	case err == nil:
		st.current = &row
		if profileID == uuid.Nil {
			profileID = row.UnitID
		}
	case !errors.Is(err, store.ErrNotFound):
		return st, err
	}
	if st.chain, err = unitChainQ(ctx, q, profileID, forUpdate); err != nil {
		return st, err
	}
	return st, nil
}

// holdMembershipUpsert stores a validated assignment upsert as a pending change, after a dry run of
// the upsert has shown the store would accept it (an unknown profile is the direct write's own 404).
func (s *Server) holdMembershipUpsert(w http.ResponseWriter, r *http.Request, a types.GovernanceMembership) {
	ctx := r.Context()
	var st membershipState
	var beforeChain []types.GovernanceUnit
	err := s.cfg.Store.DryRunGovernance(ctx, func(q store.Querier) error {
		var err error
		if st, err = readMembershipState(ctx, q, string(a.SubjectType), a.Subject, a.UnitID, false); err != nil {
			return err
		}
		beforeChain = st.chain
		if st.current != nil && st.current.UnitID != a.UnitID {
			if beforeChain, err = unitChainQ(ctx, q, st.current.UnitID, false); err != nil {
				return err
			}
		}
		_, err = store.UpsertGovernanceMembershipQ(ctx, q, a)
		return err
	})
	if notFoundIf(w, err, "governance profile", reasonGovernanceUnitNotFoundByID) {
		return
	}
	if err != nil {
		writeServerError(w, r, "hold governance assignment", err)
		return
	}
	after := s.newMembershipDiffView(a, st.chain)
	p := govProposal{
		kind: govKindMembership, op: "upsert", key: membershipKey(a.SubjectType, a.Subject),
		payload:   governanceMembershipRequest{SubjectType: a.SubjectType, Subject: a.Subject, UnitID: a.UnitID, Priority: a.Priority},
		after:     after,
		baseState: st.state(),
	}
	var before *membershipDiffView
	if st.current != nil {
		before = s.newMembershipDiffView(*st.current, beforeChain)
		p.before = before
	}
	p.changed = changedPaths(before, after)
	s.proposeGovernanceChange(w, r, p)
}

// holdMembershipDelete stores an assignment delete as a pending change.
func (s *Server) holdMembershipDelete(w http.ResponseWriter, r *http.Request, id uuid.UUID) {
	ctx := r.Context()
	var st membershipState
	err := s.cfg.Store.DryRunGovernance(ctx, func(q store.Querier) error {
		row, err := store.GetGovernanceMembershipQ(ctx, q, id, false)
		if err != nil {
			return err
		}
		st, err = readMembershipState(ctx, q, string(row.SubjectType), row.Subject, uuid.Nil, false)
		return err
	})
	if notFoundIf(w, err, "governance assignment", reasonGovernanceMembershipNotFound) {
		return
	}
	if err != nil || st.current == nil {
		writeServerError(w, r, "hold governance assignment delete", errors.Join(err, errors.New("assignment vanished")))
		return
	}
	before := s.newMembershipDiffView(*st.current, st.chain)
	s.proposeGovernanceChange(w, r, govProposal{
		kind: govKindMembership, op: "delete", key: membershipKey(st.current.SubjectType, st.current.Subject),
		payload: map[string]any{"id": id}, before: before, changed: changedPaths(before, nil),
		baseState: st.state(),
	})
}

// applyMembershipChange applies a held assignment change inside the decision transaction. It takes
// the profile-graph lock first (the order a profile delete takes its own locks in, so the two cannot
// deadlock on the assignment rows a delete's foreign-key check wants), then the key lock, then reads
// the assignment and the profile it references FOR UPDATE.
func applyMembershipChange(s *Server, r *http.Request, q store.Querier, ch types.GovernanceChange) (govApplied, error) {
	ctx := r.Context()
	subjectType, subject, ok := splitMembershipKey(ch.TargetKey)
	if !ok {
		return govApplied{}, fmt.Errorf("governance change %s: target key %q is not an assignment key", ch.ID, ch.TargetKey)
	}
	if err := store.LockGovernanceGraph(ctx, q); err != nil {
		return govApplied{}, err
	}
	if err := store.LockGovernanceMembershipKey(ctx, q, subjectType, subject); err != nil {
		return govApplied{}, err
	}
	if ch.Op == "delete" {
		var p struct {
			ID uuid.UUID `json:"id"`
		}
		if err := json.Unmarshal(ch.Payload, &p); err != nil {
			return govApplied{}, fmt.Errorf("governance change %s: payload: %w", ch.ID, err)
		}
		st, err := readMembershipState(ctx, q, subjectType, subject, uuid.Nil, true)
		if err != nil {
			return govApplied{}, err
		}
		if st.hash() != ch.BaseHash {
			return govApplied{}, store.ErrGovernanceChangeStale
		}
		if err := store.DeleteGovernanceMembershipQ(ctx, q, p.ID); err != nil {
			if errors.Is(err, store.ErrNotFound) {
				return govApplied{}, writeRefusal(http.StatusNotFound, reasonGovernanceMembershipNotFound, "governance assignment not found")
			}
			return govApplied{}, err
		}
		return govApplied{action: "governance.membership.delete", target: p.ID.String()}, nil
	}
	var req governanceMembershipRequest
	dec := json.NewDecoder(bytes.NewReader(ch.Payload))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		return govApplied{}, writeRefusal(http.StatusBadRequest, reasonGovernanceMembershipInvalid, "invalid assignment: %v", err)
	}
	a := types.GovernanceMembership{SubjectType: req.SubjectType, Subject: req.Subject, UnitID: req.UnitID, Priority: req.Priority}
	if err := validateGovernanceMembership(&a); err != nil {
		return govApplied{}, writeRefusal(http.StatusBadRequest, reasonGovernanceMembershipInvalid, "invalid assignment: %v", err)
	}
	if err := s.checkUserTypeSubject(ctx, q, a.SubjectType, a.Subject); err != nil {
		return govApplied{}, err
	}
	st, err := readMembershipState(ctx, q, subjectType, subject, a.UnitID, true)
	if err != nil {
		return govApplied{}, err
	}
	if st.hash() != ch.BaseHash {
		return govApplied{}, store.ErrGovernanceChangeStale
	}
	a.ID = uuid.New()
	a.CreatedBy = ch.ProposedBy
	saved, err := store.UpsertGovernanceMembershipQ(ctx, q, a)
	if errors.Is(err, store.ErrNotFound) {
		return govApplied{}, writeRefusal(http.StatusNotFound, reasonGovernanceUnitNotFoundByID, "governance profile not found")
	}
	if err != nil {
		return govApplied{}, err
	}
	return govApplied{action: "governance.membership.write", target: saved.ID.String(), data: membershipWriteAuditData(saved)}, nil
}

// checkUserTypeSubject is userTypeSubjectExists for a replayed write: the refusal as an error.
func (s *Server) checkUserTypeSubject(ctx context.Context, q store.Querier, subjectType types.CapabilitySubjectType, subject string) error {
	if subjectType != types.CapabilitySubjectUserType || subject == types.UserTypeStandard {
		return nil
	}
	_, err := store.GetUserTypeQ(ctx, q, subject, true)
	if errors.Is(err, store.ErrNotFound) {
		return writeRefusal(http.StatusBadRequest, reasonAccessUnknownUserType, "%s", accessUnknownUserType(subject))
	}
	return err
}

// membershipsSnapshotKey carries the governance assignments a profile write's transaction read under
// the graph lock, which every assignment write takes too: the exemption proof judges a rename on the
// assignments the write commits beside.
type membershipsSnapshotKey struct{}

// withGovernanceMemberships carries all for a later renameMaySelect on the returned context.
func withGovernanceMemberships(ctx context.Context, all []types.GovernanceMembership) context.Context {
	return context.WithValue(ctx, membershipsSnapshotKey{}, all)
}

// governanceMembershipsFor is the snapshot withGovernanceMemberships carries, or a fresh read.
func governanceMembershipsFor(ctx context.Context, s *Server) ([]types.GovernanceMembership, error) {
	if all, ok := ctx.Value(membershipsSnapshotKey{}).([]types.GovernanceMembership); ok {
		return all, nil
	}
	return s.cfg.Store.ListGovernanceMemberships(ctx)
}
