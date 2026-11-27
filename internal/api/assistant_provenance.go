// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// assistantSessionHeader is the header the console sends on a save that came from an accepted assistant draft.
const assistantSessionHeader = "X-Wardyn-Assistant-Session"

// assistantDraftWindow is how long after a session ended a save is still taken as drafted in it.
const assistantDraftWindow = 2 * time.Hour

// How a save was found to come from an assistant draft: the verified header, or the server's own match.
const (
	draftedByHeader  = "header"
	draftedBySession = "session"
)

// The key a save of a new run or template carries: its session has no target id yet.
const provenanceNewKey = "new"

// assistantDraftFinder is the store's read for the server's own match. It is optional like the session store:
// a store without it never marks a save the header did not name.
type assistantDraftFinder interface {
	DraftingAssistantSession(ctx context.Context, principal, kind, key string, endedAfter time.Time) (uuid.UUID, error)
}

// provenanceTarget is the session target a save is drafted from: a new run or template has no id, an updated
// template or a unit has one. Only a run, a template create or update and a unit create or update can be
// drafted in a session; every other save, a unit delete included, is never marked. kind is a save site's kind:
// run, template or a governance target kind.
func provenanceTarget(kind, op, key string) (sessionKind, sessionKey string, ok bool) {
	switch kind {
	case types.AssistantTargetRun:
		return types.AssistantTargetRun, "", key == provenanceNewKey
	case types.AssistantTargetTemplate:
		switch {
		case op == "create" && key == provenanceNewKey:
			return types.AssistantTargetTemplate, "", true
		case op == "update" && key != "" && key != provenanceNewKey:
			return types.AssistantTargetTemplate, key, true
		}
	case types.GovernanceTargetUnit:
		if (op == "create" || op == "update") && key != "" {
			return types.AssistantTargetUnit, key, true
		}
	}
	return "", "", false
}

// provenanceMatches reports whether a session on sessionKind and sessionKey is the one a save of kind, op and
// key is drafted from. It is a closed mapping: a kind not named in provenanceTarget matches no session.
func provenanceMatches(sessionKind, sessionKey, kind, op, key string) bool {
	k, id, ok := provenanceTarget(kind, op, key)
	return ok && sessionKind == k && (sessionKey == id || (id == "" && sessionKey == provenanceNewKey))
}

// assistantProvenance is the id of the assistant session a save was drafted in, how the server found it
// (draftedByHeader or draftedBySession), and whether it did. The mark is informational: it is never an error
// and changes nothing about the save, so a stale or foreign header is ignored. A header names a session only
// when it is the caller's own compose session on the save's target. With no usable header the server looks for
// the caller's compose session on that target that is live, or ended less than two hours ago, and in which the
// person accepted or edited a proposal.
func (s *Server) assistantProvenance(r *http.Request, kind, op, key string) (uuid.UUID, string, bool) {
	ctx := r.Context()
	person := oidcHumanFromContext(ctx)
	sessionKind, sessionKey, ok := provenanceTarget(kind, op, key)
	if person == "" || !ok {
		return uuid.Nil, "", false
	}
	if ss, ok := s.cfg.Store.(store.AssistantSessionStore); ok {
		if id, err := uuid.Parse(strings.TrimSpace(r.Header.Get(assistantSessionHeader))); err == nil {
			sess, err := ss.GetAssistantSession(ctx, id, person)
			if err == nil && sess.Principal == person && sess.Purpose == types.AssistantCompose &&
				provenanceMatches(sess.TargetKind, sess.TargetKey, kind, op, key) {
				return sess.ID, draftedByHeader, true
			}
		}
	}
	finder, ok := s.cfg.Store.(assistantDraftFinder)
	if !ok {
		return uuid.Nil, "", false
	}
	id, err := finder.DraftingAssistantSession(ctx, person, sessionKind, sessionKey, s.cfg.Now().Add(-assistantDraftWindow))
	if err != nil {
		if !errors.Is(err, store.ErrNotFound) {
			slog.WarnContext(ctx, "api: could not look for the assistant session a save was drafted in", "err", err)
		}
		return uuid.Nil, "", false
	}
	return id, draftedBySession, true
}

// stampAssistantProvenance adds the three keys of a draft mark to a save's audit data when the save came from an
// assistant draft, and says whether it did.
func (s *Server) stampAssistantProvenance(r *http.Request, data map[string]any, kind, op, key string) bool {
	id, by, ok := s.assistantProvenance(r, kind, op, key)
	if ok {
		stampDrafted(data, id, by)
	}
	return ok
}

// stampDrafted adds drafted_with_ai, assistant_session and detected_by to an audit row's data.
func stampDrafted(data map[string]any, id uuid.UUID, by string) {
	data["drafted_with_ai"], data["assistant_session"], data["detected_by"] = true, id.String(), by
}
