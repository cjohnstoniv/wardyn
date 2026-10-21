// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/google/uuid"
)

// noBuilderFixture is seedImageFixture with no image builder wired: the shape of a
// control plane that never started the builder (a Kubernetes install, or one
// built without -tags docker).
func noBuilderFixture(t *testing.T, ws types.Workspace) *Server {
	t.Helper()
	h := newHarness(t)
	seed := &seedImageStore{govEscapeStore: newGovEscapeStore(&capStore{enf: map[string]bool{capAgent: true}}), ws: ws}
	seed.workspaces = []types.Workspace{ws}
	st := &runImageStore{seedImageStore: seed, images: make(chan string, 1)}
	cfg := baseTestConfig(h, st)
	cfg.Audit = &recRecorder{}
	cfg.Broker = h.broker
	cfg.Runner = &fakeRunner{}
	cfg.Secrets = &memSecrets{m: map[string][]byte{}}
	cfg.OIDC = &oidc.Authenticator{}
	cfg.DefaultPolicy = govDeployment()
	return New(cfg)
}

// runImageStore records the image a launch resolves for its run.
type runImageStore struct {
	*seedImageStore
	images chan string
}

func (s *runImageStore) SetRunImage(_ context.Context, _ uuid.UUID, image string) error {
	s.images <- image
	return nil
}

// A member's command on an admin's workspace whose base image is byo, with no
// image builder: the run is refused, and the refusal names what works instead,
// not only the daemon flags.
func TestWorkspaceByoBaseImageNoBuilderNamesWorkingPath(t *testing.T) {
	for _, kind := range []string{"byo", "registry"} {
		t.Run(kind, func(t *testing.T) {
			ws := baseImageWorkspace("", "example.test/tools:1")
			ws.BaseImage.Kind = kind
			srv := noBuilderFixture(t, ws)
			body := fmt.Sprintf(`{"task_mode":"exec","task":"echo hi","workspace_id":%q}`, ws.ID.String())
			w := doSSO(t, srv, http.MethodPost, "/api/v1/runs", govSession(t, "sub-member", []string{"eng"}, false), body)
			if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), reasonImageBuilderUnavailable) {
				t.Fatalf("create = %d %s, want 400 %s", w.Code, w.Body.String(), reasonImageBuilderUnavailable)
			}
			for _, want := range []string{"example.test/tools:1", "recommended", "stock agent image", "convention image where a builder is wired", "needs no model provider"} {
				if !strings.Contains(w.Body.String(), want) {
					t.Errorf("body = %s, want it to name %q", w.Body.String(), want)
				}
			}
		})
	}
}

// A member's background command that includes no harness, on a workspace with no
// base image of its own, starts from the stock agent image: the image the same
// command gets with no workspace attached. Before, it was refused "pass --image or
// --agent", neither of which a run-mode command can carry.
func TestRunModeCommandOnRecommendedWorkspaceUsesAgentBaseImage(t *testing.T) {
	ws := baseImageWorkspace("", "")
	ws.BaseImage = nil
	srv := noBuilderFixture(t, ws)
	body := fmt.Sprintf(`{"title":"t","experience":"background","workload":{"kind":"command","command":"echo hi"},"workspace_id":%q}`, ws.ID.String())
	w := doSSO(t, srv, http.MethodPost, "/api/v1/runs", govSession(t, "sub-member", []string{"eng"}, false), body)
	if w.Code != http.StatusCreated {
		t.Fatalf("create = %d, want 201: %s", w.Code, w.Body.String())
	}
	st := srv.cfg.Store.(*runImageStore)
	select {
	case got := <-st.images:
		if want := agentImage("base", srv.cfg.AgentImages); got != want {
			t.Errorf("run image = %q, want the agent base image %q", got, want)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the launch never resolved the run's image")
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	for _, r := range st.runs {
		if r.Agent != "" || r.ModelProviderID != "" {
			t.Errorf("run agent=%q provider=%q, want neither: the command includes no harness", r.Agent, r.ModelProviderID)
		}
	}
}

// The no-model proof for a command that includes no harness: at dispatch it gets
// no model provider's env, no key injection and no provider host on its egress,
// even with a provider on the run whose credential nobody holds (evaluated, it
// would be refused).
func TestCommandWithoutHarnessGetsNoModelCredential(t *testing.T) {
	s := noBuilderFixture(t, baseImageWorkspace("", "unused"))
	rec := s.cfg.Audit.(*recRecorder)
	sc := types.SiteConfig{ModelProviders: &types.ModelProviders{Providers: []types.ModelProvider{{
		ID: "corp-gw", UID: "uid-gw", Kind: types.ModelProviderCustomEndpoint, BaseURL: "https://llm.corp.test",
		Harnesses: []types.ProviderHarness{{Harness: "claude-code"}},
	}}}}
	for _, agent := range []string{"", "claude-code"} {
		run := types.AgentRun{ID: uuid.New(), Agent: agent, Task: "echo hi", ModelProviderID: "corp-gw"}
		policy := &types.RunPolicySpec{}
		env := map[string]string{}
		llms, _, grants, _, ok := s.resolveProviderLane(context.Background(), run, dispatchParams{TaskMode: "exec"}, policy, env, nil, "http://wardyn-proxy:3128", sc, true)
		if !ok || len(llms) != 0 || len(grants) != 0 || len(env) != 0 || len(policy.AllowedDomains) != 0 {
			t.Errorf("agent %q: ok=%v llms=%d grants=%v env=%v domains=%v, want ok and nothing model-shaped", agent, ok, len(llms), grants, env, policy.AllowedDomains)
		}
	}
	if len(rec.events) != 0 {
		t.Errorf("audit rows = %+v, want none: no provider was evaluated", rec.events)
	}
}
