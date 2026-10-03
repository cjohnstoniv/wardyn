// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// The governance-profile write door: POST/PUT/DELETE /governance/profiles. A profile is standalone
// (a whole ceiling) or composed (a base plus an overlay that can only narrow it, migration 0109).
// Every write decides its row inside one store transaction under the profile-graph lock
// (store.WriteGovernanceProfile), from the graph as that transaction reads it, so concurrent writes
// cannot together make a cycle or a chain deeper than three.
package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"slices"
	"strings"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/composer"
	"github.com/cjohnstoniv/wardyn/internal/policyref"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// governanceProfileRequest is the POST/PUT body. ID/CreatedAt/UpdatedAt/
// CreatedBy are never accepted from the wire: the id comes from the path (PUT)
// or the server (POST), and provenance is always server-assigned — the same
// rule grantWriteRequest states for capability grants.
//
// The optional members stay raw so absent, null and {} stay distinguishable (an older client that
// does not know a member leaves it out, and that must keep the stored value, never clear it):
// decode parses each into the unexported fields beside it.
type governanceProfileRequest struct {
	Name          string                 `json:"name"`
	Ceiling       types.RunPolicySpec    `json:"ceiling"`
	Limits        types.GovernanceLimits `json:"limits"`
	Contact       json.RawMessage        `json:"contact"`
	BaseProfileID json.RawMessage        `json:"base_profile_id"`
	Overlay       json.RawMessage        `json:"overlay"`
	OverlayLimits json.RawMessage        `json:"overlay_limits"`

	contact    *policyref.Contact
	contactSet bool
	comp       compositionRequest
}

// compositionRequest is the parsed composition members. Each pair is (set, value): not set keeps the
// stored value on a PUT (NULL on a POST), and set with a nil value is the explicit null.
type compositionRequest struct {
	baseSet       bool
	base          *uuid.UUID
	overlaySet    bool
	overlay       *types.CeilingOverlay
	limitsSet     bool
	overlayLimits *types.LimitsOverlay
}

// governanceProfileResponse carries the saved profile plus any OMISSION
// warnings. Warnings are advisory by design and never a refusal: a standalone profile is authored
// from scratch, so "the deployment default carries a denied domain you
// did not" is information an author needs, not an error. A composed profile inherits
// instead of omitting, so it gets only what resolving it says.
type governanceProfileResponse struct {
	Profile  governanceProfileView `json:"profile"`
	Warnings []string              `json:"warnings,omitempty"`
}

// governanceProfileView is a stored profile beside what composing it gives. ceiling and limits stay
// exactly as stored ({} for a composed row); effective is what binds. Admin surfaces only: the base's
// name and overlay never reach a member.
type governanceProfileView struct {
	types.GovernanceProfile
	Effective governanceEffective `json:"effective"`
}

// governanceEffective is a profile's composed content. Warnings and Error carry base-level detail
// and so stay on the admin surfaces; Error is set (and the content empty) when the chain cannot be
// composed.
type governanceEffective struct {
	Ceiling  types.RunPolicySpec    `json:"ceiling"`
	Limits   types.GovernanceLimits `json:"limits"`
	Warnings []string               `json:"warnings,omitempty"`
	Error    string                 `json:"error,omitempty"`
}

// newProfileView pairs a stored profile with its resolution for an admin response. The effective
// ceiling goes through the same redaction the stored one does.
func newProfileView(row types.GovernanceProfile, res *ResolvedProfile, err error) governanceProfileView {
	row.Ceiling = redactSpecForRead(row.Ceiling, true)
	v := governanceProfileView{GovernanceProfile: row}
	switch {
	case err != nil:
		v.Effective.Error = adminResolveError(err)
	case res != nil:
		v.Effective = governanceEffective{
			Ceiling: redactSpecForRead(res.Ceiling, true), Limits: res.Limits, Warnings: res.AdminWarnings,
		}
	}
	return v
}

// adminResolveError is why a profile has no effective content, in the words an administrator needs
// (the unsatisfiable detail names the profile that failed).
func adminResolveError(err error) string {
	if u, ok := isOverlayUnsatisfiable(err); ok {
		return u.Detail
	}
	return err.Error()
}

// profileWriteError is a refusal the write decided, carried out of the store transaction.
type profileWriteError struct {
	status int
	reason string
	msg    string
}

func (e *profileWriteError) Error() string { return e.reason + ": " + e.msg }

func writeRefusal(status int, reason, format string, args ...any) *profileWriteError {
	return &profileWriteError{status: status, reason: reason, msg: fmt.Sprintf(format, args...)}
}

// decodeGovernanceProfileRequest decodes, normalizes and validates a profile
// write body, returning a human-readable message the caller surfaces as 400.
//
// Three gates, in order: strict decoding (an unknown field is a typo that must
// not silently widen behaviour — the LoadPolicySpec discipline); a real name
// (it is the UNIQUE handle and the resolver's tie-break, so blank is not a
// profile); and validatePolicySpec over a ceiling that says anything, which is the SAME
// validation a stored run_policies spec gets — a governance ceiling is a
// RunPolicySpec and must never be held to a weaker standard than a policy that
// merely gets clamped against one. An EMPTY ceiling is not judged here: a composed row stores one
// (its policy is the overlay), and an older client sends it back unchanged. A standalone row's empty
// ceiling is refused by buildGovernanceProfile, which knows which kind of row it is writing.
func decodeGovernanceProfileRequest(w http.ResponseWriter, r *http.Request) (governanceProfileRequest, string) {
	var req governanceProfileRequest
	if msg := decodeStrictMsg(w, r, &req); msg != "" {
		return governanceProfileRequest{}, msg
	}
	return finishGovernanceProfileRequest(req)
}

// finishGovernanceProfileRequest is the validation a decoded profile body gets: the part of
// decodeGovernanceProfileRequest after the bytes are read, so a held change's payload is judged by
// the same rules when it is replayed on approval.
func finishGovernanceProfileRequest(req governanceProfileRequest) (governanceProfileRequest, string) {
	req.Name = strings.TrimSpace(req.Name)
	if req.Name == "" {
		return governanceProfileRequest{}, "name is required"
	}
	if len(req.Name) > maxGovernanceProfileNameLen || !controlCharFree(req.Name) {
		return governanceProfileRequest{}, "name is invalid"
	}
	if !reflect.DeepEqual(req.Ceiling, types.RunPolicySpec{}) {
		if err := validatePolicySpec(req.Ceiling); err != nil {
			return governanceProfileRequest{}, "invalid ceiling: " + err.Error()
		}
	}
	// The limits get their own write boundary, matching the sibling ORG block's
	// identical shape (validateStorageProviders, providers400Negative). Nothing downstream
	// mis-enforces a negative — every reader treats <= 0 as unlimited — but this is
	// the door the console's own nonNegativeInt does not cover, and a stored -5
	// renders on the profile editor as a cap that binds nothing. 0 stays
	// unlimited/unset on all three.
	if msg := governanceLimitsRefusal(req.Limits); msg != "" {
		return governanceProfileRequest{}, msg
	}
	var contactMsg string
	if req.contact, req.contactSet, contactMsg = parseProfileContact(req.Contact); contactMsg != "" {
		return governanceProfileRequest{}, contactMsg
	}
	return req, ""
}

// parseComposition reads the three composition members. A message is the 400
// governance_overlay_invalid; the overlay is decoded strictly, so an unknown key is refused rather
// than ignored as a narrowing that never happened.
func (req *governanceProfileRequest) parseComposition() string {
	if raw := req.BaseProfileID; len(raw) > 0 {
		req.comp.baseSet = true
		if string(raw) != "null" {
			var s string
			if err := json.Unmarshal(raw, &s); err != nil {
				return "base_profile_id: must be a profile id or null"
			}
			id, err := uuid.Parse(s)
			if err != nil || id == uuid.Nil {
				return "base_profile_id: not a profile id"
			}
			req.comp.base = &id
		}
	}
	if raw := req.Overlay; len(raw) > 0 {
		req.comp.overlaySet = true
		if string(raw) != "null" {
			o, err := types.DecodeCeilingOverlay(raw)
			if err != nil {
				return "invalid overlay: " + err.Error()
			}
			req.comp.overlay = &o
		}
	}
	if raw := req.OverlayLimits; len(raw) > 0 {
		req.comp.limitsSet = true
		if string(raw) != "null" {
			l, err := types.DecodeLimitsOverlay(raw)
			if err != nil {
				return "invalid overlay_limits: " + err.Error()
			}
			req.comp.overlayLimits = &l
		}
	}
	return ""
}

// writeGovernanceProfile is the shared body of POST and PUT: build the row under the graph lock,
// persist, audit, and answer with the saved row plus warnings. id is the row to write (a fresh one
// for POST, the path's for PUT) and status the success code.
//
// With WARDYN_GOVERNANCE_SECOND_HUMAN on, a human's write is decoded and built exactly as above and
// then, unless the build proves it narrowing (profileWriteExempt), held for approval instead of
// stored: the same transaction that would have stored it is abandoned and the change is proposed.
func (s *Server) writeGovernanceProfile(w http.ResponseWriter, r *http.Request, id uuid.UUID, status int) {
	mode, ok := s.governanceWriteMode(w, r)
	if !ok {
		return
	}
	req, msg := decodeGovernanceProfileRequest(w, r)
	if msg != "" {
		writeErrorReason(w, http.StatusBadRequest, reasonGovernanceProfileRequestInvalid, msg)
		return
	}
	if msg := req.parseComposition(); msg != "" {
		writeErrorReason(w, http.StatusBadRequest, reasonGovernanceOverlayInvalid, msg)
		return
	}
	var effective *ResolvedProfile
	var held *heldProfileWrite
	createdBy := principalFromRequest(r)
	saved, err := s.cfg.Store.WriteGovernanceProfile(r.Context(), id,
		func(all []types.GovernanceProfile) (types.GovernanceProfile, error) {
			p, res, err := s.buildGovernanceProfile(createdBy, id, req, all)
			effective = res
			if err == nil && mode == govQueue && !s.profileWriteExempt(all, p, res) {
				held = &heldProfileWrite{all: all, row: p, res: res}
				return p, errHoldForApproval
			}
			return p, err
		})
	if held != nil && errors.Is(err, errHoldForApproval) {
		s.holdProfileWrite(w, r, id, req, *held)
		return
	}
	var refusal *profileWriteError
	switch {
	case errors.As(err, &refusal):
		writeErrorReason(w, refusal.status, refusal.reason, refusal.msg)
		return
	case errors.Is(err, store.ErrConflict):
		writeErrorReason(w, http.StatusConflict, reasonGovernanceProfileNameConflict,
			fmt.Sprintf("a governance profile named %q already exists", req.Name))
		return
	case err != nil:
		writeServerError(w, r, "write governance profile", err)
		return
	}
	s.recordAudit(r.Context(), s.auditEvent(nil, actorTypeFromRequest(r), principalFromRequest(r),
		"governance.profile.write", saved.ID.String(), "success", mustJSON(profileWriteAuditData(saved, effective))))
	if mode == govBypass {
		s.recordGovernanceBypass(r, govKindProfile, saved.ID.String(), "success", nil)
	}
	var warns []string
	if !saved.Composed() {
		warns = governanceOmissionWarnings(saved.Ceiling, s.cfg.DefaultPolicy)
	}
	warns = append(warns, effective.AdminWarnings...)
	writeJSON(w, status, governanceProfileResponse{Profile: newProfileView(saved, effective, nil), Warnings: warns})
}

// profileWriteAuditData is the governance.profile.write row's data, shared by a direct write and an
// approved one.
func profileWriteAuditData(saved types.GovernanceProfile, effective *ResolvedProfile) map[string]any {
	contactFields, contactURL := contactAudit(saved.Contact)
	return map[string]any{
		"name": saved.Name,
		// The EFFECTIVE values: a composed row stores no raw ceiling or limits, and an allow-all
		// base reaches its children, so the row's own columns would record nothing.
		"min_confinement_class": effective.Ceiling.MinConfinementClass,
		"allow_all_egress":      effective.Ceiling.AllowAllEgress,
		"limits":                effective.Limits,
		"contact_fields":        contactFields,
		"contact_request_url":   contactURL,
		"base_profile_id":       saved.BaseProfileID,
		"overlay_fields":        overlayFieldNames(saved),
	}
}

// overlayFieldNames is the names of the overlay members a composed row carries, never their values.
func overlayFieldNames(p types.GovernanceProfile) []string {
	names := []string{}
	if p.Overlay != nil {
		names = append(names, profileJSONKeys(p.Overlay)...)
	}
	if p.OverlayLimits != nil {
		names = append(names, profileJSONKeys(p.OverlayLimits)...)
	}
	slices.Sort(names)
	return names
}

func profileJSONKeys(v any) []string {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	var m map[string]json.RawMessage
	if json.Unmarshal(raw, &m) != nil {
		return nil
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	return keys
}

// composedFrom is the composition a write leaves on profile id, from the stored row (when one exists)
// and what the request said. Absent keeps the stored composition; null clears it (an older client
// that does not know the members therefore cannot flatten a profile).
func composedFrom(all []types.GovernanceProfile, id uuid.UUID, comp compositionRequest) (base *uuid.UUID, overlay *types.CeilingOverlay, limits *types.LimitsOverlay) {
	if i := slices.IndexFunc(all, func(x types.GovernanceProfile) bool { return x.ID == id }); i >= 0 {
		base, overlay, limits = all[i].BaseProfileID, all[i].Overlay, all[i].OverlayLimits
	}
	if comp.overlaySet {
		if overlay = comp.overlay; overlay == nil {
			base, limits = nil, nil
		}
	}
	if comp.baseSet {
		base = comp.base
	}
	if comp.limitsSet {
		limits = comp.overlayLimits
	}
	return base, overlay, limits
}

// buildGovernanceProfile decides the row one write stores and what it resolves to, from every
// profile as the write transaction reads them (all). Anything it returns as *profileWriteError is a
// refusal the caller can fix; the row is stored only if it returns nil.
func (s *Server) buildGovernanceProfile(createdBy string, id uuid.UUID, req governanceProfileRequest, all []types.GovernanceProfile) (types.GovernanceProfile, *ResolvedProfile, error) {
	p := types.GovernanceProfile{
		ID: id, Name: req.Name, CreatedBy: createdBy, Contact: req.contact, ContactSet: req.contactSet,
	}
	base, overlay, limits := composedFrom(all, id, req.comp)
	switch {
	case overlay == nil && base != nil:
		return p, nil, writeRefusal(http.StatusBadRequest, reasonGovernanceOverlayInvalid, "base_profile_id: a base needs an overlay (send overlay: {} for the base unchanged)")
	case overlay == nil && limits != nil:
		return p, nil, writeRefusal(http.StatusBadRequest, reasonGovernanceOverlayInvalid, "overlay_limits: needs an overlay")
	}
	if overlay != nil {
		if !reflect.DeepEqual(req.Ceiling, types.RunPolicySpec{}) || !reflect.DeepEqual(req.Limits, types.GovernanceLimits{}) {
			return p, nil, writeRefusal(http.StatusBadRequest, reasonGovernanceOverlayInvalid,
				"a composed profile states its policy in overlay and overlay_limits: ceiling and limits must be empty")
		}
		p.BaseProfileID, p.Overlay, p.OverlayLimits = base, overlay, limits
	} else {
		// A standalone row's ceiling is judged in full here, an empty one included (decode skips it).
		if err := validatePolicySpec(req.Ceiling); err != nil {
			return p, nil, writeRefusal(http.StatusBadRequest, reasonGovernanceProfileRequestInvalid, "invalid ceiling: %v", err)
		}
		// The structural bound (governance_grantbound.go). A profile may narrow the
		// deployment's credential eligibility; it may never mint eligibility the
		// deployer never provisioned. Refused at write, and re-checked at resolve
		// time because Config.DefaultPolicy is env-borne and a redeploy that drops
		// a pairing must not leave old profiles serving it.
		if err := governanceGrantsWithinCeiling(req.Ceiling.EligibleGrants, s.cfg.DefaultPolicy.EligibleGrants); err != nil {
			return p, nil, writeRefusal(http.StatusBadRequest, reasonGovernanceCeilingInvalid, "invalid ceiling: %v", err)
		}
		p.Ceiling, p.Limits = req.Ceiling, req.Limits
	}
	return s.checkGraphWrite(p, all)
}

// checkGraphWrite refuses a row that would break the graph and resolves it: its base must exist, no
// chain through it may repeat or pass three profiles, an overlay may name nothing its base does not
// permit, and no descendant may be left with a policy nothing satisfies.
func (s *Server) checkGraphWrite(p types.GovernanceProfile, all []types.GovernanceProfile) (types.GovernanceProfile, *ResolvedProfile, error) {
	proposed := append(slices.DeleteFunc(slices.Clone(all), func(x types.GovernanceProfile) bool { return x.ID == p.ID }), p)
	byID := profilesByID(proposed)
	if p.BaseProfileID != nil {
		if _, ok := byID[*p.BaseProfileID]; !ok {
			return p, nil, writeRefusal(http.StatusBadRequest, reasonGovernanceOverlayInvalid, "base_profile_id: no such profile")
		}
	}
	descendants := descendantsOf(proposed, p.ID)
	for _, row := range append([]types.GovernanceProfile{p}, descendants...) {
		if _, err := chainFromRows(byID, row.ID); err != nil {
			switch {
			case errors.Is(err, errProfileCycle):
				return p, nil, writeRefusal(http.StatusConflict, reasonGovernanceProfileCycle,
					"this change would make %q (through its base) its own base", row.Name)
			case errors.Is(err, errProfileDepth):
				return p, nil, writeRefusal(http.StatusConflict, reasonGovernanceProfileDepth,
					"this change would put %q more than %d profiles deep", row.Name, maxProfileDepth)
			}
			return p, nil, err
		}
	}
	chain, err := chainFromRows(byID, p.ID)
	if err != nil {
		return p, nil, err
	}
	if p.Composed() {
		if ref := s.checkOverlay(p, chain); ref != nil {
			return p, nil, ref
		}
	}
	res, err := s.composeChain(chain)
	if err != nil {
		return p, nil, unsatisfiableWrite(err, p.Name)
	}
	for _, d := range descendants {
		dchain, err := chainFromRows(byID, d.ID)
		if err == nil {
			_, err = s.composeChain(dchain)
		}
		if err != nil {
			if u, ok := isOverlayUnsatisfiable(err); ok {
				return p, nil, writeRefusal(http.StatusConflict, reasonGovernanceOverlayUnsatisfiable,
					"this change would leave profile %q with a policy nothing satisfies (%s)", d.Name, u.Detail)
			}
			return p, nil, err
		}
	}
	return p, res, nil
}

// unsatisfiableWrite maps a composition failure of the row being written to its refusal.
func unsatisfiableWrite(err error, name string) error {
	if u, ok := isOverlayUnsatisfiable(err); ok {
		return writeRefusal(http.StatusConflict, reasonGovernanceOverlayUnsatisfiable,
			"profile %q cannot be applied: %s", name, u.Detail)
	}
	return err
}

// checkOverlay is the strict, write-time reading of an overlay against its resolved base: a value
// outside the base, a grant the base does not hold, or an overlay that would make the effective
// policy invalid is the author's to fix.
func (s *Server) checkOverlay(p types.GovernanceProfile, chain []types.GovernanceProfile) *profileWriteError {
	base := s.deploymentAuthority()
	if len(chain) > 1 {
		res, err := s.composeChain(chain[1:])
		if err != nil {
			return writeRefusal(http.StatusConflict, reasonGovernanceOverlayUnsatisfiable,
				"the profile this one builds on cannot be applied: %s", adminResolveError(err))
		}
		base = composer.Authority{Ceiling: res.Ceiling, Limits: res.Limits}
	}
	if p.Overlay.EligibleGrants != nil {
		for _, g := range *p.Overlay.EligibleGrants {
			if err := governanceGrantWithinCeiling(g, base.Ceiling.EligibleGrants); err != nil {
				return writeRefusal(http.StatusBadRequest, reasonGovernanceOverlayInvalid, "overlay.eligible_grants: %v", err)
			}
		}
	}
	ov := overlayOf(p)
	// Every refusal of the author's own overlay is a 400, an empty meet included (a method the base
	// excludes): 409 governance_overlay_unsatisfiable is for a BASE change that strands a descendant.
	if err := composer.ValidateOverlay(base, ov); err != nil {
		return writeRefusal(http.StatusBadRequest, reasonGovernanceOverlayInvalid, "%v", err)
	}
	// What the overlay adds (an llm_inspection mode, a tool rule, a UI app) gets the validation a
	// ceiling gets, but only when the overlay is what made it fail: a deployment default this
	// validator dislikes is not the author's to fix.
	if eff, _, err := composer.ApplyOverlay(base, ov); err == nil {
		if verr := validatePolicySpec(eff.Ceiling); verr != nil && validatePolicySpec(base.Ceiling) == nil {
			return writeRefusal(http.StatusBadRequest, reasonGovernanceOverlayInvalid, "invalid overlay: %v", verr)
		}
	}
	return nil
}

// descendantsOf is every profile that composes, directly or through other profiles, on id, by name.
// It walks with a visited set, so a seeded cycle ends the walk instead of looping.
func descendantsOf(rows []types.GovernanceProfile, id uuid.UUID) []types.GovernanceProfile {
	byID := profilesByID(rows)
	var out []types.GovernanceProfile
	for _, r := range rows {
		if r.ID == id {
			continue
		}
		seen := map[uuid.UUID]bool{r.ID: true}
		for cur := r.BaseProfileID; cur != nil && !seen[*cur]; cur = byID[*cur].BaseProfileID {
			if *cur == id {
				out = append(out, r)
				break
			}
			seen[*cur] = true
			if _, ok := byID[*cur]; !ok {
				break
			}
		}
	}
	slices.SortFunc(out, func(a, b types.GovernanceProfile) int { return strings.Compare(a.Name, b.Name) })
	return out
}

// handleCreateGovernanceProfile mints a fresh id and writes a new profile
// (201). A name another profile already holds is a caller-fixable 409, never a
// raw driver error. operatorOnly (routes.go).
func (s *Server) handleCreateGovernanceProfile(w http.ResponseWriter, r *http.Request) {
	s.writeGovernanceProfile(w, r, uuid.New(), http.StatusCreated)
}

// handleUpdateGovernanceProfile replaces the profile at {id} (200), rename
// included — renaming has to work, because ON DELETE RESTRICT makes
// delete-and-recreate impossible for a profile that is actually assigned. A PUT
// naming an id no row holds creates it there, which is what PUT means.
// operatorOnly (routes.go).
func (s *Server) handleUpdateGovernanceProfile(w http.ResponseWriter, r *http.Request) {
	id, ok := parseIDParam(w, r, "id", "governance profile")
	if !ok {
		return
	}
	s.writeGovernanceProfile(w, r, id, http.StatusOK)
}

// handleDeleteGovernanceProfile removes a profile (204), or 409 when it is
// still ASSIGNED or still the base of another profile.
//
// The 409 is the whole point of the FK's ON DELETE RESTRICT: cascading
// the assignments away would move every member of this profile back to the
// deployment ceiling — a silent WIDENING, with no audit line saying so and
// nothing for an admin to notice. Refusing makes the widening a deliberate,
// separately-audited act (delete the assignments first). A base with children is refused the same
// way, naming them, because deleting it would widen every child to the deployment. operatorOnly
// (routes.go).
func (s *Server) handleDeleteGovernanceProfile(w http.ResponseWriter, r *http.Request) {
	mode, ok := s.governanceWriteMode(w, r)
	if !ok {
		return
	}
	id, ok := parseIDParam(w, r, "id", "governance profile")
	if !ok {
		return
	}
	if mode == govQueue {
		// A delete is never exempt: it widens everything its profile bound, and the store refuses
		// an assigned profile only after the fact.
		s.holdProfileDelete(w, r, id)
		return
	}
	if s.writeProfileDeleteError(w, r, s.cfg.Store.DeleteGovernanceProfile(r.Context(), id)) {
		return
	}
	s.recordAudit(r.Context(), s.auditEvent(nil, actorTypeFromRequest(r), principalFromRequest(r),
		"governance.profile.delete", id.String(), "success", nil))
	if mode == govBypass {
		s.recordGovernanceBypass(r, govKindProfile, id.String(), "success", nil)
	}
	w.WriteHeader(http.StatusNoContent)
}

// writeProfileDeleteError answers a failed profile delete (or a dry run of one) and reports true, or
// reports false for a nil error.
func (s *Server) writeProfileDeleteError(w http.ResponseWriter, r *http.Request, err error) bool {
	if err == nil {
		return false
	}
	if notFoundIf(w, err, "governance profile", reasonGovernanceProfileNotFoundByID) {
		return true
	}
	var children *store.ErrProfileHasChildren
	if errors.As(err, &children) {
		writeErrorReason(w, http.StatusConflict, reasonGovernanceProfileInUse, fmt.Sprintf(
			"this governance profile is the base of %s — delete or re-base them first "+
				"(deleting it would silently widen them back to the deployment ceiling)", strings.Join(children.Names, ", ")))
		return true
	}
	if errors.Is(err, store.ErrConflict) {
		writeErrorReason(w, http.StatusConflict, reasonGovernanceProfileInUse,
			"this governance profile is still assigned — delete its assignments first "+
				"(deleting it while assigned would silently widen everyone it bounds back to the deployment ceiling)")
		return true
	}
	writeServerError(w, r, "delete governance profile", err)
	return true
}
