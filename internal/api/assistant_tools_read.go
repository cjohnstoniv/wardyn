// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"net/http"
	"slices"
	"strings"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/assistant"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// assistantToolRead is read. Everything is read on the person's capped context, so a policy is cut at the
// tier that context yields and mount sources never leave. No secret value, credential, audit row, per-run row
// or per-person row is read; people come back as the session's letters only.
func (s *Server) assistantToolRead(e *assistantToolEnv) (any, *assistantRefusal) {
	var a types.AssistantReadArgs
	if err := decodeStrictJSON(e.args, &a); err != nil {
		return nil, assistantRefusalFor(http.StatusBadRequest, reasonInvalidRequestBody, "The arguments do not fit this tool.") // PROPOSED
	}
	switch a.Kind {
	case types.AssistantReadUnit, types.AssistantReadUnitAdmins, types.AssistantReadGrants:
		// Each is read from a unit's limits: refused, by name only, when units are never sent.
		if ref := assistantNeverSent(e.sc.Assistant, govKindUnit); ref != nil {
			return nil, ref
		}
	}
	switch a.Kind {
	case types.AssistantReadTarget:
		return s.assistantReadTarget(e), nil
	case types.AssistantReadUnit:
		return s.assistantReadUnit(e)
	case types.AssistantReadTemplates:
		return s.assistantReadTemplates(e, "")
	case types.AssistantReadTemplate:
		if _, err := uuid.Parse(a.ID); err != nil {
			return nil, assistantRefusalFor(http.StatusBadRequest, reasonInvalidRequestBody, "Name the template's id.") // PROPOSED
		}
		return s.assistantReadTemplates(e, a.ID)
	case types.AssistantReadUnitAdmins:
		return s.assistantReadUnitAdmins(e)
	case types.AssistantReadGrants:
		spec, ok := s.assistantTargetSpec(e)
		if !ok {
			ceiling, err := s.effectiveCeiling(e.ctx())
			if err != nil {
				return nil, assistantRefusalFor(http.StatusInternalServerError, "", "Could not read your limits.") // PROPOSED
			}
			spec = ceiling.Spec
		}
		lines := assistantGrantLines(spec)
		if lines == nil {
			lines = []string{}
		}
		return map[string]any{"grants": lines}, nil
	case types.AssistantReadRunHistory:
		if e.sc.Assistant == nil || !e.sc.Assistant.RunHistoryCounts {
			return nil, assistantRunHistoryOff()
		}
		counts, err := s.assistantHostCounts(e.ctx(), e.rt, e.snap)
		if err != nil {
			return nil, assistantRefusalFor(http.StatusInternalServerError, "", "Could not read the run history.") // PROPOSED
		}
		if counts == nil {
			counts = []assistant.HostCount{}
		}
		return map[string]any{"days": 30, "hosts": counts}, nil
	}
	return nil, assistantRefusalFor(http.StatusBadRequest, reasonInvalidRequestBody, "That is not something read can read.") // PROPOSED
}

// assistantTargetSpec is the session target's policy as the person may read it, when the target has one.
func (s *Server) assistantTargetSpec(e *assistantToolEnv) (types.RunPolicySpec, bool) {
	ctx := e.ctx()
	switch {
	case e.rt.unit != nil:
		return s.assistantRecordSpec(ctx, e.rt.unit.Ceiling), true
	case e.rt.run != nil && e.rt.run.PolicyID != nil:
		p, err := s.cfg.Store.GetPolicy(ctx, *e.rt.run.PolicyID)
		if err != nil {
			return types.RunPolicySpec{}, false
		}
		return s.assistantRecordSpec(ctx, p.Spec), true
	case e.rt.template != nil:
		if spec, ok := assistantTemplateSpec(e.rt.template.Document); ok {
			return s.assistantRecordSpec(ctx, spec), true
		}
	}
	return types.RunPolicySpec{}, false
}

// assistantReadTarget is the session's target as the person may read it: a unit's name, limits and ceiling; a
// run's policy (never its task text); a template's name, summary and policy (never its document); a change as
// the change review shows it to them.
func (s *Server) assistantReadTarget(e *assistantToolEnv) any {
	out := map[string]any{"kind": e.rt.Kind, "new": e.rt.ID == ""}
	spec, hasSpec := s.assistantTargetSpec(e)
	switch {
	case e.rt.unit != nil:
		out["name"], out["limits"] = e.rt.unit.Name, e.rt.unit.Limits
	case e.rt.template != nil:
		out["name"], out["summary"], out["template_kind"] = e.rt.template.Name, firstLine(e.rt.template.Description), e.rt.template.Kind
	case e.rt.change != nil:
		ch := s.changeForRead(e.r, *e.rt.change)
		out["change"] = map[string]any{"target_kind": ch.TargetKind, "op": ch.Op, "target_key": ch.TargetKey, "diff": ch.Diff, "note": ch.Note}
	}
	if hasSpec {
		out["policy"] = spec
	}
	return out
}

// assistantReadUnit is the unit the person's limits come from: the target unit, else the person's own, with
// the ceiling its parents compose. The chain of bases is an admin surface and is not sent.
func (s *Server) assistantReadUnit(e *assistantToolEnv) (any, *assistantRefusal) {
	ctx := e.ctx()
	if u := e.rt.unit; u != nil {
		return map[string]any{"name": u.Name, "ceiling": s.assistantRecordSpec(ctx, u.Ceiling), "limits": u.Limits}, nil
	}
	ceiling, err := s.effectiveCeiling(ctx)
	if err != nil {
		return nil, assistantRefusalFor(http.StatusInternalServerError, "", "Could not read your limits.") // PROPOSED
	}
	name := ""
	if ceiling.Unit != nil {
		name = ceiling.Unit.Name
	}
	return map[string]any{"name": name, "ceiling": s.assistantRecordSpec(ctx, ceiling.Spec), "limits": ceiling.Limits}, nil
}

// assistantReadTemplates is the templates the person can see, as names and one-line summaries; with id, that
// one template and its policy. A template the person cannot see answers the one not-found refusal.
func (s *Server) assistantReadTemplates(e *assistantToolEnv, id string) (any, *assistantRefusal) {
	ctx := e.ctx()
	ts, ok := s.cfg.Store.(store.TemplateStore)
	if !ok {
		return map[string]any{"templates": []string{}}, nil
	}
	list, err := ts.ListTemplatesVisible(ctx, e.snap.Principal, oidcGroupsFromContext(ctx))
	if err != nil {
		return nil, s.assistantReadFailed(err)
	}
	if id == "" {
		lines := make([]string, 0, len(list))
		for _, t := range list {
			lines = append(lines, strings.TrimSpace(t.Name+": "+firstLine(t.Description)))
		}
		return map[string]any{"templates": lines}, nil
	}
	i := slices.IndexFunc(list, func(t types.RunTemplate) bool { return t.ID.String() == id })
	if i < 0 {
		return nil, assistantTargetGone()
	}
	t := list[i]
	out := map[string]any{"name": t.Name, "summary": firstLine(t.Description), "template_kind": t.Kind}
	if spec, ok := assistantTemplateSpec(t.Document); ok {
		out["policy"] = s.assistantRecordSpec(ctx, spec)
	}
	return out, nil
}

// assistantReadUnitAdmins is the admins of the target unit, or of the person's own, as the session's letters.
// An admin the session holds no letter for is only counted, so no name leaves unlettered.
func (s *Server) assistantReadUnitAdmins(e *assistantToolEnv) (any, *assistantRefusal) {
	ctx := e.ctx()
	var unit uuid.UUID
	switch {
	case e.rt.unit != nil:
		unit = e.rt.unit.ID
	default:
		ceiling, err := s.effectiveCeiling(ctx)
		if err != nil {
			return nil, assistantRefusalFor(http.StatusInternalServerError, "", "Could not read your limits.") // PROPOSED
		}
		if ceiling.Unit == nil {
			return map[string]any{"admins": []string{}, "others": 0}, nil
		}
		unit = ceiling.Unit.ID
	}
	people, err := s.assistantUnitAdmins(ctx, unit)
	if err != nil {
		return nil, assistantRefusalFor(http.StatusInternalServerError, "", "Could not read the unit's admins.") // PROPOSED
	}
	letters := assistant.LettersFromMap(e.sess.PersonLetters)
	admins, others := []string{}, 0
	for _, p := range people {
		who := p.Email
		if who == "" {
			who = p.Name
		}
		if label := letters.Apply(who); label != who {
			admins = append(admins, label)
		} else {
			others++
		}
	}
	return map[string]any{"admins": admins, "others": others}, nil
}
