// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"reflect"
	"regexp"
	"slices"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/agentpolicy"
	"github.com/cjohnstoniv/wardyn/internal/policyref"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// An organisation's rules for each agent harness (agent_rules.go) met into every run that includes the
// harness: the run step (agentRulesRunStep), the seal layers that re-assert the rules at dispatch and revive,
// the recording floor and the inner-walls refusal that names the rule.

// agentRulesProvenance is the record of a value an agent's rules decided.
func agentRulesProvenance(harness string) policyref.Provenance {
	return policyref.Provenance{Layer: policyref.LayerAgentRules, Name: harness}
}

// agentRulesSourceRecord is one entry of policy_source.agent_rules on the run.create row: whose rules, at
// which version, and what they narrowed, removed and added. The embed is the exported alias, as
// wallPresetSourceRecord's is: encoding/json cannot allocate an embedded pointer to an unexported type when
// it reads the row back.
type agentRulesSourceRecord struct {
	Harness string `json:"harness"`
	Version int    `json:"version"`
	*WallsSourceRecord
	// policySetInnerWalls is which harness_inner_walls axes the run's policy already set before these rules
	// met into it; the inner-walls refusal then names both for that axis (agentRulesInnerWallsFor). Never on
	// the wire.
	policySetInnerWalls innerAxesSet
}

// innerAxesSet is which axes of harness_inner_walls a policy set.
type innerAxesSet struct{ sandboxMode, approvals bool }

func innerAxesOf(w *types.HarnessInnerWalls) innerAxesSet {
	if w == nil {
		return innerAxesSet{}
	}
	return innerAxesSet{sandboxMode: w.SandboxMode != "", approvals: w.Approvals != ""}
}

// has reports whether the policy set axis ("sandbox_mode" or "approvals"); any other axis, one a refusal does
// not name, asks whether it set either.
func (a innerAxesSet) has(axis string) bool {
	switch axis {
	case "sandbox_mode":
		return a.sandboxMode
	case "approvals":
		return a.approvals
	}
	return a.sandboxMode || a.approvals
}

// includedHarnessIDs is the ids of the harnesses req includes: its run-mode set, else its agent.
func includedHarnessIDs(req createRunRequest) []string {
	var ids []string
	for _, h := range harnessSet(req) {
		ids = append(ids, h.Harness)
	}
	if len(ids) == 0 && req.Agent != "" {
		ids = []string{req.Agent}
	}
	return ids
}

// runHarnessIDs is the ids of the harnesses a stored run includes: its harness rows, else its agent. A run
// with no id (a create-time stand-in) has none, and a store that keeps no harness rows has the agent alone.
// A server-launched step that runs no agent (a source scan, or a workspace step that is not an interactive
// session) has none either: its Agent is only the step default (stepRunAgent), and its WorkspaceID and
// SourceID are set by the server alone, so no request can name them to leave the rules. A record or verify
// session is interactive and a harness login is a run of the vendor CLI, so both stay bound.
func (s *Server) runHarnessIDs(ctx context.Context, run types.AgentRun) ([]string, error) {
	if run.ID == uuid.Nil || !isModelRun("", run.WorkspaceID, run.SourceID, run.Interactive) {
		return nil, nil
	}
	var ids []string
	if hs, ok := s.cfg.Store.(store.RunHarnessStore); ok {
		rows, err := hs.ListRunHarnesses(ctx, run.ID)
		if err != nil {
			return nil, err
		}
		for _, h := range rows {
			ids = append(ids, h.Harness)
		}
	}
	if len(ids) == 0 && run.Agent != "" {
		ids = []string{run.Agent}
	}
	return ids, nil
}

// agentRulesFor is the rules that bind a run including harnesses; nil for a store that keeps none. A list that
// names no harness a rule can bind ("", "none" and nothing) reads nothing: no rule binds such a run
// (agentpolicy.RulesFor), so an unreadable table cannot refuse it.
func (s *Server) agentRulesFor(ctx context.Context, harnesses []string) ([]types.AgentRules, error) {
	if !slices.ContainsFunc(harnesses, func(h string) bool { return h != "" && h != "none" }) {
		return nil, nil
	}
	rs, ok := s.cfg.Store.(store.AgentRulesStore)
	if !ok {
		return nil, nil
	}
	all, err := rs.ListAgentRules(ctx)
	if err != nil {
		return nil, err
	}
	return agentpolicy.RulesFor(harnesses, all), nil
}

// agentRulesKept reports whether this server's store keeps agent rules at all; one that does not has nothing
// to read, so no read of the run's harness rows is made for them either.
func (s *Server) agentRulesKept() bool {
	_, ok := s.cfg.Store.(store.AgentRulesStore)
	return ok
}

// agentRulesLabel names a harness's rules mid-sentence (who) and at a sentence's start (whoStart).
func agentRulesLabel(harness string) (who, whoStart string) {
	display := harness
	if d, ok := harnessByID(harness); ok && d.Display != "" {
		display = d.Display
	}
	return "your organisation's rules for " + display, "Your organisation's rules for " + display
}

// agentRulesRunStep narrows spec by the rules of each harness req includes, after the person's walls and the
// wall preset and by the same step (wallsRunStep), so it only narrows. Each knob it changes is recorded as
// that harness's rules. Rules that cannot be read refuse the run (500), and a stored rule the walls rules now
// refuse refuses it too (422): the run never launches without a rule that binds it.
func (s *Server) agentRulesRunStep(ctx context.Context, req createRunRequest, spec *types.RunPolicySpec, source *policySourceRecord) ([]string, *runRefusal) {
	rules, err := s.agentRulesFor(ctx, includedHarnessIDs(req))
	if err != nil {
		return nil, runServerError("read the agent rules", err)
	}
	var warns []string
	// Whether the run's own policy set the agent's walls, read before any rule meets into it: a later rule is not
	// the policy, and a refusal names the policy only when the policy itself set the axis.
	policySet := innerAxesOf(spec.HarnessInnerWalls)
	for _, rule := range rules {
		who, whoStart := agentRulesLabel(rule.Harness)
		if refusal := validateAgentRulesWrite(rule.Harness, types.AgentRulesWrite{Spec: rule.Spec, InnerWalls: rule.InnerWalls}); refusal != nil {
			return nil, runError(http.StatusUnprocessableEntity, reasonAgentRulesInvalid,
				whoStart+" no longer pass the rules for walls. Ask a security admin to fix them, or launch without this agent.")
		}
		prov := agentRulesProvenance(rule.Harness)
		rec, err := s.wallsRunStep(spec, source, rule.Spec, prov)
		if err != nil {
			return nil, wallsMeetRefusal(err, who, whoStart, "Ask a security admin to change the agent rules, or launch without this agent.", false)
		}
		var innerBefore *types.HarnessInnerWalls
		if spec.HarnessInnerWalls != nil {
			c := *spec.HarnessInnerWalls
			innerBefore = &c
		}
		if rule.InnerWalls != nil && *rule.InnerWalls != (types.HarnessInnerWalls{}) {
			axis := ""
			source.knobs.step(prov, spec, func() {
				var met *types.HarnessInnerWalls
				if met, axis = agentpolicy.MeetInnerWalls(spec.HarnessInnerWalls, rule.InnerWalls); axis == "" {
					spec.HarnessInnerWalls = met
				}
			})
			if axis != "" && !policySet.has(axis) {
				return nil, runError(http.StatusUnprocessableEntity, reasonAgentRulesConflict,
					whoStart+" set harness_inner_walls."+axis+" to a value that the rules of another agent in this run do not allow. Ask a security admin to change the agent rules, or launch without one of the agents.")
			}
			if axis != "" {
				return nil, runError(http.StatusUnprocessableEntity, reasonAgentRulesConflict,
					"This run's policy sets harness_inner_walls."+axis+" to a value "+who+" do not allow. Leave it out, or match the rules.")
			}
		}
		if rec != nil || !reflect.DeepEqual(innerBefore, spec.HarnessInnerWalls) {
			source.AgentRules = append(source.AgentRules, agentRulesSourceRecord{Harness: rule.Harness, Version: rule.Version,
				WallsSourceRecord: rec, policySetInnerWalls: policySet})
		}
		warns = append(warns, wallsWarnings(who, rec)...)
	}
	return warns, nil
}

var innerWallsAxisRE = regexp.MustCompile(`harness_inner_walls\.(sandbox_mode|approvals)`)

// agentRulesInnerWallsFor is innerWallsFor with its refusal worded for the rule when a rule set the knob it
// refused: a person cannot "use a policy without harness_inner_walls" against an organisation's rule. A rule
// looser than the agent's lane is refused the same way, never launched looser. When the run's policy set the
// knob before the rule met into it, the sentence names both.
func agentRulesInnerWallsFor(req createRunRequest, spec types.RunPolicySpec, harnesses []types.RunHarness, class types.ConfinementClass, source policySourceRecord) (*types.HarnessInnerWalls, *runRefusal) {
	walls, refusal := innerWallsFor(req, spec, harnesses, class)
	if refusal == nil || refusal.err != nil || source.knobs["harness_inner_walls"].Layer != policyref.LayerAgentRules {
		return walls, refusal
	}
	h := source.knobs["harness_inner_walls"].Name
	who, whoStart := agentRulesLabel(h)
	axis, named := "they bind every agent in the run", ""
	if m := innerWallsAxisRE.FindStringSubmatch(refusal.body.Error); m != nil {
		axis, named = "the agent cannot meet the "+m[1]+" rule", m[1]
	}
	for _, rec := range source.AgentRules {
		if rec.Harness == h && rec.policySetInnerWalls.has(named) {
			return nil, runError(http.StatusUnprocessableEntity, reasonAgentRulesConflict,
				"This run's policy and "+who+" together set the agent's own walls, and this run cannot launch with them: "+axis+
					". Change the policy's harness_inner_walls, or launch without the agent.")
		}
	}
	return nil, runError(http.StatusUnprocessableEntity, reasonAgentRulesConflict,
		whoStart+" set the agent's own walls, and this run cannot launch with them: "+axis+". Launch the agent on a lane that meets them, or launch without it.")
}

// agentRulesReadError is a read of the agent rules (or of the run's harness rows they are keyed by) that
// failed, kept apart from a failed read of the person's walls so the refusal names what could not be read.
type agentRulesReadError struct{ err error }

func (e *agentRulesReadError) Error() string { return "read the agent rules: " + e.err.Error() }
func (e *agentRulesReadError) Unwrap() error { return e.err }

// recordingFloorReadFailure names what a failed recording floor read could not read: the operation (audit row
// and server error) and the sentence the run fails with.
func recordingFloorReadFailure(err error) (operation, sentence string) {
	if errors.As(err, new(*agentRulesReadError)) {
		return "read the agent rules", "the rules of the run's agents could not be read, so the run's recording level cannot be set; rules that cannot be read refuse the run"
	}
	return "read the run owner's walls", "your walls could not be read, so the run's recording level cannot be set"
}

// agentRulesRecordingFloor is the highest recording level the rules of the run's harnesses name, "" when none.
// harnesses nil reads the run's own. A read that fails is returned, never ignored.
func (s *Server) agentRulesRecordingFloor(ctx context.Context, run types.AgentRun, harnesses []string) (types.RecordingLevel, error) {
	if !s.agentRulesKept() {
		return "", nil
	}
	if harnesses == nil {
		var err error
		if harnesses, err = s.runHarnessIDs(ctx, run); err != nil {
			return "", &agentRulesReadError{err}
		}
	}
	rules, err := s.agentRulesFor(ctx, harnesses)
	if err != nil {
		return "", &agentRulesReadError{err}
	}
	var floor types.RecordingLevel
	for _, r := range rules {
		if rec := r.Spec.Recording; rec != nil && rec.Rank() > floor.Rank() {
			floor = *rec
		}
	}
	return floor, nil
}

// agentRulesSealLayers is the rules of each agent the run includes as seal layers, built as the owner's walls
// are (wallsSealLayer): denies, blocked desktop directions, withheld credentials and refused pushes, never the
// allowlist. A store that keeps no rules has none. Rules or harness rows that cannot be read seal the run to no
// egress and no credential, as unread walls do.
func (s *Server) agentRulesSealLayers(ctx context.Context, run types.AgentRun) []sealLayer {
	if !s.agentRulesKept() {
		return nil
	}
	ids, err := s.runHarnessIDs(ctx, run)
	var rules []types.AgentRules
	if err == nil {
		rules, err = s.agentRulesFor(ctx, ids)
	}
	if err != nil {
		slog.ErrorContext(ctx, "wardynd: read the agent rules at the seal; the run is sealed to no egress",
			slog.String("run_id", run.ID.String()), slog.Any("err", err))
		off := false
		return []sealLayer{{rec: agentRulesProvenance(""), egress: types.CeilingOverlay{AllowedDomains: &[]string{}, AllowAllEgress: &off},
			grants: &[]types.GrantSpec{}}}
	}
	var layers []sealLayer
	for _, rule := range rules {
		layers = append(layers, wallsSealLayer(agentRulesProvenance(rule.Harness), rule.Spec)...)
	}
	return layers
}

// agentRulesReassertFlag marks the run.ceiling.reassert row when an agent's rules were among the layers.
func agentRulesReassertFlag(data map[string]any, layers []sealLayer) {
	for _, l := range layers {
		if l.rec.Layer == policyref.LayerAgentRules {
			data["agent_rules"] = true
			return
		}
	}
}

// recordBoundPolicy is the policy a workspace record or verify session launches under: the session's own, met
// with the rules of the agent it runs (stepRunAgent) as a run's policy is, then held inside the boundary. It
// only narrows. The session launches at the strongest class the runner offers (class, of caps). A rule it cannot
// meet refuses the launch with agent_rules_conflict or agent_rules_invalid: a confinement floor above that class,
// or inner walls the interactive claude-code session cannot launch with. Rules that cannot be read fail it as a
// server error, never launch without them.
func (s *Server) recordBoundPolicy(ctx context.Context, ceiling governanceCeiling, policy types.RunPolicySpec, class types.ConfinementClass, caps runner.Capabilities) (types.RunPolicySpec, error) {
	source := policySourceRecord{knobs: startProvenance(memberProvenance)}
	if _, refusal := s.agentRulesRunStep(ctx, createRunRequest{Agent: stepRunAgent}, &policy, &source); refusal != nil {
		return policy, recordRulesRefusal(refusal)
	}
	if floor := policy.MinConfinementClass; floor.Rank() > class.Rank() {
		if rec := source.knobs["min_confinement_class"]; rec.Layer == policyref.LayerAgentRules {
			_, whoStart := agentRulesLabel(rec.Name)
			msg := fmt.Sprintf("%s set min_confinement_class %s: runner %q cannot enforce confinement_class %s (available: %s). Ask a security admin to change the agent rules, or use a runner that offers it.",
				whoStart, floor, caps.Driver, floor, classesOrNone(caps.ConfinementClasses))
			return policy, &sourceRefusal{status: http.StatusUnprocessableEntity, reason: reasonAgentRulesConflict, err: errors.New(msg)}
		}
	}
	if _, refusal := agentRulesInnerWallsFor(createRunRequest{Agent: stepRunAgent, Interactive: true}, policy, nil, class, source); refusal != nil {
		return policy, recordRulesRefusal(refusal)
	}
	return boundaryRecordPolicy(ceiling, policy)
}

// recordRulesRefusal is a run refusal as the record launch returns it: a server error as an error, anything else
// as the *sourceRefusal handleRecordWorkspace answers with its own status and reason.
func recordRulesRefusal(refusal *runRefusal) error {
	if refusal.err != nil {
		return fmt.Errorf("%s: %w", refusal.operation, refusal.err)
	}
	return &sourceRefusal{status: refusal.status, reason: refusal.body.Reason, err: errors.New(refusal.body.Error)}
}
