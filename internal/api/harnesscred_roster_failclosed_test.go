// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"net/http"
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
