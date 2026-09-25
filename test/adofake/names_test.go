// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package adofake

import (
	"encoding/json"
	"net/http"
	"net/url"
	"os/exec"
	"strings"
	"testing"
)

// A PROJECT AND A REPOSITORY WITH SPACES are served the way Azure DevOps
// serves them (#485): the repositories list names them literally and escapes
// them in every URL, and git reaches the repository under any spelling that
// decodes to its names — the fake's own remoteUrl spelling and the one
// Wardyn stores.
func TestSpacedNamesServedLikeAzureDevOps(t *testing.T) {
	s := New()
	defer s.Close()
	bare := NewFixtureRepo(t)
	s.AddProject("fakeorg", "", SpacedProject)
	s.RegisterRepo("fakeorg", SpacedProject, SpacedRepo, bare)
	s.RegisterToken("rw", ScopeCodeRead, ScopeCodeWrite)

	req, _ := http.NewRequest(http.MethodGet, s.URL()+"/fakeorg/Payments%20Platform/_apis/git/repositories?api-version=7.1", nil)
	req.Header.Set("Authorization", "Bearer rw")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	var list struct {
		Value []struct {
			Name      string `json:"name"`
			RemoteURL string `json:"remoteUrl"`
			Project   struct {
				Name string `json:"name"`
			} `json:"project"`
		} `json:"value"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&list); err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("repositories: %d %v", resp.StatusCode, err)
	}
	const remote = "https://fakeorg@dev.azure.com/fakeorg/Payments%20Platform/_git/Card%20Auth%20%28v2%29.Service"
	if len(list.Value) != 1 || list.Value[0].Name != SpacedRepo || list.Value[0].Project.Name != SpacedProject ||
		list.Value[0].RemoteURL != remote {
		t.Fatalf("repositories = %+v, want %q in %q at %s", list.Value, SpacedRepo, SpacedProject, remote)
	}

	for i, path := range []string{
		"/fakeorg/Payments%20Platform/_git/Card%20Auth%20%28v2%29.Service", // the remoteUrl's spelling
		"/fakeorg/Payments%20Platform/_git/Card%20Auth%20(v2).Service",     // Wardyn's stored spelling
	} {
		dest := t.TempDir() + "/clone"
		if out, err := gitWithBearer(t, "rw", "clone", s.URL()+path, dest).CombinedOutput(); err != nil {
			t.Fatalf("clone %s: %v\n%s", path, err, out)
		}
		writeFile(t, dest+"/change.txt", "spaced\n")
		runIn(t, dest, "add", "change.txt")
		runIn(t, dest, "commit", "-m", "spaced push")
		branch := "spaced-" + string(rune('a'+i))
		if out, err := gitWithBearer(t, "rw", "push", "origin", "HEAD:"+branch).AtDir(dest).CombinedOutput(); err != nil {
			t.Fatalf("push %s: %v\n%s", path, err, out)
		}
		if out, err := exec.Command("git", "-C", bare, "rev-parse", "--verify", branch).CombinedOutput(); err != nil {
			t.Fatalf("%s: the pushed branch is not in the repository: %v\n%s", path, err, out)
		}
	}

	// Real Azure DevOps routes "+" in a path as "+": a repository spelled
	// with one is a different (here, absent) repository.
	plus := s.URL() + "/fakeorg/Payments+Platform/_git/" + url.PathEscape(SpacedRepo)
	if out, err := gitWithBearer(t, "rw", "ls-remote", plus).CombinedOutput(); err == nil || !strings.Contains(string(out), "not found") {
		t.Errorf("ls-remote with \"+\" for the space: err=%v\n%s — want the repository not found", err, out)
	}
}
