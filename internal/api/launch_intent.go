// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"maps"
	"net/http"
	"slices"
	"strconv"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/pkg/client"
)

// Launch intents: the setup a person launched a run with, kept as a kind Run
// document so Run again can open it at Review. POST /runs builds it from the
// request as asked and the store writes it in the run row's own transaction;
// only the run's creator reads it. Preflight and the preview never write one.

// askedRequestKey carries a sink for the create request as asked: after its
// preset expands, before applyRunMode projects the carriers (decodeRunRequest).
// Only POST /runs sets one.
type askedRequestKey struct{}

func withAskedRequest(r *http.Request, sink *[]byte) *http.Request {
	return r.WithContext(context.WithValue(r.Context(), askedRequestKey{}, sink))
}

func recordAskedRequest(r *http.Request, req createRunRequest) {
	if sink, ok := r.Context().Value(askedRequestKey{}).(*[]byte); ok {
		*sink, _ = json.Marshal(req) //nolint:errcheck // a request that just decoded marshals
	}
}

// Why a run kept no launch intent: run.create's launch_intent_reason.
const (
	launchIntentWholeInlinePolicy = "whole_inline_policy" // a whole inline_policy replaces the default; a document cannot say that
	launchIntentDocumentRefused   = "document_refused"    // the one door refused the converted document
	launchIntentErasureInProgress = "erasure_in_progress" // an erasure of the creator's intents held the person lock
)

// launchIntentDropped reports a request field a Run document never holds: one
// the template field registry excludes as chosen at each launch (title,
// description, max_wait_sec) or as where the request came from (preset stamps,
// mode_contract). policy_overlay, also an origin field, is moved first.
func launchIntentDropped(field string) bool {
	r, ok := templateRule(templateRequestRules, field)
	return ok && (r.Excluded == templateExcludeLaunch || r.Excluded == templateExcludeOrigin)
}

// launchIntent is the document a create will keep, or why it keeps none.
type launchIntent struct {
	doc      json.RawMessage
	reason   string
	refusals []string // the door's reasons, never a path or a value
}

// launchIntentFor converts the request as asked into a kind Run document and
// holds it to the template door. A preset has already expanded into the
// request; policy_overlay becomes intent.inline_policy.
func launchIntentFor(asked []byte) launchIntent {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(asked, &fields); err != nil {
		return launchIntent{reason: launchIntentDocumentRefused}
	}
	if _, ok := fields["inline_policy"]; ok {
		return launchIntent{reason: launchIntentWholeInlinePolicy}
	}
	if overlay, ok := fields["policy_overlay"]; ok {
		fields["inline_policy"] = overlay
		delete(fields, "policy_overlay")
	}
	maps.DeleteFunc(fields, func(k string, v json.RawMessage) bool {
		// agent and repo are the two request fields written even when empty.
		return launchIntentDropped(k) || (k == "agent" || k == "repo") && string(v) == `""`
	})
	body, err := json.Marshal(map[string]any{"api_version": client.TemplateDocumentVersion, "kind": client.RunDocumentKind, "intent": fields})
	if err != nil {
		return launchIntent{reason: launchIntentDocumentRefused}
	}
	res := validateTemplateImport(body, client.TemplateFormatJSON, client.RunDocumentKind)
	if res.Document == nil {
		var reasons []string
		for _, d := range res.Diagnostics {
			if !slices.Contains(reasons, d.Reason) {
				reasons = append(reasons, d.Reason)
			}
		}
		slices.Sort(reasons)
		return launchIntent{reason: launchIntentDocumentRefused, refusals: reasons}
	}
	doc, err := json.Marshal(res.Document)
	if err != nil {
		return launchIntent{reason: launchIntentDocumentRefused}
	}
	return launchIntent{doc: doc}
}

// stamp records on run.create whether the created run kept its intent, and why not.
func (li launchIntent) stamp(data map[string]any, kept bool) {
	if kept {
		data["launch_intent"] = "kept"
		return
	}
	data["launch_intent"] = "not_kept"
	data["launch_intent_reason"] = li.reason
	if li.reason == "" {
		data["launch_intent_reason"] = launchIntentErasureInProgress
	}
	if len(li.refusals) > 0 {
		data["launch_intent_refusals"] = li.refusals
	}
}

// The wire shapes; ui/src/app/lib/types/launch-intents.ts mirrors them.
type launchIntentSummary struct {
	RunID     uuid.UUID       `json:"run_id"`
	Title     string          `json:"title"`
	Shape     client.RunShape `json:"shape,omitempty"`
	CreatedAt time.Time       `json:"created_at"`
}

type launchIntentList struct {
	Intents []launchIntentSummary `json:"intents"`
}

type launchIntentView struct {
	RunID     uuid.UUID               `json:"run_id"`
	CreatedAt time.Time               `json:"created_at"`
	Document  client.TemplateDocument `json:"document"`
}

const (
	launchIntentListDefault = 20
	launchIntentListMax     = 50
)

// GET /me/launch-intents?limit=: the caller's own kept setups, newest first.
func (s *Server) handleListLaunchIntents(w http.ResponseWriter, r *http.Request) {
	limit := launchIntentListDefault
	if v := r.URL.Query().Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			writeErrorReason(w, http.StatusBadRequest, reasonInvalidLimitParam, "invalid limit")
			return
		}
		limit = min(cmp.Or(n, launchIntentListDefault), launchIntentListMax)
	}
	out := launchIntentList{Intents: []launchIntentSummary{}}
	ls, ok := s.cfg.Store.(store.LaunchIntentStore)
	if !ok {
		writeJSON(w, http.StatusOK, out)
		return
	}
	recs, err := ls.ListLaunchIntents(r.Context(), principalFromRequest(r), limit)
	if err != nil {
		writeServerError(w, r, "list launch intents", err)
		return
	}
	for _, rec := range recs {
		out.Intents = append(out.Intents, launchIntentSummary{RunID: rec.RunID, Title: rec.Title, Shape: client.RunShape(rec.Shape), CreatedAt: rec.CreatedAt})
	}
	writeJSON(w, http.StatusOK, out)
}

// GET /runs/{id}/launch-intent: the run's creator only. Anyone else, an
// administrator included, gets the 404 a missing run gets; the creator of a run
// that kept none gets launch_intent_not_found.
func (s *Server) handleGetLaunchIntent(w http.ResponseWriter, r *http.Request) {
	id, ok := parseIDParam(w, r, "id", "run")
	if !ok {
		return
	}
	run, ok := s.getRunAuthorizedBy(w, r, id, s.ownsRun)
	if !ok {
		return
	}
	ls, ok := s.cfg.Store.(store.LaunchIntentStore)
	if !ok {
		writeErrorReason(w, http.StatusNotFound, reasonLaunchIntentNotFound, launchIntentNotFoundMsg)
		return
	}
	rec, err := ls.GetLaunchIntent(r.Context(), id, run.CreatedBy)
	if errors.Is(err, store.ErrNotFound) {
		writeErrorReason(w, http.StatusNotFound, reasonLaunchIntentNotFound, launchIntentNotFoundMsg)
		return
	}
	if err != nil {
		writeServerError(w, r, "get launch intent", err)
		return
	}
	var doc client.TemplateDocument
	if err := json.Unmarshal(redactRepoURLsInJSON(rec.Document), &doc); err != nil {
		writeServerError(w, r, "read launch intent", err)
		return
	}
	writeJSON(w, http.StatusOK, launchIntentView{RunID: rec.RunID, CreatedAt: rec.CreatedAt, Document: doc})
}

const launchIntentNotFoundMsg = "This run's setup was not kept, or you cannot see it."
