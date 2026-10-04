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

// profileChangePayload is a held profile write: the validated request body, with each optional member
// present only when the caller sent it (absent keeps the stored value, null clears it, so the two must
// stay distinguishable when the payload is replayed).
type profileChangePayload struct {
	Name          string                 `json:"name"`
	Ceiling       types.RunPolicySpec    `json:"ceiling"`
	Limits        types.GovernanceLimits `json:"limits"`
	Contact       json.RawMessage        `json:"contact,omitempty"`
	BaseProfileID json.RawMessage        `json:"base_profile_id,omitempty"`
	Overlay       json.RawMessage        `json:"overlay,omitempty"`
	OverlayLimits json.RawMessage        `json:"overlay_limits,omitempty"`
}

func profilePayloadOf(req governanceProfileRequest) profileChangePayload {
	return profileChangePayload{
		Name: req.Name, Ceiling: req.Ceiling, Limits: req.Limits,
		Contact: req.Contact, BaseProfileID: req.BaseProfileID, Overlay: req.Overlay, OverlayLimits: req.OverlayLimits,
	}
}

// profileRequestFromPayload decodes a held profile write and validates it by the rules a direct write
// is judged by. A message is why it no longer validates.
func profileRequestFromPayload(raw json.RawMessage) (governanceProfileRequest, string) {
	var req governanceProfileRequest
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		return governanceProfileRequest{}, "invalid JSON body: " + err.Error()
	}
	req, msg := finishGovernanceProfileRequest(req)
	if msg != "" {
		return governanceProfileRequest{}, msg
	}
	if msg := req.parseComposition(); msg != "" {
		return governanceProfileRequest{}, msg
	}
	return req, ""
}

// heldProfileWrite is what a profile build proved before it was abandoned: every profile as the
// transaction read them, the row it would have stored and what that composes to.
type heldProfileWrite struct {
	all []types.GovernanceProfile
	row types.GovernanceProfile
	res *ResolvedProfile
}

// profileWriteExempt reports whether a human's profile update applies without a second human. Only an
// UPDATE of an existing profile can be exempt, and only by one of two proofs:
//
//   - metadata only: every stored field other than the name is equal to the current row, including the
//     base, the overlay and the overlay limits;
//   - narrowing: the new effective profile is Leq the current effective one (composer.Leq, which
//     carries the full git_pat dominance contract).
//
// A write that changes the contact is held even when Leq holds: a contact is who a refused person is
// told to ask, and nothing proves a new one is not a redirect. Nothing else is exempt.
func (s *Server) profileWriteExempt(all []types.GovernanceProfile, p types.GovernanceProfile, res *ResolvedProfile) bool {
	byID := profilesByID(all)
	cur, exists := byID[p.ID]
	if !exists || res == nil {
		return false
	}
	if !sameJSON(nonZeroContact(cur.Contact), nonZeroContact(profileContactAfter(cur, p))) {
		return false
	}
	if sameJSON(cur.Ceiling, p.Ceiling) && sameJSON(cur.Limits, p.Limits) && sameJSON(cur.BaseProfileID, p.BaseProfileID) &&
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
	return composer.Leq(res.Ceiling, curRes.Ceiling, res.Limits, curRes.Limits)
}

// profileContactAfter is the contact the row holds once p is written over cur.
func profileContactAfter(cur, p types.GovernanceProfile) *policyref.Contact {
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

// profileDiffStored is the stored fields of a profile the reviewer compares, without its identity or
// provenance (which do not change what the profile permits).
type profileDiffStored struct {
	Name          string                 `json:"name"`
	Ceiling       types.RunPolicySpec    `json:"ceiling"`
	Limits        types.GovernanceLimits `json:"limits"`
	Contact       *policyref.Contact     `json:"contact,omitempty"`
	BaseProfileID *uuid.UUID             `json:"base_profile_id,omitempty"`
	Overlay       *types.CeilingOverlay  `json:"overlay,omitempty"`
	OverlayLimits *types.LimitsOverlay   `json:"overlay_limits,omitempty"`
}

// profileDiffView is a profile as the reviewer reads it: the stored fields and what composing them
// gives. Every ceiling in it passes the read-redaction every other read of a ceiling does.
type profileDiffView struct {
	profileDiffStored
	Effective *governanceEffective `json:"effective,omitempty"`
}

func newProfileDiffView(row types.GovernanceProfile, res *ResolvedProfile) *profileDiffView {
	v := &profileDiffView{profileDiffStored: profileDiffStored{
		Name: row.Name, Ceiling: redactSpecForRead(row.Ceiling, true), Limits: row.Limits,
		Contact: nonZeroContact(row.Contact), BaseProfileID: row.BaseProfileID,
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
func (s *Server) resolveForDiff(all []types.GovernanceProfile, id uuid.UUID) *ResolvedProfile {
	chain, err := chainFromRows(profilesByID(all), id)
	if err != nil {
		return nil
	}
	res, err := s.composeChain(chain)
	if err != nil {
		return nil
	}
	return res
}

// profileBaseState is the profile graph as a change to profile id depends on it: the row itself (nil
// when absent) and the chain of bases above base. An approval that finds either changed refuses as
// stale, so the profile the reviewer saw, and what it composes on, is what is written.
func profileBaseState(all []types.GovernanceProfile, id uuid.UUID, base *uuid.UUID) any {
	byID := profilesByID(all)
	state := map[string]any{"self": nil}
	if row, ok := byID[id]; ok {
		state["self"] = row
	}
	bases := []types.GovernanceProfile{}
	seen := map[uuid.UUID]bool{id: true}
	for cur := base; cur != nil && !seen[*cur] && len(bases) <= maxProfileDepth; {
		row, ok := byID[*cur]
		if !ok {
			break
		}
		seen[*cur] = true
		bases = append(bases, row)
		cur = row.BaseProfileID
	}
	state["bases"] = bases
	return state
}

// holdProfileWrite stores a profile create or update that build accepted as a pending change.
func (s *Server) holdProfileWrite(w http.ResponseWriter, r *http.Request, id uuid.UUID, req governanceProfileRequest, h heldProfileWrite) {
	cur, exists := profilesByID(h.all)[id]
	row := h.row
	op := "create"
	var before *profileDiffView
	if exists {
		op = "update"
		before = newProfileDiffView(cur, s.resolveForDiff(h.all, id))
		row.Contact = profileContactAfter(cur, h.row)
	}
	after := newProfileDiffView(row, h.res)
	p := govProposal{
		kind: govKindProfile, op: op, key: id.String(), payload: profilePayloadOf(req),
		after: after, baseState: profileBaseState(h.all, id, h.row.BaseProfileID),
	}
	var beforeStored *profileDiffStored
	if before != nil {
		p.before, beforeStored = before, &before.profileDiffStored
	}
	p.changed = changedPaths(beforeStored, after.profileDiffStored)
	s.proposeGovernanceChange(w, r, p)
}

// holdProfileDelete stores a profile delete as a pending change, after a dry run of the delete has
// shown the store would accept it (so the refusals are the direct delete's own).
func (s *Server) holdProfileDelete(w http.ResponseWriter, r *http.Request, id uuid.UUID) {
	ctx := r.Context()
	var all []types.GovernanceProfile
	err := s.cfg.Store.DryRunGovernance(ctx, func(q store.Querier) error {
		if err := store.LockGovernanceGraph(ctx, q); err != nil {
			return err
		}
		var err error
		if all, err = store.ListGovernanceProfilesQ(ctx, q); err != nil {
			return err
		}
		return store.DeleteGovernanceProfileQ(ctx, q, id)
	})
	if s.writeProfileDeleteError(w, r, err) {
		return
	}
	cur := profilesByID(all)[id]
	before := newProfileDiffView(cur, s.resolveForDiff(all, id))
	s.proposeGovernanceChange(w, r, govProposal{
		kind: govKindProfile, op: "delete", key: id.String(), payload: map[string]any{"id": id},
		before: before, changed: changedPaths(before.profileDiffStored, nil),
		baseState: profileBaseState(all, id, cur.BaseProfileID),
	})
}

// applyProfileChange applies a held profile change inside the decision transaction.
func applyProfileChange(s *Server, r *http.Request, q store.Querier, ch types.GovernanceChange) (govApplied, error) {
	ctx := r.Context()
	id, err := uuid.Parse(ch.TargetKey)
	if err != nil {
		return govApplied{}, fmt.Errorf("governance change %s: target key %q is not a profile id", ch.ID, ch.TargetKey)
	}
	if ch.Op == "delete" {
		return applyProfileDelete(ctx, q, ch, id)
	}
	req, msg := profileRequestFromPayload(ch.Payload)
	if msg != "" {
		return govApplied{}, writeRefusal(http.StatusBadRequest, reasonGovernanceProfileRequestInvalid, "%s", msg)
	}
	var effective *ResolvedProfile
	saved, err := store.WriteGovernanceProfileQ(ctx, q, id, func(all []types.GovernanceProfile) (types.GovernanceProfile, error) {
		base, _, _ := composedFrom(all, id, req.comp)
		if computeETag(profileBaseState(all, id, base)) != ch.BaseHash {
			return types.GovernanceProfile{}, store.ErrGovernanceChangeStale
		}
		p, res, err := s.buildGovernanceProfile(ch.ProposedBy, id, req, all)
		effective = res
		return p, err
	})
	if errors.Is(err, store.ErrConflict) {
		return govApplied{}, writeRefusal(http.StatusConflict, reasonGovernanceProfileNameConflict,
			"a governance profile named %q already exists", req.Name)
	}
	if err != nil {
		return govApplied{}, err
	}
	return govApplied{action: "governance.profile.write", target: saved.ID.String(), data: profileWriteAuditData(saved, effective)}, nil
}

func applyProfileDelete(ctx context.Context, q store.Querier, ch types.GovernanceChange, id uuid.UUID) (govApplied, error) {
	if err := store.LockGovernanceGraph(ctx, q); err != nil {
		return govApplied{}, err
	}
	all, err := store.ListGovernanceProfilesQ(ctx, q)
	if err != nil {
		return govApplied{}, err
	}
	var base *uuid.UUID
	if cur, ok := profilesByID(all)[id]; ok {
		base = cur.BaseProfileID
	}
	if computeETag(profileBaseState(all, id, base)) != ch.BaseHash {
		return govApplied{}, store.ErrGovernanceChangeStale
	}
	err = store.DeleteGovernanceProfileQ(ctx, q, id)
	var children *store.ErrProfileHasChildren
	switch {
	case errors.Is(err, store.ErrNotFound):
		return govApplied{}, writeRefusal(http.StatusNotFound, reasonGovernanceProfileNotFoundByID, "governance profile not found")
	case errors.As(err, &children):
		return govApplied{}, writeRefusal(http.StatusConflict, reasonGovernanceProfileInUse,
			"this governance profile is the base of %s — delete or re-base them first", strings.Join(children.Names, ", "))
	case errors.Is(err, store.ErrConflict):
		return govApplied{}, writeRefusal(http.StatusConflict, reasonGovernanceProfileInUse,
			"this governance profile is still assigned — delete its assignments first")
	case err != nil:
		return govApplied{}, err
	}
	return govApplied{action: "governance.profile.delete", target: id.String()}, nil
}

// ── governance assignments ───────────────────────────────────────────────────

// assignmentKey is an assignment's natural key as target_key: the subject type, a colon, the subject.
// A subject type never contains a colon, so the first one splits the two.
func assignmentKey(subjectType types.CapabilitySubjectType, subject string) string {
	return string(subjectType) + ":" + subject
}

func splitAssignmentKey(key string) (subjectType, subject string, ok bool) {
	return strings.Cut(key, ":")
}

// assignmentWriteAuditData is the governance.assignment.write row's data, shared by a direct write
// and an approved one.
func assignmentWriteAuditData(saved types.GovernanceAssignment) map[string]any {
	return map[string]any{
		"subject_type": saved.SubjectType,
		"subject":      saved.Subject,
		"profile_id":   saved.ProfileID,
		"priority":     saved.Priority,
	}
}

type assignmentProfileView struct {
	ID      uuid.UUID              `json:"id"`
	Name    string                 `json:"name"`
	Ceiling types.RunPolicySpec    `json:"ceiling"`
	Limits  types.GovernanceLimits `json:"limits"`
	Error   string                 `json:"error,omitempty"`
}

// assignmentDiffView is an assignment as the reviewer reads it, with the profile it points at as that
// profile stood at proposal: what the assignment grants is that profile's content, not its name.
type assignmentDiffView struct {
	SubjectType types.CapabilitySubjectType `json:"subject_type"`
	Subject     string                      `json:"subject"`
	ProfileID   uuid.UUID                   `json:"profile_id"`
	Priority    int                         `json:"priority"`
	Profile     *assignmentProfileView      `json:"profile,omitempty"`
}

// newAssignmentDiffView renders a with the profile chain (leaf first) it points at. The profile's
// ceiling and limits are its EFFECTIVE ones, and the ceiling passes read-redaction.
func (s *Server) newAssignmentDiffView(a types.GovernanceAssignment, chain []types.GovernanceProfile) *assignmentDiffView {
	v := &assignmentDiffView{SubjectType: a.SubjectType, Subject: a.Subject, ProfileID: a.ProfileID, Priority: a.Priority}
	if len(chain) == 0 {
		return v
	}
	pv := &assignmentProfileView{ID: chain[0].ID, Name: chain[0].Name}
	res, err := s.composeChain(chain)
	if err != nil {
		pv.Ceiling, pv.Limits, pv.Error = redactSpecForRead(chain[0].Ceiling, true), chain[0].Limits, adminResolveError(err)
	} else {
		pv.Ceiling, pv.Limits = redactSpecForRead(res.Ceiling, true), res.Limits
	}
	v.Profile = pv
	return v
}

// profileChainQ reads profile id and the bases above it, leaf first, stopping at a missing profile or
// a repeat. forUpdate locks each row for the transaction.
func profileChainQ(ctx context.Context, q store.Querier, id uuid.UUID, forUpdate bool) ([]types.GovernanceProfile, error) {
	chain := []types.GovernanceProfile{}
	seen := map[uuid.UUID]bool{}
	for cur := &id; cur != nil && *cur != uuid.Nil && !seen[*cur] && len(chain) <= maxProfileDepth; {
		row, err := store.GetGovernanceProfileQ(ctx, q, *cur, forUpdate)
		if errors.Is(err, store.ErrNotFound) {
			break
		}
		if err != nil {
			return nil, err
		}
		seen[*cur] = true
		chain = append(chain, row)
		cur = row.BaseProfileID
	}
	return chain, nil
}

// assignmentState is the target of an assignment change as it stands: the assignment at its natural
// key (nil when none) and the profile chain it points at (profileID, or the stored row's when
// profileID is nil).
type assignmentState struct {
	current *types.GovernanceAssignment
	chain   []types.GovernanceProfile
}

// state is what a change's base_hash is computed over. It covers the assignment row AND the profile it
// references, so a rename or a ceiling change to that profile after proposal makes the change stale.
// A proposal and its approval compute one and the same thing.
func (st assignmentState) state() any {
	var cur any
	if st.current != nil {
		cur = *st.current
	}
	return map[string]any{"assignment": cur, "profile_chain": st.chain}
}

func (st assignmentState) hash() string { return computeETag(st.state()) }

func readAssignmentState(ctx context.Context, q store.Querier, subjectType, subject string, profileID uuid.UUID, forUpdate bool) (assignmentState, error) {
	var st assignmentState
	row, err := store.GetGovernanceAssignmentByKeyQ(ctx, q, subjectType, subject, forUpdate)
	switch {
	case err == nil:
		st.current = &row
		if profileID == uuid.Nil {
			profileID = row.ProfileID
		}
	case !errors.Is(err, store.ErrNotFound):
		return st, err
	}
	if st.chain, err = profileChainQ(ctx, q, profileID, forUpdate); err != nil {
		return st, err
	}
	return st, nil
}

// holdAssignmentUpsert stores a validated assignment upsert as a pending change, after a dry run of
// the upsert has shown the store would accept it (an unknown profile is the direct write's own 404).
func (s *Server) holdAssignmentUpsert(w http.ResponseWriter, r *http.Request, a types.GovernanceAssignment) {
	ctx := r.Context()
	var st assignmentState
	var beforeChain []types.GovernanceProfile
	err := s.cfg.Store.DryRunGovernance(ctx, func(q store.Querier) error {
		var err error
		if st, err = readAssignmentState(ctx, q, string(a.SubjectType), a.Subject, a.ProfileID, false); err != nil {
			return err
		}
		beforeChain = st.chain
		if st.current != nil && st.current.ProfileID != a.ProfileID {
			if beforeChain, err = profileChainQ(ctx, q, st.current.ProfileID, false); err != nil {
				return err
			}
		}
		_, err = store.UpsertGovernanceAssignmentQ(ctx, q, a)
		return err
	})
	if notFoundIf(w, err, "governance profile", reasonGovernanceProfileNotFoundByID) {
		return
	}
	if err != nil {
		writeServerError(w, r, "hold governance assignment", err)
		return
	}
	after := s.newAssignmentDiffView(a, st.chain)
	p := govProposal{
		kind: govKindAssignment, op: "upsert", key: assignmentKey(a.SubjectType, a.Subject),
		payload:   governanceAssignmentRequest{SubjectType: a.SubjectType, Subject: a.Subject, ProfileID: a.ProfileID, Priority: a.Priority},
		after:     after,
		baseState: st.state(),
	}
	var before *assignmentDiffView
	if st.current != nil {
		before = s.newAssignmentDiffView(*st.current, beforeChain)
		p.before = before
	}
	p.changed = changedPaths(before, after)
	s.proposeGovernanceChange(w, r, p)
}

// holdAssignmentDelete stores an assignment delete as a pending change.
func (s *Server) holdAssignmentDelete(w http.ResponseWriter, r *http.Request, id uuid.UUID) {
	ctx := r.Context()
	var st assignmentState
	err := s.cfg.Store.DryRunGovernance(ctx, func(q store.Querier) error {
		row, err := store.GetGovernanceAssignmentQ(ctx, q, id, false)
		if err != nil {
			return err
		}
		st, err = readAssignmentState(ctx, q, string(row.SubjectType), row.Subject, uuid.Nil, false)
		return err
	})
	if notFoundIf(w, err, "governance assignment", reasonGovernanceAssignmentNotFound) {
		return
	}
	if err != nil || st.current == nil {
		writeServerError(w, r, "hold governance assignment delete", errors.Join(err, errors.New("assignment vanished")))
		return
	}
	before := s.newAssignmentDiffView(*st.current, st.chain)
	s.proposeGovernanceChange(w, r, govProposal{
		kind: govKindAssignment, op: "delete", key: assignmentKey(st.current.SubjectType, st.current.Subject),
		payload: map[string]any{"id": id}, before: before, changed: changedPaths(before, nil),
		baseState: st.state(),
	})
}

// applyAssignmentChange applies a held assignment change inside the decision transaction. It takes
// the profile-graph lock first (the order a profile delete takes its own locks in, so the two cannot
// deadlock on the assignment rows a delete's foreign-key check wants), then the key lock, then reads
// the assignment and the profile it references FOR UPDATE.
func applyAssignmentChange(s *Server, r *http.Request, q store.Querier, ch types.GovernanceChange) (govApplied, error) {
	ctx := r.Context()
	subjectType, subject, ok := splitAssignmentKey(ch.TargetKey)
	if !ok {
		return govApplied{}, fmt.Errorf("governance change %s: target key %q is not an assignment key", ch.ID, ch.TargetKey)
	}
	if err := store.LockGovernanceGraph(ctx, q); err != nil {
		return govApplied{}, err
	}
	if err := store.LockGovernanceAssignmentKey(ctx, q, subjectType, subject); err != nil {
		return govApplied{}, err
	}
	if ch.Op == "delete" {
		var p struct {
			ID uuid.UUID `json:"id"`
		}
		if err := json.Unmarshal(ch.Payload, &p); err != nil {
			return govApplied{}, fmt.Errorf("governance change %s: payload: %w", ch.ID, err)
		}
		st, err := readAssignmentState(ctx, q, subjectType, subject, uuid.Nil, true)
		if err != nil {
			return govApplied{}, err
		}
		if st.hash() != ch.BaseHash {
			return govApplied{}, store.ErrGovernanceChangeStale
		}
		if err := store.DeleteGovernanceAssignmentQ(ctx, q, p.ID); err != nil {
			if errors.Is(err, store.ErrNotFound) {
				return govApplied{}, writeRefusal(http.StatusNotFound, reasonGovernanceAssignmentNotFound, "governance assignment not found")
			}
			return govApplied{}, err
		}
		return govApplied{action: "governance.assignment.delete", target: p.ID.String()}, nil
	}
	var req governanceAssignmentRequest
	dec := json.NewDecoder(bytes.NewReader(ch.Payload))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		return govApplied{}, writeRefusal(http.StatusBadRequest, reasonGovernanceAssignmentInvalid, "invalid assignment: %v", err)
	}
	a := types.GovernanceAssignment{SubjectType: req.SubjectType, Subject: req.Subject, ProfileID: req.ProfileID, Priority: req.Priority}
	if err := validateGovernanceAssignment(&a); err != nil {
		return govApplied{}, writeRefusal(http.StatusBadRequest, reasonGovernanceAssignmentInvalid, "invalid assignment: %v", err)
	}
	if err := s.checkUserTypeSubject(ctx, a.SubjectType, a.Subject); err != nil {
		return govApplied{}, err
	}
	st, err := readAssignmentState(ctx, q, subjectType, subject, a.ProfileID, true)
	if err != nil {
		return govApplied{}, err
	}
	if st.hash() != ch.BaseHash {
		return govApplied{}, store.ErrGovernanceChangeStale
	}
	a.ID = uuid.New()
	a.CreatedBy = ch.ProposedBy
	saved, err := store.UpsertGovernanceAssignmentQ(ctx, q, a)
	if errors.Is(err, store.ErrNotFound) {
		return govApplied{}, writeRefusal(http.StatusNotFound, reasonGovernanceProfileNotFoundByID, "governance profile not found")
	}
	if err != nil {
		return govApplied{}, err
	}
	return govApplied{action: "governance.assignment.write", target: saved.ID.String(), data: assignmentWriteAuditData(saved)}, nil
}

// checkUserTypeSubject is userTypeSubjectExists for a replayed write: the refusal as an error.
func (s *Server) checkUserTypeSubject(ctx context.Context, subjectType types.CapabilitySubjectType, subject string) error {
	if subjectType != types.CapabilitySubjectUserType || subject == types.UserTypeStandard {
		return nil
	}
	_, err := s.cfg.Store.GetUserType(ctx, subject)
	if errors.Is(err, store.ErrNotFound) {
		return writeRefusal(http.StatusBadRequest, reasonAccessUnknownUserType, "%s", accessUnknownUserType(subject))
	}
	return err
}
