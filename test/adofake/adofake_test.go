// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package adofake

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"
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

// TestGitAdvertiseRefusesAmbiguousService pins the fix for a read-only token
// reaching a receive-pack advertisement: net/http's Query().Get reads the
// FIRST value of a repeated query parameter while git http-backend reads the
// LAST, so "?service=git-upload-pack&service=git-receive-pack" used to be
// scope-checked as a read and then served a REAL receive-pack advertisement.
// Both orderings must refuse, proving the fix isn't order-dependent.
func TestGitAdvertiseRefusesAmbiguousService(t *testing.T) {
	s := New()
	defer s.Close()
	bare := NewFixtureRepo(t)
	s.RegisterRepo("fakeorg", "fakeproj", "fakerepo", bare)
	s.RegisterToken("read-only", ScopeCodeRead)

	for _, q := range []string{
		"service=git-upload-pack&service=git-receive-pack",
		"service=git-receive-pack&service=git-upload-pack",
	} {
		url := s.URL() + "/fakeorg/fakeproj/_git/fakerepo/info/refs?" + q
		req, _ := http.NewRequest(http.MethodGet, url, nil)
		req.Header.Set("Authorization", "Bearer read-only")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("GET %s: %v", url, err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if ct := resp.Header.Get("Content-Type"); strings.Contains(ct, "receive-pack") {
			t.Fatalf("ambiguous service query %q was served a REAL receive-pack advertisement (Content-Type=%s) to a read-only token", q, ct)
		}
		if resp.StatusCode == http.StatusOK {
			t.Errorf("ambiguous service query %q answered 200, want a refusal:\n%s", q, body)
		}
	}
}

// TestGitDumbProtocolDisabled proves the fake's smart-HTTP-only surface
// matches the real service: the dumb protocol (a bare object/ref fetch, or an
// info/refs advertisement with no service at all) 404s, so a lane cannot
// "prove" a read through a path production can never use.
func TestGitDumbProtocolDisabled(t *testing.T) {
	s := New()
	defer s.Close()
	bare := NewFixtureRepo(t)
	s.RegisterRepo("fakeorg", "fakeproj", "fakerepo", bare)
	s.RegisterToken("rw", ScopeCodeRead, ScopeCodeWrite)

	for _, path := range []string{"HEAD", "objects/info/packs", "info/refs"} {
		req, _ := http.NewRequest(http.MethodGet, s.URL()+"/fakeorg/fakeproj/_git/fakerepo/"+path, nil)
		req.Header.Set("Authorization", "Bearer rw")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("dumb-protocol path %q status = %d, want 404 (real Azure DevOps is smart-HTTP only)", path, resp.StatusCode)
		}
	}
}

// TestUnauthorizedBodyMatchesRealShape pins every field of the 401 body: a
// test that only checked the status code would stay green if the body were
// replaced with junk, and a caller distinguishing "wrong scope" from any
// other 401 needs typeKey and the TF400813 message specifically.
func TestUnauthorizedBodyMatchesRealShape(t *testing.T) {
	s := New()
	defer s.Close()
	s.RegisterToken("read-only", ScopeCodeRead)

	url := s.URL() + "/fakeorg/fakeproj/_apis/policy/configurations"
	status, body := postJSON(t, url, "read-only", []byte(`{}`))
	if status != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401\n%s", status, body)
	}

	var got struct {
		ID             string `json:"$id"`
		InnerException any    `json:"innerException"`
		Message        string `json:"message"`
		TypeName       string `json:"typeName"`
		TypeKey        string `json:"typeKey"`
		ErrorCode      int    `json:"errorCode"`
		EventID        int    `json:"eventId"`
	}
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("decode 401 body: %v\n%s", err, body)
	}
	if got.TypeKey != "UnauthorizedRequestException" {
		t.Errorf("typeKey = %q, want UnauthorizedRequestException", got.TypeKey)
	}
	if !strings.Contains(got.Message, "TF400813") {
		t.Errorf("message = %q, want it to contain TF400813", got.Message)
	}
	if got.EventID != 3000 {
		t.Errorf("eventId = %d, want 3000", got.EventID)
	}
	if got.InnerException != nil {
		t.Errorf("innerException = %v, want nil", got.InnerException)
	}
	if !strings.Contains(got.TypeName, "UnauthorizedRequestException") {
		t.Errorf("typeName = %q, want it to name UnauthorizedRequestException", got.TypeName)
	}
}

// TestPatRoutesRefuseWrongScopeAndAbsentToken closes the gap that let
// checkScope be forced to always grant without any test noticing: nothing
// previously sent a wrong-scoped or absent token at a PAT route.
func TestPatRoutesRefuseWrongScopeAndAbsentToken(t *testing.T) {
	s := New()
	defer s.Close()
	base := s.URL() + "/fakeorg/_apis/tokens/pats"

	req, _ := http.NewRequest(http.MethodGet, base, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET (no token): %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("PAT list with no token status = %d, want 401", resp.StatusCode)
	}

	s.RegisterToken("code-only", ScopeCodeRead)
	req, _ = http.NewRequest(http.MethodGet, base, nil)
	req.Header.Set("Authorization", "Bearer code-only")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET (wrong scope): %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("PAT list with a vso.code-only token status = %d, want 401", resp.StatusCode)
	}
}

// TestPatMintedTokenIsUsableThenRevoked proves the PAT lifecycle actually
// bites: the minted token must authorize the scope it declared, and a revoked
// token must stop authorizing anything.
func TestPatMintedTokenIsUsableThenRevoked(t *testing.T) {
	s := New()
	defer s.Close()
	s.RegisterToken("admin", ScopeTokens)
	base := s.URL() + "/fakeorg/_apis/tokens/pats"

	createBody := []byte(`{"displayName":"usable","scope":"vso.code","validTo":"2099-01-01T00:00:00Z","allOrgs":false}`)
	status, body := postJSON(t, base, "admin", createBody)
	if status != http.StatusOK {
		t.Fatalf("create: status=%d body=%s", status, body)
	}
	var created struct {
		PatToken struct {
			AuthorizationID string `json:"authorizationId"`
			Token           string `json:"token"`
		} `json:"patToken"`
	}
	if err := json.Unmarshal(body, &created); err != nil {
		t.Fatalf("decode create response: %v\n%s", err, body)
	}
	if created.PatToken.Token == "" || created.PatToken.AuthorizationID == "" {
		t.Fatalf("create response carries no usable token/authorizationId: %s", body)
	}

	policyURL := s.URL() + "/fakeorg/fakeproj/_apis/policy/configurations"
	req, _ := http.NewRequest(http.MethodGet, policyURL, nil)
	req.SetBasicAuth("", created.PatToken.Token) // the PAT convention: empty username, PAT as password
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET policy with minted PAT: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("minted PAT could not use its own vso.code scope: status=%d", resp.StatusCode)
	}

	req, _ = http.NewRequest(http.MethodDelete, base+"?authorizationId="+created.PatToken.AuthorizationID, nil)
	req.Header.Set("Authorization", "Bearer admin")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("revoke: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("revoke status = %d, want 204", resp.StatusCode)
	}

	req, _ = http.NewRequest(http.MethodGet, policyURL, nil)
	req.SetBasicAuth("", created.PatToken.Token)
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET policy with revoked PAT: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("revoked PAT status = %d, want 401", resp.StatusCode)
	}
}

// TestPatExpiredValidToIsRefused proves validTo is actually consulted: a PAT
// minted with a validTo already in the past must not authorize anything.
func TestPatExpiredValidToIsRefused(t *testing.T) {
	s := New()
	defer s.Close()
	s.RegisterToken("admin", ScopeTokens)
	base := s.URL() + "/fakeorg/_apis/tokens/pats"

	createBody := []byte(`{"displayName":"expired","scope":"vso.code","validTo":"2000-01-01T00:00:00Z","allOrgs":false}`)
	status, body := postJSON(t, base, "admin", createBody)
	if status != http.StatusOK {
		t.Fatalf("create: status=%d body=%s", status, body)
	}
	var created struct {
		PatToken struct {
			Token string `json:"token"`
		} `json:"patToken"`
	}
	if err := json.Unmarshal(body, &created); err != nil {
		t.Fatalf("decode create response: %v\n%s", err, body)
	}

	req, _ := http.NewRequest(http.MethodGet, s.URL()+"/fakeorg/fakeproj/_apis/policy/configurations", nil)
	req.SetBasicAuth("", created.PatToken.Token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET policy with expired PAT: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("expired PAT (validTo 2000-01-01) status = %d, want 401", resp.StatusCode)
	}
}

// TestProjectScopedGitAPIRoutesAccepted proves the project-scoped URL form
// Microsoft documents alongside the org-scoped one (.../{project}/_apis/git/
// repositories/{id}/...) is mounted too — a 404 there is indistinguishable
// from a scope refusal to a test only asserting "not 200".
func TestProjectScopedGitAPIRoutesAccepted(t *testing.T) {
	s := New()
	defer s.Close()
	s.RegisterToken("rw", ScopeCodeRead, ScopeCodeWrite)

	prURL := s.URL() + "/fakeorg/fakeproj/_apis/git/repositories/repo-1/pullrequests"
	status, body := postJSON(t, prURL, "rw", []byte(`{"title":"via project-scoped URL"}`))
	if status != http.StatusCreated {
		t.Fatalf("project-scoped PR create status = %d, body=%s", status, body)
	}
	var created struct {
		PullRequestID int `json:"pullRequestId"`
	}
	if err := json.Unmarshal(body, &created); err != nil {
		t.Fatalf("decode create PR response: %v\n%s", err, body)
	}

	patchURL := fmt.Sprintf("%s/fakeorg/fakeproj/_apis/git/repositories/repo-1/pullrequests/%d", s.URL(), created.PullRequestID)
	req, _ := http.NewRequest(http.MethodPatch, patchURL, bytes.NewReader([]byte(`{"status":"completed"}`)))
	req.Header.Set("Authorization", "Bearer rw")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("project-scoped PR patch: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("project-scoped PR patch status = %d, want 200", resp.StatusCode)
	}

	refsURL := s.URL() + "/fakeorg/fakeproj/_apis/git/repositories/repo-1/refs"
	status, body = postJSON(t, refsURL, "rw", []byte(`{"refUpdates":[{"name":"refs/heads/x","newObjectId":"deadbeef"}]}`))
	if status != http.StatusOK {
		t.Errorf("project-scoped refs POST status = %d, body=%s", status, body)
	}

	pushesURL := s.URL() + "/fakeorg/fakeproj/_apis/git/repositories/repo-1/pushes"
	status, body = postJSON(t, pushesURL, "rw", []byte(`{}`))
	if status != http.StatusCreated {
		t.Errorf("project-scoped pushes POST status = %d, body=%s", status, body)
	}
	req, _ = http.NewRequest(http.MethodGet, pushesURL, nil)
	req.Header.Set("Authorization", "Bearer rw")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("project-scoped pushes GET: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("project-scoped pushes GET status = %d, want 200", resp.StatusCode)
	}
}

// TestWiqlTeamScopedRouteAccepted proves the {team}-scoped wiql URL Microsoft
// also documents is accepted, canonicalized onto the project-scoped route
// (see stripWiqlTeamSegment).
func TestWiqlTeamScopedRouteAccepted(t *testing.T) {
	s := New()
	defer s.Close()
	s.RegisterToken("work-reader", ScopeWorkRead)
	url := s.URL() + "/fakeorg/fakeproj/faketeam/_apis/wit/wiql"
	status, body := postJSON(t, url, "work-reader", []byte(`{"query":"select [System.Id] from WorkItems"}`))
	if status != http.StatusOK {
		t.Fatalf("team-scoped wiql status = %d, body=%s", status, body)
	}
}

// TestWorkItemRoutesEnforceReadVsWriteSplit is the refusal-plus-success pair
// for the two work-item routes, since vso.work vs vso.work_write is the
// split they exist to prove.
func TestWorkItemRoutesEnforceReadVsWriteSplit(t *testing.T) {
	s := New()
	defer s.Close()
	s.RegisterToken("write-only", ScopeWorkWrite)
	s.RegisterToken("read-only", ScopeWorkRead)

	wiqlURL := s.URL() + "/fakeorg/fakeproj/_apis/wit/wiql"
	if status, body := postJSON(t, wiqlURL, "write-only", []byte(`{}`)); status != http.StatusUnauthorized {
		t.Errorf("wiql with a write-only token status = %d, want 401 (wiql needs vso.work): %s", status, body)
	}
	if status, body := postJSON(t, wiqlURL, "read-only", []byte(`{}`)); status != http.StatusOK {
		t.Errorf("wiql with a read-scoped token status = %d, want 200: %s", status, body)
	}

	workItemsURL := s.URL() + "/fakeorg/fakeproj/_apis/wit/workitems/42"
	patchBody := []byte(`[{"op":"add","path":"/fields/System.Title","value":"x"}]`)

	req, _ := http.NewRequest(http.MethodPatch, workItemsURL, bytes.NewReader(patchBody))
	req.Header.Set("Authorization", "Bearer read-only")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("PATCH workitems (read-only): %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("workitems PATCH with a read-only token status = %d, want 401 (needs vso.work_write)", resp.StatusCode)
	}

	req, _ = http.NewRequest(http.MethodPatch, workItemsURL, bytes.NewReader(patchBody))
	req.Header.Set("Authorization", "Bearer write-only")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("PATCH workitems (write-only): %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("workitems PATCH with a write-scoped token status = %d, want 200", resp.StatusCode)
	}
}

// TestRegisterRepoEnablesReceivePack proves RegisterRepo itself sets
// http.receivepack on a caller-supplied bare repo that never went through
// NewFixtureRepo — without it, the fake would grant ScopeCodeWrite and git
// http-backend would then refuse the push anyway for an unrelated config
// reason, indistinguishable from a real scope failure.
func TestRegisterRepoEnablesReceivePack(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("adofake: git not found on PATH, skipping git-backed test")
	}
	s := New()
	defer s.Close()

	bare := t.TempDir()
	if out, err := exec.Command("git", "init", "--bare", "-b", "main", bare).CombinedOutput(); err != nil {
		t.Fatalf("git init --bare: %v\n%s", err, out)
	}
	work := t.TempDir()
	runWithIdentity := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = work
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=adofake", "GIT_AUTHOR_EMAIL=adofake@example.com",
			"GIT_COMMITTER_NAME=adofake", "GIT_COMMITTER_EMAIL=adofake@example.com",
		)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	runWithIdentity("init", "-b", "main")
	runWithIdentity("commit", "--allow-empty", "-m", "seed")
	runWithIdentity("remote", "add", "origin", bare)
	runWithIdentity("push", "origin", "main")

	// RegisterRepo, NOT NewFixtureRepo: this is the caller-built-repo path
	// the finding is about.
	s.RegisterRepo("fakeorg", "fakeproj", "fakerepo", bare)
	s.RegisterToken("rw", ScopeCodeRead, ScopeCodeWrite)

	runWithIdentity("commit", "--allow-empty", "-m", "second")
	out, err := gitWithBearer(t, "rw", "push", s.URL()+"/fakeorg/fakeproj/_git/fakerepo", "main").AtDir(work).CombinedOutput()
	if err != nil {
		t.Fatalf("push through a RegisterRepo'd (non-fixture) repo failed: %v\n%s", err, out)
	}
}

// TestRoutingIsLenientOnCaseSlashAndAPIVersion proves "_apis" and
// "connectionData" match regardless of case, a trailing slash is tolerated,
// and ?api-version= is captured in Requests() (chosen over refusing its
// absence — see the doc comment on RecordedRequest.APIVersion).
func TestRoutingIsLenientOnCaseSlashAndAPIVersion(t *testing.T) {
	s := New()
	defer s.Close()
	s.RegisterToken("reader", ScopeProjectRead)

	for _, path := range []string{
		"/fakeorg/_apis/connectionData",
		"/fakeorg/_APIS/connectiondata",
		"/fakeorg/_apis/connectionData/",
	} {
		req, _ := http.NewRequest(http.MethodGet, s.URL()+path+"?api-version=7.1", nil)
		req.Header.Set("Authorization", "Bearer reader")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Errorf("GET %s status = %d, want 200", path, resp.StatusCode)
		}
	}

	var lastAPIVersion string
	for _, rr := range s.Requests() {
		if rr.Endpoint == EndpointConnectionData {
			lastAPIVersion = rr.APIVersion
		}
	}
	if lastAPIVersion != "7.1" {
		t.Errorf("last recorded connectionData request's APIVersion = %q, want 7.1", lastAPIVersion)
	}
}

// TestPullRequestsConcurrentPostAndPatchUnderRace hammers POST (create) and
// PATCH (guessing at ids that may not exist yet) concurrently: before the
// fix, a POST's response encode (after releasing the lock) could race a
// concurrent PATCH mutating the very same stored map.
func TestPullRequestsConcurrentPostAndPatchUnderRace(t *testing.T) {
	s := New()
	defer s.Close()
	s.RegisterToken("rw", ScopeCodeWrite)
	baseURL := s.URL() + "/fakeorg/_apis/git/repositories/repo-1/pullrequests"

	const n = 50
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			status, _ := postJSON(t, baseURL, "rw", []byte(fmt.Sprintf(`{"title":"pr-%d"}`, i)))
			if status != http.StatusCreated {
				t.Errorf("create PR %d status = %d", i, status)
			}
		}(i)
	}
	for i := 1; i <= n; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			patchURL := fmt.Sprintf("%s/%d", baseURL, id)
			req, _ := http.NewRequest(http.MethodPatch, patchURL, bytes.NewReader([]byte(`{"status":"completed"}`)))
			req.Header.Set("Authorization", "Bearer rw")
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Errorf("patch PR %d: %v", id, err)
				return
			}
			resp.Body.Close() // 200 or 404 are both fine here — we're racing timing, not asserting an outcome
		}(i)
	}
	wg.Wait()
}

// TestOverrideStillRecordsRealScopeDecision proves a SetOverride response
// records the ACTUAL scope decision, not a hardcoded one: a properly-scoped
// token must show Authorized=true in Requests() even though the override
// answered instead of the real handler.
func TestOverrideStillRecordsRealScopeDecision(t *testing.T) {
	s := New()
	defer s.Close()
	s.RegisterToken("read-write", ScopeCodeRead, ScopeCodeWrite)
	s.SetOverride(EndpointPolicyPost, http.StatusServiceUnavailable, []byte(`{"message":"simulated outage"}`))
	defer s.ClearOverride(EndpointPolicyPost)

	url := s.URL() + "/fakeorg/fakeproj/_apis/policy/configurations"
	status, _ := postJSON(t, url, "read-write", []byte(`{}`))
	if status != http.StatusServiceUnavailable {
		t.Fatalf("overridden status = %d, want 503", status)
	}

	reqs := s.Requests()
	if len(reqs) == 0 {
		t.Fatal("no requests recorded")
	}
	last := reqs[len(reqs)-1]
	if last.Endpoint != EndpointPolicyPost {
		t.Fatalf("last recorded request endpoint = %q, want %q", last.Endpoint, EndpointPolicyPost)
	}
	if !last.Authorized {
		t.Errorf("recorded Authorized = false for a properly-scoped token under override, want true (the real decision, not a hardcoded one)")
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
	// A CI runner has no global git identity; commits need one.
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=adofake", "GIT_AUTHOR_EMAIL=adofake@example.com",
		"GIT_COMMITTER_NAME=adofake", "GIT_COMMITTER_EMAIL=adofake@example.com",
	)
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
