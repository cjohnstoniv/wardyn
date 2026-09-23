// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"net/http"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// P2-5 (T-25, the 0.8 testing-gap audit's gaps-planned-08.md): S1 (M-8,
// admin_view_launch_test.go) refuses POST /runs and POST /runs/preflight from
// an Admin-view SSO session, but refuseAdminViewLaunch is called at exactly
// those two sites. The design (admin-member-modes-design.md §2.5, §6 QM-10)
// names three OTHER launch doors that are deliberately left untouched — Record,
// the site-config probes, and harness login — because they are operator
// controls, not a member's own run. Each test below proves the request still
// reaches that door's OWN next gate, never S1's 409 admin_view, for an
// Admin-view session.
//
// recordSrvWithOIDC duplicates record_test.go's own newTestSrv with one
// difference — cfg.OIDC set — because the SSO cookie branch of
// humanOrAdminAuth only activates when an *oidc.Authenticator is configured (a
// nil Config.OIDC never reaches a cookie at all, the same reason
// membermode_test.go's own fixtures set it).

func recordSrvWithOIDC(t *testing.T, fake store.Store) *Server {
	t.Helper()
	h := newHarness(t)
	cfg := baseTestConfig(h, fake)
	cfg.OIDC = &oidc.Authenticator{}
	return New(cfg)
}

// noProfileProbeStore adds the one store.Store method site_config_probe_test.go's
// *probeStore never needed until now: a security_admin's ceiling resolution
// (resolveDispatchCeiling, governance.go) reads it even where a plain admin's
// does not, and *probeStore leaves it unimplemented (a nil-embed panic, not a
// compile error, since it satisfies store.Store only via an embedded
// interface). "No governance profile configured" is capStore's own answer
// (capabilities_test.go) to the same call.
type noProfileProbeStore struct{ *probeStore }

func (noProfileProbeStore) ResolveGovernanceProfile(context.Context, []string, []string) (
	*types.GovernanceProfile, types.CapabilitySubjectType, error,
) {
	return nil, "", nil
}

// HasGroupTierAssignments is the ceiling resolver's second read on a
// security_admin's group snapshot (governance.go's ceilingWithUnusableGroups) —
// "no group carries a tier assignment", capStore's own no-groups answer.
func (noProfileProbeStore) HasGroupTierAssignments(context.Context) (bool, error) {
	return false, nil
}

// TestRecordWorkspace_LaunchesFromAdminView: Record (operatorOnly, admin
// role only — isOperator, http.go) still launches for an admin sitting in the
// Admin view. No runner is configured, so the proof is reaching the SAME 503
// TestRecordWorkspace_Guards pins for the admin token — not S1's 409.
func TestRecordWorkspace_LaunchesFromAdminView(t *testing.T) {
	wsID := uuid.New()
	ws := types.Workspace{
		ID:      wsID,
		Sources: []types.WorkspaceSource{{Type: types.WorkspaceSourceTypeLocalDir, Path: "/w", Target: "/home/agent/work"}},
		Status:  types.WorkspaceScanned,
	}
	fake := &recordStore{importStateFake: importStateFake{ws: ws}}
	srv := recordSrvWithOIDC(t, fake)
	url := "/api/v1/workspaces/" + wsID.String() + "/record"
	cookie := memberModeSSOSession(t, "sub-admin-view-record", "av-record@corp.example", oidc.RoleAdmin, false)

	w := doSSO(t, srv, http.MethodPost, url, cookie, `{"name":"build & test"}`)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 (no runner — reached past S1, not refused by it); body=%s",
			w.Code, w.Body.String())
	}
}

// TestSiteConfigProbes_LaunchFromAdminView: the two site-config probes
// (securityOps: admin OR security_admin, requireSecurityOperator) still launch
// for both tiers sitting in the Admin view.
func TestSiteConfigProbes_LaunchFromAdminView(t *testing.T) {
	for _, door := range []struct {
		path string
		body string
	}{
		{"/api/v1/site-config/test-proxy", "{}"},
		{"/api/v1/site-config/test-redirect", `{"from":"registry.npmjs.org","to":"artifactory.corp/npm"}`},
	} {
		t.Run(door.path, func(t *testing.T) {
			for _, role := range []string{oidc.RoleAdmin, oidc.RoleSecurityAdmin} {
				t.Run(role, func(t *testing.T) {
					fr := &probeFakeRunner{exitCode: 0}
					h := newHarness(t)
					ps := newProbeStore(redirectSiteConfig())
					cfg := baseTestConfig(h, noProfileProbeStore{ps})
					cfg.Audit = ps
					cfg.Runner = fr
					cfg.OIDC = &oidc.Authenticator{}
					srv := New(cfg)

					cookie := memberModeSSOSession(t, "sub-admin-view-probe-"+role, "av-probe@corp.example", role, false)
					w := doSSO(t, srv, http.MethodPost, door.path, cookie, door.body)
					if w.Code != http.StatusOK {
						t.Fatalf("status = %d, want 200 (reached past S1, not refused by it); body=%s",
							w.Code, w.Body.String())
					}
				})
			}
		})
	}
}

// TestHarnessLogin_LaunchesFromAdminView: the shared harness-login launch
// (humanOrAdminAuth; authorizeHarnessLogin's isOperator arm, admin only — the
// design's QM-10 "the shared harness login stays admin until MP-4b") still
// launches for an admin sitting in the Admin view.
func TestHarnessLogin_LaunchesFromAdminView(t *testing.T) {
	srv, _ := perUserLoginSrvWithRunner(t, &fakeRunner{}, perUserAWSRow())
	cookie := memberModeSSOSession(t, "sub-admin-view-harness-login", "av-login@corp.example", oidc.RoleAdmin, false)

	w := doSSO(t, srv, http.MethodPost, "/api/v1/setup/harness-login", cookie, `{"provider":"aws"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (reached past S1, not refused by it); body=%s", w.Code, w.Body.String())
	}
}
