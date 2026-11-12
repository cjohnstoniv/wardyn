// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// GET /governance/units/{id}/model-usage: the token usage of the runs frozen to a unit and the units below
// it, over a window bounded by the run's creation time.

// seedUsage stores one counted call for run.
func (e *unitAuditEnv) seedUsage(run uuid.UUID, host, model string, in, out int64) {
	e.t.Helper()
	u := egress.ModelUsage{CallID: uuid.New(), Vendor: "anthropic", Host: host, Model: model, Input: in, Output: out, State: "counted"}
	if _, err := e.pg.InsertModelUsage(context.Background(), run, u, time.Now(), ""); err != nil {
		e.t.Fatalf("seed usage: %v", err)
	}
}

// seedRunAged is seedRun with its creation time moved back by age.
func (e *unitAuditEnv) seedRunAged(unit *uuid.UUID, age time.Duration) uuid.UUID {
	e.t.Helper()
	id := e.seedRun(unit, uaRunOwner)
	if _, err := e.pool.Exec(context.Background(), `UPDATE agent_runs SET created_at = $2 WHERE id = $1`, id, time.Now().Add(-age)); err != nil {
		e.t.Fatalf("age the run: %v", err)
	}
	return id
}

func (e *unitAuditEnv) usage(c *http.Cookie, unit uuid.UUID, query string) *httptest.ResponseRecorder {
	e.t.Helper()
	return e.call(c, http.MethodGet, "/api/v1/governance/units/"+unit.String()+"/model-usage"+query, "")
}

func (e *unitAuditEnv) usageOK(c *http.Cookie, unit uuid.UUID, query string) UnitModelUsageResponse {
	e.t.Helper()
	w := e.usage(c, unit, query)
	var out UnitModelUsageResponse
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &out) != nil {
		e.t.Fatalf("model-usage %s: %d %s", query, w.Code, w.Body.String())
	}
	return out
}

func usageUnit(t *testing.T, r UnitModelUsageResponse, id uuid.UUID) (UnitModelUsageView, bool) {
	t.Helper()
	for _, u := range r.Units {
		if u.UnitID == id {
			return u, true
		}
	}
	return UnitModelUsageView{}, false
}

func TestUnitModelUsageSubtreeOnly(t *testing.T) {
	e := newUnitAuditEnv(t)
	e.seedUsage(e.runA, "api.anthropic.com", "claude-x", 10, 5)
	e.seedUsage(e.runA1, "api.anthropic.com", "claude-x", 20, 10)
	e.seedUsage(e.runA1, "api.anthropic.com", "claude-y", 1, 1)
	e.seedUsage(e.runB, "api.anthropic.com", "claude-x", 1000, 1000)
	e.seedUsage(e.runNone, "api.anthropic.com", "claude-x", 2000, 2000)
	got := e.usageOK(e.adminA, e.a.ID, "")
	if len(got.Units) != 2 || got.Units[0].UnitID != e.a.ID || got.Units[1].UnitID != e.a1.ID {
		t.Fatalf("units = %+v, want ua-a first and ua-a1 (ua-a2 has no usage, ua-b is not theirs)", got.Units)
	}
	if u := got.Units[0]; u.UnitName != "ua-a" || u.Totals.InputTokens != 10 || u.Totals.OutputTokens != 5 || u.Totals.Calls != 1 {
		t.Errorf("ua-a = %+v, want its own run's 10 in, 5 out, 1 call", u)
	}
	if u := got.Units[1]; u.UnitName != "ua-a1" || u.Totals.InputTokens != 21 || u.Totals.Calls != 2 || len(u.ByModel) != 2 {
		t.Errorf("ua-a1 = %+v, want 21 in over 2 calls in 2 models", u)
	}
	if _, ok := usageUnit(t, got, e.b.ID); ok {
		t.Error("ua-b is served to the admin of ua-a")
	}
	below := e.usageOK(e.adminA1, e.a1.ID, "")
	if len(below.Units) != 1 || below.Units[0].UnitID != e.a1.ID {
		t.Errorf("the admin of ua-a1 got %+v, want ua-a1 alone", below.Units)
	}
	empty := e.usageOK(e.adminA, e.a2.ID, "")
	if len(empty.Units) != 1 || empty.Units[0].UnitID != e.a2.ID || empty.Units[0].Totals.Calls != 0 || empty.Units[0].ByModel == nil {
		t.Errorf("a unit with no usage = %+v, want itself with zero totals and an empty by_model", empty.Units)
	}
	if sec := e.usageOK(e.alice, e.a.ID, ""); len(sec.Units) != 2 {
		t.Errorf("a security admin got %d units for ua-a, want 2", len(sec.Units))
	}
}

func TestUnitModelUsageOutsideScopeRefused(t *testing.T) {
	e := newUnitAuditEnv(t)
	e.seedUsage(e.runB, "api.anthropic.com", "claude-x", 1000, 1000)
	for name, w := range map[string]*httptest.ResponseRecorder{
		"a sibling":        e.usage(e.adminA, e.b.ID, ""),
		"a unit above":     e.usage(e.adminA1, e.a.ID, ""),
		"an unknown unit":  e.usage(e.adminA, uuid.New(), ""),
		"the default unit": e.usage(e.adminA, store.DefaultUnitID, ""),
	} {
		if !scopeRefused(t, w) {
			t.Errorf("%s: %d %s, want 403 unit_scope_denied", name, w.Code, w.Body.String())
		}
	}
	sibling, unknown := e.usage(e.adminA, e.b.ID, ""), e.usage(e.adminA, uuid.New(), "")
	if sibling.Code != unknown.Code {
		t.Errorf("an unknown unit answers %d, a unit outside scope %d: existence leaks", unknown.Code, sibling.Code)
	}
	if w := e.usage(e.adminA, e.b.ID, "?since=yesterday"); w.Code != http.StatusBadRequest {
		t.Errorf("a malformed window on a unit outside scope: %d, want the window refused first", w.Code)
	}
}

func TestUnitModelUsageMemberRefused(t *testing.T) {
	e := newUnitAuditEnv(t)
	w := e.usage(e.member, e.a.ID, "")
	if !scopeRefused(t, w) || !strings.Contains(w.Body.String(), unitScopeDoorSentence) {
		t.Errorf("a plain member: %d %s, want 403 unit_scope_denied at the door", w.Code, w.Body.String())
	}
}

func TestUnitModelUsageUnknownUnitSecurity404(t *testing.T) {
	e := newUnitAuditEnv(t)
	w := e.usage(e.alice, uuid.New(), "")
	if w.Code != http.StatusNotFound || wireReason(t, w) != reasonGovernanceUnitNotFoundByID {
		t.Errorf("a security admin asking for an unknown unit: %d %s, want 404 governance_unit_not_found", w.Code, w.Body.String())
	}
}

func TestUnitModelUsageWindowBounded(t *testing.T) {
	e := newUnitAuditEnv(t)
	recent := e.seedRunAged(&e.a.ID, 10*24*time.Hour)
	old := e.seedRunAged(&e.a.ID, 40*24*time.Hour)
	ancient := e.seedRunAged(&e.a.ID, 200*24*time.Hour)
	e.seedUsage(recent, "api.anthropic.com", "claude-x", 1, 1)
	e.seedUsage(old, "api.anthropic.com", "claude-x", 100, 100)
	e.seedUsage(ancient, "api.anthropic.com", "claude-x", 10000, 10000)
	calls := func(r UnitModelUsageResponse) int64 {
		u, _ := usageUnit(t, r, e.a.ID)
		return u.Totals.Calls
	}
	def := e.usageOK(e.adminA, e.a.ID, "")
	if span := def.Until.Sub(def.Since); span != 30*24*time.Hour {
		t.Errorf("the default window is %v, want 30 days", span)
	}
	if def.Until.Before(time.Now().Add(-time.Minute)) || def.Until.After(time.Now().Add(time.Minute)) {
		t.Errorf("the default window ends at %v, want now", def.Until)
	}
	if n := calls(def); n != 1 {
		t.Errorf("the default window counts %d calls of ua-a, want the recent run's 1", n)
	}
	until := time.Now().UTC().Truncate(time.Second)
	q := func(since, until time.Time) string {
		return "?" + url.Values{"since": {since.Format(time.RFC3339)}, "until": {until.Format(time.RFC3339)}}.Encode()
	}
	if n := calls(e.usageOK(e.adminA, e.a.ID, q(until.Add(-60*24*time.Hour), until))); n != 2 {
		t.Errorf("a 60-day window counts %d calls, want 2 (the 10 and 40 days old runs)", n)
	}
	if w := e.usage(e.adminA, e.a.ID, q(until.Add(-92*24*time.Hour), until)); w.Code != http.StatusOK {
		t.Errorf("a 92-day window: %d %s, want 200", w.Code, w.Body.String())
	}
	invalid := map[string]string{
		"93 days":       q(until.Add(-93*24*time.Hour), until),
		"since = until": q(until, until),
		"since > until": q(until, until.Add(-time.Hour)),
		"not a time":    "?since=yesterday",
		"until only":    "?until=not-a-time",
	}
	for name, query := range invalid {
		w := e.usage(e.adminA, e.a.ID, query)
		if w.Code != http.StatusBadRequest || wireReason(t, w) != reasonModelUsageWindowInvalid {
			t.Errorf("%s: %d %s, want 400 model_usage_window_invalid", name, w.Code, w.Body.String())
		}
	}
}

func TestUnitModelUsageProviderNamedBySite(t *testing.T) {
	e := newUnitAuditEnv(t)
	site := types.SiteConfig{ModelProviders: &types.ModelProviders{Providers: []types.ModelProvider{
		{ID: "corp-anthropic", Kind: types.ModelProviderAnthropicAPIKey},
		{ID: "corp-gw", Kind: types.ModelProviderCustomEndpoint, BaseURL: "https://llm.corp.example/v1"},
	}}}
	if _, err := e.pg.PutSiteConfig(context.Background(), site); err != nil {
		t.Fatalf("site config: %v", err)
	}
	e.seedUsage(e.runA, "api.anthropic.com", "claude-x", 10, 5)
	e.seedUsage(e.runA, "llm.corp.example", "claude-y", 1, 1)
	e.seedUsage(e.runA, "unlisted.example", "claude-z", 1, 1)
	u, ok := usageUnit(t, e.usageOK(e.adminA, e.a.ID, ""), e.a.ID)
	if !ok || len(u.ByModel) != 3 {
		t.Fatalf("ua-a = %+v, want 3 models", u)
	}
	want := map[string]string{"api.anthropic.com": "corp-anthropic", "llm.corp.example": "corp-gw", "unlisted.example": "unlisted.example"}
	for _, m := range u.ByModel {
		if m.Provider != want[m.Host] {
			t.Errorf("host %s is named %q, want %q", m.Host, m.Provider, want[m.Host])
		}
	}
}
