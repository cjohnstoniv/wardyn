// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strings"

	"github.com/cjohnstoniv/wardyn/internal/assistant"
	"github.com/cjohnstoniv/wardyn/internal/composer"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/pkg/client"
)

// assistantDraftPartsMax is the most parts one proposal takes, as the schema says.
const assistantDraftPartsMax = 50

// assistantProposal is the server-written proposal event, and draft's answer. It writes nothing else: no
// policy, unit, template or change, and not the session's working draft.
type assistantProposal struct {
	TargetKind      string                        `json:"target_kind"`
	Parts           []types.AssistantProposalPart `json:"parts"`
	Note            string                        `json:"note,omitempty"`
	Dropped         []assistant.Dropped           `json:"dropped"`
	Valid           bool                          `json:"valid"`
	Errors          []assistantFieldError         `json:"errors"`
	Review          *changeReview                 `json:"review"`
	Held            bool                          `json:"held"`
	ApprovalsNeeded int                           `json:"approvals_needed"`
}

// assistantClamped is a run or template draft bounded to the person's ceiling, with what the bound refused.
type assistantClamped struct {
	draft    json.RawMessage
	refusals []assistant.Refusal
	unit     *ResolvedUnit
}

// assistantClampDraft bounds a run or template draft's policy to the person's ceiling (assistantClampSpec). A
// unit draft, or a template with no policy, comes back as it is.
func (s *Server) assistantClampDraft(ctx context.Context, sub assistantSubject) (assistantClamped, error) {
	out := assistantClamped{draft: sub.draft}
	if sub.govKind != "" {
		return out, nil
	}
	spec, ok, err := s.assistantDraftSpec(ctx, sub, reviewCaller{})
	if err != nil || !ok {
		return out, err
	}
	ceiling, err := s.effectiveCeiling(ctx)
	if err != nil {
		return out, err
	}
	out.unit = ceiling.Unit
	clamped, refusals := assistantClampSpec(spec, ceiling)
	out.refusals = refusals
	out.draft, err = assistantWithSpec(sub, clamped)
	return out, err
}

// assistantClampSpec bounds spec to ceiling: a host the ceiling's allowlist does not cover
// (composer.AllowlistWithin) or that composer.ClampMember drops is refused, and every other field the clamp
// changes is refused as clamped. An operator's ceiling bounds nothing.
func assistantClampSpec(spec types.RunPolicySpec, ceiling governanceCeiling) (types.RunPolicySpec, []assistant.Refusal) {
	if ceiling.Operator {
		return spec, nil
	}
	unitName := ""
	if ceiling.Unit != nil {
		unitName = ceiling.Unit.Name
	}
	var refusals []assistant.Refusal
	clamped, _ := composer.ClampMember(spec, ceiling.Spec, ceiling.Limits, ceiling.authoredSize(), ceiling.memberSize())
	kept := []string{}
	for _, h := range spec.AllowedDomains {
		within := ceiling.Spec.AllowAllEgress || composer.AllowlistWithin([]string{h}, ceiling.Spec.AllowedDomains)
		if !within || !slices.Contains(clamped.AllowedDomains, h) {
			refusals = append(refusals, assistant.Refusal{Host: h, Code: assistant.DropOutsideCeiling, Unit: unitName})
			continue
		}
		kept = append(kept, h)
	}
	if spec.AllowedDomains != nil {
		clamped.AllowedDomains = kept
	}
	for _, f := range assistantSpecFieldsChanged(&spec, clamped) {
		if f != "allowed_domains" {
			refusals = append(refusals, assistant.Refusal{Field: f, Code: assistant.DropClamped, Unit: unitName})
		}
	}
	return clamped, refusals
}

// assistantWithSpec is the draft with its policy replaced by spec: a run's whole body, or a template intent's
// inline policy.
func assistantWithSpec(sub assistantSubject, spec types.RunPolicySpec) (json.RawMessage, error) {
	if sub.targetKind == types.AssistantTargetRun {
		return json.Marshal(spec)
	}
	var req client.TemplateSaveRequest
	if err := decodeStrictJSON(sub.draft, &req); err != nil {
		return nil, err
	}
	if err := req.Document.Intent.Set("inline_policy", spec); err != nil {
		return nil, err
	}
	return json.Marshal(req)
}

// assistantAssembleDraft lays each part on the stored target's readable body (a run's policy, a template's
// saved fields; empty for a unit or a new draft) as a top-level field. A part whose section the kind's body
// does not name, or whose value its body refuses, is refused as a whole.
func (s *Server) assistantAssembleDraft(sub assistantSubject, parts []types.AssistantProposalPart) (json.RawMessage, []assistant.Refusal) {
	body := map[string]json.RawMessage{}
	switch {
	case sub.targetKind == types.AssistantTargetRun && sub.before != nil:
		raw, _ := json.Marshal(sub.before)
		_ = json.Unmarshal(raw, &body)
	case sub.targetKind == types.AssistantTargetTemplate && sub.template != nil:
		raw, _ := json.Marshal(map[string]any{"name": sub.template.Name, "description": sub.template.Description,
			"kind": sub.template.Kind, "document": json.RawMessage(sub.template.Document)})
		_ = json.Unmarshal(raw, &body)
	}
	var refused []assistant.Refusal
	for _, p := range parts {
		one, _ := json.Marshal(map[string]json.RawMessage{p.Section: p.Value})
		if err := assistantPartFits(sub, one); err != nil {
			code := assistant.DropInvalid
			if strings.Contains(err.Error(), "unknown field") {
				code = assistant.DropUnknownSection
			}
			refused = append(refused, assistant.Refusal{Field: p.Section, Code: code})
			continue
		}
		body[p.Section] = p.Value
	}
	raw, _ := json.Marshal(body)
	return raw, refused
}

// assistantPartFits decodes one part as its kind's save body, refusing a field the body does not name.
func assistantPartFits(sub assistantSubject, one json.RawMessage) error {
	switch {
	case sub.govKind == govKindUnit:
		if _, msg := unitRequestFromPayload(one); msg != "" {
			return errors.New(msg)
		}
		return nil
	case sub.targetKind == types.AssistantTargetRun:
		var spec types.RunPolicySpec
		return decodeStrictJSON(one, &spec)
	case sub.targetKind == types.AssistantTargetTemplate:
		var req client.TemplateSaveRequest
		return decodeStrictJSON(one, &req)
	}
	return errors.New("unknown field: this target takes no draft")
}

// assistantPartsOf is draft as parts, one per top-level field, each marked as not from a build file.
func assistantPartsOf(draft json.RawMessage) []types.AssistantProposalPart {
	var body map[string]json.RawMessage
	_ = json.Unmarshal(draft, &body)
	keys := make([]string, 0, len(body))
	for k := range body {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	parts := make([]types.AssistantProposalPart, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, types.AssistantProposalPart{Section: k, Value: body[k]})
	}
	return parts
}

// assistantToolDraft is draft. It only proposes: it bounds the draft to the person's ceiling, runs the save
// route's validator without its write, keeps a widening the save would hold with its chip, lists every outright
// refusal with the approvals a change to that unit needs, and appends one proposal event. It never calls the
// held-change writer.
func (s *Server) assistantToolDraft(e *assistantToolEnv) (any, *assistantRefusal) {
	if e.sess.Purpose == types.AssistantReview {
		return nil, assistantRefusalFor(http.StatusForbidden, reasonAssistantToolRefused, "A review session cannot propose a draft.") // PROPOSED
	}
	var a types.AssistantProposeArgs
	if err := decodeStrictJSON(e.args, &a); err != nil || a.TargetKind != e.sess.TargetKind || len(a.Parts) == 0 || len(a.Parts) > assistantDraftPartsMax {
		return nil, assistantRefusalFor(http.StatusBadRequest, reasonInvalidRequestBody, "Propose 1 to 50 parts for this session's target.") // PROPOSED
	}
	for _, p := range a.Parts {
		if p.Section == "" || !json.Valid(p.Value) {
			return nil, assistantRefusalFor(http.StatusBadRequest, reasonInvalidRequestBody, "Each part names a section and a JSON value.") // PROPOSED
		}
	}
	ctx := e.ctx()
	sub, ref := s.assistantSubjectFor(ctx, e.rt, json.RawMessage("{}"))
	if ref != nil {
		return nil, ref
	}
	draft, refusals := s.assistantAssembleDraft(sub, a.Parts)
	sub.draft = draft
	p, ref := s.assistantPropose(e.r, sub, refusals)
	if ref != nil {
		return nil, ref
	}
	p.TargetKind, p.Note = a.TargetKind, a.Note
	raw, ref := assistantToolResult(p, assistant.LettersFromMap(e.sess.PersonLetters))
	if ref != nil {
		return nil, ref
	}
	if _, err := e.ss.AppendAssistantEvent(ctx, e.sess.ID, types.AssistantEventProposal, raw); err != nil {
		if errors.Is(err, store.ErrAssistantSessionEnded) || errors.Is(err, store.ErrNotFound) {
			return nil, assistantRefusalFor(http.StatusConflict, reasonAssistantSessionNotFound, "That session is not the live one.") // PROPOSED
		}
		return nil, assistantRefusalFor(http.StatusInternalServerError, "", "Could not keep the proposal.") // PROPOSED
	}
	s.assistantChanged(e.sess.ID, e.sess.RunID)
	return raw, nil
}

// assistantPropose is the proposal of sub: clamped, validated, reviewed, with its dropped entries. The person
// routes' checks build the same answer.
func (s *Server) assistantPropose(r *http.Request, sub assistantSubject, refusals []assistant.Refusal) (assistantProposal, *assistantRefusal) {
	ctx := r.Context()
	caller := s.assistantReviewCaller(ctx)
	clamped, err := s.assistantClampDraft(ctx, sub)
	if err != nil {
		return assistantProposal{}, assistantCheckFailed(err)
	}
	sub.draft = clamped.draft
	refusals = append(refusals, clamped.refusals...)
	errs, err := s.assistantValidate(r, sub, caller)
	if err != nil {
		return assistantProposal{}, assistantCheckFailed(err)
	}
	p := assistantProposal{Parts: assistantPartsOf(sub.draft), Valid: len(errs) == 0, Errors: errs}
	if p.Valid {
		if p.Review, p.ApprovalsNeeded, err = s.assistantReview(ctx, sub, caller); err != nil {
			return assistantProposal{}, assistantCheckFailed(err)
		}
		p.Held = p.Review != nil && p.ApprovalsNeeded > 0
	}
	// The count a change to the refusing unit's limits needs: asked once, and an error is a 500, never a guess.
	unitApprovals := 0
	if clamped.unit != nil && len(refusals) > 0 {
		if unitApprovals, _, err = s.requiredApprovals(ctx, govKindUnit, clamped.unit.ID.String()); err != nil {
			return assistantProposal{}, assistantCheckFailed(err)
		}
	}
	approvals := func(unit string) int {
		if clamped.unit != nil && unit == clamped.unit.Name {
			return unitApprovals
		}
		return 0
	}
	p.Dropped = assistant.DroppedOf(refusals, approvals)
	return p, nil
}
