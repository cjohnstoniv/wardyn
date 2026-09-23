// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// Azure DevOps names a project or repository may carry (#485): a space, and
// every other printable character the naming rules leave permitted — `'` in a
// repository only, since a project name forbids it. See
// learn.microsoft.com/azure/devops/organizations/settings/naming-restrictions.
//
// Each row is the literal names, then the ONE stored spelling of the clone URL.
var adoNameFixtures = []struct{ project, repo, canonical string }{
	{"Payments Platform", "Card Auth (v2).Service",
		"https://dev.azure.com/contoso/Payments%20Platform/_git/Card%20Auth%20(v2).Service"},
	{"R&D Ops!", "Bob's @Tools~1",
		"https://dev.azure.com/contoso/R&D%20Ops!/_git/Bob's%20@Tools~1"},
	{"Café Équipe", "Ünïcode Repo",
		"https://dev.azure.com/contoso/Café%20Équipe/_git/Ünïcode%20Repo"},
	{"100% Done", "Half 50%",
		"https://dev.azure.com/contoso/100%25%20Done/_git/Half%2050%25"},
}

// adoNameSpellings are the ways one repository reaches a door: typed with its
// literal names, pasted as Azure DevOps' own %20-encoded URL, and escaped the
// way Go's url package spells a path segment. A name holding a literal "%" has
// only the escaped spelling: in a URL a bare "%" starts an escape, and
// guessing otherwise is the ambiguity the rule refuses.
func adoNameSpellings(project, repo string) []string {
	escaped := "https://dev.azure.com/contoso/" + url.PathEscape(project) + "/_git/" + url.PathEscape(repo)
	if strings.Contains(project+repo, "%") {
		return []string{escaped}
	}
	sp := func(s string) string { return strings.ReplaceAll(s, " ", "%20") }
	return []string{
		"https://dev.azure.com/contoso/" + project + "/_git/" + repo,
		"https://dev.azure.com/contoso/" + sp(project) + "/_git/" + sp(repo),
		escaped,
	}
}

// THE WORKSPACE DOOR takes every spelling of a spaced repository and stores
// the one canonical URL.
func TestADONames_WorkspaceDoorStoresOneSpelling(t *testing.T) {
	for _, fx := range adoNameFixtures {
		for _, in := range adoNameSpellings(fx.project, fx.repo) {
			body, _ := json.Marshal(map[string]any{"name": "ws", "sources": []map[string]string{{"type": "repo", "source": in}}})
			r := httptest.NewRequest(http.MethodPost, "/api/v1/workspaces", strings.NewReader(string(body)))
			req, msg := decodeWorkspaceRequest(httptest.NewRecorder(), r)
			if msg != "" {
				t.Errorf("%q refused: %s", in, msg)
				continue
			}
			if got := req.Sources[0].Source; got != fx.canonical {
				t.Errorf("%q stored as %q, want %q", in, got, fx.canonical)
			}
		}
	}
}

// THE SOURCE LIBRARY DOOR does the same, and names the entry after the
// repository as Azure DevOps shows it, not after its escapes.
func TestADONames_SourceLibraryDoor(t *testing.T) {
	for _, fx := range adoNameFixtures {
		for _, in := range adoNameSpellings(fx.project, fx.repo) {
			locator, _ := canonicalSourceIdentity(types.SourceRepo, in, "")
			if locator != fx.canonical {
				t.Errorf("%q stored as %q, want %q", in, locator, fx.canonical)
			}
			src := types.Source{Kind: types.SourceRepo, Locator: locator, Name: lastPathSegment(locator)}
			if msg := validateSourceWrite(src); msg != "" {
				t.Errorf("%q refused: %s", in, msg)
			}
			if src.Name != fx.repo {
				t.Errorf("%q named %q, want %q", in, src.Name, fx.repo)
			}
		}
	}
}

// THE RUN DOOR canonicalises the run's repo before any gate reads it, so the
// launch gates, the stored run row and WARDYN_REPOS all see one spelling.
func TestADONames_RunDoorCanonicalises(t *testing.T) {
	h := newHarness(t)
	fx := adoNameFixtures[0]
	for _, in := range adoNameSpellings(fx.project, fx.repo) {
		body, _ := json.Marshal(map[string]string{"agent": "claude-code", "repo": in})
		r := httptest.NewRequest(http.MethodPost, "/api/v1/runs", strings.NewReader(string(body)))
		w := httptest.NewRecorder()
		req, _, _, _, ok := h.srv.decodeAndValidateCreateRun(w, r)
		if !ok {
			t.Fatalf("%q refused: %d %s", in, w.Code, w.Body.String())
		}
		if req.Repo != fx.canonical {
			t.Errorf("%q reached the gates as %q, want %q", in, req.Repo, fx.canonical)
		}
	}
}

// WARDYN_REPOS carries the canonical URL, and the default clone directory is
// the repository's own name with its whitespace made a field-safe "-".
func TestADONames_RepoRecords(t *testing.T) {
	fx := adoNameFixtures[0]
	got, warns := buildRepoRecords("", []types.WorkspaceRepo{{Repo: fx.canonical}})
	want := fx.canonical + "\t/home/agent/work/Card-Auth-(v2).Service\t" + fx.canonical + "\t"
	if got != want || len(warns) != 0 {
		t.Fatalf("records = %q (warnings %v), want %q", got, warns, want)
	}
	if leaf := repoCloneLeaf(fx.canonical); leaf != "Card-Auth-(v2).Service" {
		t.Errorf("repoCloneLeaf = %q, want the clone directory's own name", leaf)
	}
}

// ADMISSION and the Entra lane both read a canonical URL; a spaced repository
// under an admitted organisation resolves its lane like any other.
func TestADONames_AdmissionAndEntraLane(t *testing.T) {
	for _, fx := range adoNameFixtures {
		if _, ok := parseCloneTarget(fx.canonical); !ok {
			t.Errorf("parseCloneTarget(%q) refused it", fx.canonical)
		}
		if run, ok := resolveADOEntraRun(adoSite(adoEntraTestRow()), []string{fx.canonical}, adoTestOwner); !ok || run.org != "contoso" {
			t.Errorf("%q: lane = %+v ok=%v, want organisation contoso", fx.canonical, run, ok)
		}
	}
}

// A PROJECT-SCOPED provider row whose project name has a space is storable,
// admits that project's repositories, and still refuses its neighbours —
// compared on the decoded names, case-sensitively as before.
func TestADONames_ProjectScopedProviderRow(t *testing.T) {
	for _, base := range []string{
		"https://tfs.corp.example/Payments Platform",
		"https://tfs.corp.example/Payments%20Platform",
	} {
		block := normalizeWorkspaceProviders(&types.WorkspaceProviders{Git: []types.GitProvider{
			{ID: "row", Kind: types.GitProviderAzureDevOps, BaseURLs: []string{base}},
		}})
		if err := validateWorkspaceProviders(block, true); err != nil {
			t.Errorf("%q refused: %v", base, err)
			continue
		}
		if got := block.Git[0].BaseURLs[0]; got != "https://tfs.corp.example/Payments%20Platform" {
			t.Errorf("%q stored as %q", base, got)
		}
		sc := types.SiteConfig{WorkspaceProviders: block}
		for repo, want := range map[string]bool{
			"https://tfs.corp.example/Payments%20Platform/_git/Card%20Auth%20(v2).Service": true,
			"https://tfs.corp.example/Payments%20Platform%20Two/_git/app":                  false,
			"https://tfs.corp.example/payments%20platform/_git/app":                        false,
		} {
			if _, ok := providerFor(sc, repo); ok != want {
				t.Errorf("base %q admits %q = %v, want %v", base, repo, ok, want)
			}
		}
	}
}

// NOTHING WIDENS: an encoded separator, a doubly-encoded one, an encoded dot
// segment and an edge space stay refused at every door.
func TestADONames_StructureStaysRefused(t *testing.T) {
	for _, in := range []string{
		"https://dev.azure.com/contoso/a%2Fb/_git/r",
		"https://dev.azure.com/contoso/p/_git/a%5Cb",
		"https://dev.azure.com/contoso/p/_git/a%252Fb",
		"https://dev.azure.com/contoso/%2E%2E/_git/r",
		"https://dev.azure.com/contoso/p%20/_git/r",
		"https://dev.azure.com/contoso/p/_git/r%0A",
		"https://github.com/acme/app%20x",
	} {
		body, _ := json.Marshal(map[string]any{"name": "ws", "sources": []map[string]string{{"type": "repo", "source": in}}})
		r := httptest.NewRequest(http.MethodPost, "/api/v1/workspaces", strings.NewReader(string(body)))
		if _, msg := decodeWorkspaceRequest(httptest.NewRecorder(), r); msg == "" {
			t.Errorf("workspace door accepted %q", in)
		}
		if _, ok := parseCloneTarget(in); ok {
			t.Errorf("parseCloneTarget accepted %q", in)
		}
	}
}
