// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/assistant"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// The written policy sets every inspection field, the validator accepts it, and it carries nothing a person wrote.
func TestAssistantRunPolicy(t *testing.T) {
	spec := assistantRunPolicy(types.CC2)
	li := spec.LLMInspection
	if li == nil {
		t.Fatal("no inspection block")
	}
	want := types.LLMInspectionSpec{Mode: "block", DetectSecrets: true, DetectSecretPatterns: true, OnScannerError: "block",
		RequireInspectableLLM: true, InterceptTLS: true, BlockMinSeverity: "low", MaxScanBytes: 1048576}
	gotJSON, _ := json.Marshal(li)
	wantJSON, _ := json.Marshal(want)
	if string(gotJSON) != string(wantJSON) {
		t.Errorf("LLMInspection = %s, want %s", gotJSON, wantJSON)
	}
	if err := validateLLMInspection(spec); err != nil {
		t.Errorf("validateLLMInspection = %v, want nil", err)
	}
	if spec.MinConfinementClass != types.CC2 || spec.AutoStopAfterSec != 7200 || len(spec.AllowedDomains) != 0 || spec.AllowedDomains == nil {
		t.Errorf("policy = %+v, want CC2, 7200 s and an empty allow-list", spec)
	}
	if len(spec.WorkspaceMounts) != 0 || len(spec.EligibleGrants) != 0 || len(spec.WorkspaceRepos) != 0 {
		t.Errorf("policy carries mounts, repositories or grants: %+v", spec)
	}
}

func TestAssistantRunPayloadOK(t *testing.T) {
	ev := func(kind, payload string) assistantRunEvent {
		return assistantRunEvent{Kind: kind, Payload: json.RawMessage(payload)}
	}
	cases := []struct {
		name string
		ev   assistantRunEvent
		want bool
	}{
		{"no payload", ev(types.AssistantEventText, ""), true},
		{"object", ev(types.AssistantEventText, `{"text":"hi"}`), true},
		{"not an object", ev(types.AssistantEventText, `"hi"`), false},
		{"a state naming a turn", ev(types.AssistantEventState, `{"turn":"reset"}`), false},
		{"a state of its own", ev(types.AssistantEventState, `{"state":"ready"}`), true},
		{"records the server alone writes", ev(types.AssistantEventState, `{"state":"records","records":[]}`), false},
		{"records on a text event", ev(types.AssistantEventText, `{"text":"hi","records":[]}`), false},
	}
	for _, c := range cases {
		if got := assistantRunPayloadOK(c.ev); got != c.want {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
}

// Counts are runs, not rows, per host and decision: a host one run reached and another was refused prints twice.
func TestAssistantHostCountsOf(t *testing.T) {
	r1, r2, r3 := uuid.New(), uuid.New(), uuid.New()
	rows := []store.RunHostDecision{
		{RunID: r1, Host: "a.example", Decision: "allow"}, {RunID: r1, Host: "a.example", Decision: "allow"},
		{RunID: r2, Host: "a.example", Decision: "allow"}, {RunID: r3, Host: "a.example", Decision: "deny"},
		{RunID: r2, Host: "b.example", Decision: "deny"},
	}
	got := assistantHostCountsOf(rows)
	want := []assistant.HostCount{{Host: "a.example", Decision: "allow", Count: 2}, {Host: "a.example", Decision: "deny", Count: 1},
		{Host: "b.example", Decision: "deny", Count: 1}}
	if !slices.Equal(got, want) {
		t.Errorf("counts = %+v, want %+v", got, want)
	}
}

func TestAssistantTargetPairing(t *testing.T) {
	if assistantStaticKind(types.AssistantTarget{Kind: types.AssistantTargetUnit}) != govKindUnit ||
		assistantStaticKind(types.AssistantTarget{Kind: types.AssistantTargetChange}) != "" {
		t.Error("assistantStaticKind reads the wrong kinds")
	}
	if got := assistantToolNames(types.AssistantReview); len(got) != 8 || slices.Contains(got, "draft") || slices.Contains(got, "build_files") {
		t.Errorf("review tools = %v, want eight with neither draft nor build_files anywhere", got)
	}
	if got := assistantToolNames(types.AssistantCompose); len(got) != 10 || !slices.Contains(got, "draft") || !slices.Contains(got, "build_files") {
		t.Errorf("compose tools = %v, want ten with draft and build_files", got)
	}
	set := &types.AssistantSettings{NeverSentKinds: []string{govKindUnit}}
	if assistantNeverSent(set, govKindUnit) == nil || assistantNeverSent(set, "other") != nil || assistantNeverSent(nil, govKindUnit) != nil ||
		assistantNeverSent(set, "") != nil {
		t.Error("assistantNeverSent refuses the wrong kinds")
	}
}

// Dispatch pins the run's proxy to the binding's model, whatever the ceiling allowed.
func TestAssistantDispatchPinsTheBindingModel(t *testing.T) {
	srv := &Server{}
	l := assistantLaunch{token: "tok", cc: types.CC2, model: "claude-sonnet-x",
		dc: dispatchCeiling{modelLimit: allowedModelsLimit{models: []string{"claude-sonnet-x", "claude-opus-x"}}}}
	dc, params := srv.assistantDispatchArgs(l)
	if !slices.Equal(dc.modelLimit.models, []string{"claude-sonnet-x"}) {
		t.Errorf("model limit = %v, want only the binding's model", dc.modelLimit.models)
	}
	if !slices.Equal(l.dc.modelLimit.models, []string{"claude-sonnet-x", "claude-opus-x"}) {
		t.Errorf("the launch's own ceiling was changed: %v", l.dc.modelLimit.models)
	}
	if params.RunToken != "tok" || params.Policy.MinConfinementClass != types.CC2 {
		t.Errorf("params = %+v", params)
	}
}

// The seal refuses a provider that defers inspection on an assistant run, and on a run it could not check: a
// failed session read refuses, never passes. A run with no session row keeps its route.
func TestAssistantSealFailsClosed(t *testing.T) {
	p := testGatewayProvider()
	p.GatewayIdentity.Inspection = types.GatewayInspectionGateway
	cases := []struct {
		name    string
		session bool
		readErr error
		want    bool
	}{
		{"not an assistant run", false, nil, true},
		{"an assistant run", true, nil, false},
		{"the session read fails", false, errors.New("store down"), false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newAssistantFake(types.SiteConfig{})
			srv, h := assistantSessionServer(t, f)
			run, err := f.CreateRun(context.Background(), types.AgentRun{ID: uuid.New(), CreatedBy: "sub-seal", State: types.RunStarting})
			if err != nil {
				t.Fatal(err)
			}
			if c.session {
				f.seed(assistantLiveSession("sub-seal", &run.ID, types.AssistantBinding{}))
			}
			f.runIDsErr = c.readErr
			var d providerDispatch
			if got := srv.takeProviderGatewayRoute(context.Background(), run, &d, p, llmTransport{}); got != c.want {
				t.Fatalf("route kept = %v, want %v", got, c.want)
			}
			if refused := len(govCovAudits(h, "run.create")) > 0; refused == c.want {
				t.Errorf("run.create refusal audited = %v, want %v", refused, !c.want)
			}
		})
	}
}

// A member cannot ask for the assistant's own task: it is set by the server alone.
func TestAssistantRunTaskIsReserved(t *testing.T) {
	h := newHarness(t)
	r := httptest.NewRequest(http.MethodPost, "/api/v1/runs", strings.NewReader(`{"agent":"claude-code","task":"`+assistantRunTask+`"}`))
	w := httptest.NewRecorder()
	if _, _, _, _, ok := h.srv.decodeAndValidateCreateRun(w, r); ok {
		t.Fatalf("a member create with the assistant's task was accepted: %d %s", w.Code, w.Body)
	}
	if w.Code != http.StatusBadRequest || errorReason(w) != reasonRunTaskReserved {
		t.Errorf("refusal = %d %s, want 400 %s", w.Code, w.Body, reasonRunTaskReserved)
	}
}
