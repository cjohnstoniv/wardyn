// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/composer"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// A held change's verdict: what the check that held it found.
const (
	// verdictWidens: the change makes at least one field more permissive than it now is.
	verdictWidens = "widens"
	// verdictNarrowsHeld: nothing widens, and the change is held for a reason held_because names.
	verdictNarrowsHeld = "narrows_held"
	// verdictNotChecked: no check ran, or the check could not say why it held the change.
	verdictNotChecked = "not_checked"
)

// Why a change that widens nothing is still held.
const (
	heldContact        = "contact"         // the contact a refused person is told to ask changes
	heldLockRemoved    = "lock_removed"    // a lock comes off the unit, or a re-parent leaves one behind
	heldRename         = "rename"          // a rename can change which profile a person is given
	heldLegacyShift    = "legacy_shift"    // a move or rename changes whose legacy row wins
	heldFirstAllowlist = "first_allowlist" // a restriction names an allowlist where it had none
)

// Fields a widening names beyond a run policy's own (composer.LeqWidenings).
const (
	wideningLimits         = "limits"          // composer.Leq fails on the limits alone
	wideningMemberCap      = "member_cap"      // the member CPU or memory cap rises (memberCapRaises)
	wideningAllowedDomains = "allowed_domains" // a restriction's allowlist gains entries
	wideningWithheldGrants = "withheld_grants" // a restriction stops withholding a grant
	wideningVCS            = "vcs"             // a restriction's version-control caps loosen
	wideningMembers        = "members"         // a restriction stops binding someone it bound
	wideningOwningUnit     = "owning_unit"     // a workspace or component pin moves
)

// changeReview is diff.review: the structured reason a governance change was held, computed by the
// check that held it, stored with the change at proposal. It names no key "ceiling" or "overlay":
// changeForRead decodes those as specs at the member tier and serves no diff when one fails. Its
// entries never carry a mount's host path, and its risk items name only a mount's target.
type changeReview struct {
	Verdict     string              `json:"verdict"`
	Widens      []composer.Widening `json:"widens,omitempty"`
	HeldBecause []string            `json:"held_because,omitempty"`
	Risk        *changeRisk         `json:"risk,omitempty"`
}

// changeRisk is the deterministic risk grade of a policy-shaped change: before is nil for a create.
type changeRisk struct {
	Before *riskGrade `json:"before"`
	After  riskGrade  `json:"after"`
}

// riskGrade is one side's grade: composer.Grade's items and their highest level.
type riskGrade struct {
	Overall composer.RiskLevel  `json:"overall"`
	Items   []composer.RiskItem `json:"items"`
}

// notCheckedReview is the review of a held change no check judged.
func notCheckedReview() *changeReview { return &changeReview{Verdict: verdictNotChecked} }

// reviewFrom is the review of what a check found: widens when anything widens, else narrows_held when
// a reason holds it, else not_checked. A check that could not judge the change (checked false) gives
// not_checked whatever it found.
func reviewFrom(widens []composer.Widening, held []string, checked bool) *changeReview {
	if !checked {
		return notCheckedReview()
	}
	return (&changeReview{Verdict: verdictWidens, Widens: widens, HeldBecause: held}).settled()
}

// settled is the review as it is stored: a widens verdict always names a widening and a narrows_held
// verdict a reason, so a check that held a write for a reason the review does not model stores
// not_checked rather than an empty claim. renderGovernanceDiff applies it to every review it stores.
func (r *changeReview) settled() *changeReview {
	if r == nil {
		return nil
	}
	out := *r
	switch {
	case out.Verdict == verdictNotChecked:
		out.Widens, out.HeldBecause = nil, nil
	case len(out.Widens) > 0:
		out.Verdict = verdictWidens
	case len(out.HeldBecause) > 0:
		out.Verdict = verdictNarrowsHeld
	default:
		out.Verdict, out.Widens, out.HeldBecause = verdictNotChecked, nil, nil
	}
	return &out
}

// withRisk is r with the risk of a change from before (nil for a create) to after. A baseline that
// cannot be read leaves the risk out: the grade is advisory and never changes the hold.
func (s *Server) withRisk(ctx context.Context, r *changeReview, before *types.RunPolicySpec, after types.RunPolicySpec) *changeReview {
	if r == nil {
		r = notCheckedReview()
	}
	out := *r
	b, err := s.egressBaseline(ctx)
	if err != nil {
		return &out
	}
	grade := func(spec types.RunPolicySpec) riskGrade {
		items := composer.Grade(composer.RunInput{Baseline: b}, spec)
		if items == nil {
			items = []composer.RiskItem{}
		}
		return riskGrade{Overall: composer.OverallLevel(items), Items: items}
	}
	out.Risk = &changeRisk{After: grade(after)}
	if before != nil {
		g := grade(*before)
		out.Risk.Before = &g
	}
	return &out
}

// addWidening is list with field named, its entries merged into a field the list already names.
func addWidening(list []composer.Widening, field string, entries []string) []composer.Widening {
	if i := slices.IndexFunc(list, func(w composer.Widening) bool { return w.Field == field }); i >= 0 {
		for _, e := range entries {
			if !slices.Contains(list[i].Entries, e) {
				list[i].Entries = append(list[i].Entries, e)
			}
		}
		return list
	}
	return append(list, composer.Widening{Field: field, Entries: entries})
}

// wallWidenings is what keeps next's walls from being within cur's, as the narrowing proofs judge it:
// the policy fields composer.LeqWidenings names when composer.Leq fails (limits when no single field
// accounts for it), and the member cap where it rises.
func (s *Server) wallWidenings(next, cur *ResolvedUnit) []composer.Widening {
	var out []composer.Widening
	if !composer.Leq(next.Ceiling, cur.Ceiling, next.Limits, cur.Limits) {
		out = composer.LeqWidenings(next.Ceiling, cur.Ceiling)
		if len(out) == 0 {
			out = append(out, composer.Widening{Field: wideningLimits})
		}
	}
	if raises := memberCapRaises(s.resolvedMemberCap(next), s.resolvedMemberCap(cur)); len(raises) > 0 {
		out = append(out, composer.Widening{Field: wideningMemberCap, Entries: raises})
	}
	return out
}

// unitMetadataOnly reports whether p changes no stored field of cur other than the name: the base,
// the overlay and the overlay limits included.
func unitMetadataOnly(cur, p types.GovernanceUnit) bool {
	return sameJSON(cur.Ceiling, p.Ceiling) && sameJSON(cur.Limits, p.Limits) && sameJSON(cur.ParentID, p.ParentID) &&
		sameJSON(cur.Overlay, p.Overlay) && sameJSON(cur.OverlayLimits, p.OverlayLimits)
}

// unitWriteVerdict is unitWriteExempt's decision and, for a write it holds, the review stored with
// the held change (unitWriteReview). The decision is the exemption's proofs in their order; the review
// is computed only once the write is held, so it never changes what the decision reads.
func (s *Server) unitWriteVerdict(ctx context.Context, all []types.GovernanceUnit, p types.GovernanceUnit, res *ResolvedUnit) (exempt bool, review *changeReview) {
	byID := unitsByID(all)
	cur, exists := byID[p.ID]
	if !exists || res == nil || locksRemoved(cur.LockedKnobs, p.LockedKnobs) || s.reparentDropsLocks(all, cur, p) {
		return false, s.unitWriteReview(ctx, all, p, res)
	}
	if !sameJSON(nonZeroContact(cur.Contact), nonZeroContact(unitContactAfter(cur, p))) {
		return false, s.unitWriteReview(ctx, all, p, res)
	}
	if cur.Name != p.Name && s.renameMaySelect(ctx, p.ID) {
		return false, s.unitWriteReview(ctx, all, p, res)
	}
	if unitMetadataOnly(cur, p) {
		return true, nil
	}
	if s.envelopeExempt(ctx, all, p, res) {
		return true, nil
	}
	chain, err := chainFromRows(byID, p.ID, store.MaxUnitDepthCap)
	if err != nil {
		return false, s.unitWriteReview(ctx, all, p, res)
	}
	curRes, err := s.composeChain(chain)
	if err != nil {
		return false, s.unitWriteReview(ctx, all, p, res)
	}
	// Leq reads a maximum set where none was as tighter; the member cap reads it as the size, so the
	// cap is compared too.
	if composer.Leq(res.Ceiling, curRes.Ceiling, res.Limits, curRes.Limits) &&
		len(memberCapRaises(s.resolvedMemberCap(res), s.resolvedMemberCap(curRes))) == 0 {
		return true, nil
	}
	return false, s.unitWriteReview(ctx, all, p, res)
}

// unitWriteReview is the review of a held profile write, plus any reasons extra names: why it is held
// (a lock removed, the contact, a rename that may select) and what widens against the current
// effective profile, never against an envelope's walls. A create, a write that did not resolve and a
// current profile that does not compose are not_checked.
func (s *Server) unitWriteReview(ctx context.Context, all []types.GovernanceUnit, p types.GovernanceUnit, res *ResolvedUnit, extra ...string) *changeReview {
	byID := unitsByID(all)
	cur, exists := byID[p.ID]
	if !exists || res == nil {
		return notCheckedReview()
	}
	var held []string
	if locksRemoved(cur.LockedKnobs, p.LockedKnobs) || s.reparentDropsLocks(all, cur, p) {
		held = append(held, heldLockRemoved)
	}
	if !sameJSON(nonZeroContact(cur.Contact), nonZeroContact(unitContactAfter(cur, p))) {
		held = append(held, heldContact)
	}
	if cur.Name != p.Name && s.renameMaySelect(ctx, p.ID) {
		held = append(held, heldRename)
	}
	for _, e := range extra {
		if !slices.Contains(held, e) {
			held = append(held, e)
		}
	}
	if unitMetadataOnly(cur, p) {
		return reviewFrom(nil, held, true)
	}
	chain, err := chainFromRows(byID, p.ID, store.MaxUnitDepthCap)
	if err != nil {
		return notCheckedReview()
	}
	curRes, err := s.composeChain(chain)
	if err != nil {
		return notCheckedReview()
	}
	return reviewFrom(s.wallWidenings(res, curRes), held, true)
}

// moveReview is the review of a unit admin's held membership move from walls src to walls dst: what
// widens from src to dst, and legacy_shift when the move changes whose legacy row wins. A path that
// does not compose (src or dst nil) is not_checked.
func (s *Server) moveReview(src, dst *ResolvedUnit, shift legacyShift) *changeReview {
	if src == nil || dst == nil {
		return notCheckedReview()
	}
	var held []string
	if len(shift.activates)+len(shift.vacates) > 0 {
		held = append(held, heldLegacyShift)
	}
	return reviewFrom(s.wallWidenings(dst, src), held, true)
}

// pinReview is the review of a held workspace or component pin move.
func pinReview() *changeReview {
	return &changeReview{Verdict: verdictWidens, Widens: []composer.Widening{{Field: wideningOwningUnit}}}
}

// unitReviewRisk is review with the risk of profile id's write resolving to res: before is the current
// profile's composition, none for a create. An update whose current profile does not compose, or a
// write that did not resolve, is left ungraded.
func (s *Server) unitReviewRisk(ctx context.Context, all []types.GovernanceUnit, id uuid.UUID, review *changeReview, res *ResolvedUnit) *changeReview {
	if res == nil {
		return review
	}
	var before *types.RunPolicySpec
	if _, exists := unitsByID(all)[id]; exists {
		cur := s.resolveForDiff(all, id)
		if cur == nil {
			return review
		}
		before = &cur.Ceiling
	}
	return s.withRisk(ctx, review, before, res.Ceiling)
}

// restrictionHeldReview is the review a held restriction write stores, nil for one that applies
// directly: a create is held only for naming an allowlist (first_allowlist), and a replacement only
// when it is not at least as strict as cur (restrictionReview).
func restrictionHeldReview(create bool, cur types.Restriction, body types.RestrictionWrite) *changeReview {
	if create {
		if body.Spec.AllowedDomains == nil {
			return nil
		}
		return reviewFrom(nil, []string{heldFirstAllowlist}, true)
	}
	if restrictionAtLeastAsStrict(cur, body) {
		return nil
	}
	return restrictionReview(cur, body)
}

// reviewFor is the review a write of kind to targetKey would be held with: the verdict builder the hold
// sites use, called with no request and no proposal. after is the write's body as its route takes it
// (nil for a delete, which no check judges); before, when set, stands in for the stored row (a profile
// as a types.GovernanceUnit, a restriction as a types.Restriction), and when nil the stored row is read.
// caller says who writes, the parts of the request a hold reads: a unit admin's rename is also held for
// a legacy shift (unitWriteApplies), and a super admin's body may set a mount's host path. The envelope
// proof reads ctx's caller, as the write's does.
//
// A nil review with no error is a write that applies without approval. A profile or a restriction is
// judged in full; a pin answers the review its held move stores; a request kind stores none; every
// other kind is not_checked. The write's refusals are not repeated here, except that a body its route
// would refuse is an error.
func (s *Server) reviewFor(ctx context.Context, kind, targetKey string, before, after json.RawMessage, caller reviewCaller) (*changeReview, error) {
	switch kind {
	case govKindUnit:
		return s.unitReviewFor(ctx, targetKey, before, after, caller)
	case govKindRestriction:
		return s.restrictionReviewFor(ctx, targetKey, before, after)
	case govKindWorkspaceOwningUnit, govKindComponentOwningUnit:
		return pinReview(), nil
	case types.GovernanceTargetElevation, types.GovernanceTargetImageRequest:
		return nil, nil
	}
	return notCheckedReview(), nil
}

// reviewCaller is who writes, for reviewFor: a unit admin, and a super admin (isOperator).
type reviewCaller struct {
	unitAdmin, super bool
}

// unitReviewFor is reviewFor for a profile: the row built as writeGovernanceUnit builds it, judged by
// unitWriteVerdict and graded by unitReviewRisk, as holdUnitWrite stores it. A unit admin's new unit
// binds nobody and applies directly.
func (s *Server) unitReviewFor(ctx context.Context, targetKey string, before, after json.RawMessage, caller reviewCaller) (*changeReview, error) {
	id, err := uuid.Parse(targetKey)
	if err != nil {
		return nil, fmt.Errorf("target key %q is not a profile id", targetKey)
	}
	all, err := s.cfg.Store.ListGovernanceUnits(ctx)
	if err != nil {
		return nil, err
	}
	if before != nil {
		var row types.GovernanceUnit
		if err := json.Unmarshal(before, &row); err != nil {
			return nil, fmt.Errorf("before: %w", err)
		}
		row.ID = id
		all = append(slices.DeleteFunc(slices.Clone(all), func(u types.GovernanceUnit) bool { return u.ID == id }), row)
	}
	cur, exists := unitsByID(all)[id]
	if after == nil {
		return notCheckedReview(), nil
	}
	if caller.unitAdmin && !exists {
		return nil, nil
	}
	req, msg := unitRequestFromPayload(after)
	if msg != "" {
		return nil, errors.New(msg)
	}
	b, err := s.governanceBoundary(ctx)
	if err != nil {
		return nil, err
	}
	limit, err := s.unitDepthLimit(ctx)
	if err != nil {
		return nil, err
	}
	p, res, _, err := s.buildGovernanceUnit("", caller.super, id, req, all, b, limit)
	if err != nil {
		return nil, err
	}
	exempt, review := s.unitWriteVerdict(ctx, all, p, res)
	if exempt && caller.unitAdmin && cur.Name != p.Name {
		shift, err := s.renameShift(ctx, id, p.Name)
		if err != nil {
			return nil, err
		}
		if len(shift.activates)+len(shift.vacates) > 0 {
			exempt, review = false, s.unitWriteReview(ctx, all, p, res, heldRename)
		}
	}
	if exempt {
		return nil, nil
	}
	return s.unitReviewRisk(ctx, all, id, review, res), nil
}

// restrictionReviewFor is reviewFor for a restriction: after is a types.RestrictionWrite, validated as
// the route validates it, and a target with no stored restriction is a create.
func (s *Server) restrictionReviewFor(ctx context.Context, targetKey string, before, after json.RawMessage) (*changeReview, error) {
	id, err := uuid.Parse(targetKey)
	if err != nil {
		return nil, fmt.Errorf("target key %q is not a restriction id", targetKey)
	}
	if after == nil {
		return notCheckedReview(), nil
	}
	var body types.RestrictionWrite
	if err := decodeHeldPayload(after, &body); err != nil {
		return nil, fmt.Errorf("after: %w", err)
	}
	if err := validateRestriction(&body); err != nil {
		return nil, err
	}
	var cur types.Restriction
	create := false
	switch rs, ok := s.restrictionStore(); {
	case before != nil:
		if err := json.Unmarshal(before, &cur); err != nil {
			return nil, fmt.Errorf("before: %w", err)
		}
	case !ok:
		return notCheckedReview(), nil
	default:
		cur, err = rs.GetRestriction(ctx, id)
		if create = errors.Is(err, store.ErrNotFound); !create && err != nil {
			return nil, err
		}
	}
	return restrictionHeldReview(create, cur, body), nil
}
