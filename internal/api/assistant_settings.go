// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"errors"
	"net/http"
	"strings"

	"github.com/cjohnstoniv/wardyn/internal/db"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// The organisation's assistant block (site config `assistant`): which purposes are on, which model
// providers may serve them, and what is never sent. GET and PUT /assistant/settings are the super admin's
// door to it; PUT /site-config carries it forward. A PUT that only narrows applies at once; one that
// widens anything is held for a super admin to approve (assistant_settings_hold.go).

const assistantIfMatchMsg = "If-Match is required: GET /assistant/settings, then send its ETag back" // PROPOSED

// assistantState is the stored block as GET answers it, a PUT replaces it and an approval compares it: a
// missing block reads as the zero block, normalised so equal blocks hash equal.
func assistantState(sc types.SiteConfig) types.AssistantSettings {
	if sc.Assistant == nil {
		return types.AssistantSettings{}.Normalized()
	}
	return sc.Assistant.Normalized()
}

// withAssistant stores a, an all-off block with nothing listed as none, as PUT /site-config does.
func withAssistant(sc types.SiteConfig, a types.AssistantSettings) types.SiteConfig {
	sc.Assistant = nil
	if !a.IsZero() {
		n := a.Normalized()
		sc.Assistant = &n
	}
	return sc
}

// siteProviders is the configured model providers, none when the block is absent.
func siteProviders(sc types.SiteConfig) []types.ModelProvider {
	if sc.ModelProviders == nil {
		return nil
	}
	return sc.ModelProviders.Providers
}

// governanceKindKnown is the validator's view of which record kinds a never-sent entry may name: the kinds
// of governance change.
func governanceKindKnown(kind string) bool {
	_, ok := governanceChangeKinds[kind]
	return ok
}

// assistantMarkServable reports whether p can serve the assistant now: it pairs with the claude-code
// harness, is not a Bedrock kind and does not leave prompt inspection to its gateway.
func assistantMarkServable(p types.ModelProvider) bool {
	return p.Serves(types.AssistantHarness) && !p.Kind.IsBedrock() && !inspectionDeferredFor(p)
}

// assistantSettingsAnswer is the block as GET and PUT answer it: a mark whose provider was deleted, can no
// longer serve or now defers inspection is left out, so the console never shows a mark that does nothing.
func assistantSettingsAnswer(sc types.SiteConfig) types.AssistantSettings {
	a := assistantState(sc)
	marks := make([]types.AssistantProviderMark, 0, len(a.Providers))
	for _, m := range a.Providers {
		if p, ok := modelProviderByID(sc.ModelProviders, m.ProviderID); ok && assistantMarkServable(p) {
			marks = append(marks, m)
		}
	}
	a.Providers = marks
	return a
}

// writeAssistantRefusal answers a refused block under its own reason.
func writeAssistantRefusal(w http.ResponseWriter, err error) {
	var ae *types.AssistantSettingsError
	if errors.As(err, &ae) {
		writeErrorReason(w, http.StatusBadRequest, ae.Reason, ae.Msg)
		return
	}
	writeErrorReason(w, http.StatusBadRequest, reasonAssistantSettingsInvalid, err.Error())
}

// handleGetAssistantSettings is GET /api/v1/assistant/settings. Super admin. The ETag hashes the stored
// block, which the answer may leave marks out of, so a PUT of the answer is judged against what is stored.
func (s *Server) handleGetAssistantSettings(w http.ResponseWriter, r *http.Request) {
	sc, err := s.cfg.Store.GetSiteConfig(r.Context())
	if err != nil {
		writeServerError(w, r, "get site config", err)
		return
	}
	w.Header().Set("ETag", computeETag(assistantState(sc)))
	writeJSON(w, http.StatusOK, assistantSettingsAnswer(sc))
}

// handlePutAssistantSettings is PUT /api/v1/assistant/settings: the whole block, replaced. Super admin,
// If-Match required. A change that only narrows is written and answered 200; one that widens is held and
// answered 202 with the pending change; an identical block is a 200 and no audit row.
func (s *Server) handlePutAssistantSettings(w http.ResponseWriter, r *http.Request) {
	var body types.AssistantSettings
	if !decodeStrict(w, r, &body) {
		return
	}
	if strings.TrimSpace(r.Header.Get("If-Match")) == "" {
		writeErrorReason(w, http.StatusPreconditionRequired, reasonAssistantSettingsInvalid, assistantIfMatchMsg)
		return
	}
	after := body.Normalized()
	before, held := s.writeAssistantNarrowing(w, r, after)
	if !held {
		return
	}
	s.proposeGovernanceChange(w, r, govProposal{
		kind: govKindAssistantSettings, op: "replace", key: assistantTargetKey, payload: after,
		before: assistantViewOf(before), after: assistantViewOf(after),
		changed: changedPaths(assistantViewOf(before), assistantViewOf(after)), baseState: before,
	})
}

// writeAssistantNarrowing judges after against the stored block under the site-config lock. It answers the
// request itself unless the change widens, when it returns held with the block the proposal reviewed and
// answers nothing: the caller proposes it, with the lock released.
func (s *Server) writeAssistantNarrowing(w http.ResponseWriter, r *http.Request, after types.AssistantSettings) (before types.AssistantSettings, held bool) {
	r, unlock, ok := s.lockDoor(w, r, db.SiteConfigLockClass)
	if !ok {
		return before, false
	}
	defer unlock()
	sc, err := s.cfg.Store.GetSiteConfig(r.Context())
	if err != nil {
		writeServerError(w, r, "get site config", err)
		return before, false
	}
	before = assistantState(sc)
	if !ifMatchSatisfied(r, computeETag(before)) {
		writeErrorReason(w, http.StatusPreconditionFailed, reasonAssistantSettingsInvalid, assistantStaleMsg)
		return before, false
	}
	if computeETag(before) != computeETag(after) {
		if err := after.Validate(siteProviders(sc), governanceKindKnown, inspectionDeferredFor); err != nil {
			writeAssistantRefusal(w, err)
			return before, false
		}
		if assistantWidens(before, after) {
			return before, true
		}
		if _, err := s.cfg.Store.PutSiteConfig(r.Context(), withAssistant(sc, after)); err != nil {
			writeServerError(w, r, "put site config", err)
			return before, false
		}
		s.recordAudit(r.Context(), s.auditEvent(nil, actorTypeFromRequest(r), principalFromRequest(r),
			assistantSettingsWriteAction, assistantTargetKey, "success", mustJSON(assistantAuditData(before, after))))
		sc = withAssistant(sc, after)
	}
	w.Header().Set("ETag", computeETag(assistantState(sc)))
	writeJSON(w, http.StatusOK, assistantSettingsAnswer(sc))
	return before, false
}

const assistantStaleMsg = "If-Match does not match the current assistant settings — GET /assistant/settings again and retry" // PROPOSED

// carryAssistant keeps the stored assistant block on a generic site-config write. The block has its own
// audited door, so a document naming a different one is refused rather than quietly ignored; an echo of
// the stored block, or no mention at all, carries it forward. False: answered.
func carryAssistant(w http.ResponseWriter, cfg *types.SiteConfig, existing types.SiteConfig) bool {
	if cfg.Assistant != nil && computeETag(assistantState(*cfg)) != computeETag(assistantState(existing)) {
		writeErrorReason(w, http.StatusBadRequest, reasonAssistantSettingsViaOwnRoute,
			"the assistant settings are a super-admin switch: change them with PUT /assistant/settings, not PUT /site-config")
		return false
	}
	cfg.Assistant = existing.Assistant
	return true
}
