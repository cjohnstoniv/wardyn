// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// grantCapStore records the grant rows persistRunGrants writes, which is where a
// forced owner_only shows.
type grantCapStore struct {
	*govEscapeStore
	grants []types.CredentialGrant
}

func (s *grantCapStore) CreateGrant(_ context.Context, g types.CredentialGrant) (types.CredentialGrant, error) {
	s.grants = append(s.grants, g)
	return g, nil
}

func grantCapFixture(t *testing.T, sc types.SiteConfig) (*Server, *grantCapStore) {
	t.Helper()
	h := newHarness(t)
	gs := newGovEscapeStore(&capStore{})
	gs.siteConfig = sc
	st := &grantCapStore{govEscapeStore: gs}
	cfg := baseTestConfig(h, st)
	cfg.Audit = &recRecorder{}
	cfg.Broker = h.broker
	cfg.Runner = &fakeRunner{}
	cfg.Secrets = &memSecrets{m: map[string][]byte{govCorpSecret: []byte("v")}}
	cfg.OIDC = &oidc.Authenticator{}
	cfg.DefaultPolicy = govDeployment()
	return New(cfg), st
}

func persistOne(t *testing.T, srv *Server, g types.GrantSpec) (grantWiring, bool) {
	t.Helper()
	return srv.persistRunGrants(context.Background(), httptest.NewRecorder(),
		httptest.NewRequest(http.MethodPost, "/api/v1/runs", nil), uuid.New(), time.Now().UTC(),
		types.RunPolicySpec{EligibleGrants: []types.GrantSpec{g}})
}

// TestADOGitGrantsReadOnlyTheOwnersRowAndDropSSH (#1429 review F3): a git_pat
// grant for an Azure DevOps host is owner_only whatever the policy said, so the
// operator namespace's token is never the fallback, in legacy open mode, with
// every Azure DevOps row turned off and with an enabled per-person Server row
// alike. An ssh_key grant for one is dropped with a warning and wires nothing.
// A GitHub host is untouched.
func TestADOGitGrantsReadOnlyTheOwnersRowAndDropSSH(t *testing.T) {
	server := func(disabled bool) types.SiteConfig {
		return providersConfig([]types.GitProvider{adoServerPAT(adoRow("ados", disabled, "https://tfs.corp.example/acme"))})
	}
	pat := func(host string) types.GrantSpec {
		return types.GrantSpec{Kind: types.GrantGitPAT, Scope: mustJSON(map[string]any{"host": host, "secret_name": "git-pat-" + slugHost(host)})}
	}
	for _, tc := range []struct {
		name string
		sc   types.SiteConfig
		host string
	}{
		{"no provider rows (legacy open mode), Services", types.SiteConfig{}, "dev.azure.com"},
		{"every Azure DevOps row turned off, Server", server(true), "tfs.corp.example"},
		{"an enabled Server row", server(false), "tfs.corp.example"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, st := grantCapFixture(t, tc.sc)
			gw, ok := persistOne(t, srv, pat(tc.host))
			if !ok || len(st.grants) != 1 {
				t.Fatalf("persistRunGrants ok=%v grants=%d", ok, len(st.grants))
			}
			if !st.grants[0].Spec.OwnerOnly {
				t.Errorf("git_pat grant for %s is not owner_only: the operator's token could serve it", tc.host)
			}
			_ = gw
		})
	}

	t.Run("a GitHub host keeps the policy's own owner_only", func(t *testing.T) {
		srv, st := grantCapFixture(t, types.SiteConfig{})
		if _, ok := persistOne(t, srv, pat("git.corp.example")); !ok {
			t.Fatal("persistRunGrants failed")
		}
		if st.grants[0].Spec.OwnerOnly {
			t.Error("a non-Azure DevOps host was made owner_only")
		}
	})
	t.Run("a host a GitHub row also names is not treated as Azure DevOps", func(t *testing.T) {
		sc := providersConfig([]types.GitProvider{
			adoServerPAT(adoRow("ados", true, "https://git.corp.example/tfs")),
			githubRow("ghes", false, "https://git.corp.example/org"),
		})
		srv, st := grantCapFixture(t, sc)
		if _, ok := persistOne(t, srv, pat("git.corp.example")); !ok {
			t.Fatal("persistRunGrants failed")
		}
		if st.grants[0].Spec.OwnerOnly {
			t.Error("the GHES host was made owner_only")
		}
	})

	t.Run("an organisation's visualstudio.com host that no row or scm_hosts entry names (#1440 R2-1)", func(t *testing.T) {
		srv, st := grantCapFixture(t, types.SiteConfig{})
		if _, ok := persistOne(t, srv, pat("acme.visualstudio.com")); !ok || len(st.grants) != 1 {
			t.Fatalf("persistRunGrants ok=%v grants=%d", ok, len(st.grants))
		}
		if !st.grants[0].Spec.OwnerOnly {
			t.Error("git_pat grant for acme.visualstudio.com is not owner_only: the operator's shared token could serve it")
		}
	})

	t.Run("ssh_key for an Azure DevOps host is dropped", func(t *testing.T) {
		srv, st := grantCapFixture(t, types.SiteConfig{})
		gw, ok := persistOne(t, srv, types.GrantSpec{Kind: types.GrantSSHKey,
			Scope: mustJSON(map[string]any{"host": "dev.azure.com", "key_secret_ref": "ssh-key-dev-azure-com"})})
		if !ok || len(st.grants) != 1 {
			t.Fatalf("ok=%v grants=%d, want the eligibility row kept", ok, len(st.grants))
		}
		if len(gw.sshGrants) != 0 || len(gw.sshEgress) != 0 {
			t.Errorf("ssh wiring = %v/%v, want none", gw.sshGrants, gw.sshEgress)
		}
		if len(gw.warnings) != 1 || gw.warnings[0] != adoSSHGrantDropped {
			t.Errorf("warnings = %v, want the Azure DevOps ssh sentence", gw.warnings)
		}
	})
	t.Run("ssh_key for github.com is still wired", func(t *testing.T) {
		srv, _ := grantCapFixture(t, types.SiteConfig{})
		gw, _ := persistOne(t, srv, types.GrantSpec{Kind: types.GrantSSHKey,
			Scope: mustJSON(map[string]any{"host": "github.com", "key_secret_ref": "ssh-key-github-com"})})
		if len(gw.sshGrants) != 1 {
			t.Errorf("ssh wiring = %v, want github.com wired", gw.sshGrants)
		}
	})
}

// TestPatLaneVetoedOnServicesHostsOfAMixedRow (#1429 review F6): a row naming
// both an Azure DevOps Services address and a Server address may keep its pat
// lane for the Server one only.
func TestPatLaneVetoedOnServicesHostsOfAMixedRow(t *testing.T) {
	mixed := providersConfig([]types.GitProvider{adoServerPAT(adoRow("ado", false,
		"https://dev.azure.com/acme", "https://tfs.corp.example/acme"))})
	pat := func(host string) types.GrantSpec {
		return types.GrantSpec{Kind: types.GrantGitPAT, Scope: mustJSON(map[string]any{"host": host, "secret_name": "p"})}
	}
	srv, _ := grantCapFixture(t, mixed)
	gw, _ := persistOne(t, srv, pat("dev.azure.com"))
	if len(gw.gitPATGrants) != 0 || len(gw.gitPATEgress) != 0 || len(gw.warnings) != 1 {
		t.Errorf("Services host on a mixed row: wiring=%v egress=%v warnings=%v, want it vetoed", gw.gitPATGrants, gw.gitPATEgress, gw.warnings)
	}
	srv, _ = grantCapFixture(t, mixed)
	gw, _ = persistOne(t, srv, pat("tfs.corp.example"))
	if len(gw.gitPATGrants) != 1 || len(gw.warnings) != 0 {
		t.Errorf("Server host on a mixed row: wiring=%v warnings=%v, want it wired", gw.gitPATGrants, gw.warnings)
	}
}

// TestEmptyADOLanesAdmitNoLegacyLane (#1429 review F7): at runtime an empty
// lane list on an Azure DevOps row is no longer the legacy pat, ssh and app
// lanes; on any other row it still is.
func TestEmptyADOLanesAdmitNoLegacyLane(t *testing.T) {
	for _, lane := range types.LegacyGitLanes {
		if laneAllowed(adoRow("ado", false, "https://dev.azure.com/acme"), lane) {
			t.Errorf("an empty-lane Azure DevOps row admits %q", lane)
		}
		if !laneAllowed(githubRow("gh", false, "https://github.com/acme"), lane) {
			t.Errorf("an empty-lane GitHub row no longer admits %q", lane)
		}
	}
}

// TestRetiredADOSharedNamesRefusedAtPutSecret (#1429 review F3): the operator
// namespace cannot be given a retired shared Azure DevOps name again — a 400 —
// while a host a GitHub row also names, another forge's name and a person's own
// namespace are still writable.
func TestRetiredADOSharedNamesRefusedAtPutSecret(t *testing.T) {
	sc := providersConfig([]types.GitProvider{
		adoServerPAT(adoRow("ados", false, "https://tfs.corp.example/acme")),
		adoServerPAT(adoRow("ados2", true, "https://git.corp.example/tfs")),
		githubRow("ghes", false, "https://git.corp.example/org"),
	})
	sec := &memSecrets{m: map[string][]byte{}}
	h, srv := secretsRBACServer(t, sec)
	srv.cfg.Store = &fakeSiteConfigStore{cfg: sc}
	_ = h
	alice := ssoSession(t, "alice", "alice@corp.example", oidc.RoleUser)
	const value = `{"value":"operator-typed-token-value"}`
	for _, tc := range []struct {
		name, secret string
		member       bool
		want         int
	}{
		{"the Services token", "git-pat-dev-azure-com", false, http.StatusBadRequest},
		{"the SSH key", "ssh-key-ssh-dev-azure-com", false, http.StatusBadRequest},
		{"its known-hosts", "known-hosts-dev-azure-com", false, http.StatusBadRequest},
		{"a Server row's token", "git-pat-tfs-corp-example", false, http.StatusBadRequest},
		{"a host a GitHub row also names", "git-pat-git-corp-example", false, http.StatusNoContent},
		{"an organisation's visualstudio.com token no row names (R2-1)", "git-pat-acme-visualstudio-com", false, http.StatusBadRequest},
		{"its known-hosts", "known-hosts-acme-visualstudio-com", false, http.StatusBadRequest},
		{"a person's own copy of it", "git-pat-acme-visualstudio-com", true, http.StatusNoContent},
		{"github.com", "git-pat-github-com", false, http.StatusNoContent},
		{"another name", "npm-token", false, http.StatusNoContent},
		{"a person's own token under the same name", "git-pat-dev-azure-com", true, http.StatusNoContent},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var w *httptest.ResponseRecorder
			if tc.member {
				w = doSSO(t, srv, http.MethodPut, "/api/v1/secrets/"+tc.secret, alice, value)
			} else {
				w = do(t, srv, http.MethodPut, "/api/v1/secrets/"+tc.secret, adminToken, value)
			}
			if w.Code != tc.want {
				t.Fatalf("PUT %s = %d, want %d; body=%s", tc.secret, w.Code, tc.want, w.Body.String())
			}
			if tc.want == http.StatusBadRequest {
				var body struct{ Reason, Error string }
				_ = json.Unmarshal(w.Body.Bytes(), &body)
				if body.Reason != reasonSecretNameReserved || !strings.Contains(body.Error, "per person") {
					t.Errorf("refusal = %s, want the reserved-name reason and the per-person sentence", w.Body.String())
				}
			}
		})
	}
}

// TestSeveralOwnPATRowsCanBeEnabled (#1429 review F4): an own_pat row has no
// sign-in, so an install migrated with several organisations can turn each
// one's row back on; two rows that DO sign in are still refused, and the setup
// check counts only those.
func TestSeveralOwnPATRowsCanBeEnabled(t *testing.T) {
	own := func(id, org string) types.GitProvider {
		return adoEntra(adoRow(id, false, "https://dev.azure.com/"+org))
	}
	for _, tc := range []struct {
		name string
		rows []types.GitProvider
		want bool
	}{
		{"two enabled own_pat rows", []types.GitProvider{own("a", "acme"), own("b", "beta")}, true},
		{"three enabled own_pat rows", []types.GitProvider{own("a", "acme"), own("b", "beta"), own("c", "gamma")}, true},
		{"an own_pat row beside a signing-in row", []types.GitProvider{own("a", "acme"),
			entraRow(func(r *types.GitProvider) { r.ID, r.BaseURLs = "b", []string{"https://dev.azure.com/beta"} })}, true},
		{"two signing-in rows are still refused", []types.GitProvider{
			entraRow(func(r *types.GitProvider) { r.ID, r.BaseURLs = "a", []string{"https://dev.azure.com/acme"} }),
			entraRow(func(r *types.GitProvider) { r.ID, r.BaseURLs = "b", []string{"https://dev.azure.com/beta"} })}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw, err := json.Marshal(types.WorkspaceProviders{Git: tc.rows})
			if err != nil {
				t.Fatal(err)
			}
			for _, door := range []struct{ path, body string }{
				{"/api/v1/workspace-providers", string(raw)},
				{"/api/v1/site-config", `{"workspace_providers":` + string(raw) + `}`},
			} {
				srv, _ := newProvidersHarness(t, &fakeProvidersStore{fakeSiteConfigStore: &fakeSiteConfigStore{}})
				w := do(t, srv, http.MethodPut, door.path, adminToken, door.body)
				if got := w.Code == http.StatusOK; got != tc.want {
					t.Errorf("PUT %s = %d, want ok=%v; body=%s", door.path, w.Code, tc.want, w.Body.String())
				}
			}
			sc := types.SiteConfig{WorkspaceProviders: &types.WorkspaceProviders{Git: tc.rows}}
			if _, warned := adoEntraRowsCheck(sc); warned == tc.want {
				t.Errorf("ado_entra_rows warned = %v for %s; it counts only rows that sign in", warned, tc.name)
			}
		})
	}
}

// TestRetiredADOSharedNamePutFailsClosed (#1440 R2-4): with the site config
// unreadable the operator's write of a retired-looking name is refused (5xx,
// nothing stored) rather than let through, since the sweep runs only once.
func TestRetiredADOSharedNamePutFailsClosed(t *testing.T) {
	sec := &memSecrets{m: map[string][]byte{}}
	_, srv := secretsRBACServer(t, sec)
	srv.cfg.Store = &fakeSiteConfigStore{getErr: context.DeadlineExceeded}
	w := do(t, srv, http.MethodPut, "/api/v1/secrets/git-pat-dev-azure-com", adminToken, `{"value":"operator-typed-token-value"}`)
	if w.Code < 500 {
		t.Fatalf("PUT with an unreadable site config = %d, want a 5xx; body=%s", w.Code, w.Body.String())
	}
	if _, ok := sec.m["git-pat-dev-azure-com"]; ok {
		t.Error("the value was stored anyway")
	}
}
