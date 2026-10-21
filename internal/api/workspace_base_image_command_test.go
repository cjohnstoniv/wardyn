// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// noBuilderFixture is seedImageFixture with no image builder wired: the shape of a
// control plane that never started the builder (a Kubernetes install, or one
// built without -tags docker).
func noBuilderFixture(t *testing.T, ws types.Workspace) *Server {
	t.Helper()
	h := newHarness(t)
	st := &seedImageStore{govEscapeStore: newGovEscapeStore(&capStore{enf: map[string]bool{capAgent: true}}), ws: ws}
	st.workspaces = []types.Workspace{ws}
	cfg := baseTestConfig(h, st)
	cfg.Audit = &recRecorder{}
	cfg.Broker = h.broker
	cfg.Runner = &fakeRunner{}
	cfg.Secrets = &memSecrets{m: map[string][]byte{}}
	cfg.OIDC = &oidc.Authenticator{}
	cfg.DefaultPolicy = govDeployment()
	return New(cfg)
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
			for _, want := range []string{"example.test/tools:1", "recommended", "stock agent image", "needs no model provider"} {
				if !strings.Contains(w.Body.String(), want) {
					t.Errorf("body = %s, want it to name %q", w.Body.String(), want)
				}
			}
		})
	}
}
