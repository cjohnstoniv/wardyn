// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/assistant"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// assistantRunTask is the label of an assistant run, and what marks it for the sandbox's WARDYN_ASSISTANT.
const assistantRunTask = "Assistant session" // PROPOSED

// assistantRunLifetimeSec is how long an assistant run lives: the session's lifetime. The relay ends it
// sooner after fifteen idle minutes.
const assistantRunLifetimeSec = 7200

// assistantScanBytes is the scanner's span for an assistant run: the default span, spelled out, and above
// every cap on what a first message and a tool answer carry.
const assistantScanBytes = 1 << 20

// assistantRunPolicy is the policy Wardyn writes for an assistant run, taken from no template and no draft:
// no mounts, no workspace, no repositories, no grants, no SSH, and an empty allow-list (dispatch adds the
// provider's endpoint). Prompt inspection is forced to block with no gateway deferral, and every field is set.
func assistantRunPolicy(cc types.ConfinementClass) types.RunPolicySpec {
	return types.RunPolicySpec{
		MinConfinementClass: cc,
		AllowedDomains:      []string{},
		AutoStopAfterSec:    assistantRunLifetimeSec,
		LLMInspection: &types.LLMInspectionSpec{
			Mode:                  "block",
			DetectSecrets:         true,
			DetectSecretPatterns:  true,
			DetectEntropy:         false, // lockfile integrity hashes in build files would block every repository draft
			DetectPII:             false, // a visibility signal, never a control; letters cover names
			OnScannerError:        "block",
			RequireInspectableLLM: true,
			InterceptTLS:          true, // only TLS interception inspects an opaque CONNECT
			BlockMinSeverity:      "low",
			MaxScanBytes:          assistantScanBytes,
		},
	}
}

// runIsAssistant reports whether a live or ended session row holds the run.
func (s *Server) runIsAssistant(ctx context.Context, run uuid.UUID) bool {
	ss, ok := s.cfg.Store.(store.AssistantSessionStore)
	if !ok {
		return false
	}
	ids, err := ss.AssistantRunIDs(ctx, []uuid.UUID{run})
	return err == nil && len(ids) == 1
}

// assistantLaunch is an assistant run created but not yet dispatched, with what dispatch needs.
type assistantLaunch struct {
	run   types.AgentRun
	token string
	cc    types.ConfinementClass
	dc    dispatchCeiling
}

// assistantNewRun creates the run of a session, pending. It is launchSourceScanRun's shape: the host-capacity
// door, the person's ceiling, placement, the strongest confinement class at or above their floor, and the
// limits axis, where max_concurrent_runs binds. Nothing is dispatched: the caller writes the session row with
// the run id first.
func (s *Server) assistantNewRun(ctx context.Context, actor string, binding types.AssistantBinding) (assistantLaunch, error) {
	if herr := s.admitHostCapacity(ctx, actor, "assistant", true); herr != nil {
		return assistantLaunch{}, herr
	}
	ctx = context.WithoutCancel(ctx)
	dc, ceiling, err := s.resolveDispatchCeiling(ctx)
	if err != nil {
		return assistantLaunch{}, err
	}
	if ref := s.placementDeniedRefusal(ctx, types.PlacementRemote, ceiling); ref != nil {
		return assistantLaunch{}, &placementDeniedError{ref}
	}
	if s.cfg.Runner == nil {
		return assistantLaunch{}, errors.New("api: no runner is configured")
	}
	caps, err := s.cfg.Runner.Capabilities(ctx)
	if err != nil {
		return assistantLaunch{}, fmt.Errorf("runner capabilities unavailable: %w", err)
	}
	cc := strongestAdvertisedAtOrAbove(caps.ConfinementClasses, ceilingFloorClass(ceiling))
	run, token, err := s.newStepRun(ctx, uuid.New(), actor, assistantRunTask, cc, stepRunGoverned(ceiling, false, true), func(run *types.AgentRun) {
		run.ModelProviderID = binding.ProviderID
		run.AutoStopAfterSec = assistantRunLifetimeSec
	})
	if err != nil {
		return assistantLaunch{}, err
	}
	created, err := s.createRun(ctx, run)
	if err != nil {
		return assistantLaunch{}, fmt.Errorf("create assistant run: %w", err)
	}
	return assistantLaunch{run: created, token: token, cc: cc, dc: dc}, nil
}

// assistantDispatch dispatches a created run under the policy Wardyn wrote. A deferring provider is refused
// again where the gateway route is sealed (takeProviderGatewayRoute).
func (s *Server) assistantDispatch(ctx context.Context, l assistantLaunch) types.AgentRun {
	ctx = context.WithoutCancel(ctx)
	return s.dispatchAndSettle(ctx, l.run, l.dc, dispatchParams{
		RunToken: l.token,
		Image:    agentImage("claude-code", s.cfg.AgentImages),
		Policy:   assistantRunPolicy(l.cc),
	})
}

// assistantRunLimited reports whether err is max_concurrent_runs refusing the run.
func assistantRunLimited(err error) bool { return errors.Is(err, errRecordCeilingLimit) }

// stopAssistantRun stops the run of a session, as DELETE /runs/{id} does, unless it already ended.
func (s *Server) stopAssistantRun(ctx context.Context, runID uuid.UUID) {
	run, err := s.cfg.Store.GetRun(ctx, runID)
	if err != nil || run.State.IsTerminal() {
		return
	}
	if _, _, err := s.killRunCascade(ctx, run, types.ActorSystem, "wardynd", map[string]any{"reason": "the assistant session ended"}); err != nil {
		slog.WarnContext(ctx, "wardynd: could not stop an assistant run", slog.String("run_id", runID.String()), slog.Any("err", err))
	}
}

// assistantTokens sums the run's counted model tokens: input and output.
func (s *Server) assistantTokens(ctx context.Context, run *uuid.UUID) (in, out int64) {
	mu, ok := s.cfg.Store.(store.ModelUsageStore)
	if !ok || run == nil {
		return 0, 0
	}
	sum, err := mu.RunModelUsage(ctx, *run)
	if err != nil {
		return 0, 0
	}
	for _, g := range sum.Groups {
		in += g.Input
		out += g.Output
	}
	return in, out
}

// assistantEndAudit writes assistant.session.end: why it ended, how many turns, and the run's counted tokens.
func (s *Server) assistantEndAudit(ctx context.Context, ss store.AssistantSessionStore, sess types.AssistantSession, reason string) {
	turns, _ := ss.AssistantTurnCount(ctx, sess.ID)
	tin, tout := s.assistantTokens(ctx, sess.RunID)
	s.recordAudit(ctx, s.auditEvent(sess.RunID, types.ActorSystem, "wardynd", assistantSessionEndAction, sess.ID.String(), "success",
		mustJSON(map[string]any{"person": sess.Principal, "end_reason": reason, "turns": turns, "input_tokens": tin, "output_tokens": tout})))
}

// endAssistantSession ends sess once, writes assistant.session.end, and stops its run when stopRun is set and
// no newer live session uses it. It reports whether this call ended it.
func (s *Server) endAssistantSession(ctx context.Context, ss store.AssistantSessionStore, sess types.AssistantSession, reason string, stopRun bool) bool {
	ctx = context.WithoutCancel(ctx)
	ended, err := ss.EndAssistantSession(ctx, sess.ID, reason)
	if err != nil {
		slog.WarnContext(ctx, "wardynd: could not end an assistant session", slog.String("session", sess.ID.String()), slog.Any("err", err))
		return false
	}
	if !ended {
		return false
	}
	s.assistantEndAudit(ctx, ss, sess, reason)
	if !stopRun || sess.RunID == nil {
		return true
	}
	if live, err := ss.LiveAssistantSessionForRun(ctx, *sess.RunID); err == nil && live.ID != sess.ID {
		return true
	}
	s.stopAssistantRun(ctx, *sess.RunID)
	return true
}

// assistantRunsOf is the distinct run ids of the sessions, for a caller that stops them.
func assistantRunsOf(runs []uuid.UUID) []uuid.UUID {
	out := slices.Clone(runs)
	slices.SortFunc(out, func(a, b uuid.UUID) int { return slices.Compare(a[:], b[:]) })
	return slices.Compact(out)
}

// assistantRecordsEvent is the payload of the state event that carries the records a session began with, stored so
// the first turn sends exactly what the preview showed and nothing is rebuilt later.
func assistantRecordsEvent(prep *assistantPrep, turn string, session uuid.UUID) map[string]any {
	m := map[string]any{"state": "records", "digest": prep.digest, "records": prep.payload.Sections}
	if turn != "" {
		m["turn"], m["session_id"] = turn, session
	}
	return m
}

// assistantSuggestion is the audit row of a completed turn, stored beside the relay's turn_end as a suggestion
// event. The tool names come from the server's own tool_record events since the last turn, never from the
// relay's tool_call events. No row carries prompt or output text, only digests.
func (s *Server) assistantSuggestion(r *http.Request, ss store.AssistantSessionStore, sess types.AssistantSession, turnEnd types.AssistantEvent) {
	ctx := context.WithoutCancel(r.Context())
	evs, err := ss.ListAssistantEvents(ctx, sess.ID, 0, assistantEventsScan)
	if err != nil {
		return
	}
	var text, lastTurn, records string
	var proposal []byte
	tools := []string{}
	for _, e := range evs {
		if e.Seq >= turnEnd.Seq {
			break
		}
		var p struct {
			Text    string                   `json:"text"`
			Turn    string                   `json:"turn"`
			Name    string                   `json:"name"`
			State   string                   `json:"state"`
			Records []types.AssistantSection `json:"records"`
		}
		_ = json.Unmarshal(e.Payload, &p)
		switch e.Kind {
		case types.AssistantEventTurnEnd:
			text, proposal, tools = "", nil, []string{}
		case types.AssistantEventText:
			text += p.Text
		case types.AssistantEventProposal:
			proposal = e.Payload
		case types.AssistantEventToolRecord:
			tools = append(tools, p.Name)
		case types.AssistantEventUserMessage:
			lastTurn = p.Turn
		case types.AssistantEventState:
			if p.Records != nil {
				records = assistant.RecordBytes(p.Records)
			}
		}
	}
	promptDigest, outputDigest := assistant.PromptDigest(records+lastTurn), assistant.OutputDigest(text, proposal)
	body, _ := json.Marshal(map[string]any{"prompt_digest": promptDigest, "output_digest": outputDigest, "tools": tools})
	if _, err := ss.AppendAssistantEvent(ctx, sess.ID, types.AssistantEventSuggestion, body); err != nil {
		return
	}
	s.recordAudit(ctx, s.auditEvent(sess.RunID, types.ActorSystem, "wardynd", assistantSuggestionAction, sess.ID.String(), "success",
		mustJSON(map[string]any{"person": sess.Principal, "purpose": sess.Purpose, "provider_id": sess.Binding.ProviderID, "model": sess.Binding.Model,
			"prompt_digest": promptDigest, "output_digest": outputDigest, "tools": tools, "target_kind": sess.TargetKind, "target_key": sess.TargetKey})))
}
