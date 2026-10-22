// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/pkg/client"
)

// liCreate launches body as cookie's person and returns the stored run and its
// run.create data.
func liCreate(t *testing.T, srv *Server, cookie *http.Cookie, body string) (types.AgentRun, map[string]any) {
	t.Helper()
	w := doSSO(t, srv, http.MethodPost, "/api/v1/runs", cookie, body)
	var created struct {
		ID uuid.UUID `json:"id"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil || w.Code != http.StatusCreated {
		t.Fatalf("create = %d %v: %s", w.Code, err, w.Body.String())
	}
	run, err := srv.cfg.Store.GetRun(context.Background(), created.ID)
	if err != nil {
		t.Fatal(err)
	}
	var data map[string]any
	ev := waitForRecAudit(t, srv.cfg.Audit.(*recRecorder), created.ID, "run.create", "success")
	if err := json.Unmarshal(ev.Data, &data); err != nil {
		t.Fatal(err)
	}
	return run, data
}

// liIntent is a kept document's intent, field by field.
func liIntent(t *testing.T, doc json.RawMessage) map[string]json.RawMessage {
	t.Helper()
	var d struct {
		APIVersion string                     `json:"api_version"`
		Kind       string                     `json:"kind"`
		Intent     map[string]json.RawMessage `json:"intent"`
	}
	if err := json.Unmarshal(doc, &d); err != nil || d.Kind != "Run" || d.APIVersion != "wardyn/v1" {
		t.Fatalf("kept document %s (%v), want a wardyn/v1 Run", doc, err)
	}
	return d.Intent
}

// liReplayBody is what Run again sends for a kept document: the intent as the
// create body, its inline_policy back under policy_overlay, and a new title.
func liReplayBody(t *testing.T, doc json.RawMessage) string {
	t.Helper()
	intent := liIntent(t, doc)
	if p, ok := intent["inline_policy"]; ok {
		intent["policy_overlay"] = p
		delete(intent, "inline_policy")
	}
	intent["title"] = json.RawMessage(`"again"`)
	return string(mustJSON(intent))
}

func TestLaunchIntentContent(t *testing.T) {
	const secretTask = "deploy with ghp_0123456789abcdefghijABCDEFGHIJ012345"
	for _, tc := range []struct {
		name, body, reason string
		refusal            string
		want, absent       []string
		plain              bool // no operator mount on the default, which no_repositories_or_drives refuses
	}{
		{
			name: "overlay, title and description",
			body: `{"title":"Nightly","description":"d","agent":"claude-code","task":"t","policy_overlay":{"denied_domains":["x.example"]}}`,
			want: []string{"agent", "inline_policy", "task"}, absent: []string{"title", "description", "policy_overlay", "repo"},
		},
		{
			name: "a run-mode request, as asked",
			body: `{"mode_contract":"0.9","shape":"agent_task",` + bgAgentTask + `}`,
			want: []string{"experience", "no_repositories_or_drives", "shape", "tools", "workload"}, absent: []string{"mode_contract", "agent", "task"},
			plain: true,
		},
		{
			name: "the wait chosen at launch",
			body: `{"agent":"claude-code","task":"t","max_wait_sec":600}`,
			want: []string{"agent", "task"}, absent: []string{"max_wait_sec"},
		},
		{
			name: "a whole inline_policy", reason: launchIntentWholeInlinePolicy,
			body: `{"agent":"claude-code","task":"t","inline_policy":{"min_confinement_class":"CC1","allowed_domains":["api.anthropic.com"]}}`,
		},
		{
			name: "secret-shaped task text", reason: launchIntentDocumentRefused, refusal: reasonTemplateSecretRefused,
			body: `{"agent":"claude-code","task":"` + secretTask + `"}`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, _ := ovOverlayFixture(t)
			if tc.plain {
				srv, _ = ovMemberFixture(t)
				srv.cfg.AgentImages = composeAgentImages
			}
			run, data := liCreate(t, srv, ovCookie(t), tc.body)
			if tc.reason != "" {
				if run.LaunchIntent != nil || data["launch_intent"] != "not_kept" || data["launch_intent_reason"] != tc.reason {
					t.Fatalf("kept %s; run.create says %v %v, want not_kept %s", run.LaunchIntent, data["launch_intent"], data["launch_intent_reason"], tc.reason)
				}
				if tc.refusal != "" && !slices.Contains(anyStrings(data["launch_intent_refusals"]), tc.refusal) {
					t.Errorf("launch_intent_refusals = %v, want %s", data["launch_intent_refusals"], tc.refusal)
				}
				if strings.Contains(string(mustJSON(data)), "ghp_") {
					t.Error("run.create carries the secret-shaped text")
				}
				return
			}
			if data["launch_intent"] != "kept" || data["launch_intent_reason"] != nil {
				t.Fatalf("run.create says %v %v, want kept", data["launch_intent"], data["launch_intent_reason"])
			}
			intent := liIntent(t, run.LaunchIntent)
			for _, k := range tc.want {
				if _, ok := intent[k]; !ok {
					t.Errorf("intent lacks %s: %s", k, run.LaunchIntent)
				}
			}
			for _, k := range tc.absent {
				if _, ok := intent[k]; ok {
					t.Errorf("intent holds %s: %s", k, run.LaunchIntent)
				}
			}
			if p, ok := intent["inline_policy"]; ok && string(p) != `{"denied_domains":["x.example"]}` {
				t.Errorf("intent.inline_policy = %s, want the overlay", p)
			}
		})
	}
}

func anyStrings(v any) []string {
	var out []string
	for _, s := range v.([]any) {
		out = append(out, s.(string))
	}
	return out
}

// Run again's body, built from the kept document, folds to the same run: the
// same resolved policy, confinement class and placement.
func TestLaunchIntentRunAgainEquivalence(t *testing.T) {
	srv, _ := ovOverlayFixture(t)
	cookie := ovCookie(t)
	first, _ := liCreate(t, srv, cookie, ovOverlayBody(`{"denied_domains":["x.example"],"allowed_domains":["`+ovHostInside+`"]}`))
	if first.LaunchIntent == nil {
		t.Fatal("the first run kept no intent")
	}
	again, _ := liCreate(t, srv, cookie, liReplayBody(t, first.LaunchIntent))
	audit := srv.cfg.Audit.(*recRecorder)
	spec := func(id uuid.UUID) string {
		return string(waitForRecAudit(t, audit, id, "run.policy.resolve", "success").Data)
	}
	if a, b := spec(first.ID), spec(again.ID); a != b {
		t.Errorf("the resolved policy differs:\n first %s\n again %s", a, b)
	}
	if first.ConfinementClass != again.ConfinementClass || first.RunnerTarget != again.RunnerTarget ||
		!reflect.DeepEqual(first.RunnerPoolID, again.RunnerPoolID) || first.Task != again.Task {
		t.Errorf("first %+v\nagain %+v", first, again)
	}
}

// The run's creator reads its intent; the super admin, the security admin and
// another member get the 404 a missing run gets, byte for byte.
func TestLaunchIntentOwnerOnly(t *testing.T) {
	srv, ast, _, _ := newAuthzMatrixServer(t)
	owner := ssoSession(t, "sub-li-owner", "owner@corp.example", oidc.RoleUser)
	doc := json.RawMessage(`{"api_version":"wardyn/v1","kind":"Run","intent":{"agent":"claude-code","task":"t"}}`)
	kept, bare := uuid.New(), uuid.New()
	ast.mu.Lock()
	ast.runs[kept] = types.AgentRun{ID: kept, CreatedBy: "sub-li-owner", State: types.RunCompleted, Agent: "claude-code", LaunchIntent: doc}
	ast.runs[bare] = types.AgentRun{ID: bare, CreatedBy: "sub-li-owner", State: types.RunCompleted, Agent: "claude-code"}
	ast.mu.Unlock()
	get := func(c *http.Cookie, id uuid.UUID) (int, string) {
		w := doSSO(t, srv, http.MethodGet, "/api/v1/runs/"+id.String()+"/launch-intent", c, "")
		return w.Code, w.Body.String()
	}

	if code, body := get(owner, kept); code != http.StatusOK || !strings.Contains(body, `"kind":"Run"`) {
		t.Fatalf("owner = %d %s", code, body)
	}
	if code, body := get(owner, bare); code != http.StatusNotFound || !strings.Contains(body, reasonLaunchIntentNotFound) {
		t.Errorf("owner, a run that kept none = %d %s", code, body)
	}
	missingCode, missing := get(owner, uuid.New())
	for name, c := range map[string]*http.Cookie{
		"super admin":    ssoSession(t, "sub-li-admin", "admin@corp.example", oidc.RoleAdmin),
		"security admin": ssoSession(t, "sub-li-sec", "sec@corp.example", oidc.RoleSecurityAdmin),
		"another member": ssoSession(t, "sub-li-other", "other@corp.example", oidc.RoleUser),
	} {
		if code, body := get(c, kept); code != missingCode || body != missing {
			t.Errorf("%s = %d %s, want the missing run's %d %s", name, code, body, missingCode, missing)
		}
	}
}

// Run again and a shared template both name an added secret the caller does not
// hold. Preview and create refuse it with one sentence, and nothing relaxes
// create's check for a body that came from a document; Review lists the secret
// as a setup item to add instead.
func TestReplay_AddSecretsNeedsSetup(t *testing.T) {
	srv, _ := ovMemberFixture(t)
	cookie := ovCookie(t)
	first, _ := liCreate(t, srv, cookie, `{"agent":"claude-code","task":"t","overrides":{"agent":{"add_secrets":[{"secret_name":"my-key","host":"`+ovHostInside+`"}]}}}`)
	if first.LaunchIntent == nil {
		t.Fatal("the first run kept no intent")
	}
	replay := liReplayBody(t, first.LaunchIntent)
	other := govSession(t, "sub-other", []string{"eng"}, false) // holds "theirs", not "my-key"

	sameRefusal := func(name string, c *http.Cookie) {
		var sentence string
		for _, door := range []string{previewDoor, "/api/v1/runs"} {
			w := doSSO(t, srv, http.MethodPost, door, c, replay)
			if w.Code != http.StatusUnprocessableEntity || errorReason(w) != reasonOverrideRefused {
				t.Errorf("%s at %s = %d %s, want 422 %s", name, door, w.Code, w.Body.String(), reasonOverrideRefused)
				continue
			}
			var body struct{ Error string }
			_ = json.Unmarshal(w.Body.Bytes(), &body)
			if sentence == "" {
				sentence = body.Error
			} else if body.Error != sentence {
				t.Errorf("%s at %s says %q, the preview %q", name, door, body.Error, sentence)
			}
		}
	}
	sameRefusal("a shared template's second member", other)
	var pf preflightResponse
	if w := doSSO(t, srv, http.MethodPost, preflightDoor, other, replay); w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &pf) != nil ||
		!slices.ContainsFunc(pf.SetupItems, func(it SetupItem) bool { return it.ID == "secret:my-key" && it.Status == "missing" }) {
		t.Errorf("Review of a shared template for a member without the secret = %d %s, want 200 listing secret:my-key as missing", w.Code, w.Body.String())
	}
	memSecretsMu.Lock()
	delete(srv.cfg.Secrets.(*memSecrets).owned[govMemberSub], "my-key")
	memSecretsMu.Unlock()
	sameRefusal("Run again after the creator deleted the secret", cookie)
}

// TestLaunchIntentWireTwin pins ui/src/app/lib/types/launch-intents.ts to the
// Go wire structs, key for key.
func TestLaunchIntentWireTwin(t *testing.T) {
	ts, err := os.ReadFile("../../ui/src/app/lib/types/launch-intents.ts")
	if err != nil {
		t.Fatal(err)
	}
	for name, v := range map[string]any{"LaunchIntentSummary": launchIntentSummary{}, "LaunchIntentList": launchIntentList{}, "LaunchIntent": launchIntentView{}} {
		body := regexp.MustCompile(`(?s)export interface ` + name + ` \{(.*?)\n\}`).FindSubmatch(ts)
		if body == nil {
			t.Fatalf("launch-intents.ts has no interface %s", name)
		}
		var tsKeys, goKeys []string
		for _, m := range regexp.MustCompile(`(?m)^\s+(\w+)\??:`).FindAllSubmatch(body[1], -1) {
			tsKeys = append(tsKeys, string(m[1]))
		}
		rt := reflect.TypeOf(v)
		for i := range rt.NumField() {
			tag, _, _ := strings.Cut(rt.Field(i).Tag.Get("json"), ",")
			goKeys = append(goKeys, tag)
		}
		slices.Sort(tsKeys)
		slices.Sort(goKeys)
		if !slices.Equal(tsKeys, goKeys) {
			t.Errorf("%s: TS keys %v, Go %v", name, tsKeys, goKeys)
		}
	}
}

// A reference's explicit empty mcp_harnesses ("on for no harness") is kept in
// the saved intent as [], and a reference that names none stays without the key.
func TestLaunchIntent_KeepsAnEmptyMCPHarnessesList(t *testing.T) {
	asked := mustJSON(createRunRequest{Components: []types.ComponentRef{
		{Name: "off", Inline: &types.ComponentDefinition{Hosts: []string{"mcp.off.test"}}, MCPHarnesses: []string{}},
		{Name: "default", Inline: &types.ComponentDefinition{Hosts: []string{"mcp.default.test"}}},
	}})
	li := launchIntentFor(asked)
	if li.doc == nil {
		t.Fatalf("no intent kept: %q %v", li.reason, li.refusals)
	}
	var refs []map[string]json.RawMessage
	if err := json.Unmarshal(liIntent(t, li.doc)["components"], &refs); err != nil || len(refs) != 2 {
		t.Fatalf("components = %v (%v)", refs, err)
	}
	if got := string(refs[0]["mcp_harnesses"]); got != "[]" {
		t.Errorf("explicit none kept as %q, want []", got)
	}
	if _, ok := refs[1]["mcp_harnesses"]; ok {
		t.Errorf("a reference with no list gained mcp_harnesses: %s", refs[1]["mcp_harnesses"])
	}
}

// A kept launch intent is a Run document, so it keeps each agent's own task (#3417).
func TestLaunchIntent_KeepsEachAgentsTask(t *testing.T) {
	asked := mustJSON(createRunRequest{
		Experience: client.ExperienceBackground,
		Workload:   &client.RunWorkload{Kind: client.WorkloadAgentTask, Agent: "claude-code", Task: "fix the build"},
		Tools: []client.IncludedTool{
			{ID: "claude-code", Kind: client.IncludedToolHarness},
			{ID: "codex-cli", Kind: client.IncludedToolHarness, Task: "review the diff"},
		},
		NoRepositoriesOrDrives: true,
	})
	li := launchIntentFor(asked)
	if li.doc == nil {
		t.Fatalf("no intent kept: %q %v", li.reason, li.refusals)
	}
	var tools []client.IncludedTool
	if err := json.Unmarshal(liIntent(t, li.doc)["tools"], &tools); err != nil || len(tools) != 2 {
		t.Fatalf("tools = %v (%v)", tools, err)
	}
	if tools[1].Task != "review the diff" {
		t.Errorf("codex-cli task = %q, want it kept", tools[1].Task)
	}
}
