// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sync/atomic"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/secretmask"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// siteConfigErrStore answers every roster read with a transient failure — the
// store blip authorizeHarnessLogin used to swallow.
type siteConfigErrStore struct{ *integStore }

func (siteConfigErrStore) GetSiteConfig(context.Context) (types.SiteConfig, error) {
	return types.SiteConfig{}, errors.New("site config read failed")
}

// TestHarnessLogin_RosterReadFailureFailsClosed (V1-r2 lens-S S-2).
//
// authorizeHarnessLogin read the roster as `sc, _ := s.siteConfigSnapshot(...)`,
// so a transient GetSiteConfig error read as "no per_user row": the launch went
// ahead with an EMPTY pin, stamped an empty sso_account_id/sso_role_name on
// harness.login.started, and bindCaptureToPin then treated the run as "launched
// unpinned" and accepted whatever account/role the sandbox named. The same read
// failure also dropped the admin-owned SSOStartURL, so the CALLER's own
// sso_start_url became the bound portal.
//
// Every other door on this lane fails closed; this one must too — no run, no
// stamp, 503.
func TestHarnessLogin_RosterReadFailureFailsClosed(t *testing.T) {
	rnr := &fakeRunner{}
	h := newHarness(t)
	audit := &memAudit{}
	cfg := baseTestConfig(h, siteConfigErrStore{integStore: &integStore{govEscapeStore: newGovEscapeStore(&capStore{})}})
	cfg.Audit = audit
	cfg.OIDC = &oidc.Authenticator{}
	cfg.Runner = rnr
	cfg.Secrets = &memSecrets{m: map[string][]byte{}}
	cfg.MaskRegistry = secretmask.NewRegistry()
	cfg.BedrockRegion = "us-east-1"
	cfg.DefaultPolicy = govDeployment()
	srv := New(cfg)

	// An OPERATOR, with a start URL of their own: the shape that LAUNCHED
	// before this fix (a member is refused by the per_user predicate anyway).
	w := doSSO(t, srv, http.MethodPost, "/api/v1/setup/harness-login",
		ssoSession(t, "sub-admin", "admin@corp.example", oidc.RoleAdmin),
		`{"provider":"aws","sso_start_url":"https://attacker.awsapps.com/start"}`)
	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503 — an unreadable roster must not launch an UNPINNED login; body=%s", w.Code, w.Body.String())
	}
	if env := rnr.lastSandboxEnv(); env != nil {
		t.Errorf("a login sandbox was launched on an unreadable roster: env=%v", env)
	}
	if rows := audit.find("harness.login.started"); len(rows) != 0 {
		t.Errorf("harness.login.started rows = %d, want 0 — an empty stamp pin reads as 'launched unpinned' at capture", len(rows))
	}
}

// siteConfigFlakyStore answers the FIRST roster read and fails every one after
// it: the store blip that opens BETWEEN authorizeHarnessLogin's read and the
// launch (a runner RPC, the ceiling resolve and the identity mint sit in that
// window).
type siteConfigFlakyStore struct {
	*integStore
	reads atomic.Int64
}

func (s *siteConfigFlakyStore) GetSiteConfig(ctx context.Context) (types.SiteConfig, error) {
	if s.reads.Add(1) > 1 {
		return types.SiteConfig{}, errors.New("site config read failed")
	}
	return s.integStore.GetSiteConfig(ctx)
}

// TestHarnessLogin_ScopeIsTheAuthorizedReadNotASecondOne (V1-r2 lens-S2 S2-01).
//
// launchHarnessLoginRun re-resolved the credential scope through the FAIL-OPEN
// awsSSOScopeForAgent, so a roster read that failed after authorizeHarnessLogin
// had already proved the row stamped `credential_source: shared`, `owner: ""` on
// a MEMBER's login run — and loginRunScope then let that member's capture
// overwrite the deployment-wide credential every run inherits.
//
// The scope the authorized read proved is the only one this launch may stamp.
func TestHarnessLogin_ScopeIsTheAuthorizedReadNotASecondOne(t *testing.T) {
	rnr := &fakeRunner{}
	h := newHarness(t)
	audit := &memAudit{}
	st := &siteConfigFlakyStore{integStore: &integStore{
		govEscapeStore: newGovEscapeStore(&capStore{}),
		site: agentRoster(types.AgentProvider{
			ID: "claude-code", Mechanism: types.AgentMechanismBedrockSSO,
			CredentialSource: types.CredentialSourcePerUser, SSOStartURL: perUserPortal,
		}),
	}}
	cfg := baseTestConfig(h, st)
	cfg.Audit = audit
	cfg.OIDC = &oidc.Authenticator{}
	cfg.Runner = rnr
	cfg.Secrets = &memSecrets{m: map[string][]byte{}}
	cfg.MaskRegistry = secretmask.NewRegistry()
	cfg.BedrockRegion = "us-east-1"
	cfg.DefaultPolicy = govDeployment()
	srv := New(cfg)

	w := doSSO(t, srv, http.MethodPost, "/api/v1/setup/harness-login",
		ssoSession(t, "sub-member", "member@corp.example", oidc.RoleUser), `{"provider":"aws"}`)
	rows := audit.find("harness.login.started")
	if w.Code != http.StatusOK {
		// Refusing is an acceptable answer — stamping the SHARED credential is not.
		if len(rows) != 0 {
			t.Fatalf("status = %d yet a login was stamped: %s", w.Code, rows[0].Data)
		}
		return
	}
	if len(rows) != 1 {
		t.Fatalf("harness.login.started rows = %d, want 1", len(rows))
	}
	var data struct {
		CredentialSource string `json:"credential_source"`
		Owner            string `json:"owner"`
	}
	if err := json.Unmarshal(rows[0].Data, &data); err != nil {
		t.Fatalf("decode login audit data: %v", err)
	}
	if data.CredentialSource != string(types.CredentialSourcePerUser) || data.Owner != "sub-member" {
		t.Errorf("stamped credential_source=%q owner=%q, want per_user / sub-member — a store blip after the authorized read must never re-point a member's capture at the shared credential",
			data.CredentialSource, data.Owner)
	}
}

// TestHarnessDisconnect_RosterReadFailureFailsClosed (V1-r2 lens-S2 S2-08).
//
// The Disconnect door resolved its namespace through the same fail-open
// resolver: a roster read that failed read as "not per-user", so the Delete
// landed on the OPERATOR-WIDE row instead of the caller's own capture. A blip
// must refuse, not delete somebody else's credential.
func TestHarnessDisconnect_RosterReadFailureFailsClosed(t *testing.T) {
	h := newHarness(t)
	audit := &memAudit{}
	secrets := &memSecrets{m: map[string][]byte{harnessCredSecretName(awsSSOProvider): []byte(`{"access_token":"operator-wide"}`)}}
	cfg := baseTestConfig(h, siteConfigErrStore{integStore: &integStore{govEscapeStore: newGovEscapeStore(&capStore{})}})
	cfg.Audit = audit
	cfg.OIDC = &oidc.Authenticator{}
	cfg.Secrets = secrets
	cfg.MaskRegistry = secretmask.NewRegistry()
	srv := New(cfg)

	w := doSSO(t, srv, http.MethodDelete, "/api/v1/setup/harness-credential/aws",
		ssoSession(t, "sub-admin", "admin@corp.example", oidc.RoleAdmin), "")
	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503 — an unreadable roster must not decide WHOSE credential is deleted; body=%s", w.Code, w.Body.String())
	}
	if _, ok := secrets.m[harnessCredSecretName(awsSSOProvider)]; !ok {
		t.Error("the operator-wide AWS SSO row was deleted on a roster blip — the unscoped Delete is the fail-open")
	}
}
