// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// newSSOUploadSrvWith wires the upload handler over an explicit login-run audit
// trail and an explicit LIVE site config (zero = the default upload
// provider's), so a test can make the two disagree.
func newSSOUploadSrvWith(t *testing.T, events []types.AuditEvent, site types.SiteConfig, runID uuid.UUID) (*Server, *memSecrets, string) {
	t.Helper()
	h := newHarness(t)
	st := ssoLoginRunStore{
		run:     types.AgentRun{ID: runID, Task: harnessLoginTask, Agent: awsSSOAgent},
		events:  events,
		siteCfg: site,
	}
	sec := &memSecrets{m: map[string][]byte{}}
	cfg := baseTestConfig(h, st)
	cfg.Secrets = sec
	cfg.BedrockRegion = "us-west-2"
	srv := New(cfg)
	h.srv = srv
	return srv, sec, h.mintRunToken(t, runID)
}

const ssoLaunchPortal = "https://my-sso.awsapps.com/start"

// TestUploadSSOToken_ScopeIsBoundToLaunchNotUploadTime pins that the scope is
// fixed at launch time, not re-resolved at upload time: the capture lands in
// the launcher's own namespace under the provider's name the stamp recorded,
// whatever else the live configuration now says — never the operator's.
func TestUploadSSOToken_ScopeIsBoundToLaunchNotUploadTime(t *testing.T) {
	// mintRunToken mints the identity with this subject; launchHarnessLoginRun
	// stamps the same value as the launch-time owner.
	const subject = "alice@example.com"
	runID := uuid.New()
	site := uploadSite(defaultUploadProvider())
	site.AgentProviders = &types.AgentProviders{Agents: []types.AgentProvider{{ID: "claude-code"}}}
	srv, sec, tok := newSSOUploadSrvWith(t, ssoLoginStartedPerUser(runID, ssoLaunchPortal, subject), site, runID)

	w := do(t, srv, http.MethodPut, "/api/v1/internal/sso-token/"+runID.String(), tok, validSSOBody)
	if w.Code != http.StatusNoContent {
		t.Fatalf("upload: code = %d, want 204; body=%s", w.Code, w.Body.String())
	}
	if len(sec.m) != 0 {
		t.Fatalf("a member's capture wrote the operator namespace: %v", sec.m)
	}
	if _, ok := uploadedBlob(sec, subject); !ok {
		t.Fatalf("the capture did not land in the launch-time owner's namespace; owned=%v", sec.owned)
	}
}

// TestUploadSSOToken_UnstampedRunRefused: a login run launched before the
// conversion carries no model provider, so the server cannot say which
// provider's name, or whose namespace, it was authorized to write. Refused
// with the sentence that says so — before the binding, whose mismatch would
// not be the reason — and nothing is stored anywhere.
func TestUploadSSOToken_UnstampedRunRefused(t *testing.T) {
	runID := uuid.New()
	legacy := []types.AuditEvent{{
		ID: uuid.New(), RunID: &runID, ActorType: types.ActorSystem, Actor: "wardynd",
		Action: "harness.login.start", Target: runID.String(), Outcome: "success",
		Data: mustJSON(map[string]any{"provider": awsSSOProvider, "sso_start_url": ssoLaunchPortal,
			"credential_source": string(types.CredentialSourcePerUser), "owner": "alice@example.com"}),
	}}
	srv, sec, tok := newSSOUploadSrvWith(t, legacy, types.SiteConfig{}, runID)
	w := do(t, srv, http.MethodPut, "/api/v1/internal/sso-token/"+runID.String(), tok, validSSOBody)
	if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), ssoTokenUnstampedScopeRefusal) {
		t.Fatalf("code = %d, want 409 %q; body=%s", w.Code, ssoTokenUnstampedScopeRefusal, w.Body.String())
	}
	if len(sec.m) != 0 || len(sec.owned) != 0 {
		t.Errorf("a refused upload wrote a namespace: operator=%v owned=%v", sec.m, sec.owned)
	}
}
