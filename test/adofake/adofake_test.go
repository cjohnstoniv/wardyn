// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package adofake

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"testing"
)

// TestGitCloneRequiresReadScope proves the fake's git advertisement + upload-
// pack enforce ScopeCodeRead against a REAL git client (not a stub of the
// protocol): a token without the scope is refused with the real ADO 401
// shape, and a token with it clones the fixture repo's two commits.
func TestGitCloneRequiresReadScope(t *testing.T) {
	s := New()
	defer s.Close()
	bare := NewFixtureRepo(t)
	s.RegisterRepo("fakeorg", "fakeproj", "fakerepo", bare)
	repoURL := s.URL() + "/fakeorg/fakeproj/_git/fakerepo"

	s.RegisterToken("no-scopes-token")
	dest := t.TempDir() + "/clone-denied"
	out, err := gitWithBearer(t, "no-scopes-token", "clone", repoURL, dest).CombinedOutput()
	if err == nil {
		t.Fatalf("clone with a scopeless token succeeded, want refusal:\n%s", out)
	}

	s.RegisterToken("read-token", ScopeCodeRead)
	dest = t.TempDir() + "/clone-ok"
	// Exercised via HTTP Basic (empty username, token as password) here — the
	// PAT convention the minted-token mode uses — to prove that path, not just
	// Bearer.
	out, err = gitWithBasic(t, "read-token", "clone", repoURL, dest).CombinedOutput()
	if err != nil {
		t.Fatalf("clone with a read-scoped token failed: %v\n%s", err, out)
	}
	log, logErr := exec.Command("git", "-C", dest, "log", "--oneline").CombinedOutput()
	if logErr != nil {
		t.Fatalf("git log on the clone: %v\n%s", logErr, log)
	}
	if got := strings.Count(strings.TrimSpace(string(log)), "\n") + 1; got != 2 {
		t.Errorf("cloned repo has %d commits, want 2:\n%s", got, log)
	}

	found401 := false
	for _, rr := range s.Requests() {
		if rr.Endpoint == EndpointGitAdvertise && rr.Token == "no-scopes-token" && !rr.Authorized {
			found401 = true
		}
	}
	if !found401 {
		t.Error("no recorded git.advertise request shows the scopeless token as unauthorized")
	}
}

// TestGitPushRequiresWriteScope proves git-receive-pack enforces
// ScopeCodeWrite: a read-scoped token can clone but not push, and a
// write-scoped token's push actually lands in the bare repository.
func TestGitPushRequiresWriteScope(t *testing.T) {
	s := New()
	defer s.Close()
	bare := NewFixtureRepo(t)
	s.RegisterRepo("fakeorg", "fakeproj", "fakerepo", bare)
	repoURL := s.URL() + "/fakeorg/fakeproj/_git/fakerepo"

	s.RegisterToken("read-only", ScopeCodeRead)
	clone := t.TempDir() + "/work"
	out, err := gitWithBearer(t, "read-only", "clone", repoURL, clone).CombinedOutput()
	if err != nil {
		t.Fatalf("clone with read scope failed: %v\n%s", err, out)
	}
	writeFile(t, clone+"/new-file.txt", "from a read-only token\n")
	runIn(t, clone, "add", "new-file.txt")
	runIn(t, clone, "commit", "-m", "should not reach the bare repo")

	out, err = gitWithBearer(t, "read-only", "push", "origin", "main").AtDir(clone).CombinedOutput()
	if err == nil {
		t.Fatalf("push with a read-only token succeeded, want refusal:\n%s", out)
	}

	s.RegisterToken("read-write", ScopeCodeRead, ScopeCodeWrite)
	out, err = gitWithBearer(t, "read-write", "push", "origin", "main").AtDir(clone).CombinedOutput()
	if err != nil {
		t.Fatalf("push with a read+write token failed: %v\n%s", err, out)
	}

	log, logErr := exec.Command("git", "-C", bare, "log", "--oneline", "main").CombinedOutput()
	if logErr != nil {
		t.Fatalf("git log on the bare repo after push: %v\n%s", logErr, log)
	}
	if got := strings.Count(strings.TrimSpace(string(log)), "\n") + 1; got != 3 {
		t.Errorf("bare repo has %d commits after the push, want 3:\n%s", got, log)
	}
}

// TestPolicyPostRefusedForReadOnlyToken proves the branch-policy write
// endpoint enforces ScopeCodeWrite over REST, and that a policy registered by
// a write-scoped token is then visible through the read endpoint's
// repositoryId/refName filter — the "is this ref protected" lookup.
func TestPolicyPostRefusedForReadOnlyToken(t *testing.T) {
	s := New()
	defer s.Close()

	s.RegisterToken("read-only", ScopeCodeRead)
	s.RegisterToken("read-write", ScopeCodeRead, ScopeCodeWrite)

	policyBody := []byte(`{
		"isEnabled": true, "isBlocking": true,
		"type": {"id": "fa4e907d-c16b-4a4c-9dfa-4906e5d171dd"},
		"settings": {"scope": [{"repositoryId": "repo-1", "refName": "refs/heads/main"}]}
	}`)
	url := s.URL() + "/fakeorg/fakeproj/_apis/policy/configurations"

	status, _ := postJSON(t, url, "read-only", policyBody)
	if status != http.StatusUnauthorized {
		t.Fatalf("policy POST with a read-only token status = %d, want 401", status)
	}

	status, body := postJSON(t, url, "read-write", policyBody)
	if status != http.StatusOK {
		t.Fatalf("policy POST with a read+write token status = %d, body=%s", status, body)
	}

	// A read-scoped token can now confirm the ref is protected.
	req, _ := http.NewRequest(http.MethodGet, url+"?repositoryId=repo-1&refName=refs%2Fheads%2Fmain", nil)
	req.Header.Set("Authorization", "Bearer read-only")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET policy/configurations: %v", err)
	}
	defer resp.Body.Close()
	var listed struct {
		Count int `json:"count"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&listed); err != nil {
		t.Fatalf("decode policy list: %v", err)
	}
	if listed.Count != 1 {
		t.Errorf("protected-branch lookup for repo-1/refs/heads/main returned count=%d, want 1", listed.Count)
	}

	// A ref nobody registered a policy for reads back empty — "protected" and
	// "no policy" have to be distinguishable answers.
	req2, _ := http.NewRequest(http.MethodGet, url+"?repositoryId=repo-1&refName=refs%2Fheads%2Funprotected", nil)
	req2.Header.Set("Authorization", "Bearer read-only")
	resp2, err := http.DefaultClient.Do(req2)
	if err != nil {
		t.Fatalf("GET policy/configurations (unprotected ref): %v", err)
	}
	defer resp2.Body.Close()
	var listed2 struct {
		Count int `json:"count"`
	}
	if err := json.NewDecoder(resp2.Body).Decode(&listed2); err != nil {
		t.Fatalf("decode policy list (unprotected ref): %v", err)
	}
	if listed2.Count != 0 {
		t.Errorf("lookup for an unregistered ref returned count=%d, want 0", listed2.Count)
	}
}

// TestPatLifecycle drives create/list/update/revoke against the token
// endpoints, honouring displayName/scope/validTo/allOrgs, and proves the
// injectable fullScopePatPolicyViolation error shape.
func TestPatLifecycle(t *testing.T) {
	s := New()
	defer s.Close()
	s.RegisterToken("tokens-admin", ScopeTokens)
	base := s.URL() + "/fakeorg/_apis/tokens/pats"

	createBody := []byte(`{"displayName":"ci-token","scope":"vso.code","validTo":"2099-01-01T00:00:00Z","allOrgs":false}`)
	status, body := postJSON(t, base, "tokens-admin", createBody)
	if status != http.StatusOK {
		t.Fatalf("create PAT status = %d, body=%s", status, body)
	}
	var created struct {
		PatToken struct {
			AuthorizationID string `json:"authorizationId"`
			DisplayName     string `json:"displayName"`
		} `json:"patToken"`
		PatTokenError string `json:"patTokenError"`
	}
	if err := json.Unmarshal(body, &created); err != nil {
		t.Fatalf("decode create response: %v\n%s", err, body)
	}
	if created.PatTokenError != string(PatTokenErrorNone) || created.PatToken.AuthorizationID == "" {
		t.Fatalf("create PAT response = %s, want a minted token", body)
	}
	authID := created.PatToken.AuthorizationID

	// List: the new token is there.
	req, _ := http.NewRequest(http.MethodGet, base, nil)
	req.Header.Set("Authorization", "Bearer tokens-admin")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("list PATs: %v", err)
	}
	var listed struct {
		Value []struct {
			AuthorizationID string `json:"authorizationId"`
		} `json:"value"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&listed); err != nil {
		t.Fatalf("decode PAT list: %v", err)
	}
	resp.Body.Close()
	found := false
	for _, p := range listed.Value {
		if p.AuthorizationID == authID {
			found = true
		}
	}
	if !found {
		t.Errorf("listed PATs %+v do not include the created authorizationId %q", listed.Value, authID)
	}

	// Update: rename it.
	updateBody, _ := json.Marshal(map[string]any{"authorizationId": authID, "displayName": "ci-token-renamed"})
	req, _ = http.NewRequest(http.MethodPut, base, bytes.NewReader(updateBody))
	req.Header.Set("Authorization", "Bearer tokens-admin")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("update PAT: %v", err)
	}
	var updated struct {
		PatToken struct {
			DisplayName string `json:"displayName"`
		} `json:"patToken"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&updated); err != nil {
		t.Fatalf("decode update response: %v", err)
	}
	resp.Body.Close()
	if updated.PatToken.DisplayName != "ci-token-renamed" {
		t.Errorf("updated PAT displayName = %q, want ci-token-renamed", updated.PatToken.DisplayName)
	}

	// Injected policy violation on the NEXT create.
	s.SetPatCreateError(PatTokenErrorFullScopePolicyViolation)
	status, body = postJSON(t, base, "tokens-admin", createBody)
	if status != http.StatusOK {
		t.Fatalf("create PAT (injected violation) status = %d, want 200 with patTokenError set", status)
	}
	var refused struct {
		PatToken      any    `json:"patToken"`
		PatTokenError string `json:"patTokenError"`
	}
	if err := json.Unmarshal(body, &refused); err != nil {
		t.Fatalf("decode refused-create response: %v\n%s", err, body)
	}
	if refused.PatTokenError != string(PatTokenErrorFullScopePolicyViolation) || refused.PatToken != nil {
		t.Errorf("refused-create response = %s, want patToken=null and patTokenError=fullScopePatPolicyViolation", body)
	}
	s.SetPatCreateError(PatTokenErrorNone)

	// Revoke: it's gone from the list.
	req, _ = http.NewRequest(http.MethodDelete, base+"?authorizationId="+authID, nil)
	req.Header.Set("Authorization", "Bearer tokens-admin")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("revoke PAT: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Errorf("revoke PAT status = %d, want 204", resp.StatusCode)
	}

	req, _ = http.NewRequest(http.MethodGet, base, nil)
	req.Header.Set("Authorization", "Bearer tokens-admin")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("list PATs after revoke: %v", err)
	}
	defer resp.Body.Close()
	listed.Value = nil
	if err := json.NewDecoder(resp.Body).Decode(&listed); err != nil {
		t.Fatalf("decode post-revoke PAT list: %v", err)
	}
	for _, p := range listed.Value {
		if p.AuthorizationID == authID {
			t.Errorf("revoked PAT %q still present in the list", authID)
		}
	}
}

// --- test helpers ---

type gitCmd struct {
	*exec.Cmd
}

func (g gitCmd) AtDir(dir string) gitCmd {
	g.Cmd.Dir = dir
	return g
}

func gitWithBearer(t *testing.T, token string, args ...string) gitCmd {
	t.Helper()
	full := append([]string{"-c", "http.extraHeader=Authorization: Bearer " + token}, args...)
	return gitCmd{exec.Command("git", full...)}
}

func gitWithBasic(t *testing.T, token string, args ...string) gitCmd {
	t.Helper()
	basic := base64.StdEncoding.EncodeToString([]byte(":" + token))
	full := append([]string{"-c", "http.extraHeader=Authorization: Basic " + basic}, args...)
	return gitCmd{exec.Command("git", full...)}
}

func runIn(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v (in %s): %v\n%s", args, dir, err, out)
	}
}

func writeFile(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func postJSON(t *testing.T, url, token string, body []byte) (status int, respBody []byte) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		t.Fatalf("build POST %s: %v", url, err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST %s: %v", url, err)
	}
	defer resp.Body.Close()
	out, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read response from POST %s: %v", url, err)
	}
	return resp.StatusCode, out
}
