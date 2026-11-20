// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestAssistantWidens is the narrowing rule: each row is one case, the stored block built by from and the
// new one by from then to.
func TestAssistantWidens(t *testing.T) {
	base := func(from func(*types.AssistantSettings)) types.AssistantSettings {
		a := types.AssistantSettings{Compose: true, Review: true, RunHistoryCounts: true, NeverSentKinds: []string{"role_mapping"},
			Providers: []types.AssistantProviderMark{assistantTestMark("a", true, true, "m1", "m2")}}
		if from != nil {
			from(&a)
		}
		return a
	}
	for _, tc := range []struct {
		name     string
		from, to func(*types.AssistantSettings)
		held     bool
	}{
		{"a purpose turned off", nil, func(a *types.AssistantSettings) { a.Review = false }, false},
		{"a purpose turned on", func(a *types.AssistantSettings) { a.Review = false }, func(a *types.AssistantSettings) { a.Review = true }, true},
		{"a mark removed", nil, func(a *types.AssistantSettings) { a.Providers = nil }, false},
		{"a mark added", nil, func(a *types.AssistantSettings) {
			a.Providers = append(a.Providers, assistantTestMark("b", false, false))
		}, true},
		{"a purpose removed from a mark", nil, func(a *types.AssistantSettings) { a.Providers[0].Review = false }, false},
		{"a purpose added to a mark", func(a *types.AssistantSettings) { a.Providers[0].Review = false },
			func(a *types.AssistantSettings) { a.Providers[0].Review = true }, true},
		{"a model removed, the list stays non-empty", nil, func(a *types.AssistantSettings) { a.Providers[0].Models = []string{"m1"} }, false},
		{"models emptied", nil, func(a *types.AssistantSettings) { a.Providers[0].Models = nil }, true},
		{"a model added", nil, func(a *types.AssistantSettings) { a.Providers[0].Models = []string{"m1", "m2", "m3"} }, true},
		{"a never-sent kind added", nil, func(a *types.AssistantSettings) { a.NeverSentKinds = []string{"role_mapping", "governance_unit"} }, false},
		{"a never-sent kind removed", nil, func(a *types.AssistantSettings) { a.NeverSentKinds = nil }, true},
		{"run-history switch turned off", nil, func(a *types.AssistantSettings) { a.RunHistoryCounts = false }, false},
		{"run-history switch turned on", func(a *types.AssistantSettings) { a.RunHistoryCounts = false },
			func(a *types.AssistantSettings) { a.RunHistoryCounts = true }, true},
		{"a narrowing row and a held row together", nil, func(a *types.AssistantSettings) { a.Compose = false; a.NeverSentKinds = nil }, true},
		{"identical", nil, func(*types.AssistantSettings) {}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before, after := base(tc.from), base(tc.from)
			tc.to(&after)
			if got := assistantWidens(before.Normalized(), after.Normalized()); got != tc.held {
				t.Fatalf("assistantWidens = %v, want %v\nbefore %+v\nafter  %+v", got, tc.held, before, after)
			}
		})
	}
}

// assistantHoldEnv is a deployment over a real database with one provider and an assistant block stored.
type assistantHoldEnv struct {
	*govEnv
	super2 *http.Cookie
}

func newAssistantHoldEnv(t *testing.T, stored *types.AssistantSettings, providers ...types.ModelProvider) *assistantHoldEnv {
	t.Helper()
	e := &assistantHoldEnv{govEnv: newGovEnvOn(t, throwawayPGPool(t))}
	e.super2 = ssoSession(t, "sub-super2", "super2@corp.example", oidc.RoleAdmin)
	if _, err := e.pg.PutSiteConfig(context.Background(), assistantSiteConfig(stored, providers...)); err != nil {
		t.Fatal(err)
	}
	return e
}

func (e *assistantHoldEnv) stored() types.SiteConfig {
	e.t.Helper()
	sc, err := e.pg.GetSiteConfig(context.Background())
	if err != nil {
		e.t.Fatal(err)
	}
	return sc
}

func (e *assistantHoldEnv) put(c *http.Cookie, body string) *httptest.ResponseRecorder {
	e.t.Helper()
	return doSSOIfMatch(e.t, e.srv, http.MethodPut, assistantSettingsPath, c, computeETag(assistantState(e.stored())), body)
}

const (
	assistantReviewOnBody  = `{"compose":true,"review":true,"run_history_counts":false,"never_sent_kinds":[],"providers":[{"provider_id":"direct","compose":true,"review":true,"models":[]}]}`
	assistantComposeOnBody = `{"compose":true,"review":false,"run_history_counts":false,"never_sent_kinds":[],"providers":[{"provider_id":"direct","compose":true,"review":false,"models":[]}]}`
)

func assistantHoldStored() *types.AssistantSettings {
	return &types.AssistantSettings{Compose: true, Providers: []types.AssistantProviderMark{assistantTestMark("direct", true, false)}}
}

// A widening PUT is held: 202 with the change id, the stored block unchanged, and only a second super admin
// writes it. A security admin cannot decide it, and neither can its proposer.
func TestAssistantSettingsWideningIsHeldUntilASecondSuperAdminApproves(t *testing.T) {
	direct := assistantTestProvider("direct", "uid-direct", types.ModelProviderAnthropicAPIKey, "m")
	e := newAssistantHoldEnv(t, assistantHoldStored(), direct)
	before := e.stored().Assistant

	ch := e.pending(e.put(e.super, assistantReviewOnBody))
	if ch.TargetKind != govKindAssistantSettings || ch.TargetKey != assistantTargetKey || ch.Op != "replace" {
		t.Fatalf("held change = %+v", ch)
	}
	if got := e.stored().Assistant; got == nil || got.Review || computeETag(*got) != computeETag(*before) {
		t.Fatalf("a held PUT changed the stored block: %+v", got)
	}
	if n := len(e.audits(assistantSettingsWriteAction)); n != 0 {
		t.Fatalf("%d assistant.settings.write rows before approval, want none", n)
	}
	// A second widening while one waits is refused as pending.
	wantReason(t, e.put(e.super, assistantReviewOnBody), http.StatusConflict, reasonGovernanceChangePending)

	if w := e.call(e.alice, http.MethodPost, approvePath(ch.ID), ""); w.Code != http.StatusForbidden || wireReason(t, w) != "admin_surface" {
		t.Fatalf("a security admin approving = %d %s, want 403 admin_surface", w.Code, w.Body)
	}
	if w := e.call(e.super, http.MethodPost, approvePath(ch.ID), ""); w.Code != http.StatusForbidden || wireReason(t, w) != "second_human_required" {
		t.Fatalf("the proposer approving = %d %s, want 403 second_human_required", w.Code, w.Body)
	}
	if e.stored().Assistant.Review {
		t.Fatal("a refused approval wrote the block")
	}

	if w := e.call(e.super2, http.MethodPost, approvePath(ch.ID), ""); w.Code != http.StatusOK {
		t.Fatalf("a second super admin approving = %d %s, want 200", w.Code, w.Body)
	}
	if got := e.stored().Assistant; got == nil || !got.Review || !got.Providers[0].Review {
		t.Fatalf("stored block after approval = %+v, want review on", got)
	}
	rows := e.audits(assistantSettingsWriteAction)
	if len(rows) != 1 || rows[0].Actor != "sub-super2" || rows[0].Target != assistantTargetKey {
		t.Fatalf("assistant.settings.write rows = %+v, want one by the approver", rows)
	}
	d := auditData(t, rows[0])
	b, _ := d["before"].(map[string]any)
	a, _ := d["after"].(map[string]any)
	if b["review"] != false || a["review"] != true || d["change_id"] == nil || d["proposed_by"] != "sub-super" {
		t.Errorf("row data = %v, want before and after, the change id and the proposer", d)
	}
}

// One narrowing row and one widening row in the same PUT is held as a whole, and nothing is written.
func TestAssistantSettingsMixedChangeIsHeldAsAWhole(t *testing.T) {
	direct := assistantTestProvider("direct", "uid-direct", types.ModelProviderAnthropicAPIKey, "m")
	stored := &types.AssistantSettings{Compose: true, NeverSentKinds: []string{"role_mapping"}, Providers: []types.AssistantProviderMark{assistantTestMark("direct", true, false)}}
	e := newAssistantHoldEnv(t, stored, direct)
	// Review on (widens) and compose off (narrows).
	e.pending(e.put(e.super, `{"compose":false,"review":true,"run_history_counts":false,"never_sent_kinds":["role_mapping"],"providers":[]}`))
	if got := e.stored().Assistant; !got.Compose || got.Review {
		t.Fatalf("stored block = %+v, want it untouched", got)
	}
}

// A change that only narrows applies at once, without a held change.
func TestAssistantSettingsNarrowingAppliesAtOnce(t *testing.T) {
	direct := assistantTestProvider("direct", "uid-direct", types.ModelProviderAnthropicAPIKey, "m")
	e := newAssistantHoldEnv(t, &types.AssistantSettings{Compose: true, Review: true, Providers: []types.AssistantProviderMark{assistantTestMark("direct", true, true)}}, direct)
	if w := e.put(e.super, assistantComposeOnBody); w.Code != http.StatusOK {
		t.Fatalf("narrowing PUT = %d %s, want 200", w.Code, w.Body)
	}
	if got := e.stored().Assistant; got == nil || got.Review || e.pendingCount() != 0 || len(e.audits(assistantSettingsWriteAction)) != 1 {
		t.Fatalf("after a narrowing PUT: block %+v, %d held, want it applied and none held", got, e.pendingCount())
	}
}

// An approval applies against the providers as they are now: one deleted since the proposal refuses it.
func TestAssistantSettingsApplyAfterTheProviderWasDeletedIsRefused(t *testing.T) {
	direct := assistantTestProvider("direct", "uid-direct", types.ModelProviderAnthropicAPIKey, "m")
	e := newAssistantHoldEnv(t, assistantHoldStored(), direct)
	ch := e.pending(e.put(e.super, assistantReviewOnBody))

	if _, err := e.pg.PutSiteConfig(context.Background(), assistantSiteConfig(e.stored().Assistant)); err != nil {
		t.Fatal(err)
	}
	w := e.call(e.super2, http.MethodPost, approvePath(ch.ID), "")
	if w.Code != http.StatusBadRequest || wireReason(t, w) != reasonAssistantSettingsInvalid {
		t.Fatalf("approving after the provider was deleted = %d %s, want 400 %s", w.Code, w.Body, reasonAssistantSettingsInvalid)
	}
	if e.stored().Assistant.Review || len(e.audits(assistantSettingsWriteAction)) != 0 || e.changeState(ch.ID) != types.GovernanceChangePending {
		t.Fatal("a refused approval wrote the block or its row, or settled the change")
	}
}

// An approval after the stored block changed since the proposal is refused as stale, and writes nothing.
func TestAssistantSettingsApplyAfterTheBlockChangedIsStale(t *testing.T) {
	direct := assistantTestProvider("direct", "uid-direct", types.ModelProviderAnthropicAPIKey, "m")
	stored := &types.AssistantSettings{Compose: true, NeverSentKinds: []string{"role_mapping"}, Providers: []types.AssistantProviderMark{assistantTestMark("direct", true, false)}}
	e := newAssistantHoldEnv(t, stored, direct)
	ch := e.pending(e.put(e.super, assistantReviewOnBody))

	// A narrowing lands first (a never-sent kind is added), which changes the block the proposal reviewed.
	if w := e.put(e.super, `{"compose":true,"review":false,"run_history_counts":false,"never_sent_kinds":["role_mapping","governance_unit"],"providers":[{"provider_id":"direct","compose":true,"review":false,"models":[]}]}`); w.Code != http.StatusOK {
		t.Fatalf("narrowing PUT = %d %s, want 200", w.Code, w.Body)
	}
	rowsBefore := len(e.audits(assistantSettingsWriteAction))
	wantReason(t, e.call(e.super2, http.MethodPost, approvePath(ch.ID), ""), http.StatusConflict, reasonGovernanceChangeStale)
	got := e.stored().Assistant
	if got.Review || !slices.Equal(got.NeverSentKinds, []string{"governance_unit", "role_mapping"}) || len(e.audits(assistantSettingsWriteAction)) != rowsBefore {
		t.Fatalf("a stale approval changed the block %+v or wrote a row", got)
	}
}
