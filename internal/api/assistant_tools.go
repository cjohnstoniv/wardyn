// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/cjohnstoniv/wardyn/internal/assistant"
	"github.com/cjohnstoniv/wardyn/internal/audit"
	"github.com/cjohnstoniv/wardyn/internal/authz"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// assistantToolArgsMax is the largest tool call body: the proxy caps the call at the same 256 KiB.
const assistantToolArgsMax = 256 << 10

// assistantToolResultMax is the largest tool result sent back, under the scanner span the run's inspection
// reads; a larger one is cut and says so.
const assistantToolResultMax = 64 << 10

// assistantToolLimiter is the tools' own bucket, per person: thirty at once, one a second after. The person
// routes /assistant/checks and /assistant/replay spend from it too.
// ponytail: process-wide, keyed by person, as assistantMessageLimiter.
var assistantToolLimiter = &principalLimiter{rate: 1, burst: 30, max: preflightLimiterMaxPeople}

// assistantToolRateRefused spends one of person's tool tokens and answers 429 when none is left.
func (s *Server) assistantToolRateRefused(w http.ResponseWriter, r *http.Request, person string) bool {
	if person == "" || assistantToolLimiter.allow(person, s.cfg.Now()) {
		return false
	}
	assistantToolRateLimited().write(w, r)
	return true
}

// assistantToolRateLimited is the 429 of an empty tool bucket.
func assistantToolRateLimited() *assistantRefusal {
	return &assistantRefusal{custom: func(w http.ResponseWriter, _ *http.Request) bool {
		w.Header().Set("Retry-After", "1")
		writeErrorReason(w, http.StatusTooManyRequests, reasonAssistantRateLimited, "Too many checks. Wait a moment.") // PROPOSED
		return true
	}}
}

// assistantToolEnv is one tool call: the session, the request on the person's capped tool context, the
// session's target as that person may read it now, and the call's arguments.
type assistantToolEnv struct {
	r    *http.Request
	ss   store.AssistantSessionStore
	sess types.AssistantSession
	snap assistantSnapshot
	sc   types.SiteConfig
	rt   assistantTarget
	args json.RawMessage
}

func (e *assistantToolEnv) ctx() context.Context { return e.r.Context() }

// assistantToolFunc runs one tool and returns its result, or the refusal the call answers.
type assistantToolFunc func(s *Server, e *assistantToolEnv) (any, *assistantRefusal)

// assistantToolRegistry is every tool by name. The tool list, the dispatcher and the no-write test all read it.
func assistantToolRegistry() map[string]assistantToolFunc {
	return map[string]assistantToolFunc{
		types.AssistantToolRead:           (*Server).assistantToolRead,
		types.AssistantToolValidate:       (*Server).assistantToolValidate,
		types.AssistantToolDiff:           (*Server).assistantToolDiff,
		types.AssistantToolNarrowingCheck: (*Server).assistantToolExplain,
		types.AssistantToolGrade:          (*Server).assistantToolGrade,
		types.AssistantToolImpact:         (*Server).assistantToolImpact,
		types.AssistantToolSimulate:       (*Server).assistantToolSimulate,
		types.AssistantToolExplain:        (*Server).assistantToolExplain,
		types.AssistantToolDraft:          (*Server).assistantToolDraft,
	}
}

// assistantToolRefusedInReview reports a tool a review session is never offered: it proposes a draft.
func assistantToolRefusedInReview(purpose types.AssistantPurpose, name string) bool {
	return purpose == types.AssistantReview && name == types.AssistantToolDraft
}

// assistantToolList is the tools a purpose is offered, each with its schema. It is never empty.
func assistantToolList(purpose types.AssistantPurpose) []types.AssistantToolSpec {
	return slices.DeleteFunc(types.AssistantToolSpecs(), func(t types.AssistantToolSpec) bool {
		return assistantToolRefusedInReview(purpose, t.Name)
	})
}

// handleAssistantToolList is GET /internal/assistant/tools: the session the run's token speaks for, found by
// the token's run id as the turn poll finds it, and the tools its purpose is offered. A run with no session is 404.
func (s *Server) handleAssistantToolList(w http.ResponseWriter, r *http.Request) {
	_, sess, ok := s.assistantRunSession(w, r)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, assistantToolList(sess.Purpose))
}

// handleAssistantToolCall is POST /internal/assistant/tools/{name}. In order: the run's session by its token's
// run id (404 when none); the call names that live session (409 otherwise); a known tool (404), one the
// purpose is offered (403) and the tool bucket (429); the person's capped context (403 when sign-in would now
// refuse them, ending the session); the session still available (409, ending the session); the target read again
// as that person. The tool then runs under a dry run, its strings pass the session's letters, and the result is
// cut at 64 KiB. Every call that names the live session leaves one tool_record event, refused or not.
func (s *Server) handleAssistantToolCall(w http.ResponseWriter, r *http.Request) {
	ss, sess, ok := s.assistantRunSession(w, r)
	if !ok {
		return
	}
	body, ok := readCappedBody(w, r, assistantToolArgsMax, "tool call")
	if !ok {
		return
	}
	var call types.AssistantToolCall
	if err := decodeStrictJSON(body, &call); err != nil {
		writeErrorReason(w, http.StatusBadRequest, reasonInvalidRequestBody, "The tool call is not valid.") // PROPOSED
		return
	}
	if !sess.Live() || call.SessionID != sess.ID {
		writeErrorReason(w, http.StatusConflict, reasonAssistantSessionNotFound, "That session is not the live one.") // PROPOSED
		return
	}
	name := chi.URLParam(r, "name")
	result, ref := s.assistantRunTool(r, ss, sess, name, call.Args)
	if types.AssistantToolNameValid(name) {
		s.assistantToolRecord(r.Context(), ss, sess, name, ref == nil)
	}
	if ref != nil {
		ref.write(w, r)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

// assistantRunTool is the call's checks after the session match, then the tool. It returns the encoded result.
func (s *Server) assistantRunTool(r *http.Request, ss store.AssistantSessionStore, sess types.AssistantSession, name string, args json.RawMessage) (json.RawMessage, *assistantRefusal) {
	fn, known := assistantToolRegistry()[name]
	if !known {
		return nil, assistantRefusalFor(http.StatusNotFound, reasonAssistantToolUnknown, "There is no such tool.") // PROPOSED
	}
	if assistantToolRefusedInReview(sess.Purpose, name) {
		return nil, assistantRefusalFor(http.StatusForbidden, reasonAssistantToolRefused, "A review session cannot propose a draft.") // PROPOSED
	}
	if !assistantToolLimiter.allow(sess.Principal, s.cfg.Now()) {
		return nil, assistantToolRateLimited()
	}
	snap := assistantSnapshotOfSession(sess)
	tctx, release, err := s.assistantToolCtx(r, snap)
	if err != nil {
		if errors.Is(err, errAssistantRoleDenied) {
			s.endAssistantSession(r.Context(), ss, sess, types.AssistantEndRoleDenied, true)
		}
		return nil, s.assistantToolCtxRefusal(err)
	}
	defer release()
	tctx = audit.WithDryRun(tctx)
	if ref := s.assistantStillAvailable(tctx, ss, sess); ref != nil {
		return nil, ref
	}
	tr := r.WithContext(tctx)
	rt, ref := s.assistantResolveTarget(tr, types.AssistantTarget{Kind: sess.TargetKind, ID: sess.TargetKey}, snap)
	if ref != nil {
		return nil, ref
	}
	if rt.Kind == types.AssistantTargetUnit && rt.unit == nil {
		if ref := s.assistantUnitTier(tctx); ref != nil {
			return nil, ref
		}
	}
	sc, err := s.cfg.Store.GetSiteConfig(tctx)
	if err != nil {
		return nil, assistantRefusalFor(http.StatusInternalServerError, "", "Could not read the settings.") // PROPOSED
	}
	if len(args) == 0 {
		args = json.RawMessage("{}")
	}
	out, ref := fn(s, &assistantToolEnv{r: tr, ss: ss, sess: sess, snap: snap, sc: sc, rt: rt, args: args})
	if ref != nil {
		return nil, ref
	}
	return assistantToolResult(out, assistant.LettersFromMap(sess.PersonLetters))
}

// assistantToolResult is v encoded with every string passed through letters, cut to assistantToolResultMax.
func assistantToolResult(v any, letters *assistant.Letters) (json.RawMessage, *assistantRefusal) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, assistantRefusalFor(http.StatusInternalServerError, "", "Could not encode the result.") // PROPOSED
	}
	var tree any
	if err := json.Unmarshal(raw, &tree); err != nil {
		return nil, assistantRefusalFor(http.StatusInternalServerError, "", "Could not encode the result.") // PROPOSED
	}
	raw, err = json.Marshal(assistantLettered(tree, letters))
	if err != nil {
		return nil, assistantRefusalFor(http.StatusInternalServerError, "", "Could not encode the result.") // PROPOSED
	}
	return assistantCapped(raw), nil
}

// assistantLettered is a decoded JSON value with every string value replaced through letters. Keys are the
// schema's own names and are left alone.
func assistantLettered(v any, letters *assistant.Letters) any {
	switch x := v.(type) {
	case string:
		return letters.Apply(x)
	case []any:
		for i := range x {
			x[i] = assistantLettered(x[i], letters)
		}
		return x
	case map[string]any:
		for k := range x {
			x[k] = assistantLettered(x[k], letters)
		}
		return x
	}
	return v
}

// assistantCapped is raw when it fits in assistantToolResultMax, else the leading part of it as text, marked
// truncated. The cut text is never parsed again, so a cut inside a string or a number is harmless.
func assistantCapped(raw json.RawMessage) json.RawMessage {
	if len(raw) <= assistantToolResultMax {
		return raw
	}
	keep := raw[:assistantToolResultMax-256]
	for len(keep) > 0 && keep[len(keep)-1]&0xC0 == 0x80 {
		keep = keep[:len(keep)-1] // never cut inside a UTF-8 sequence
	}
	if len(keep) > 0 && keep[len(keep)-1] >= 0xC0 {
		keep = keep[:len(keep)-1]
	}
	out, _ := json.Marshal(map[string]any{"truncated": true, "partial": string(keep)})
	return out
}

// assistantUnitTier is the unit tier POST /governance/units sits behind (requireUnitScope), asked of a session
// on a new unit before any tool composes it: a security caller passes, a caller holding a unit admin grant
// passes (the parent is checked against their scope when the draft is built), and anyone else is refused.
func (s *Server) assistantUnitTier(ctx context.Context) *assistantRefusal {
	if s.isSecurityOperator(ctx) {
		return nil
	}
	grants, err := s.callerUnitGrants(ctx)
	if err != nil {
		return assistantRefusalFor(http.StatusInternalServerError, "", "Could not read your unit admin grants.") // PROPOSED
	}
	if len(grants) == 0 {
		return assistantRefusalFor(http.StatusForbidden, string(authz.ReasonUnitScopeDenied), "A new unit needs a unit admin or a security admin.") // PROPOSED
	}
	return nil
}

// assistantToolRecord appends the call's tool_record event: the one list the turn's suggestion row names.
func (s *Server) assistantToolRecord(ctx context.Context, ss store.AssistantSessionStore, sess types.AssistantSession, name string, ok bool) {
	payload, _ := json.Marshal(map[string]any{"name": name, "ok": ok, "dry_run": true, "at": s.cfg.Now().UTC().Format(time.RFC3339Nano)})
	if _, err := ss.AppendAssistantEvent(ctx, sess.ID, types.AssistantEventToolRecord, payload); err == nil {
		s.assistantChanged(sess.ID, sess.RunID)
	}
}

// assistantDraftArgsOf reads a check tool's arguments: the target kind must be the session's own.
func assistantDraftArgsOf(e *assistantToolEnv) (types.AssistantDraftArgs, *assistantRefusal) {
	var a types.AssistantDraftArgs
	if err := decodeStrictJSON(e.args, &a); err != nil || a.TargetKind != e.sess.TargetKind {
		return a, assistantRefusalFor(http.StatusBadRequest, reasonInvalidRequestBody, "The arguments do not fit this tool or this session's target.") // PROPOSED
	}
	return a, nil
}
