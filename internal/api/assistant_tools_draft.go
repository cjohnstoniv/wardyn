// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strings"

	"github.com/cjohnstoniv/wardyn/internal/assistant"
	"github.com/cjohnstoniv/wardyn/internal/authz"
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
	// echo is draft as the person may read it (assistantRecordSpec): the parts the answer and the proposal
	// event carry are cut from it, never from draft, whose ceiling copies can name what the person's read
	// withholds. withheld is every such value, for the validator's messages.
	echo     json.RawMessage
	withheld []string
}

// assistantClampDraft bounds a run or template draft's policy as the save route does: first the person's
// capability grants (narrowUserInlinePolicy, the run route's narrowing), then their ceiling (assistantClampSpec).
// A unit draft, or a template with no policy, comes back as it is.
func (s *Server) assistantClampDraft(r *http.Request, sub assistantSubject) (assistantClamped, error) {
	ctx := r.Context()
	out := assistantClamped{draft: sub.draft, echo: sub.draft}
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
	narrowed := spec
	_, drops, err := s.narrowUserInlinePolicy(ctx, s.secretOwnerFromRequest(r), &narrowed)
	if err != nil {
		return out, err
	}
	for _, d := range drops {
		if d.reason == authz.ReasonCapabilityEgressHost {
			out.refusals = append(out.refusals, assistant.Refusal{Host: d.detail, Code: assistant.DropNotGranted})
		}
	}
	for _, f := range assistantSpecFieldsChanged(&spec, narrowed) {
		if f != "allowed_domains" {
			out.refusals = append(out.refusals, assistant.Refusal{Field: f, Code: assistant.DropNotGranted})
		}
	}
	clamped, refusals := assistantClampSpec(narrowed, ceiling)
	out.refusals = append(out.refusals, refusals...)
	if out.draft, err = assistantWithSpec(sub, clamped); err != nil {
		return out, err
	}
	read := s.assistantRecordSpec(ctx, clamped)
	out.withheld = assistantWithheld(clamped, read)
	out.echo, err = assistantWithSpec(sub, read)
	return out, err
}

// assistantWithheld is every secret name full carries that read, its cut for the person, does not show: the
// inspection's workspace_secret_names and the grants' secret-naming scope fields (memberSecretScopeKeys).
func assistantWithheld(full, read types.RunPolicySpec) []string {
	var out []string
	if li := full.LLMInspection; li != nil && (read.LLMInspection == nil || !slices.Equal(li.WorkspaceSecretNames, read.LLMInspection.WorkspaceSecretNames)) {
		out = append(out, li.WorkspaceSecretNames...)
	}
	for i, g := range full.EligibleGrants {
		if i < len(read.EligibleGrants) && string(g.Scope) == string(read.EligibleGrants[i].Scope) {
			continue
		}
		var scope map[string]any
		_ = json.Unmarshal(g.Scope, &scope)
		for _, k := range memberSecretScopeKeys {
			if v, ok := scope[k].(string); ok && v != "" {
				out = append(out, v)
			}
		}
	}
	return slices.DeleteFunc(out, func(v string) bool { return v == "" })
}

// assistantScrubWithheld replaces a validator message that names a withheld value with one that does not.
func assistantScrubWithheld(errs []assistantFieldError, withheld []string) []assistantFieldError {
	for i, fe := range errs {
		if slices.ContainsFunc(withheld, func(v string) bool { return strings.Contains(fe.Message, v) }) {
			errs[i].Message = "A value your limits set is refused, and your reads do not show it. An admin of your unit can fix it." // PROPOSED
		}
	}
	return errs
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
	refused := map[string]bool{}
	for _, h := range spec.AllowedDomains {
		within := ceiling.Spec.AllowAllEgress || composer.AllowlistWithin([]string{h}, ceiling.Spec.AllowedDomains)
		if !within || !slices.Contains(clamped.AllowedDomains, h) {
			refusals = append(refusals, assistant.Refusal{Host: h, Code: assistant.DropOutsideCeiling, Unit: unitName})
			refused[h] = true
		}
	}
	// A narrowing: the draft's own hosts less the refused ones, never a host the draft did not name.
	if spec.AllowedDomains != nil {
		clamped.AllowedDomains = slices.DeleteFunc(slices.Clone(spec.AllowedDomains), func(h string) bool { return refused[h] })
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

// assistantPartFits decodes one part as its kind's save body, refusing a field the body does not name. A unit body
// needs a name (finishGovernanceUnitRequest), so a unit part that is not the name is judged with a stand-in one: the
// assembled draft's own name is the validator's to judge.
func assistantPartFits(sub assistantSubject, one json.RawMessage) error {
	switch {
	case sub.govKind == govKindUnit:
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(one, &fields); err != nil {
			return err
		}
		if _, named := fields["name"]; !named && fields != nil {
			fields["name"] = json.RawMessage(`"part"`)
			one, _ = json.Marshal(fields)
		}
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

// assistantRefusalSection is the draft section a clamp refusal is about: a run policy's field (a host is in
// allowed_domains), or a template's document, which holds its inline policy.
func assistantRefusalSection(sub assistantSubject, f assistant.Refusal) string {
	switch {
	case sub.targetKind == types.AssistantTargetTemplate:
		return "document"
	case f.Host != "":
		return "allowed_domains"
	}
	return f.Field
}

// assistantPartsOf is draft as parts, one per top-level field that proposed names (every field when proposed is
// nil), each marked as not from a build file.
func assistantPartsOf(draft json.RawMessage, proposed map[string]bool) []types.AssistantProposalPart {
	var body map[string]json.RawMessage
	_ = json.Unmarshal(draft, &body)
	keys := make([]string, 0, len(body))
	for k := range body {
		if proposed == nil || proposed[k] {
			keys = append(keys, k)
		}
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
	proposed := map[string]bool{}
	for _, part := range a.Parts {
		proposed[part.Section] = true
	}
	for _, f := range refusals {
		delete(proposed, f.Field)
	}
	p, ref := s.assistantPropose(e.r, sub, refusals, proposed)
	if ref == nil {
		ref = s.assistantMarkBuildFiles(e, &p)
	}
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
// routes' checks build the same answer. proposed names the sections the model proposed: the parts and the
// clamp's refusals carry those sections only, so a stored field the model did not propose is never echoed. nil
// means every section is the caller's own (the checks strip sends its whole draft).
func (s *Server) assistantPropose(r *http.Request, sub assistantSubject, refusals []assistant.Refusal, proposed map[string]bool) (assistantProposal, *assistantRefusal) {
	ctx := r.Context()
	caller := s.assistantReviewCaller(ctx)
	clamped, err := s.assistantClampDraft(r, sub)
	if err != nil {
		return assistantProposal{}, assistantCheckFailed(err)
	}
	sub.draft = clamped.draft
	for _, f := range clamped.refusals {
		if proposed == nil || proposed[assistantRefusalSection(sub, f)] {
			refusals = append(refusals, f)
		}
	}
	errs, err := s.assistantValidate(r, sub, caller)
	if err != nil {
		return assistantProposal{}, assistantCheckFailed(err)
	}
	errs = assistantScrubWithheld(errs, clamped.withheld)
	// The clamped draft is validated and reviewed; what is echoed is its read cut.
	p := assistantProposal{Parts: assistantPartsOf(clamped.echo, proposed), Valid: len(errs) == 0, Errors: errs}
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
