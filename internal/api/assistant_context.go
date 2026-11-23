// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/assistant"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// assistantRefusal is a refusal decided while preparing a preview or a session; it is answered once. custom,
// when set, answers it its own way and reports whether it did; otherwise the status, reason and message answer.
type assistantRefusal struct {
	status int
	reason string
	msg    string
	custom func(http.ResponseWriter, *http.Request) bool
}

func (f *assistantRefusal) write(w http.ResponseWriter, r *http.Request) {
	if f.custom != nil && f.custom(w, r) {
		return
	}
	writeErrorReason(w, f.status, f.reason, f.msg)
}

// assistantTargetGone is the one refusal for a target the person cannot read or that does not exist, so the
// answer never says which.
func assistantTargetGone() *assistantRefusal {
	return assistantRefusalFor(http.StatusNotFound, reasonAssistantTargetNotFound, "That target was not found.") // PROPOSED
}

func assistantRefusalFor(status int, reason, msg string) *assistantRefusal {
	return &assistantRefusal{status: status, reason: reason, msg: msg}
}

// assistantNeverSent is the 409 of a target whose governance kind is on the organisation's never-sent list.
func assistantNeverSent(set *types.AssistantSettings, kind string) *assistantRefusal {
	if kind == "" || set == nil || !slices.Contains(set.NeverSentKinds, kind) {
		return nil
	}
	return assistantRefusalFor(http.StatusConflict, reasonAssistantKindNeverSent, "Your organisation keeps this kind of record away from the assistant.") // PROPOSED: the board's words
}

// assistantStaticKind is the governance kind a target names by its own kind, before anything is read: only a
// unit does. A change names its kind once it is read.
func assistantStaticKind(t types.AssistantTarget) string {
	if t.Kind == types.AssistantTargetUnit {
		return govKindUnit
	}
	return ""
}

// assistantToolNames is the tools a purpose is offered. A review session has no draft and no build-files tool.
func assistantToolNames(purpose types.AssistantPurpose) []string {
	tools := []string{"read", "validate", "diff", "narrowing_check", "grade", "impact", "simulate", "explain"}
	if purpose == types.AssistantCompose {
		tools = append(tools, "draft", "build_files")
	}
	return tools
}

// assistantServing is availability's ready answer pinned to one provider row: the binding of a preview and its
// session.
func assistantServing(ans availabilityAnswer, sc types.SiteConfig) (types.ModelProvider, types.AssistantBinding, bool) {
	if ans.State != types.AssistantReady {
		return types.ModelProvider{}, types.AssistantBinding{}, false
	}
	p, ok := modelProviderByID(sc.ModelProviders, ans.ProviderID)
	if !ok {
		return types.ModelProvider{}, types.AssistantBinding{}, false
	}
	return p, types.AssistantBinding{ProviderID: p.ID, ProviderUID: p.UID, Model: ans.Model}, true
}

// assistantTarget is a target the person has been shown to be able to read, with what was read to show it.
type assistantTarget struct {
	types.AssistantTarget
	govKind  string
	unit     *ResolvedUnit
	run      *types.AgentRun
	change   *types.GovernanceChange
	template *types.RunTemplate
}

// key is the stored identity of the target: its id, empty for a new draft.
func (t assistantTarget) key() string { return t.ID }

// assistantResolveTarget checks that the person on tr can read the target, as the read doors do, and reads it.
// Anything unreadable answers the one not-found refusal.
func (s *Server) assistantResolveTarget(tr *http.Request, t types.AssistantTarget, snap assistantSnapshot) (assistantTarget, *assistantRefusal) {
	out := assistantTarget{AssistantTarget: t, govKind: assistantStaticKind(t)}
	ctx := tr.Context()
	if t.ID == "" {
		if t.Kind == types.AssistantTargetChange {
			return out, assistantRefusalFor(http.StatusBadRequest, reasonAssistantPurposeTargetMismatch, "A review needs a change.") // PROPOSED
		}
		return out, nil
	}
	id, err := uuid.Parse(t.ID)
	if err != nil {
		return out, assistantRefusalFor(http.StatusBadRequest, reasonInvalidRequestBody, "The target id is not valid.") // PROPOSED
	}
	switch t.Kind {
	case types.AssistantTargetUnit:
		in, err := s.unitScopeEvery(ctx, false, id)
		if err != nil || !in {
			return out, s.assistantReadFailed(err)
		}
		u, err := s.resolveUnitByID(ctx, id)
		if err != nil {
			return out, s.assistantReadFailed(err)
		}
		out.unit = u
	case types.AssistantTargetRun:
		run, err := s.cfg.Store.GetRun(ctx, id)
		if err != nil || !(run.CreatedBy == snap.Principal || s.isSecurityOperator(ctx)) {
			return out, s.assistantReadFailed(err)
		}
		out.run = &run
	case types.AssistantTargetTemplate:
		ts, ok := s.cfg.Store.(store.TemplateStore)
		if !ok {
			return out, assistantTargetGone()
		}
		list, err := ts.ListTemplatesVisible(ctx, snap.Principal, oidcGroupsFromContext(ctx))
		if err != nil {
			return out, s.assistantReadFailed(err)
		}
		i := slices.IndexFunc(list, func(x types.RunTemplate) bool { return x.ID == id })
		if i < 0 {
			return out, assistantTargetGone()
		}
		out.template = &list[i]
	case types.AssistantTargetChange:
		ch, err := s.cfg.Store.GetGovernanceChange(ctx, id)
		if err != nil || !s.canSeeGovernanceChange(tr, ch) {
			return out, s.assistantReadFailed(err)
		}
		out.change, out.govKind = &ch, ch.TargetKind
	}
	return out, nil
}

// assistantReadFailed is the not-found refusal for a missing or unreadable target, and a server error for a
// store failure, which is never dressed as a not-found.
func (s *Server) assistantReadFailed(err error) *assistantRefusal {
	if err == nil || errors.Is(err, store.ErrNotFound) {
		return assistantTargetGone()
	}
	return assistantRefusalFor(http.StatusInternalServerError, "", "Could not read the target.") // PROPOSED
}

// assistantJSON is v as indented JSON text for a record.
func assistantJSON(v any) string {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return ""
	}
	return string(b)
}

// assistantRecordSpec is a policy spec as the person may read it: cut at the tier their context yields, mount
// sources removed at the security tier and below.
func (s *Server) assistantRecordSpec(ctx context.Context, spec types.RunPolicySpec) types.RunPolicySpec {
	return redactSpecForRead(spec, s.specReadTierFor(ctx))
}

// assistantGrantLines is the unit's credential grants by kind and host, never a secret name or value.
func assistantGrantLines(spec types.RunPolicySpec) []string {
	var lines []string
	for _, g := range spec.EligibleGrants {
		var scope struct {
			Host string `json:"host"`
		}
		_ = json.Unmarshal(g.Scope, &scope)
		lines = append(lines, strings.TrimSpace(fmt.Sprintf("%s %s", g.Kind, scope.Host)))
	}
	return lines
}

// assistantPerson is a person's e-mail when the subject is one, else their name.
func assistantPerson(subject string) assistant.Person { return assistant.PersonOf(subject) }

// assistantInputs reads the records the first message carries, through the person's own read paths. It reads
// no secret value, no credential, no audit row and no per-run or per-person row.
func (s *Server) assistantInputs(tr *http.Request, rt assistantTarget, purpose types.AssistantPurpose, set *types.AssistantSettings, snap assistantSnapshot) (assistant.Inputs, error) {
	ctx := tr.Context()
	in := assistant.Inputs{Purpose: purpose, RunHistoryOn: set != nil && set.RunHistoryCounts}
	var spec types.RunPolicySpec
	switch {
	case rt.unit != nil:
		spec = s.assistantRecordSpec(ctx, rt.unit.Ceiling)
		in.UnitRecord = assistantJSON(map[string]any{"name": rt.unit.Name, "ceiling": spec, "limits": rt.unit.Limits})
		admins, err := s.assistantUnitAdmins(ctx, rt.unit.ID)
		if err != nil {
			return in, err
		}
		in.UnitAdmins = admins
	case rt.run != nil:
		// The run's policy, under the run's own label. Its free-text task is a person's words with no place
		// in the records, so it is never sent.
		in.RunOwners = []assistant.Person{assistantPerson(rt.run.CreatedBy)}
		if rt.run.PolicyID != nil {
			p, err := s.cfg.Store.GetPolicy(ctx, *rt.run.PolicyID)
			if err != nil {
				return in, err
			}
			spec = s.assistantRecordSpec(ctx, p.Spec)
			in.RunRecord = assistantJSON(spec)
		}
	case rt.change != nil:
		ch := s.changeForRead(tr, *rt.change)
		in.Change = assistantJSON(map[string]any{"target_kind": ch.TargetKind, "op": ch.Op, "target_key": ch.TargetKey, "diff": ch.Diff, "note": ch.Note})
		p := assistantPerson(rt.change.ProposedBy)
		in.ChangeProposer = &p
		// Everyone else the change names, such as the person a membership assigns, becomes a letter too.
		in.ChangePeople = assistant.PeopleNamedIn([]byte(in.Change))
	}
	in.Grants = assistantGrantLines(spec)
	in.Templates = s.assistantTemplateLines(ctx, rt, snap)
	if in.RunHistoryOn {
		counts, err := s.assistantHostCounts(ctx, rt, snap)
		if err != nil {
			return in, err
		}
		in.RunHistory = counts
	}
	return in, nil
}

func (s *Server) assistantUnitAdmins(ctx context.Context, unit uuid.UUID) ([]assistant.Person, error) {
	st := s.unitAdminStore()
	if st == nil {
		return nil, nil
	}
	rows, err := st.ListUnitAdmins(ctx)
	if err != nil {
		return nil, err
	}
	var out []assistant.Person
	for _, a := range rows {
		if a.UnitID == unit && a.SubjectType == types.CapabilitySubjectUser {
			out = append(out, assistantPerson(a.Subject))
		}
	}
	return out, nil
}

// assistantTemplateLines are one-line summaries of the templates the person can see: the target template, or for
// a new draft the visible names. A template's document is never sent.
func (s *Server) assistantTemplateLines(ctx context.Context, rt assistantTarget, snap assistantSnapshot) []string {
	line := func(t types.RunTemplate) string {
		return strings.TrimSpace(t.Name + ": " + firstLine(t.Description))
	}
	if rt.template != nil {
		return []string{line(*rt.template)}
	}
	ts, ok := s.cfg.Store.(store.TemplateStore)
	if !ok || rt.ID != "" {
		return nil
	}
	list, err := ts.ListTemplatesVisible(ctx, snap.Principal, oidcGroupsFromContext(ctx))
	if err != nil {
		return nil
	}
	lines := make([]string, 0, len(list))
	for _, t := range list {
		lines = append(lines, line(t))
	}
	return lines
}

func firstLine(s string) string {
	line, _, _ := strings.Cut(strings.TrimSpace(s), "\n")
	return line
}

// assistantHistoryWindow and assistantHistoryRuns bound the run history the first message may carry.
const (
	assistantHistoryWindow = 30 * 24 * time.Hour
	assistantHistoryRuns   = 500
	assistantHistoryHosts  = 50
)

// assistantHostCounts is how many runs of the last 30 days reached each host, through the replay store, over
// the unit's runs or the person's own, leaving out every run an assistant session ever served.
func (s *Server) assistantHostCounts(ctx context.Context, rt assistantTarget, snap assistantSnapshot) ([]assistant.HostCount, error) {
	rd, ok := s.cfg.Store.(store.RunHostDecisionReader)
	ss, ok2 := s.cfg.Store.(store.AssistantSessionStore)
	if !ok || !ok2 {
		return nil, nil
	}
	now := s.cfg.Now().UTC()
	since, until := now.Add(-assistantHistoryWindow), now.Add(time.Minute)
	exclude, err := ss.AssistantRunIDsCreated(ctx, since, until)
	if err != nil {
		return nil, err
	}
	scope := store.ReplayScope{Owner: snap.Principal}
	if rt.unit != nil {
		scope = store.ReplayScope{Units: []uuid.UUID{rt.unit.ID}}
	}
	rows, _, err := rd.RunHostDecisions(ctx, scope, since, until, assistantHistoryRuns, exclude)
	if err != nil {
		return nil, err
	}
	return assistantHostCountsOf(rows), nil
}

// assistantHostCountsOf counts the distinct runs per host and decision, most first, capped at
// assistantHistoryHosts lines, so a host refused 400 times never reads as reached.
func assistantHostCountsOf(rows []store.RunHostDecision) []assistant.HostCount {
	type key struct{ host, decision string }
	runs := map[key]map[uuid.UUID]bool{}
	for _, r := range rows {
		k := key{r.Host, r.Decision}
		if runs[k] == nil {
			runs[k] = map[uuid.UUID]bool{}
		}
		runs[k][r.RunID] = true
	}
	out := make([]assistant.HostCount, 0, len(runs))
	for k, set := range runs {
		out = append(out, assistant.HostCount{Host: k.host, Decision: k.decision, Count: len(set)})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Count != out[j].Count {
			return out[i].Count > out[j].Count
		}
		if out[i].Host != out[j].Host {
			return out[i].Host < out[j].Host
		}
		return out[i].Decision < out[j].Decision
	})
	if len(out) > assistantHistoryHosts {
		out = out[:assistantHistoryHosts]
	}
	return out
}

// assistantPrep is everything a preview and a session start share: the binding, the records and their digest.
type assistantPrep struct {
	sc       types.SiteConfig
	provider types.ModelProvider
	binding  types.AssistantBinding
	target   assistantTarget
	payload  assistant.Payload
	digest   string
	snap     assistantSnapshot
	lim      assistantLimits
	// role is the role the tool context acts at, the lower of the snapshot's and the fresh one.
	role string
}

// assistantPrepare runs the checks both the preview and the session start make, in order, and returns the
// refusal that stopped it: a person on the request; availability ready; the purpose and target pair; the
// never-sent kind a target names itself; that the person can read the target; its kind once read; the
// deferral refusal; and the records with their digest.
func (s *Server) assistantPrepare(r *http.Request, purpose types.AssistantPurpose, target types.AssistantTarget) (*assistantPrep, *assistantRefusal) {
	snap := assistantSnapshotOf(r.Context())
	if assistantNoPerson(snap) {
		return nil, assistantRefusalFor(http.StatusForbidden, reasonAssistantRoleDenied, "The assistant works for a signed-in person.") // PROPOSED
	}
	if !purpose.Valid() || !target.ValidFor(purpose) {
		return nil, assistantRefusalFor(http.StatusBadRequest, reasonAssistantPurposeTargetMismatch, "Compose takes a run, unit or template. Review takes a change.") // PROPOSED
	}
	prep := &assistantPrep{snap: snap}
	if ref := s.assistantAvailable(r, purpose, prep); ref != nil {
		return nil, ref
	}
	if ref := assistantNeverSent(prep.sc.Assistant, assistantStaticKind(target)); ref != nil {
		return nil, ref
	}
	tctx, release, err := s.assistantToolCtx(r, snap)
	if err != nil {
		return nil, s.assistantToolCtxRefusal(err)
	}
	defer release()
	prep.role = oidcRoleFromContext(tctx)
	tr := r.WithContext(tctx)
	rt, ref := s.assistantResolveTarget(tr, target, snap)
	if ref != nil {
		return nil, ref
	}
	if ref := assistantNeverSent(prep.sc.Assistant, rt.govKind); ref != nil {
		return nil, ref
	}
	if ref := refuseAssistantDeferral(prep.provider); ref != nil {
		return nil, ref
	}
	in, err := s.assistantInputs(tr, rt, purpose, prep.sc.Assistant, snap)
	if err != nil {
		return nil, assistantRefusalFor(http.StatusInternalServerError, "", "Could not read the records.") // PROPOSED
	}
	prep.target, prep.payload = rt, assistant.FirstPayload(in)
	prep.digest = assistant.PreviewDigest(prep.binding, prep.payload.Sections)
	return prep, nil
}

// assistantToolCtxRefusal answers a tool context that could not be built: a person who is no longer allowed is
// 403, anything else a server error.
func (s *Server) assistantToolCtxRefusal(err error) *assistantRefusal {
	if errors.Is(err, errAssistantRoleDenied) {
		return assistantRefusalFor(http.StatusForbidden, reasonAssistantRoleDenied, "Your role no longer allows the assistant.") // PROPOSED
	}
	return assistantRefusalFor(http.StatusInternalServerError, "", "Could not check your role.") // PROPOSED
}

// assistantAvailable is step one: availability must be ready, else 409 with the state's reason. It fills the
// site config, the person's limits, the serving provider and the binding.
func (s *Server) assistantAvailable(r *http.Request, purpose types.AssistantPurpose, prep *assistantPrep) *assistantRefusal {
	ctx := r.Context()
	sc, err := s.cfg.Store.GetSiteConfig(ctx)
	if err != nil {
		return assistantRefusalFor(http.StatusInternalServerError, "", "Could not read the settings.") // PROPOSED
	}
	prep.sc = sc
	if sc.Assistant == nil || !sc.Assistant.On(purpose) {
		return assistantRefusalFor(http.StatusConflict, reasonAssistantOff, "The assistant is off for your organisation.") // PROPOSED: the board's words
	}
	lim, err := s.assistantLimitsFor(ctx, sc, prep.snap.Principal)
	if err != nil {
		return assistantRefusalFor(http.StatusInternalServerError, "", "Could not read your limits.") // PROPOSED
	}
	prep.lim = lim
	ans := assistantAvailability(sc.Assistant, purpose, siteProviders(sc), inspectionDeferredFor, lim.deny, lim.allowed, lim.connected)
	if ans.State == types.AssistantReady {
		full, err := s.assistantAtRunLimit(ctx, lim.ceiling, prep.snap.Principal, purpose)
		if err != nil {
			return assistantRefusalFor(http.StatusInternalServerError, "", "Could not count your runs.") // PROPOSED
		}
		if full {
			return assistantRefusalFor(http.StatusConflict, reasonAssistantAtRunLimit, "You're at your run limit. End a run to start the assistant.") // PROPOSED: the settings card's words
		}
	}
	p, b, ok := assistantServing(ans, sc)
	if !ok {
		return assistantRefusalFor(http.StatusConflict, ans.Reason, "The assistant is not available to you.") // PROPOSED
	}
	prep.provider, prep.binding = p, b
	return nil
}

// assistantPreviewRequest is the body of POST /assistant/preview.
type assistantPreviewRequest struct {
	Purpose types.AssistantPurpose `json:"purpose"`
	Target  types.AssistantTarget  `json:"target"`
}

// handleAssistantPreview is POST /api/v1/assistant/preview: the records the first message will carry, byte for
// byte, with their digest and the letters for the browser. It starts nothing.
func (s *Server) handleAssistantPreview(w http.ResponseWriter, r *http.Request) {
	var req assistantPreviewRequest
	if msg := decodeStrictMsg(w, r, &req); msg != "" {
		writeErrorReason(w, http.StatusBadRequest, reasonInvalidRequestBody, msg)
		return
	}
	if s.assistantRateRefused(w, oidcHumanFromContext(r.Context())) {
		return
	}
	prep, ref := s.assistantPrepare(r, req.Purpose, req.Target)
	if ref != nil {
		ref.write(w, r)
		return
	}
	writeJSON(w, http.StatusOK, types.AssistantPreview{
		Provider: providerDisplayName(prep.provider), Model: prep.binding.Model, Sections: prep.payload.Sections,
		Digest: prep.digest, Letters: prep.payload.Letters, NeverSent: slices.Clone(prep.sc.Assistant.Normalized().NeverSentKinds),
		Tools: assistantToolNames(req.Purpose),
	})
}
