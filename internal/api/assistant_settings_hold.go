// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"fmt"
	"net/http"
	"slices"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// govKindAssistantSettings is the held change of the assistant block. Its target is a singleton, keyed
// assistantTargetKey, as the egress baseline's is.
const govKindAssistantSettings = "assistant_settings"

// assistantLocalModeMsg answers a widening in local mode, where no second super admin can decide it.
const assistantLocalModeMsg = "This change would be held for a second super admin, and local mode authenticates nobody, so no one could decide it. Configure SSO to widen the assistant settings." // PROPOSED

// assistantHoldDecidable reports whether a held widening could ever be decided, answering 503 itself when
// not. The PUT asks it before holding one and decideGovernanceChange before approving one, so a change
// proposed before the server went to local mode is refused too; a reject is not, as it applies nothing. A widening is held whatever WARDYN_GOVERNANCE_SECOND_HUMAN says, so unlike governanceWriteMode this
// refuses in local mode with the switch off too. The admin token stays the way past it, as there.
func (s *Server) assistantHoldDecidable(w http.ResponseWriter, r *http.Request) bool {
	if s.cfg.LocalMode && !isAdminTokenCaller(r) {
		writeErrorReason(w, http.StatusServiceUnavailable, reasonGovernanceSecondHumanLocalMode, assistantLocalModeMsg)
		return false
	}
	return true
}

// assistantWidens reports whether after lets the assistant do anything before did not: a purpose or the
// run-history switch turned on, a never-sent kind removed, a provider marked, a purpose or a model added to
// a mark, or a model list emptied (which falls back to the harness model). A change that only turns
// things off, removes marks, models or purposes, or adds never-sent kinds applies at once.
func assistantWidens(before, after types.AssistantSettings) bool {
	if (!before.Compose && after.Compose) || (!before.Review && after.Review) || (!before.RunHistoryCounts && after.RunHistoryCounts) {
		return true
	}
	if slices.ContainsFunc(before.NeverSentKinds, func(k string) bool { return !slices.Contains(after.NeverSentKinds, k) }) {
		return true
	}
	for _, m := range after.Providers {
		i := slices.IndexFunc(before.Providers, func(b types.AssistantProviderMark) bool { return b.ProviderID == m.ProviderID })
		if i < 0 {
			return true
		}
		was := before.Providers[i]
		if (!was.Compose && m.Compose) || (!was.Review && m.Review) {
			return true
		}
		if len(m.Models) == 0 && len(was.Models) > 0 {
			return true
		}
		if slices.ContainsFunc(m.Models, func(x string) bool { return !slices.Contains(was.Models, x) }) {
			return true
		}
	}
	return false
}

// applyAssistantSettingsChange applies a held replacement inside the decision transaction: the target
// lock, then the site-config lock every site-config writer holds, the block compared with what the proposal
// reviewed, the providers checked again as they are now, then the whole document written back on q. The row
// is the gate's, written after the commit.
func applyAssistantSettingsChange(s *Server, r *http.Request, q store.Querier, ch types.GovernanceChange) (govApplied, error) {
	ctx := r.Context()
	var after types.AssistantSettings
	if err := decodeHeldPayload(ch.Payload, &after); err != nil {
		return govApplied{}, fmt.Errorf("governance change %s: payload: %w", ch.ID, err)
	}
	if err := store.LockGovernanceTarget(ctx, q, govKindAssistantSettings); err != nil {
		return govApplied{}, err
	}
	if err := store.LockSiteConfig(ctx, q); err != nil {
		return govApplied{}, err
	}
	sc, err := store.GetSiteConfigQ(ctx, q)
	if err != nil {
		return govApplied{}, err
	}
	before := assistantState(sc)
	if computeETag(before) != ch.BaseHash {
		return govApplied{}, store.ErrGovernanceChangeStale
	}
	// The record kinds were checked when the change was proposed; the table of kinds cannot be read from
	// here, since it holds this function.
	if err := after.Validate(siteProviders(sc), func(string) bool { return true }, inspectionDeferredFor); err != nil {
		return govApplied{}, writeRefusal(http.StatusBadRequest, reasonAssistantSettingsInvalid, "invalid assistant settings: %s", err.Error())
	}
	if _, err := store.PutSiteConfigQ(ctx, q, withAssistant(sc, after)); err != nil {
		return govApplied{}, err
	}
	return govApplied{action: assistantSettingsWriteAction, target: assistantTargetKey, data: assistantAuditData(before, after)}, nil
}
