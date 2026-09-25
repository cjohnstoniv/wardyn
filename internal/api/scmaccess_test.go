// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/adoscope"
	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/secretmask"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// adoAccessState: the pure per-user-row grader

func TestAdoAccessState(t *testing.T) {
	cases := []struct {
		name               string
		isMechanism, found bool
		want               string
	}{
		{"human, found: live", false, true, modelAccessLive},
		{"human, not found: not_configured", false, false, modelAccessNotConfigured},
		{"mechanism caller: not_applicable, regardless of found", true, true, modelAccessNotApplicable},
		{"mechanism caller, nothing found: still not_applicable", true, false, modelAccessNotApplicable},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := adoAccessState(tc.isMechanism, tc.found); got != tc.want {
				t.Errorf("adoAccessState(%v,%v) = %q, want %q", tc.isMechanism, tc.found, got, tc.want)
			}
		})
	}
}

func TestScmAccessSourceFor(t *testing.T) {
	cases := map[string]string{
		adoEntraSourceLogin:  scmAccessSourceOrg,
		adoEntraSourceSignIn: scmAccessSourceSeparate,
		"":                   "",
		"something-unknown":  "",
	}
	for in, want := range cases {
		if got := scmAccessSourceFor(in); got != want {
			t.Errorf("scmAccessSourceFor(%q) = %q, want %q", in, got, want)
		}
	}
}

// fixtures shared by every test below

// scmTestStore is the minimal store.Store the PURE grading functions read:
// one SiteConfig, nothing else — the embed answers every other method with a
// nil panic no test here should ever reach. The F5/F4 HTTP-level tests below
// use ownerStore instead (adoRunHarness): a real request reaches many more
// Store methods (CreateRun, ListRoleMappings, ...) than this double answers.
type scmTestStore struct {
	store.Store
	site types.SiteConfig
	err  error // a failed site-config read, when set
}

func (s *scmTestStore) GetSiteConfig(context.Context) (types.SiteConfig, error) { return s.site, s.err }

// The capability reads: no grant rows and no switch, so a member caller is
// offered every row, as a deployment that adopted no grants offers it.
func (s *scmTestStore) ListCapabilityGrantsFor(context.Context, []string, []string) ([]types.CapabilityGrant, error) {
	return nil, nil
}
func (s *scmTestStore) ListGroupDenyGrants(context.Context, string) ([]types.CapabilityGrant, error) {
	return nil, nil
}
func (s *scmTestStore) GetCapabilityEnforcement(context.Context) (map[string]bool, error) {
	return map[string]bool{}, nil
}

const scmTestRowID = "ado-row-1"

// scmTestBaseline is what a sign-in on these rows must cover to be live: the
// scopes of the default (read) profile.
func scmTestBaseline(t *testing.T) []string {
	t.Helper()
	scopes, err := adoscope.ScopesFor(adoscope.ProfileRead())
	if err != nil {
		t.Fatal(err)
	}
	return scopes
}

const scmTestADORepo = "https://dev.azure.com/contoso/proj/_git/repo"
const scmTestADOOrg = "https://dev.azure.com/contoso"

func adoTestSiteConfig(disabled bool) types.SiteConfig {
	return types.SiteConfig{WorkspaceProviders: &types.WorkspaceProviders{Git: []types.GitProvider{{
		ID: scmTestRowID, Kind: types.GitProviderAzureDevOps, Disabled: disabled,
		BaseURLs: []string{scmTestADOOrg},
	}}}}
}

// newSCMTestServer builds a Server whose Azure DevOps row (id scmTestRowID)
// is per-user ONLY when entra is true — a nil ADOEntra source otherwise,
// which is exactly what an un-configured (shared/PAT) row looks like from
// this package's own seam (#383 has not landed; see scmaccess.go's file
// doc).
func newSCMTestServer(t *testing.T, sc types.SiteConfig, entra bool) *Server {
	t.Helper()
	cfg := Config{
		Store:        &scmTestStore{site: sc},
		Secrets:      &memSecrets{m: map[string][]byte{}},
		MaskRegistry: secretmask.NewRegistry(),
		Now:          func() time.Time { return adoTestNow },
	}
	if entra {
		cfg.ADOEntra = adoTestEntraSource
	}
	return &Server{cfg: cfg}
}

func adoTestEntraSource(context.Context) (ADOEntraConfig, bool, error) {
	return ADOEntraConfig{
		RowID: scmTestRowID, TenantID: "tenant-1", ClientID: "client-1",
		RedirectURL: "https://console.example.invalid/cb", Scopes: []string{"vso.code"},
		LoginClientID: "client-1", LoginTenantID: "tenant-1",
	}, true, nil
}

// scmAccessRows is computeSCMAccessRowsFor for a case whose reads succeed.
func scmAccessRows(t *testing.T, s *Server, ctx context.Context, sc types.SiteConfig, subject string) []SCMAccess {
	t.Helper()
	rows, err := s.computeSCMAccessRowsFor(ctx, sc, subject)
	if err != nil {
		t.Fatalf("computeSCMAccessRowsFor: %v", err)
	}
	return rows
}

// computeSCMAccessRowsFor / scmAccessValue — row-shaped grading

func TestComputeSCMAccessRowsFor(t *testing.T) {
	t.Run("no Azure DevOps row: empty", func(t *testing.T) {
		s := newSCMTestServer(t, types.SiteConfig{}, false)
		rows := scmAccessRows(t, s, context.Background(), types.SiteConfig{}, "alice")
		if len(rows) != 0 {
			t.Fatalf("got %+v, want none", rows)
		}
	})

	t.Run("a disabled row is the same as no row", func(t *testing.T) {
		sc := adoTestSiteConfig(true)
		s := newSCMTestServer(t, sc, true)
		rows := scmAccessRows(t, s, context.Background(), sc, "alice")
		if len(rows) != 0 {
			t.Fatalf("got %+v, want none", rows)
		}
	})

	t.Run("a SHARED row (no entra) is never graded (F4) — not shared_expired, not live, nothing", func(t *testing.T) {
		sc := adoTestSiteConfig(false)
		s := newSCMTestServer(t, sc, false)
		rows := scmAccessRows(t, s, context.Background(), sc, "alice")
		if len(rows) != 0 {
			t.Fatalf("got %+v, want none — a plain PAT row must report nothing, not shared_expired", rows)
		}
	})

	t.Run("a shared row with a stored git-pat- secret STILL reports nothing (F4)", func(t *testing.T) {
		sc := adoTestSiteConfig(false)
		s := newSCMTestServer(t, sc, false)
		s.cfg.Secrets.(*memSecrets).m["git-pat-dev-azure-com"] = []byte("x")
		rows := scmAccessRows(t, s, context.Background(), sc, "alice")
		if len(rows) != 0 {
			t.Fatalf("got %+v, want none — secret presence must never be graded into a state (F4)", rows)
		}
	})

	t.Run("per-user row, never signed in: one row, not_configured, row-is-newer cause", func(t *testing.T) {
		sc := adoTestSiteConfig(false)
		s := newSCMTestServer(t, sc, true)
		rows := scmAccessRows(t, s, context.Background(), sc, "alice")
		if len(rows) != 1 || rows[0].State != modelAccessNotConfigured || rows[0].Cause != scmAccessCauseRowIsNewer || rows[0].Kind != string(types.GitProviderAzureDevOps) {
			t.Fatalf("got %+v, want one row state=not_configured cause=row_is_newer kind=azure_devops", rows)
		}
	})

	t.Run("per-user row, a captured sign-in: live, sourced from the blob", func(t *testing.T) {
		sc := adoTestSiteConfig(false)
		s := newSCMTestServer(t, sc, true)
		ctx := context.Background()
		if err := s.storeADOEntraBlob(ctx, "alice", scmTestRowID, adoEntraBlob{
			RefreshToken: "rt", Scopes: scmTestBaseline(t), TenantID: "tenant-1", ClientID: "client-1",
			Subject: "alice", Source: adoEntraSourceLogin,
		}); err != nil {
			t.Fatalf("store blob: %v", err)
		}
		rows := scmAccessRows(t, s, ctx, sc, "alice")
		if len(rows) != 1 || rows[0].State != modelAccessLive || rows[0].Source != scmAccessSourceOrg {
			t.Fatalf("got %+v, want one row state=live source=org", rows)
		}
	})

	t.Run("per-user row, a DIFFERENT person's captured sign-in is invisible", func(t *testing.T) {
		sc := adoTestSiteConfig(false)
		s := newSCMTestServer(t, sc, true)
		ctx := context.Background()
		if err := s.storeADOEntraBlob(ctx, "bob", scmTestRowID, adoEntraBlob{
			RefreshToken: "rt", Scopes: scmTestBaseline(t), TenantID: "tenant-1", ClientID: "client-1",
			Subject: "bob", Source: adoEntraSourceSignIn,
		}); err != nil {
			t.Fatalf("store blob: %v", err)
		}
		rows := scmAccessRows(t, s, ctx, sc, "alice")
		if len(rows) != 1 || rows[0].State != modelAccessNotConfigured {
			t.Fatalf("alice read bob's connection: got %+v", rows)
		}
	})

	t.Run("per-user row, a mechanism caller (no OIDC subject): not_applicable", func(t *testing.T) {
		sc := adoTestSiteConfig(false)
		s := newSCMTestServer(t, sc, true)
		rows := scmAccessRows(t, s, context.Background(), sc, "")
		if len(rows) != 1 || rows[0].State != modelAccessNotApplicable {
			t.Fatalf("got %+v, want one row state=not_applicable", rows)
		}
	})
}

func TestScmAccessValue(t *testing.T) {
	t.Run("no row: the zero value", func(t *testing.T) {
		s := newSCMTestServer(t, types.SiteConfig{}, false)
		if v := s.scmAccessValue(context.Background(), types.SiteConfig{}, "alice"); v.State != "" {
			t.Fatalf("got %+v, want the zero value", v)
		}
	})
	t.Run("one per-user row: that row", func(t *testing.T) {
		sc := adoTestSiteConfig(false)
		s := newSCMTestServer(t, sc, true)
		v := s.scmAccessValue(context.Background(), sc, "alice")
		if v.State != modelAccessNotConfigured || v.Kind != string(types.GitProviderAzureDevOps) {
			t.Fatalf("got %+v", v)
		}
	})
}

// the wire handler: GET /me/scm-access is an array (review finding F6)

func TestHandleGetSCMAccess(t *testing.T) {
	sc := adoTestSiteConfig(false)
	s := newSCMTestServer(t, sc, true)
	r := httptest.NewRequest(http.MethodGet, "/api/v1/me/scm-access", nil)
	r = r.WithContext(withOIDCHuman(r.Context(), "alice"))
	w := httptest.NewRecorder()
	s.handleGetSCMAccess(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status %d body %q", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.HasPrefix(strings.TrimSpace(body), "[") {
		t.Fatalf("body = %s, want a JSON array (F6)", body)
	}
	if !strings.Contains(body, `"kind":"azure_devops"`) || !strings.Contains(body, `"state":"not_configured"`) {
		t.Errorf("body = %s", body)
	}
	// review follow-up N3: no row id in a member-visible body.
	if strings.Contains(body, "row_id") {
		t.Errorf("body = %s, must not carry a row id", body)
	}
}

func TestHandleGetSCMAccess_NoRowConfigured_AnswersEmptyArrayNot404(t *testing.T) {
	s := newSCMTestServer(t, types.SiteConfig{}, false)
	r := httptest.NewRequest(http.MethodGet, "/api/v1/me/scm-access", nil)
	r = r.WithContext(withOIDCHuman(r.Context(), "alice"))
	w := httptest.NewRecorder()
	s.handleGetSCMAccess(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status %d", w.Code)
	}
	if body := strings.TrimSpace(w.Body.String()); body != "[]" {
		t.Errorf("body = %s, want the empty array", body)
	}
}

// the informational, per-run preflight fact (review finding F2)

func TestGitCredentialFactForRepos(t *testing.T) {
	sc := adoTestSiteConfig(false)
	s := newSCMTestServer(t, sc, true)
	ctx := withOIDCHuman(context.Background(), "alice")

	t.Run("a repo on the per-user row, never signed in: the fact, not a refusal", func(t *testing.T) {
		gc := s.gitCredentialFactForRepos(ctx, "alice", []string{scmTestADORepo})
		if gc == nil || gc.State != modelAccessNotConfigured {
			t.Fatalf("got %+v, want not_configured", gc)
		}
	})

	t.Run("no repos on any Azure DevOps row: nil, not a guess", func(t *testing.T) {
		if gc := s.gitCredentialFactForRepos(ctx, "alice", []string{"https://github.com/acme/widgets"}); gc != nil {
			t.Fatalf("got %+v, want nil", gc)
		}
	})

	t.Run("no repos at all: nil", func(t *testing.T) {
		if gc := s.gitCredentialFactForRepos(ctx, "alice", nil); gc != nil {
			t.Fatalf("got %+v, want nil", gc)
		}
	})

	t.Run("a shared row's repo: nil (F4) — never a shared_expired guess", func(t *testing.T) {
		sharedSC := adoTestSiteConfig(false)
		sharedSrv := newSCMTestServer(t, sharedSC, false)
		if gc := sharedSrv.gitCredentialFactForRepos(ctx, "alice", []string{scmTestADORepo}); gc != nil {
			t.Fatalf("got %+v, want nil", gc)
		}
	})
}

// the launch door's 422 (review findings F1, F7)

func TestGitCredentialRefusal(t *testing.T) {
	t.Run("per-user row, no captured sign-in: 422 with the org, no row id (F1, N3)", func(t *testing.T) {
		sc := adoTestSiteConfig(false)
		s := newSCMTestServer(t, sc, true)
		r := httptest.NewRequest(http.MethodPost, "/api/v1/runs", nil)
		r = r.WithContext(withOIDCHuman(r.Context(), "alice"))
		w := httptest.NewRecorder()
		if !s.gitCredentialRefusal(w, r, scmTestADORepo) {
			t.Fatalf("want refused")
		}
		if w.Code != http.StatusUnprocessableEntity {
			t.Errorf("status = %d, want 422", w.Code)
		}
		body := w.Body.String()
		for _, want := range []string{
			`"reason":"git_credential"`,
			`"org":"` + scmTestADOOrg + `"`,
			gitCredentialNotConnectedRefusal,
		} {
			if !strings.Contains(body, want) {
				t.Errorf("body = %s, want it to contain %s", body, want)
			}
		}
		if strings.Contains(body, "row_id") {
			t.Errorf("body = %s, must not carry a row id (N3)", body)
		}
	})

	t.Run("per-user row, a captured sign-in: not refused", func(t *testing.T) {
		sc := adoTestSiteConfig(false)
		s := newSCMTestServer(t, sc, true)
		ctx := context.Background()
		if err := s.storeADOEntraBlob(ctx, "alice", scmTestRowID, adoEntraBlob{
			RefreshToken: "rt", Scopes: scmTestBaseline(t), TenantID: "tenant-1", ClientID: "client-1",
			Subject: "alice", Source: adoEntraSourceSignIn,
		}); err != nil {
			t.Fatalf("store blob: %v", err)
		}
		r := httptest.NewRequest(http.MethodPost, "/api/v1/runs", nil).WithContext(withOIDCHuman(ctx, "alice"))
		w := httptest.NewRecorder()
		if s.gitCredentialRefusal(w, r, scmTestADORepo) {
			t.Fatalf("refused body %q, want admitted", w.Body.String())
		}
	})

	t.Run("shared row (no entra): never refused — nothing per-person to connect (F4)", func(t *testing.T) {
		sc := adoTestSiteConfig(false)
		s := newSCMTestServer(t, sc, false)
		r := httptest.NewRequest(http.MethodPost, "/api/v1/runs", nil)
		r = r.WithContext(withOIDCHuman(r.Context(), "alice"))
		w := httptest.NewRecorder()
		if s.gitCredentialRefusal(w, r, scmTestADORepo) {
			t.Fatalf("refused body %q, want a shared row to pass through untouched", w.Body.String())
		}
	})

	t.Run("a repo on no Azure DevOps row: never refused", func(t *testing.T) {
		sc := adoTestSiteConfig(false)
		s := newSCMTestServer(t, sc, true)
		r := httptest.NewRequest(http.MethodPost, "/api/v1/runs", nil)
		r = r.WithContext(withOIDCHuman(r.Context(), "alice"))
		w := httptest.NewRecorder()
		if s.gitCredentialRefusal(w, r, "https://github.com/acme/widgets") {
			t.Fatalf("refused a GitHub repo over an Azure DevOps row's credential")
		}
	})

	t.Run("no session subject at all: never refused — nothing to bind a repair to", func(t *testing.T) {
		sc := adoTestSiteConfig(false)
		s := newSCMTestServer(t, sc, true)
		r := httptest.NewRequest(http.MethodPost, "/api/v1/runs", nil)
		w := httptest.NewRecorder()
		if s.gitCredentialRefusal(w, r, scmTestADORepo) {
			t.Fatalf("refused a mechanism caller, which admitRepoSources already let through")
		}
	})
}

// TestGitCredentialRefusalMatchesCanon pins gitCredentialNotConnectedRefusal
// byte-for-byte against §7.1's composed-by-server table (review finding F7),
// parsed out of the canon doc the way ado-entra-copy.test.ts parses §7.
func TestGitCredentialRefusalMatchesCanon(t *testing.T) {
	raw, err := os.ReadFile("../../docs/design/ado-entra-prompt.md")
	if err != nil {
		t.Fatalf("read canon doc: %v", err)
	}
	// The row: "| Not connected, at run create (422, `reason: git_credential`) | `ADO_422.*`, `runs_create_validate.go` | git_credential: ... |"
	// Take the LAST pipe-delimited cell of the line naming "Not connected, at run create".
	var canon string
	for _, line := range strings.Split(string(raw), "\n") {
		if !strings.Contains(line, "Not connected, at run create") {
			continue
		}
		cells := strings.Split(line, "|")
		if len(cells) < 2 {
			t.Fatalf("malformed canon row: %s", line)
		}
		canon = strings.TrimSpace(cells[len(cells)-2])
	}
	if canon == "" {
		t.Fatal("canon row for \"Not connected, at run create\" not found in docs/design/ado-entra-prompt.md §7.1")
	}
	if gitCredentialNotConnectedRefusal != canon {
		t.Errorf("gitCredentialNotConnectedRefusal = %q\nwant (canon)                    = %q", gitCredentialNotConnectedRefusal, canon)
	}
}

// HTTP-level integration: the gate at every door a repo reaches a run
// through (review finding F5), and the byte-identical requirement for a
// deployment with no per-user row (review finding F4).

// adoRunHarness wires a full Server (real routing, real OIDC session
// cookies) over ownerStore — the package's own comprehensive Store double
// (CreateRun, ListRoleMappings, workspaces, ...; ad hoc doubles like
// scmTestStore panic the moment a request actually reaches routing) — with
// an Azure DevOps per-user row and a fake runner, so the F5 cases below
// drive POST /api/v1/runs and POST /api/v1/runs/preflight for real rather
// than calling gitCredentialRefusal directly.
//
// The site config admits BOTH dev.azure.com and github.com, so a case can
// prove the gate checks EVERY resolved repo (a non-first, GitHub-first
// workspace source) without a github.com admission failure masking it.
func adoRunHarness(t *testing.T, entra bool) (*Server, *ownerStore, *fakeRunner) {
	t.Helper()
	st := newOwnerStore()
	st.siteConfig = types.SiteConfig{WorkspaceProviders: &types.WorkspaceProviders{Git: []types.GitProvider{
		{ID: scmTestRowID, Kind: types.GitProviderAzureDevOps, BaseURLs: []string{scmTestADOOrg}},
		{ID: "github", Kind: types.GitProviderGitHub, BaseURLs: []string{"https://github.com"}},
	}}}
	h := newHarness(t)
	cfg := baseTestConfig(h, st)
	cfg.OIDC = &oidc.Authenticator{}
	cfg.Broker = h.broker
	fr := &fakeRunner{}
	cfg.Runner = fr
	cfg.Secrets = &memSecrets{m: map[string][]byte{}}
	cfg.MaskRegistry = secretmask.NewRegistry()
	cfg.Now = func() time.Time { return adoTestNow }
	if entra {
		cfg.ADOEntra = adoTestEntraSource
	}
	cfg.DefaultPolicy = types.RunPolicySpec{MinConfinementClass: types.CC2, AllowAllEgress: true}
	return New(cfg), st, fr
}

// adoOperatorSession is a signed-in operator cookie — the create-workspace
// and create-run doors below need no member-capability grants to focus these
// cases on the git_credential gate alone.
func adoOperatorSession(t *testing.T) *http.Cookie {
	return ssoSession(t, "sub-ado-op", "op@corp.example", oidc.RoleAdmin)
}

func adoCreateWorkspace(t *testing.T, st *ownerStore, sources ...types.WorkspaceSource) uuid.UUID {
	t.Helper()
	return st.put(types.Workspace{Sources: sources})
}

// gitCredentialBody422 is the shared assertion every F5 case below makes.
func assertGitCredential422(t *testing.T, w *httptest.ResponseRecorder) {
	t.Helper()
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422; body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"reason":"git_credential"`) {
		t.Fatalf("body = %s, want reason git_credential", w.Body.String())
	}
}

func TestGitCredentialGate_ResolvedSpecChokepoint(t *testing.T) {
	// F5 a: the repo arrives ENTIRELY through workspace_id — req.Repo is
	// empty, so the free-text gate (requestRepoProviderRefusals) sees
	// nothing; only the resolved-spec chokepoint (seedAndAdmitWorkspace)
	// can catch this.
	t.Run("a workspace-attached repo with no free-text repo field", func(t *testing.T) {
		srv, st, fr := adoRunHarness(t, true)
		wsID := adoCreateWorkspace(t, st, types.WorkspaceSource{
			Type: types.WorkspaceSourceTypeRepo, Source: scmTestADORepo, Target: "/home/agent/work",
		})
		cookie := adoOperatorSession(t)
		body := `{"agent":"claude-code","task":"do the thing","workspace_id":"` + wsID.String() + `"}`
		w := doSSO(t, srv, http.MethodPost, "/api/v1/runs", cookie, body)
		assertGitCredential422(t, w)
		if fr.createCalls != 0 {
			t.Errorf("CreateSandbox calls = %d, want 0", fr.createCalls)
		}
	})

	// F5 b: two sources on one workspace — GitHub first (admits fine), the
	// Azure DevOps repo SECOND. The gate must check every resolved repo, not
	// just the first.
	t.Run("a non-first repo in a multi-source workspace", func(t *testing.T) {
		srv, st, fr := adoRunHarness(t, true)
		wsID := adoCreateWorkspace(t, st,
			types.WorkspaceSource{Type: types.WorkspaceSourceTypeRepo, Source: "https://github.com/acme/widgets", Target: "/home/agent/first"},
			types.WorkspaceSource{Type: types.WorkspaceSourceTypeRepo, Source: scmTestADORepo, Target: "/home/agent/second"},
		)
		cookie := adoOperatorSession(t)
		body := `{"agent":"claude-code","task":"do the thing","workspace_id":"` + wsID.String() + `"}`
		w := doSSO(t, srv, http.MethodPost, "/api/v1/runs", cookie, body)
		assertGitCredential422(t, w)
		if fr.createCalls != 0 {
			t.Errorf("CreateSandbox calls = %d, want 0", fr.createCalls)
		}
	})

	// F5 c/d: an inline policy names the repo directly on WorkspaceRepos —
	// no workspace_id, no free-text repo field, exactly an API client that
	// "omits repo". A STORED policy resolves through the identical
	// resolveRunPolicy chokepoint before reaching this same gate, so this
	// one case proves both.
	t.Run("an inline policy naming the repo directly, no repo field, no workspace_id", func(t *testing.T) {
		srv, st, fr := adoRunHarness(t, true)
		// Onboarded so validateWorkspaceSources admits the inline policy's
		// WorkspaceRepos entry at all — a SEPARATE gate from this issue's own,
		// and not itself keyed on workspace_id (indexWorkspacesBySource matches
		// by repo string against every onboarded workspace).
		adoCreateWorkspace(t, st, types.WorkspaceSource{Type: types.WorkspaceSourceTypeRepo, Source: scmTestADORepo})
		cookie := adoOperatorSession(t)
		body := `{"agent":"claude-code","task":"do the thing","inline_policy":{"min_confinement_class":"CC2","allow_all_egress":true,"workspace_repos":[{"repo":"` + scmTestADORepo + `"}]}}`
		w := doSSO(t, srv, http.MethodPost, "/api/v1/runs", cookie, body)
		assertGitCredential422(t, w)
		if fr.createCalls != 0 {
			t.Errorf("CreateSandbox calls = %d, want 0", fr.createCalls)
		}
	})

	// Counterfactual: the SAME inline-policy repo, WITH a captured sign-in,
	// launches — proving the gate above is not simply refusing every ADO run.
	t.Run("counterfactual: the same inline-policy repo, connected, launches", func(t *testing.T) {
		srv, st, fr := adoRunHarness(t, true)
		adoCreateWorkspace(t, st, types.WorkspaceSource{Type: types.WorkspaceSourceTypeRepo, Source: scmTestADORepo})
		if err := srv.storeADOEntraBlob(context.Background(), "sub-ado-op", scmTestRowID, adoEntraBlob{
			RefreshToken: "rt", Scopes: scmTestBaseline(t), TenantID: "tenant-1", ClientID: "client-1",
			Subject: "sub-ado-op", Source: adoEntraSourceSignIn,
		}); err != nil {
			t.Fatalf("store blob: %v", err)
		}
		cookie := adoOperatorSession(t)
		body := `{"agent":"claude-code","task":"do the thing","inline_policy":{"min_confinement_class":"CC2","allow_all_egress":true,"workspace_repos":[{"repo":"` + scmTestADORepo + `"}]}}`
		w := doSSO(t, srv, http.MethodPost, "/api/v1/runs", cookie, body)
		if w.Code != http.StatusCreated {
			t.Fatalf("status = %d, want 201; body=%s", w.Code, w.Body.String())
		}
		// POST /runs answers once the run exists; dispatch continues
		// server-side, so wait for the sandbox rather than reading it now.
		fr.waitForCreates(t, 1)
	})
}

// TestGitCredentialGate_PreflightNeverRefuses is review finding F2's other
// half: the SAME body that 422s at launch answers 200 at preflight, with the
// fact carried informationally.
func TestGitCredentialGate_PreflightNeverRefuses(t *testing.T) {
	srv, st, _ := adoRunHarness(t, true)
	adoCreateWorkspace(t, st, types.WorkspaceSource{Type: types.WorkspaceSourceTypeRepo, Source: scmTestADORepo})
	cookie := adoOperatorSession(t)
	body := `{"agent":"claude-code","task":"do the thing","inline_policy":{"min_confinement_class":"CC2","allow_all_egress":true,"workspace_repos":[{"repo":"` + scmTestADORepo + `"}]}}`

	w := doSSO(t, srv, http.MethodPost, "/api/v1/runs/preflight", cookie, body)
	if w.Code != http.StatusOK {
		t.Fatalf("preflight status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"git_credential":{`) || !strings.Contains(w.Body.String(), `"state":"not_configured"`) {
		t.Errorf("preflight body = %s, want a git_credential fact", w.Body.String())
	}

	// The identical body still 422s at launch.
	launch := doSSO(t, srv, http.MethodPost, "/api/v1/runs", cookie, body)
	assertGitCredential422(t, launch)
}

// TestGitCredentialGate_FreeTextRepo is review follow-up N2: the FREE-TEXT
// `repo` field specifically (requestRepoProviderRefusals' own gate, not
// seedAndAdmitWorkspace's resolved-spec one — the other cases in this file
// exercise that one via inline_policy/workspace_id). Proven by mutation: see
// the commit message for the two reverts run against this test and
// TestGitCredentialGate_FreeTextRepo_PreflightNeverRefuses, each restored
// after confirming a failure.
func TestGitCredentialGate_FreeTextRepo(t *testing.T) {
	srv, _, _ := adoRunHarness(t, true)
	cookie := adoOperatorSession(t)
	body := `{"agent":"claude-code","task":"do the thing","repo":"` + scmTestADORepo + `"}`
	w := doSSO(t, srv, http.MethodPost, "/api/v1/runs", cookie, body)
	assertGitCredential422(t, w)
	if !strings.Contains(w.Body.String(), `"org":"`+scmTestADOOrg+`"`) {
		t.Errorf("body = %s, want the org (F1)", w.Body.String())
	}
}

func TestGitCredentialGate_FreeTextRepo_PreflightNeverRefuses(t *testing.T) {
	srv, _, _ := adoRunHarness(t, true)
	cookie := adoOperatorSession(t)
	body := `{"agent":"claude-code","task":"do the thing","repo":"` + scmTestADORepo + `"}`
	w := doSSO(t, srv, http.MethodPost, "/api/v1/runs/preflight", cookie, body)
	if w.Code != http.StatusOK {
		t.Fatalf("preflight status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"git_credential":{`) || !strings.Contains(w.Body.String(), `"state":"not_configured"`) {
		t.Errorf("preflight body = %s, want a git_credential fact", w.Body.String())
	}
}

// TestGitCredentialFact_OnlyThisRunsOwnRepos is F2's "never a deployment-wide
// guess" half: a run whose repos are NOT on the Azure DevOps row gets no
// git_credential fact at all, even though the deployment HAS a per-user row
// (which a member could reach on a DIFFERENT run).
func TestGitCredentialFact_OnlyThisRunsOwnRepos(t *testing.T) {
	srv, _, _ := adoRunHarness(t, true)
	cookie := adoOperatorSession(t)
	body := `{"agent":"claude-code","task":"do the thing","repo":"https://github.com/acme/widgets"}`
	w := doSSO(t, srv, http.MethodPost, "/api/v1/runs/preflight", cookie, body)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), "git_credential") {
		t.Errorf("body = %s, want no git_credential fact for a GitHub-only run", w.Body.String())
	}
}

// TestByteIdentical_SharedADORow is review finding F4's handler-level proof:
// a deployment whose only Azure DevOps row is a plain PAT row (no entra)
// gets NEITHER scm_access NOR git_credential anywhere — /setup/status and
// /runs/preflight read exactly as they did before this issue existed.
func TestByteIdentical_SharedADORow(t *testing.T) {
	srv, _, _ := adoRunHarness(t, false) // false: no ADOEntra source at all — a plain PAT row
	cookie := adoOperatorSession(t)

	t.Run("/setup/status carries no scm_access", func(t *testing.T) {
		w := doSSO(t, srv, http.MethodGet, "/api/v1/setup/status", cookie, "")
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, body=%s", w.Code, w.Body.String())
		}
		if strings.Contains(w.Body.String(), "scm_access") {
			t.Errorf("body contains scm_access on a plain-PAT-row deployment")
		}
	})

	t.Run("/runs/preflight carries no git_credential, even for a run on that row's repo", func(t *testing.T) {
		body := `{"agent":"claude-code","task":"do the thing","repo":"` + scmTestADORepo + `"}`
		w := doSSO(t, srv, http.MethodPost, "/api/v1/runs/preflight", cookie, body)
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, body=%s", w.Code, w.Body.String())
		}
		if strings.Contains(w.Body.String(), "git_credential") {
			t.Errorf("body contains git_credential on a plain-PAT-row deployment: %s", w.Body.String())
		}
	})

	t.Run("POST /runs on that row's repo launches — a shared row is untouched by this gate", func(t *testing.T) {
		body := `{"agent":"claude-code","task":"do the thing","repo":"` + scmTestADORepo + `"}`
		w := doSSO(t, srv, http.MethodPost, "/api/v1/runs", cookie, body)
		if w.Code != http.StatusCreated {
			t.Fatalf("status = %d, want 201 (a shared row is never gated); body=%s", w.Code, w.Body.String())
		}
	})

	t.Run("GET /me/scm-access answers the empty array, not a per-row guess", func(t *testing.T) {
		w := doSSO(t, srv, http.MethodGet, "/api/v1/me/scm-access", cookie, "")
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, body=%s", w.Code, w.Body.String())
		}
		if body := strings.TrimSpace(w.Body.String()); body != "[]" {
			t.Errorf("body = %s, want []", body)
		}
	})
}

// TestByteIdentical_NoADORowAtAll is F4's other counterfactual: a deployment
// with no Azure DevOps row whatsoever (today's shape, pre-#386) is equally
// silent.
func TestByteIdentical_NoADORowAtAll(t *testing.T) {
	st := newOwnerStore() // zero SiteConfig: no provider rows at all
	h := newHarness(t)
	cfg := baseTestConfig(h, st)
	cfg.OIDC = &oidc.Authenticator{}
	cfg.Broker = h.broker
	cfg.Runner = &fakeRunner{}
	cfg.DefaultPolicy = types.RunPolicySpec{MinConfinementClass: types.CC2, AllowAllEgress: true}
	srv := New(cfg)
	cookie := adoOperatorSession(t)

	w := doSSO(t, srv, http.MethodGet, "/api/v1/setup/status", cookie, "")
	if w.Code != http.StatusOK || strings.Contains(w.Body.String(), "scm_access") {
		t.Errorf("status=%d body contains scm_access with no Azure DevOps row at all", w.Code)
	}
}

// review follow-up N4: the same gate at the other doors that clone a
// repo server-side — the Build step, the Scan step, and a record session.

func adoWorkspaceWithADORepo(t *testing.T, st *ownerStore) string {
	t.Helper()
	id := adoCreateWorkspace(t, st, types.WorkspaceSource{
		Type: types.WorkspaceSourceTypeRepo, Source: scmTestADORepo, Target: "/home/agent/work",
	})
	return id.String()
}

func TestGitCredentialGate_WorkspaceBuild(t *testing.T) {
	srv, st, _ := adoRunHarness(t, true)
	id := adoWorkspaceWithADORepo(t, st)
	cookie := adoOperatorSession(t)
	w := doSSO(t, srv, http.MethodPost, "/api/v1/workspaces/"+id+"/build", cookie, "{}")
	assertGitCredential422(t, w)
}

func TestGitCredentialGate_WorkspaceScan(t *testing.T) {
	srv, st, _ := adoRunHarness(t, true)
	id := adoWorkspaceWithADORepo(t, st)
	cookie := adoOperatorSession(t)
	w := doSSO(t, srv, http.MethodPost, "/api/v1/workspaces/"+id+"/scan", cookie, "{}")
	assertGitCredential422(t, w)
}

// POST /workspaces/{id}/record is operator-only (routes.go), so a member
// case does not apply here the way it does for Build/Scan.
func TestGitCredentialGate_RecordSession(t *testing.T) {
	srv, st, _ := adoRunHarness(t, true)
	id := adoWorkspaceWithADORepo(t, st)
	cookie := adoOperatorSession(t)
	w := doSSO(t, srv, http.MethodPost, "/api/v1/workspaces/"+id+"/record", cookie, `{"name":"e2e record"}`)
	assertGitCredential422(t, w)
}

// TestGitCredentialGate_RecordSession_MechanismPrincipal is the admin-token
// bearer — a MECHANISM, not a person (review follow-up N4's own instruction:
// "follow what gitCredentialRefusal does for the admin-token principal" —
// oidcHumanFromContext answers "" for it, so gitCredentialRefusalForLauncher
// never refuses; the record attempt proceeds to whatever the next gate
// answers, never a git_credential 422 it could not possibly repair).
func TestGitCredentialGate_RecordSession_MechanismPrincipal(t *testing.T) {
	srv, st, _ := adoRunHarness(t, true)
	id := adoWorkspaceWithADORepo(t, st)
	w := do(t, srv, http.MethodPost, "/api/v1/workspaces/"+id+"/record", adminToken, `{"name":"e2e record"}`)
	if strings.Contains(w.Body.String(), "git_credential") {
		t.Errorf("the admin-token mechanism principal was refused git_credential: %s", w.Body.String())
	}
}
