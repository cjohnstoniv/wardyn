// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/test/adofake"
)

// TestADONames_ImportLaunchCloneFetchPush is #485's done-when, end to end on
// the per-person Azure DevOps lane: a repository named "Card Auth (v2).Service"
// in a project named "Payments Platform"
//
//   - IMPORTS through the workspace door from the remoteUrl Azure DevOps' own
//     repositories list hands out, and is stored in its one canonical spelling;
//   - LAUNCHES: admission and the dispatch lane resolve it, and WARDYN_REPOS
//     carries it into a directory named after the repository;
//   - CLONES, FETCHES and PUSHES through the git broker — the real agent-run
//     clone code, the real proxy sidecar dispatch authored, and a real git
//     http-backend behind a fake that looks the repository up by its names.
func TestADONames_ImportLaunchCloneFetchPush(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	const bearer = "entra-bearer-for-contoso"
	fake := adofake.New()
	t.Cleanup(fake.Close)
	fake.RegisterToken(bearer, adofake.ScopeCodeRead, adofake.ScopeCodeWrite)
	bare := adofake.NewFixtureRepo(t)
	fake.AddProject("contoso", "", adofake.SpacedProject)
	fake.RegisterRepo("contoso", adofake.SpacedProject, adofake.SpacedRepo, bare)

	// IMPORT: the URL a person copies out of Azure DevOps.
	remote := adoRemoteURLFromFake(t, fake, bearer)
	body, _ := json.Marshal(map[string]any{"name": "payments", "sources": []map[string]string{{"type": "repo", "source": remote}}})
	ws, msg := decodeWorkspaceRequest(httptest.NewRecorder(),
		httptest.NewRequest(http.MethodPost, "/api/v1/workspaces", strings.NewReader(string(body))), nil)
	if msg != "" {
		t.Fatalf("import of %q refused: %s", remote, msg)
	}
	repo := ws.Sources[0].Source
	if want := "https://contoso@dev.azure.com/contoso/Payments%20Platform/_git/Card%20Auth%20(v2).Service"; repo != want {
		t.Fatalf("imported %q as %q, want %q", remote, repo, want)
	}

	// LAUNCH: admission, the dispatch lane, and the clone records.
	ado, ok := resolveADOEntraRun(adoSite(adoEntraTestRow()), []string{repo}, adoTestOwner)
	if !ok {
		t.Fatalf("no Azure DevOps lane for %q", repo)
	}
	upstreamCA := newTestUpstreamCA(t, "dev.azure.com")
	upstream, _ := countingUpstream(t, fake.URL())
	corp := newTLSTerminatingCorpProxy(t, upstreamCA.leaf, upstream)
	cp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/api/v1/internal/injection/") {
			w.WriteHeader(http.StatusOK)
			return
		}
		_ = json.NewEncoder(w).Encode(types.ResolvedInjection{
			Header: "Authorization", Value: "Bearer " + bearer, ExpiresAt: time.Now().Add(time.Hour).UnixMilli(),
			Organisation: "contoso", Capabilities: []string{"read", "code_write"},
		})
	}))
	t.Cleanup(cp.Close)
	caCert, caKey, err := generateRunCA(time.Now())
	if err != nil {
		t.Fatalf("generateRunCA: %v", err)
	}
	s, _ := newADODispatchServer(&adoTestStore{})
	policy, env, runID := types.RunPolicySpec{}, map[string]string{}, uuid.New()
	lane, ok := s.authorADOEntraLane(context.Background(), types.AgentRun{ID: runID}, ado, true, adoEntraUngraded(),
		dispatchLLMPlan{mitmCACertPEM: string(caCert), mitmCAKeyPEM: string(caKey)}, &policy, env, nil)
	if !ok {
		t.Fatal("dispatch refused the lane")
	}
	records, warns := buildRepoRecords("", []types.WorkspaceRepo{{Repo: repo}})
	work := t.TempDir()
	const wantDest = "/home/agent/work/Card-Auth-(v2).Service"
	if len(warns) != 0 || !strings.Contains(records, "\t"+wantDest+"\t") {
		t.Fatalf("WARDYN_REPOS = %q (warnings %v), want a clone into %s", records, warns, wantDest)
	}
	listen := startADOLaneSidecar(t, runID, lane, policy, string(caCert), string(caKey), cp.URL, corp, upstreamCA.caPEM)

	// CLONE: agent-run's own broker rewrite and clone loop, fed what dispatch wrote.
	home := t.TempDir()
	gitEnv := adoNamesGitEnv(home, runID)
	_, self, _, _ := runtime.Caller(0)
	lib := filepath.Join(filepath.Dir(self), "..", "..", "deploy", "images", "common", "agent-run-lib.sh")
	cmd := exec.Command("bash", "-c", `set -euo pipefail; source "$1"; configure_git_pat_broker_insteadof; dispatch_repo_clones`, "bash", lib)
	cmd.Env = append(gitEnv, "WARDYN_PROXY_URL=http://"+listen,
		"WARDYN_GIT_PAT_BROKER_HOSTS="+env["WARDYN_GIT_PAT_BROKER_HOSTS"],
		"WARDYN_REPOS="+strings.Replace(records, "/home/agent/work", work, 1))
	out, err := cmd.CombinedOutput()
	dest := filepath.Join(work, "Card-Auth-(v2).Service")
	if err != nil || !strings.Contains(string(out), "clone OK") {
		t.Fatalf("clone: %v\n%s", err, out)
	}

	// FETCH and PUSH on the run's own branch, through the same broker.
	branch := "wardyn/" + runID.String() + "/work"
	for _, args := range [][]string{
		{"-C", dest, "fetch", "origin"},
		{"-C", dest, "commit", "--allow-empty", "-m", "run work"},
		{"-C", dest, "push", "origin", branch},
	} {
		c := exec.Command("git", args...)
		c.Env = gitEnv
		if out, err := c.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	if out, err := exec.Command("git", "-C", bare, "rev-parse", "--verify", "refs/heads/"+branch).CombinedOutput(); err != nil {
		t.Fatalf("the pushed branch is not in %q: %v\n%s", adofake.SpacedRepo, err, out)
	}
	for _, r := range fake.Requests() {
		if r.Endpoint == adofake.EndpointGitReceivePack &&
			r.Path == "/contoso/"+adofake.SpacedProject+"/_git/"+adofake.SpacedRepo+"/git-receive-pack" && r.Authorized {
			return
		}
	}
	t.Errorf("Azure DevOps never saw an authorized push to %q in %q", adofake.SpacedRepo, adofake.SpacedProject)
}

// adoRemoteURLFromFake reads the one repository's remoteUrl from the fake's
// repositories list, the way a person copies it out of Azure DevOps.
func adoRemoteURLFromFake(t *testing.T, fake *adofake.Server, bearer string) string {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, fake.URL()+"/contoso/Payments%20Platform/_apis/git/repositories?api-version=7.1", nil)
	req.Header.Set("Authorization", "Bearer "+bearer)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	var list struct {
		Value []struct {
			RemoteURL string `json:"remoteUrl"`
		} `json:"value"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&list); err != nil || len(list.Value) != 1 {
		t.Fatalf("repositories: %d %v %+v", resp.StatusCode, err, list)
	}
	return list.Value[0].RemoteURL
}

// adoNamesGitEnv is a git environment isolated from the host's: its own HOME
// (where agent-run writes the broker rewrite), no system config, no proxy
// variables, no prompt, and an identity to commit with.
func adoNamesGitEnv(home string, runID uuid.UUID) []string {
	var env []string
	for _, kv := range os.Environ() {
		k, _, _ := strings.Cut(kv, "=")
		if strings.HasPrefix(k, "GIT_") || strings.HasSuffix(strings.ToLower(k), "_proxy") || k == "HOME" || k == "WARDYN_REPOS" {
			continue
		}
		env = append(env, kv)
	}
	return append(env, "HOME="+home, "WARDYN_RUN_ID="+runID.String(), "GIT_CONFIG_NOSYSTEM=1", "GIT_TERMINAL_PROMPT=0",
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com")
}
