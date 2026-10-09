// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/runner"
)

// The mint door is the one control-plane answer about a grant that reaches
// the inside of a sandbox: the proxy's own mint route forwards a grant id a
// process there posts and passes the answer back as it came. For a `shared`
// grant that answer says nothing of what the organisation's secret is called.
func TestInternalMint_ASharedGrantsAnswerNamesNoSecret(t *testing.T) {
	mint := func(s *sharedSink) *httptest.ResponseRecorder {
		return do(t, s.h.srv, http.MethodPost, "/api/v1/internal/credentials/mint", s.token, `{"grant_id":"`+s.grant.String()+`"}`)
	}
	t.Run("a shared grant", func(t *testing.T) {
		s := newSharedSink(t, sharedScope, false)
		w := mint(s)
		body := w.Body.String()
		if w.Code != http.StatusOK || !strings.Contains(body, `"jti":"jti-shared"`) || !strings.Contains(body, sharedHost) {
			t.Fatalf("mint = %d %s, want the minted rule for %s", w.Code, body, sharedHost)
		}
		if strings.Contains(body, sharedSecretName) {
			t.Errorf("the answer relayed into the sandbox names the organisation's secret: %s", body)
		}
	})
	// A grant the run's own list does not hold may be a shared one.
	t.Run("a grant whose scope cannot be read", func(t *testing.T) {
		s := newSharedSink(t, sharedScope, false)
		s.st.grants = nil
		w := mint(s)
		if body := w.Body.String(); w.Code != http.StatusOK || strings.Contains(body, sharedSecretName) {
			t.Errorf("mint = %d %s, want 200 without the secret's name", w.Code, body)
		}
	})
	t.Run("an unreadable grant list", func(t *testing.T) {
		s := newSharedSink(t, sharedScope, false)
		s.st.grantsErr = errors.New("connection refused")
		w := mint(s)
		if body := w.Body.String(); w.Code == http.StatusOK || strings.Contains(body, sharedSecretName) {
			t.Errorf("mint = %d %s, want a refusal that names nothing", w.Code, body)
		}
	})
	// Every other grant answers as it did.
	t.Run("an unshared grant", func(t *testing.T) {
		s := newSharedSink(t, `{"host":"org-api.example","require_tls":true,"secret_name":"org-tool-token"}`, false)
		w := mint(s)
		var got struct {
			Injection struct {
				Host       string `json:"host"`
				Header     string `json:"header"`
				SecretName string `json:"secret_name"`
				Format     string `json:"format"`
			} `json:"injection"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil || w.Code != http.StatusOK {
			t.Fatalf("mint = %d %s (%v), want 200", w.Code, w.Body.String(), err)
		}
		if got.Injection.SecretName != sharedSecretName || got.Injection.Host != sharedHost || got.Injection.Header != "Authorization" || got.Injection.Format != "Bearer %s" {
			t.Errorf("injection = %+v, want the rule whole", got.Injection)
		}
	})
}

// What dispatch hands the sandbox itself — its environment, its secret
// environment, its labels and the files written into it — carries no shared
// secret's name; the proxy sidecar's own config is where the rule lives.
func TestDispatch_TheSandboxIsHandedNoSharedSecretName(t *testing.T) {
	run := launchSharedRun(t)
	waitForRecAudit(t, run.f.rec, run.id, "run.exec", "success")
	fr := run.f.srv.cfg.Runner.(*fakeRunner)
	fr.mu.Lock()
	spec := fr.lastSpec
	fr.mu.Unlock()
	if spec.RunID != run.id {
		t.Fatalf("the runner was handed run %s, want %s", spec.RunID, run.id)
	}
	inside := string(mustJSON(struct {
		Env, SecretEnv, Labels map[string]string
		Mounts                 []runner.Mount
	}{spec.Env, spec.SecretEnv, spec.Labels, spec.Mounts}))
	for _, f := range spec.ManagedFiles {
		inside += " " + f.Path + " " + string(f.Content)
	}
	if strings.Contains(inside, compOperatorSecret) {
		t.Errorf("the sandbox is handed the organisation's secret name: %s", inside)
	}
	var rule bool
	for _, in := range spec.ProxyConfig.Injection {
		rule = rule || in.Rule.Host == "org-api.example"
	}
	if !rule {
		t.Errorf("the proxy's config carries no rule for the shared grant's host: %+v", spec.ProxyConfig.Injection)
	}
}
