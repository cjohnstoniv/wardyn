// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"net/http"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// newSSOUploadSrvWith wires the upload handler over an explicit login-run audit
// trail and an explicit LIVE roster, so a test can make the two disagree — the
// shape finding 3 is about.
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

// sharedClaudeRow is the roster an admin flips TO mid-run: one model credential
// for everybody, which resolves to the operator namespace for every caller.
func sharedClaudeRow() types.SiteConfig {
	return agentRoster(types.AgentProvider{ID: "claude-code", Mechanism: types.AgentMechanismBedrockSSO})
}

// TestUploadSSOToken_ScopeIsBoundToLaunchNotUploadTime pins that the scope is
// fixed at launch time, not re-resolved at upload time.
//
// handleUploadSSOToken must not re-resolve the credential scope from the live
// roster. A login run stays alive to harnessLoginIdleCap, so an admin flipping
// the row from per_user to shared inside that window would turn a member's
// still-running sandbox's PUT into a write of the OPERATOR-WIDE reserved
// harness name — the one credential every later Bedrock run inherits, with an
// account_id and role_name the blob is free to name (repoFieldSafe only).
//
// Region and start_url were already bound to launch-time state; the scope is
// the third field, and it now rides the same carrier (this run's own
// harness.login.start row).
func TestUploadSSOToken_ScopeIsBoundToLaunchNotUploadTime(t *testing.T) {
	// mintRunToken mints the identity with this subject; launchHarnessLoginRun
	// stamps the same value as the launch-time owner.
	const subject = "alice@example.com"
	runID := uuid.New()
	srv, sec, tok := newSSOUploadSrvWith(t,
		ssoLoginStartedPerUser(runID, ssoLaunchPortal, subject),
		sharedClaudeRow(), // THE FLIP: the roster now says shared
		runID)

	w := do(t, srv, http.MethodPut, "/api/v1/internal/sso-token/"+runID.String(), tok, validSSOBody)
	if w.Code != http.StatusNoContent {
		t.Fatalf("upload: code = %d, want 204; body=%s", w.Code, w.Body.String())
	}
	if _, ok := sec.m[harnessCredSecretName(awsSSOProvider)]; ok {
		t.Fatal("a roster edit made a MEMBER's capture overwrite the operator-wide credential every run inherits")
	}
	if _, ok := sec.owned[subject][harnessCredSecretName(awsSSOProvider)]; !ok {
		t.Fatalf("the capture did not land in the launch-time owner's namespace; owned=%v", sec.owned)
	}
}

// TestUploadSSOToken_UnstampedRunUnderPerUserRefused: the other half. A login
// run launched BEFORE the stamp existed carries no launch-time scope, so on a
// deployment whose row now reads per_user the server cannot prove whose
// namespace it was authorized to write — and the only fallback available is the
// operator's. Refused, never guessed.
//
// The same unstamped run on a roster that does NOT read per_user is an
// operator's by construction (authorizeHarnessLogin lets nobody else launch
// one), and stays byte-for-byte what it was.
func TestUploadSSOToken_UnstampedRunUnderPerUserRefused(t *testing.T) {
	perUser := agentRoster(types.AgentProvider{
		ID: "claude-code", Mechanism: types.AgentMechanismBedrockSSO,
		CredentialSource: types.CredentialSourcePerUser, SSOStartURL: ssoLaunchPortal,
	})
	for name, c := range map[string]struct {
		site     types.SiteConfig
		wantCode int
	}{
		"per_user now": {site: perUser, wantCode: http.StatusConflict},
		"shared now":   {site: sharedClaudeRow(), wantCode: http.StatusNoContent},
		"no roster":    {site: types.SiteConfig{}, wantCode: http.StatusNoContent},
	} {
		t.Run(name, func(t *testing.T) {
			runID := uuid.New()
			srv, sec, tok := newSSOUploadSrvWith(t, ssoLoginStartedEvents(runID, ssoLaunchPortal), c.site, runID)
			w := do(t, srv, http.MethodPut, "/api/v1/internal/sso-token/"+runID.String(), tok, validSSOBody)
			if w.Code != c.wantCode {
				t.Fatalf("code = %d, want %d; body=%s", w.Code, c.wantCode, w.Body.String())
			}
			_, stored := sec.m[harnessCredSecretName(awsSSOProvider)]
			if want := c.wantCode == http.StatusNoContent; stored != want {
				t.Errorf("operator-namespace capture = %v, want %v", stored, want)
			}
			if c.wantCode == http.StatusConflict && len(sec.owned) != 0 {
				t.Errorf("a refused upload still wrote a namespace: %v", sec.owned)
			}
		})
	}
}
