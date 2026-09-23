// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/secretmask"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestChooseModelProvider is the resolution order, one case per step and per
// refusal (multi-provider §2.4, §4 rule 1).
func TestChooseModelProvider(t *testing.T) {
	off := func(p types.ModelProvider) types.ModelProvider { p.Disabled = true; return p }
	site := func(def string, ps ...types.ModelProvider) types.SiteConfig {
		sc := types.SiteConfig{ModelProviders: providerBlock(ps...)}
		if def != "" {
			sc.AgentProviders = agentBlock(types.AgentProvider{ID: "claude-code", DefaultProvider: def})
		}
		return sc
	}
	a, b := keyProvider("a", "claude-code"), keyProvider("b", "claude-code")
	codexOnly := keyProvider("codex", "codex-cli")
	all := func(string) (bool, error) { return true, nil }
	only := func(ids ...string) func(string) (bool, error) {
		return func(id string) (bool, error) { return slices.Contains(ids, id), nil }
	}
	refusal := func(id, state string) string { return fmt.Sprintf(mpRunRefusal, id, state, mpRunRemedy) }

	for _, tc := range []struct {
		name           string
		sc             types.SiteConfig
		requested, pin string
		granted        func(string) (bool, error)
		wantID         string // the chosen provider, "" for none
		wantRefusal    string
		wantNotGranted bool
	}{
		{name: "the request, when it is a candidate", sc: site("a", a, b), requested: "b", granted: all, wantID: "b"},
		{name: "the request beats the pin", sc: site("", a, b), requested: "a", pin: "b", granted: all, wantID: "a"},
		{name: "a requested provider that does not exist", sc: site("", a), requested: "nope", granted: all,
			wantRefusal: refusal("nope", mpRunStateMissing)},
		{name: "a requested provider that is off", sc: site("", a, off(b)), requested: "b", granted: all,
			wantRefusal: refusal("b", mpRunStateOff)},
		{name: "a requested provider that does not serve the agent", sc: site("", a, codexOnly), requested: "codex", granted: all,
			wantRefusal: refusal("codex", fmt.Sprintf(mpRunStateNotServing, "claude-code"))},
		{name: "a requested provider the caller is not granted", sc: site("", a, b), requested: "b", granted: only("a"),
			wantRefusal: refusal("b", mpRunStateNotGranted), wantNotGranted: true},
		{name: "the pin, with no request", sc: site("a", a, b), pin: "b", granted: all, wantID: "b"},
		{name: "a pin the caller is not granted is refused, never passed over", sc: site("a", a, b), pin: "b", granted: only("a"),
			wantRefusal: refusal("b", mpRunStateNotGranted), wantNotGranted: true},
		{name: "a pin that is off is refused, never passed over", sc: site("a", a, off(b)), pin: "b", granted: all,
			wantRefusal: refusal("b", mpRunStateOff)},
		{name: "the default among two candidates", sc: site("b", a, b), granted: all, wantID: "b"},
		{name: "a disabled default is refused even with exactly one other candidate", sc: site("b", a, off(b)), granted: all,
			wantRefusal: refusal("b", mpRunStateOff)},
		{name: "a default the caller is not granted passes to the single candidate", sc: site("b", a, b), granted: only("a"), wantID: "a"},
		{name: "the single candidate", sc: site("", a, codexOnly), granted: all, wantID: "a"},
		{name: "two candidates and no default", sc: site("", a, b), granted: all,
			wantRefusal: fmt.Sprintf(mpRunChoose, "claude-code")},
		{name: "one provider serves the agent and the caller is not granted it", sc: site("", a), granted: only(),
			wantRefusal: refusal("a", mpRunStateNotGranted), wantNotGranted: true},
		{name: "several serve the agent and the caller is granted none", sc: site("", a, b), granted: only(),
			wantRefusal: fmt.Sprintf(mpRunNoneGranted, "claude-code"), wantNotGranted: true},
		{name: "no provider serves the agent: no choice, today's path", sc: site("", codexOnly, off(a)), granted: all},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := chooseModelProvider(tc.sc, "claude-code", tc.requested, tc.pin, tc.granted)
			if err != nil {
				t.Fatal(err)
			}
			if got.chosen != (tc.wantID != "") || got.provider.ID != tc.wantID {
				t.Errorf("chose %q (chosen=%v), want %q", got.provider.ID, got.chosen, tc.wantID)
			}
			if got.refusal != tc.wantRefusal || got.notGranted != tc.wantNotGranted {
				t.Errorf("refusal = %q (notGranted=%v)\nwant      %q (notGranted=%v)", got.refusal, got.notGranted, tc.wantRefusal, tc.wantNotGranted)
			}
		})
	}

	t.Run("a capability that cannot be read refuses rather than guesses", func(t *testing.T) {
		boom := errors.New("store down")
		if _, err := chooseModelProvider(site("", a), "claude-code", "", "", func(string) (bool, error) { return false, boom }); !errors.Is(err, boom) {
			t.Errorf("err = %v, want the capability read's error", err)
		}
	})
}

// providerRunFixture is a create/preflight server whose site config carries
// site, whose capability store is cs, and whose one workspace is ws (when set).
func providerRunFixture(t *testing.T, site types.SiteConfig, cs *capStore, ws *types.Workspace) *Server {
	t.Helper()
	h := newHarness(t)
	st := &integStore{govEscapeStore: newGovEscapeStore(cs), site: site}
	if ws != nil {
		st.workspaces = []types.Workspace{*ws}
	}
	cfg := baseTestConfig(h, st)
	cfg.Audit = &recRecorder{}
	cfg.Broker = h.broker
	cfg.Runner = &fakeRunner{}
	cfg.Secrets = &memSecrets{m: map[string][]byte{}}
	cfg.MaskRegistry = secretmask.NewRegistry()
	cfg.OIDC = &oidc.Authenticator{}
	cfg.DefaultPolicy = govDeployment()
	return New(cfg)
}

// TestRunModelProviderDoors drives create and Review with the same body and
// caller: each case must answer the same status and body at both doors.
func TestRunModelProviderDoors(t *testing.T) {
	enforced := map[string]bool{capModelProvider: true}
	twoKeys := types.SiteConfig{ModelProviders: providerBlock(keyProvider("anthropic", "claude-code"), keyProvider("corp", "claude-code"))}
	pinned := &types.Workspace{
		ID: uuid.New(), Name: "hello", Status: types.WorkspaceScanned,
		Sources: []types.WorkspaceSource{{Type: types.WorkspaceSourceTypeRepo, Source: govWorkspaceRepo}},
		LLMCred: &types.WorkspaceLLMCred{ProviderRef: "corp"},
	}
	onPinned := `{"agent":"claude-code","task":"t","inline_policy":{"min_confinement_class":"CC2",` +
		`"allowed_domains":["api.anthropic.com"],"workspace_repos":[{"repo":"` + govWorkspaceRepo + `"}]}}`
	// workspace_id without interactive is how the CLI (--workspace) and the
	// console attach a workspace. It is a model run at dispatch — a create body
	// never sets run.WorkspaceID — so it must choose like any other.
	plain := &types.Workspace{ID: uuid.New(), Name: "plain", Status: types.WorkspaceScanned, Sources: pinned.Sources}
	byID := func(ws *types.Workspace, extra string) string {
		return fmt.Sprintf(`{"agent":"claude-code","task":"t","workspace_id":%q%s}`, ws.ID, extra)
	}

	for _, tc := range []struct {
		name     string
		site     types.SiteConfig
		cs       *capStore
		ws       *types.Workspace
		operator bool
		body     string
		want     int
		wantBody string
		denied   bool // an authz.denied capability_model_provider row
	}{
		{name: "a requested provider the member is not granted", site: twoKeys, cs: &capStore{enf: enforced},
			body: `{"agent":"claude-code","task":"t","model_provider":"corp"}`,
			want: http.StatusForbidden, wantBody: fmt.Sprintf(mpRunRefusal, "corp", mpRunStateNotGranted, mpRunRemedy), denied: true},
		{name: "a workspace pin naming a provider the member is not granted", site: twoKeys, ws: pinned,
			cs: &capStore{enf: enforced, grants: []types.CapabilityGrant{
				grant(types.CapabilitySubjectAll, "", capModelProvider, "anthropic", types.CapabilityAllow)}},
			body: onPinned, want: http.StatusForbidden, wantBody: fmt.Sprintf(mpRunRefusal, "corp", mpRunStateNotGranted, mpRunRemedy), denied: true},
		{name: "workspace_id: the pin names a provider the member is not granted", site: twoKeys, ws: pinned,
			cs: &capStore{enf: enforced, grants: []types.CapabilityGrant{
				grant(types.CapabilitySubjectAll, "", capModelProvider, "anthropic", types.CapabilityAllow)}},
			body: byID(pinned, ""), want: http.StatusForbidden, wantBody: fmt.Sprintf(mpRunRefusal, "corp", mpRunStateNotGranted, mpRunRemedy), denied: true},
		{name: "workspace_id: two candidates and no choice", site: twoKeys, ws: plain, cs: &capStore{}, operator: true,
			body: byID(plain, ""), want: http.StatusUnprocessableEntity, wantBody: fmt.Sprintf(mpRunChoose, "claude-code")},
		{name: "workspace_id: a chosen provider whose kind has no dispatch arm yet", site: twoKeys, ws: plain, cs: &capStore{}, operator: true,
			body: byID(plain, `,"model_provider":"corp"`), want: http.StatusUnprocessableEntity, wantBody: fmt.Sprintf(mpRunNotYet, "corp")},
		{name: "a chosen provider whose kind has no dispatch arm yet", site: twoKeys, cs: &capStore{}, operator: true,
			body: `{"agent":"claude-code","task":"t","model_provider":"corp"}`,
			want: http.StatusUnprocessableEntity, wantBody: fmt.Sprintf(mpRunNotYet, "corp")},
		{name: "a disabled default with one other candidate", cs: &capStore{}, operator: true,
			site: types.SiteConfig{
				ModelProviders: providerBlock(keyProvider("anthropic", "claude-code"), func() types.ModelProvider {
					p := keyProvider("corp", "claude-code")
					p.Disabled = true
					return p
				}()),
				AgentProviders: agentBlock(types.AgentProvider{ID: "claude-code", DefaultProvider: "corp"}),
			},
			body: `{"agent":"claude-code","task":"t"}`,
			want: http.StatusUnprocessableEntity, wantBody: fmt.Sprintf(mpRunRefusal, "corp", mpRunStateOff, mpRunRemedy)},
		{name: "two candidates and no choice", site: twoKeys, cs: &capStore{}, operator: true,
			body: `{"agent":"claude-code","task":"t"}`,
			want: http.StatusUnprocessableEntity, wantBody: fmt.Sprintf(mpRunChoose, "claude-code")},
		{name: "model_provider with no provider block", cs: &capStore{}, operator: true,
			body: `{"agent":"claude-code","task":"t","model_provider":"corp"}`,
			want: http.StatusUnprocessableEntity, wantBody: fmt.Sprintf(mpRunNoBlock, "corp")},
		{name: "model_provider on a run that calls no model", site: twoKeys, cs: &capStore{}, operator: true,
			body: `{"agent":"claude-code","task":"echo hi","task_mode":"exec","model_provider":"corp"}`,
			want: http.StatusBadRequest, wantBody: mpRunNoModel},
		{name: "a model_provider that is not a provider id", site: twoKeys, cs: &capStore{}, operator: true,
			body: `{"agent":"claude-code","task":"t","model_provider":"Corp Gateway"}`,
			want: http.StatusBadRequest, wantBody: fmt.Sprintf(mpRunBadID, "Corp Gateway")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var codes [2]int
			for i, path := range []string{"/api/v1/runs/preflight", "/api/v1/runs"} {
				srv := providerRunFixture(t, tc.site, tc.cs, tc.ws)
				session := govSession(t, govMemberSub, []string{"eng"}, false)
				if tc.operator {
					session = admitAdminSession(t)
				}
				w := doSSO(t, srv, http.MethodPost, path, session, tc.body)
				codes[i] = w.Code
				var body errorBody
				_ = json.Unmarshal(w.Body.Bytes(), &body)
				if w.Code != tc.want || body.Error != tc.wantBody {
					t.Errorf("%s = %d %s\nwant %d carrying %q", path, w.Code, w.Body.String(), tc.want, tc.wantBody)
				}
				if got := slices.Contains(auditReasons(t, srv, "authz.denied"), "capability_model_provider"); got != tc.denied {
					t.Errorf("%s: authz.denied capability_model_provider written = %v, want %v", path, got, tc.denied)
				}
			}
			if codes[0] != codes[1] {
				t.Errorf("preflight answered %d, create %d — Review must preview the refusal launch makes", codes[0], codes[1])
			}
		})
	}

	t.Run("a block that serves no provider for this agent leaves the run on today's path", func(t *testing.T) {
		site := types.SiteConfig{ModelProviders: providerBlock(keyProvider("codex", "codex-cli"))}
		w := doSSO(t, providerRunFixture(t, site, &capStore{}, nil), http.MethodPost, "/api/v1/runs",
			admitAdminSession(t), `{"agent":"claude-code","task":"t"}`)
		if w.Code != http.StatusCreated {
			t.Errorf("create = %d, want 201: %s", w.Code, w.Body.String())
		}
	})
}

// TestRunModelProviderPersistsOnTheRow is #527: once a provider's kind has a
// dispatch arm (MP-7/8/9 — simulated here since none has landed yet, exactly
// the way each of THOSE PRs will exercise this same plumbing), the run's
// chosen provider freezes onto AgentRun.ModelProviderID (the id alone) and
// onto the run.create audit event's model_provider snapshot ({id, kind} —
// the kind is NOT on the row; see the field's doc on types.AgentRun).
func TestRunModelProviderPersistsOnTheRow(t *testing.T) {
	// The single candidate: no AgentProviders row at all, so the legacy
	// declared-mechanism gate (enforceCreateLLMMechanism, unrelated to #527)
	// sees no row for this agent and stays out of the way — exactly what
	// TestChooseModelProvider's "the single candidate" case exercises.
	provider := keyProvider("corp", "claude-code")
	site := types.SiteConfig{ModelProviders: providerBlock(provider)}
	srv := providerRunFixture(t, site, &capStore{}, nil)

	// providerKindDispatched lacks the key kinds until MP-7 lands them; a real PR
	// flips this bit permanently, a test flips it for the span of one call.
	providerKindDispatched[provider.Kind] = true
	defer delete(providerKindDispatched, provider.Kind)

	w := doSSO(t, srv, http.MethodPost, "/api/v1/runs", admitAdminSession(t), `{"agent":"claude-code","task":"t"}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("create = %d, want 201: %s", w.Code, w.Body.String())
	}
	var got types.AgentRun
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode run: %v", err)
	}
	if got.ModelProviderID != "corp" {
		t.Errorf("run.model_provider_id = %q, want %q", got.ModelProviderID, "corp")
	}

	st, ok := srv.cfg.Store.(*integStore)
	if !ok {
		t.Fatalf("store = %T, want *integStore", srv.cfg.Store)
	}
	st.mu.Lock()
	stored := st.runs[got.ID].ModelProviderID
	st.mu.Unlock()
	if stored != "corp" {
		t.Errorf("stored model_provider_id = %q, want %q", stored, "corp")
	}

	rec, ok := srv.cfg.Audit.(*recRecorder)
	if !ok {
		t.Fatalf("audit recorder = %T, want *recRecorder", srv.cfg.Audit)
	}
	var snapshot map[string]any
	for _, ev := range rec.snapshot() {
		if ev.Action != "run.create" {
			continue
		}
		var data struct {
			ModelProvider map[string]any `json:"model_provider"`
		}
		if err := json.Unmarshal(ev.Data, &data); err != nil {
			t.Fatalf("unmarshal run.create data: %v", err)
		}
		snapshot = data.ModelProvider
	}
	if snapshot == nil {
		t.Fatal("run.create carries no model_provider snapshot")
	}
	if snapshot["id"] != "corp" {
		t.Errorf("model_provider.id = %v, want %q", snapshot["id"], "corp")
	}
	if snapshot["kind"] != string(provider.Kind) {
		t.Errorf("model_provider.kind = %v, want %q", snapshot["kind"], provider.Kind)
	}
}
