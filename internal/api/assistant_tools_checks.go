// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"reflect"
	"slices"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/pkg/client"
)

// assistantSubject is a draft of one target, as every check judges it.
type assistantSubject struct {
	targetKind string // run, unit, template or change
	// govKind is the governance kind reviewFor, impactFor and requiredApprovals judge: empty for a run or a
	// template, whose saves are not governance writes.
	govKind string
	// key is the target's stored key. A new unit gets a fresh id, so the unit checks judge it as a create.
	key   string
	draft json.RawMessage
	// before is the stored policy as the person may read it, nil for a new draft or a kind with none.
	before *types.RunPolicySpec
	// template is the stored template a template draft replaces, nil for a new one.
	template *types.RunTemplate
}

// assistantFieldError is one validator error.
type assistantFieldError struct {
	Field   string `json:"field"`
	Message string `json:"message"`
}

// assistantReviewCaller is reviewFor's caller for the person on ctx, the capped tool or person context: a unit
// admin unless they are at the security tier, and a super admin only by isOperator, which the security-tier cap
// leaves false. The zero value is every security-tier caller's.
func (s *Server) assistantReviewCaller(ctx context.Context) reviewCaller {
	return reviewCaller{unitAdmin: s.isUnitAdminCaller(ctx), super: s.isOperator(ctx)}
}

// assistantSaveJudged reports whether a save of govKind by caller would be judged for a hold, mirroring
// governanceSaveHeld and unitWriteMode for a person: the second-human switch is on, or a unit admin writes one of
// the kinds whose routes use unitWriteMode, which queues a unit admin's write whatever the switch says. The admin
// token and local mode never reach here.
func assistantSaveJudged(govKind string, caller reviewCaller) bool {
	return envEnabled(envGovernanceSecondHuman) || (caller.unitAdmin && assistantUnitWriteKind(govKind))
}

// assistantUnitWriteKind reports a kind whose save routes decide through unitWriteMode: a unit (save and
// delete), a membership, a unit admin grant and a unit's deciders.
func assistantUnitWriteKind(govKind string) bool {
	switch govKind {
	case govKindUnit, types.GovernanceTargetMembership, types.GovernanceTargetUnitAdmin, types.GovernanceTargetUnitDeciders:
		return true
	}
	return false
}

// assistantReview is the review a save of sub would carry and how many approvals it would wait for. A run or
// template save applies as saved, so its review is reviewFor's own answer and it needs none. A governance
// save the switch lets through applies directly: no review, no approvals. Otherwise the review is reviewFor's,
// a nil one applies directly, and a held one waits for requiredApprovals' count.
func (s *Server) assistantReview(ctx context.Context, sub assistantSubject, caller reviewCaller) (*changeReview, int, error) {
	if sub.govKind == "" {
		rv, err := s.reviewFor(ctx, "", sub.key, nil, sub.draft, caller)
		return rv, 0, err
	}
	if !assistantSaveJudged(sub.govKind, caller) {
		return nil, 0, nil
	}
	rv, err := s.reviewFor(ctx, sub.govKind, sub.key, nil, sub.draft, caller)
	if err != nil || rv == nil {
		return rv, 0, err
	}
	required, _, err := s.requiredApprovals(ctx, sub.govKind, sub.key)
	if err != nil {
		return nil, 0, err
	}
	return rv, required, nil
}

// assistantSubjectOf is the draft of the session's target the arguments name. An empty draft stands for the
// stored target: for a change, the change's own body.
func (s *Server) assistantSubjectOf(e *assistantToolEnv, a types.AssistantDraftArgs) (assistantSubject, *assistantRefusal) {
	return s.assistantSubjectFor(e.ctx(), e.rt, a.Draft)
}

// assistantSubjectFor builds the subject of a target already read and checked for the person on ctx.
func (s *Server) assistantSubjectFor(ctx context.Context, rt assistantTarget, draft json.RawMessage) (assistantSubject, *assistantRefusal) {
	sub := assistantSubject{targetKind: rt.Kind, key: rt.key(), draft: draft}
	switch {
	case rt.change != nil:
		sub.govKind, sub.key = rt.change.TargetKind, rt.change.TargetKey
		if len(sub.draft) == 0 {
			sub.draft = rt.change.Payload
		}
	case rt.Kind == types.AssistantTargetUnit:
		sub.govKind = govKindUnit
		if rt.unit != nil {
			spec := s.assistantRecordSpec(ctx, rt.unit.Ceiling)
			sub.before = &spec
		} else {
			sub.key = uuid.NewString()
		}
	case rt.run != nil && rt.run.PolicyID != nil:
		p, err := s.cfg.Store.GetPolicy(ctx, *rt.run.PolicyID)
		if err != nil {
			return sub, s.assistantReadFailed(err)
		}
		spec := s.assistantRecordSpec(ctx, p.Spec)
		sub.before = &spec
	case rt.template != nil:
		view := s.assistantTemplateView(ctx, *rt.template)
		sub.template = &view
		if spec, ok := assistantTemplateSpec(view.Document); ok {
			sub.before = &spec
		}
	}
	if len(sub.draft) == 0 {
		return sub, assistantRefusalFor(http.StatusBadRequest, reasonInvalidRequestBody, "Name a draft to check.") // PROPOSED
	}
	return sub, nil
}

// assistantTemplateView is a stored template as the template read shows it: every repository URL that carries a
// credential redacted (redactRepoURLsInJSON), and its inline policy cut as the person may read it
// (assistantRecordSpec). Nothing of the stored row reaches a check, a result or the model in any other form.
func (s *Server) assistantTemplateView(ctx context.Context, t types.RunTemplate) types.RunTemplate {
	t.Document = redactRepoURLsInJSON(t.Document)
	spec, ok := assistantTemplateSpec(t.Document)
	if !ok {
		return t
	}
	var doc client.TemplateDocument
	if json.Unmarshal(t.Document, &doc) != nil || doc.Intent.Set("inline_policy", s.assistantRecordSpec(ctx, spec)) != nil {
		t.Document = nil
		return t
	}
	if raw, err := json.Marshal(doc); err == nil {
		t.Document = raw
	} else {
		t.Document = nil
	}
	return t
}

// assistantTemplateSpec is the inline policy a template document's intent names, if any.
func assistantTemplateSpec(document json.RawMessage) (types.RunPolicySpec, bool) {
	var doc client.TemplateDocument
	var spec types.RunPolicySpec
	if json.Unmarshal(document, &doc) != nil {
		return spec, false
	}
	raw := doc.Intent.Raw("inline_policy")
	if len(raw) == 0 || json.Unmarshal(raw, &spec) != nil {
		return spec, false
	}
	return spec, true
}

// assistantUnitBuild is a unit draft built as the unit save builds it (unitReviewFor's steps): the unit rows
// with the draft in place of the unit, composed with its parents. super is the caller's isOperator.
func (s *Server) assistantUnitBuild(ctx context.Context, key string, draft json.RawMessage, super bool) (*ResolvedUnit, error) {
	id, err := uuid.Parse(key)
	if err != nil {
		return nil, errors.New("the unit id is not valid")
	}
	all, err := s.unitRowsForWrite(ctx)
	if err != nil {
		return nil, err
	}
	req, msg := unitRequestFromPayload(draft)
	if msg != "" {
		return nil, errAssistantDraftInvalid{msg}
	}
	b, err := s.governanceBoundary(ctx)
	if err != nil {
		return nil, err
	}
	limit, err := s.unitDepthLimit(ctx)
	if err != nil {
		return nil, err
	}
	p, res, _, err := s.buildGovernanceUnit("", super, id, req, all, b, limit)
	if err != nil {
		return nil, errAssistantDraftInvalid{err.Error()}
	}
	if err := s.assistantUnitParentInScope(ctx, all, id, p.ParentID); err != nil {
		return nil, err
	}
	return res, nil
}

// assistantUnitParentInScope is the parent check a unit admin's save makes (unitWritePrecheck, scopedUnitBuild):
// a new unit, or one the draft moves, must sit under a unit they govern (unitScopeEvery); a unit with no parent
// is a security admin's. A security caller is not checked. The answer is a validator error, as the save's 403 is
// a refusal of this draft, not of the session.
func (s *Server) assistantUnitParentInScope(ctx context.Context, all []types.GovernanceUnit, id uuid.UUID, parent *uuid.UUID) error {
	if !s.isUnitAdminCaller(ctx) {
		return nil
	}
	if cur, exists := unitsByID(all)[id]; exists && sameJSON(cur.ParentID, parent) {
		return nil
	}
	if parent == nil {
		return errAssistantDraftInvalid{"A unit with no parent is a security admin's to create."}
	}
	in, err := s.unitScopeEvery(ctx, false, *parent)
	if err != nil {
		return err
	}
	if !in {
		return errAssistantDraftInvalid{"The parent unit is outside the units you govern."}
	}
	return nil
}

// errAssistantDraftInvalid is a draft its save route refuses, told apart from a failed read.
type errAssistantDraftInvalid struct{ msg string }

func (e errAssistantDraftInvalid) Error() string { return e.msg }

// assistantDraftSpec is the policy a draft sets: a run's spec, a template intent's inline policy, or a unit's
// ceiling composed with its parents. ok is false for a kind whose draft sets no policy.
func (s *Server) assistantDraftSpec(ctx context.Context, sub assistantSubject, caller reviewCaller) (spec types.RunPolicySpec, ok bool, err error) {
	switch {
	case sub.govKind == govKindUnit:
		res, err := s.assistantUnitBuild(ctx, sub.key, sub.draft, caller.super)
		if err != nil {
			return spec, false, err
		}
		return res.Ceiling, true, nil
	case sub.targetKind == types.AssistantTargetRun:
		if err := decodeStrictJSON(sub.draft, &spec); err != nil {
			return spec, false, errAssistantDraftInvalid{"the draft is not a run policy: " + err.Error()}
		}
		return spec, true, nil
	case sub.targetKind == types.AssistantTargetTemplate:
		var req client.TemplateSaveRequest
		if err := decodeStrictJSON(sub.draft, &req); err != nil {
			return spec, false, errAssistantDraftInvalid{"the draft is not a template: " + err.Error()}
		}
		doc, _ := json.Marshal(req.Document)
		spec, ok = assistantTemplateSpec(doc)
		return spec, ok, nil
	}
	return spec, false, nil
}

// assistantValidate runs the save route's own validator for the draft's kind, without its write.
func (s *Server) assistantValidate(r *http.Request, sub assistantSubject, caller reviewCaller) ([]assistantFieldError, error) {
	ctx := r.Context()
	fail := func(err error) ([]assistantFieldError, error) {
		var inv errAssistantDraftInvalid
		if errors.As(err, &inv) {
			return []assistantFieldError{{Message: inv.msg}}, nil
		}
		return nil, err
	}
	switch {
	case sub.govKind == govKindUnit:
		if _, err := s.assistantUnitBuild(ctx, sub.key, sub.draft, caller.super); err != nil {
			return fail(err)
		}
		return []assistantFieldError{}, nil
	case sub.targetKind == types.AssistantTargetRun:
		spec, _, err := s.assistantDraftSpec(ctx, sub, caller)
		if err != nil {
			return fail(err)
		}
		// The policy save's checks (decodePolicyRequest, then ruleUIAppCallers), so a draft called valid saves.
		if err := validateAuthoredPolicySpec(spec); err != nil {
			return []assistantFieldError{{Message: err.Error()}}, nil
		}
		if err := s.ruleUIAppCallers(ctx, spec); err != nil {
			if errors.Is(err, store.ErrDelegatePersonUnknownPortal) {
				return []assistantFieldError{{Message: delegatePersonUnknownPortalMsg}}, nil
			}
			return nil, err
		}
		return []assistantFieldError{}, nil
	case sub.targetKind == types.AssistantTargetTemplate:
		return s.assistantValidateTemplate(r, sub)
	}
	// Any other governance kind: its route's own validator refuses the body, as restrictionReviewFor runs it.
	// reviewFor then only reads, so its error is a failed read and a 500, never a message to the model.
	if sub.govKind == govKindRestriction && sub.draft != nil {
		var body types.RestrictionWrite
		if err := decodeHeldPayload(sub.draft, &body); err != nil {
			return []assistantFieldError{{Message: "the draft is not a restriction: " + err.Error()}}, nil
		}
		if err := validateRestriction(&body); err != nil {
			return []assistantFieldError{{Message: err.Error()}}, nil
		}
	}
	if _, err := s.reviewFor(ctx, sub.govKind, sub.key, nil, sub.draft, caller); err != nil {
		return nil, err
	}
	return []assistantFieldError{}, nil
}

// assistantValidateTemplate is the template save's validator (templateForSave), for the stored template's owner
// or, for a new one, the owner the draft's scope names for this caller.
func (s *Server) assistantValidateTemplate(r *http.Request, sub assistantSubject) ([]assistantFieldError, error) {
	var req client.TemplateSaveRequest
	if err := decodeStrictJSON(sub.draft, &req); err != nil {
		return []assistantFieldError{{Message: "the draft is not a template: " + err.Error()}}, nil
	}
	kind, owner, held := req.Kind, types.TemplateOwner{}, []types.GrantSpec(nil)
	if sub.template != nil {
		kind, owner, held = sub.template.Kind, sub.template.Owner, storedTemplateGrants(sub.template.Document)
	} else {
		ts, ok := s.cfg.Store.(store.TemplateStore)
		if !ok {
			return nil, errors.New("templates are not available")
		}
		c, err := s.templateCallerOf(r.Context(), r, ts)
		if err != nil {
			return nil, err
		}
		o, diag := c.targetOwner(req.Scope, req.GroupID)
		if diag != nil {
			return []assistantFieldError{{Field: diag.Path, Message: diag.Message}}, nil
		}
		owner = o
	}
	if kind == "" {
		kind = types.TemplateKindTemplate
	}
	_, diags := templateForSave(req, owner, kind, held)
	out := []assistantFieldError{}
	for _, d := range diags {
		out = append(out, assistantFieldError{Field: d.Path, Message: d.Message})
	}
	return out, nil
}

// assistantRisk is composer.Grade before and after, as the change review grades a policy-shaped change.
func (s *Server) assistantRisk(ctx context.Context, sub assistantSubject, caller reviewCaller) (*changeRisk, error) {
	after, ok, err := s.assistantDraftSpec(ctx, sub, caller)
	var inv errAssistantDraftInvalid
	if errors.As(err, &inv) || (err == nil && !ok) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return s.withRisk(ctx, notCheckedReview(), sub.before, after).Risk, nil
}

// assistantSpecFieldsChanged is the top-level fields of a policy that differ between before and after, by the
// names the policy's JSON gives them.
func assistantSpecFieldsChanged(before *types.RunPolicySpec, after types.RunPolicySpec) []string {
	var b types.RunPolicySpec
	if before != nil {
		b = *before
	}
	bm, am := assistantJSONFields(b), assistantJSONFields(after)
	out := []string{}
	for k, v := range am {
		if !reflect.DeepEqual(bm[k], v) {
			out = append(out, k)
		}
	}
	for k := range bm {
		if _, ok := am[k]; !ok {
			out = append(out, k)
		}
	}
	slices.Sort(out)
	return out
}

func assistantJSONFields(v any) map[string]any {
	raw, _ := json.Marshal(v)
	var m map[string]any
	_ = json.Unmarshal(raw, &m)
	return m
}

// assistantCheckFailed answers a check that could not run: a refused draft is 400 with the validator's words,
// anything else 500.
func assistantCheckFailed(err error) *assistantRefusal {
	var inv errAssistantDraftInvalid
	if errors.As(err, &inv) {
		return assistantRefusalFor(http.StatusBadRequest, reasonInvalidRequestBody, inv.msg)
	}
	return assistantRefusalFor(http.StatusInternalServerError, "", "Could not check the draft.") // PROPOSED
}

// assistantToolValidate is validate: {valid, errors[{field, message}]}.
func (s *Server) assistantToolValidate(e *assistantToolEnv) (any, *assistantRefusal) {
	a, ref := assistantDraftArgsOf(e)
	if ref != nil {
		return nil, ref
	}
	sub, ref := s.assistantSubjectOf(e, a)
	if ref != nil {
		return nil, ref
	}
	errs, err := s.assistantValidate(e.r, sub, s.assistantReviewCaller(e.ctx()))
	if err != nil {
		return nil, assistantCheckFailed(err)
	}
	return map[string]any{"valid": len(errs) == 0, "errors": errs}, nil
}

// assistantToolDiff is diff: the policy fields the draft changes against the stored target, by name only.
func (s *Server) assistantToolDiff(e *assistantToolEnv) (any, *assistantRefusal) {
	a, ref := assistantDraftArgsOf(e)
	if ref != nil {
		return nil, ref
	}
	sub, ref := s.assistantSubjectOf(e, a)
	if ref != nil {
		return nil, ref
	}
	after, ok, err := s.assistantDraftSpec(e.ctx(), sub, s.assistantReviewCaller(e.ctx()))
	if err != nil {
		return nil, assistantCheckFailed(err)
	}
	if !ok {
		return map[string]any{"changed": []string{}, "computed": false}, nil
	}
	return map[string]any{"changed": assistantSpecFieldsChanged(sub.before, s.assistantRecordSpec(e.ctx(), after)), "computed": true}, nil
}

// assistantToolExplain is narrowing_check and explain: the structured review a save of the draft would carry,
// built by reviewFor for the person's own caller, with the approvals it would wait for. It adds no words.
func (s *Server) assistantToolExplain(e *assistantToolEnv) (any, *assistantRefusal) {
	a, ref := assistantDraftArgsOf(e)
	if ref != nil {
		return nil, ref
	}
	sub, ref := s.assistantSubjectOf(e, a)
	if ref != nil {
		return nil, ref
	}
	caller := s.assistantReviewCaller(e.ctx())
	// A draft the save route's validator refuses has no verdict: it says so, with the validator's errors, as the
	// checks strip does, rather than failing on reviewFor's build error.
	errs, err := s.assistantValidate(e.r, sub, caller)
	if err != nil {
		return nil, assistantCheckFailed(err)
	}
	if len(errs) > 0 {
		return map[string]any{"valid": false, "errors": errs, "review": nil, "held": false, "approvals_needed": 0}, nil
	}
	review, approvals, err := s.assistantReview(e.ctx(), sub, caller)
	if err != nil {
		return nil, assistantCheckFailed(err)
	}
	return map[string]any{"valid": true, "errors": errs, "review": review, "held": review != nil && approvals > 0, "approvals_needed": approvals}, nil
}

// assistantToolGrade is grade: composer.Grade's risk before and after.
func (s *Server) assistantToolGrade(e *assistantToolEnv) (any, *assistantRefusal) {
	a, ref := assistantDraftArgsOf(e)
	if ref != nil {
		return nil, ref
	}
	sub, ref := s.assistantSubjectOf(e, a)
	if ref != nil {
		return nil, ref
	}
	risk, err := s.assistantRisk(e.ctx(), sub, s.assistantReviewCaller(e.ctx()))
	if err != nil {
		return nil, assistantCheckFailed(err)
	}
	return map[string]any{"risk": risk}, nil
}

// assistantToolImpact is impact: impactFor's counts and nothing else, no run, owner or name.
func (s *Server) assistantToolImpact(e *assistantToolEnv) (any, *assistantRefusal) {
	a, ref := assistantDraftArgsOf(e)
	if ref != nil {
		return nil, ref
	}
	sub, ref := s.assistantSubjectOf(e, a)
	if ref != nil {
		return nil, ref
	}
	c, err := s.impactFor(e.ctx(), sub.govKind, sub.key, sub.draft)
	if err != nil {
		return nil, assistantCheckFailed(err)
	}
	return c, nil
}
