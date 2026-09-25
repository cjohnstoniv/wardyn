// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
)

// A read the git_credential gate needs that FAILS is a daemon fault: 503
// roster_unreadable at every launch door, 5xx on /me/scm-access, and never
// the 422 "you are not connected" whose remedy (reconnect) is wrong for it.

var errSCMTestWedged = errors.New("secret store wedged")

// assertGitCredential503 is the unreadable twin of assertGitCredential422.
func assertGitCredential503(t *testing.T, w *httptest.ResponseRecorder) {
	t.Helper()
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503; body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, `"reason":"roster_unreadable"`) || !strings.Contains(body, adoResolveRosterUnreadable) {
		t.Errorf("body = %s, want reason roster_unreadable and its sentence", body)
	}
	if strings.Contains(body, "not connected") || strings.Contains(body, errSCMTestWedged.Error()) {
		t.Errorf("body = %s, must neither blame the person nor carry the store's error text", body)
	}
}

// wedgedADORunHarness is adoRunHarness over a secret store that cannot answer.
func wedgedADORunHarness(t *testing.T) (*Server, *ownerStore, *fakeRunner) {
	t.Helper()
	srv, st, fr := adoRunHarness(t, true)
	srv.cfg.Secrets = wedgedSecrets{err: errSCMTestWedged}
	return srv, st, fr
}

func TestGitCredentialGate_WedgedSecretStore_CreateRun503(t *testing.T) {
	srv, _, fr := wedgedADORunHarness(t)
	body := `{"agent":"claude-code","task":"do the thing","repo":"` + scmTestADORepo + `"}`
	w := doSSO(t, srv, http.MethodPost, "/api/v1/runs", adoOperatorSession(t), body)
	assertGitCredential503(t, w)
	if fr.createCalls != 0 {
		t.Errorf("CreateSandbox calls = %d, want 0", fr.createCalls)
	}
}

func TestGitCredentialGate_WedgedSecretStore_RecordSession503(t *testing.T) {
	srv, st, _ := wedgedADORunHarness(t)
	id := adoWorkspaceWithADORepo(t, st)
	w := doSSO(t, srv, http.MethodPost, "/api/v1/workspaces/"+id+"/record", adoOperatorSession(t), `{"name":"e2e record"}`)
	assertGitCredential503(t, w)
}

// The ADOEntra source reads the site config too; its failure must not make
// the row look "not per-user" and wave the launch through ungraded.
func TestGitCredentialRefusal_UnreadableSigninConfig503(t *testing.T) {
	s := newSCMTestServer(t, adoTestSiteConfig(false), true)
	s.cfg.ADOEntra = func(context.Context) (ADOEntraConfig, bool, error) {
		return ADOEntraConfig{}, false, errSCMTestWedged
	}
	r := httptest.NewRequest(http.MethodPost, "/api/v1/runs", nil)
	r = r.WithContext(withOIDCHuman(r.Context(), "alice"))
	w := httptest.NewRecorder()
	if !s.gitCredentialRefusal(w, r, scmTestADORepo) {
		t.Fatal("admitted a launch whose sign-in configuration could not be read")
	}
	assertGitCredential503(t, w)
}

func TestHandleGetSCMAccess_UnreadableAnswers5xx(t *testing.T) {
	cases := map[string]func(*Server){
		"secret store": func(s *Server) { s.cfg.Secrets = wedgedSecrets{err: errSCMTestWedged} },
		"site config":  func(s *Server) { s.cfg.Store.(*scmTestStore).err = errSCMTestWedged },
		"signin config": func(s *Server) {
			s.cfg.ADOEntra = func(context.Context) (ADOEntraConfig, bool, error) { return ADOEntraConfig{}, false, errSCMTestWedged }
		},
	}
	for name, wedge := range cases {
		t.Run(name, func(t *testing.T) {
			s := newSCMTestServer(t, adoTestSiteConfig(false), true)
			wedge(s)
			r := httptest.NewRequest(http.MethodGet, "/api/v1/me/scm-access", nil)
			r = r.WithContext(withOIDCHuman(r.Context(), "alice"))
			w := httptest.NewRecorder()
			s.handleGetSCMAccess(w, r)
			if w.Code < 500 {
				t.Fatalf("status = %d body %s, want 5xx — never a graded state or an empty array", w.Code, w.Body.String())
			}
			if strings.Contains(w.Body.String(), errSCMTestWedged.Error()) {
				t.Errorf("body = %s, must not carry the store's error text", w.Body.String())
			}
		})
	}
}

// The informational surfaces leave an unreadable fact out rather than grade it.
func TestSCMAccessInformational_UnreadableLeavesTheFactOut(t *testing.T) {
	s := newSCMTestServer(t, adoTestSiteConfig(false), true)
	s.cfg.Secrets = wedgedSecrets{err: errSCMTestWedged}
	if v := s.scmAccessValue(context.Background(), adoTestSiteConfig(false), "alice"); v != (SCMAccess{}) {
		t.Errorf("scmAccessValue = %+v, want the zero value", v)
	}
	if gc := s.gitCredentialFactForRepos(context.Background(), "alice", []string{scmTestADORepo}); gc != nil {
		t.Errorf("gitCredentialFactForRepos = %+v, want nil", gc)
	}
}

func TestADOCaptureScopes(t *testing.T) {
	ceiling := []string{"vso.code", "vso.work"}
	cases := []struct {
		granted string
		want    []string
	}{
		{"vso.work offline_access vso.code", []string{"vso.code", "vso.work"}},
		{"vso.code\topenid", []string{"vso.code"}},
		{"offline_access openid", nil},
		{"", nil},
	}
	for _, tc := range cases {
		if got := adoCaptureScopes(tc.granted, ceiling); !slices.Equal(got, tc.want) {
			t.Errorf("adoCaptureScopes(%q) = %v, want %v", tc.granted, got, tc.want)
		}
	}
}
