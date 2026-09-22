// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// fakeProvidersStore is fakeSiteConfigStore plus the two list reads
// sourcesNoLongerAdmitted makes — the counting pass is part of the write, so a
// store that cannot answer them cannot exercise the handler at all.
type fakeProvidersStore struct {
	*fakeSiteConfigStore
	workspaces []types.Workspace
	sources    []types.Source
	listErr    error
}

func (f *fakeProvidersStore) ListWorkspaces(context.Context) ([]types.Workspace, error) {
	return f.workspaces, f.listErr
}

func (f *fakeProvidersStore) ListSources(context.Context) ([]types.Source, error) {
	return f.sources, f.listErr
}

func newProvidersHarness(t *testing.T, fake *fakeProvidersStore) (*Server, *recRecorder) {
	t.Helper()
	h := newHarness(t)
	return New(baseTestConfig(h, fake)), h.audit
}

// doWithHeaders is `do` plus request headers — If-Match is the whole point of
// these two endpoints' concurrency contract and `do` cannot set one.
func doWithHeaders(t *testing.T, srv *Server, method, path, bearer, body string, hdr map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, path, nil)
	} else {
		r = httptest.NewRequest(method, path, strings.NewReader(body))
	}
	r.Header.Set("Authorization", "Bearer "+bearer)
	for k, v := range hdr {
		r.Header.Set(k, v)
	}
	r.Host = "127.0.0.1"
	r.RemoteAddr = "127.0.0.1:54321"
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, r)
	return w
}

// githubRow / adoRow are the two shapes every predicate case is built from.
func githubRow(id string, disabled bool, baseURLs ...string) types.GitProvider {
	return types.GitProvider{ID: id, Kind: types.GitProviderGitHub, Disabled: disabled, BaseURLs: baseURLs}
}

func adoRow(id string, disabled bool, baseURLs ...string) types.GitProvider {
	return types.GitProvider{ID: id, Kind: types.GitProviderAzureDevOps, Disabled: disabled, BaseURLs: baseURLs}
}

func providersConfig(rows []types.GitProvider, scmHosts ...string) types.SiteConfig {
	sc := types.SiteConfig{ScmHosts: scmHosts}
	if rows != nil {
		sc.WorkspaceProviders = &types.WorkspaceProviders{Git: rows}
	}
	return sc
}

// TestValidateWorkspaceProviders is the base-URL/lane/storage validation table:
// every rule §5.1 and §6.0b name, each as its own case, because these are the
// only thing standing between an admin's typo and a policy that reads narrower
// than it is.
func TestValidateWorkspaceProviders(t *testing.T) {
	ok := func(rows ...types.GitProvider) *types.WorkspaceProviders {
		return &types.WorkspaceProviders{Git: rows}
	}
	for _, tc := range []struct {
		name    string
		block   *types.WorkspaceProviders
		wantErr bool
	}{
		{"nil block is legacy open mode", nil, false},
		{"empty block", &types.WorkspaceProviders{}, false},
		{"github.com with an org", ok(githubRow("gh", false, "https://github.com/acme")), false},
		{"bare github.com", ok(githubRow("gh", false, "https://github.com")), false},
		{"GHES host with two segments", ok(githubRow("ghes", false, "https://git.corp.example/acme/team")), false},
		{"dev.azure.com with the org", ok(adoRow("ado", false, "https://dev.azure.com/acme")), false},
		{"legacy visualstudio.com", ok(adoRow("ado", false, "https://acme.visualstudio.com")), false},
		{"ADO Server host", ok(adoRow("ados", false, "https://tfs.corp.example/acme")), false},

		{"http is refused", ok(githubRow("gh", false, "http://github.com/acme")), true},
		{"userinfo is refused", ok(githubRow("gh", false, "https://user:pw@github.com/acme")), true},
		{"a query is refused", ok(githubRow("gh", false, "https://github.com/acme?x=1")), true},
		{"a fragment is refused", ok(githubRow("gh", false, "https://github.com/acme#frag")), true},
		{"a port is refused", ok(githubRow("gh", false, "https://github.com:8443/acme")), true},
		{"three path segments are refused", ok(githubRow("ghes", false, "https://git.corp.example/a/b/c")), true},
		{"a repo path on github.com is refused", ok(githubRow("gh", false, "https://github.com/acme/repo")), true},
		{"a non-dotted host is refused", ok(githubRow("gh", false, "https://localhost/acme")), true},
		{"a shell metacharacter is refused", ok(githubRow("gh", false, "https://github.com/acme`id`")), true},
		{"a percent-ESCAPED metacharacter is refused too",
			ok(githubRow("gh", false, "https://github.com/%60id%60")), true},
		{"a percent-escaped slash is refused (it hides a second segment)",
			ok(githubRow("gh", false, "https://github.com/acme%2Fevil")), true},
		{"a doubled slash normalizes to one segment, so it validates",
			ok(githubRow("gh", false, "https://github.com//acme")), false},
		{"no base URLs", ok(githubRow("gh", false)), true},
		{"nine base URLs", ok(githubRow("gh", false,
			"https://github.com/a1", "https://github.com/a2", "https://github.com/a3",
			"https://github.com/a4", "https://github.com/a5", "https://github.com/a6",
			"https://github.com/a7", "https://github.com/a8", "https://github.com/a9")), true},

		{"an ADO host on a github row", ok(githubRow("gh", false, "https://dev.azure.com/acme")), true},
		{"visualstudio.com on a github row", ok(githubRow("gh", false, "https://acme.visualstudio.com")), true},
		{"github.com on an ADO row", ok(adoRow("ado", false, "https://github.com/acme")), true},
		{"dev.azure.com without an org", ok(adoRow("ado", false, "https://dev.azure.com")), true},
		{"dev.azure.com with two segments", ok(adoRow("ado", false, "https://dev.azure.com/acme/team")), true},

		{"an empty id", ok(githubRow("", false, "https://github.com/acme")), true},
		{"an uppercase id", ok(githubRow("GH", false, "https://github.com/acme")), true},
		{"a duplicate id", ok(
			githubRow("gh", false, "https://github.com/acme"),
			githubRow("gh", false, "https://git.corp.example/acme")), true},
		{"an unknown kind", ok(types.GitProvider{
			ID: "x", Kind: "gitlab", BaseURLs: []string{"https://gitlab.com/acme"},
		}), true},

		{"app on github.com", ok(types.GitProvider{
			ID: "gh", Kind: types.GitProviderGitHub, BaseURLs: []string{"https://github.com"},
			Lanes: []types.GitLane{types.GitLaneApp, types.GitLanePAT, types.GitLaneSSH},
		}), false},
		{"app on a GHES row is refused", ok(types.GitProvider{
			ID: "ghes", Kind: types.GitProviderGitHub, BaseURLs: []string{"https://git.corp.example/acme"},
			Lanes: []types.GitLane{types.GitLaneApp},
		}), true},
		{"app on ADO is refused", ok(types.GitProvider{
			ID: "ado", Kind: types.GitProviderAzureDevOps, BaseURLs: []string{"https://dev.azure.com/acme"},
			Lanes: []types.GitLane{types.GitLaneApp},
		}), true},
		{"ssh on an ADO Server row is refused", ok(types.GitProvider{
			ID: "ados", Kind: types.GitProviderAzureDevOps, BaseURLs: []string{"https://tfs.corp.example/acme"},
			Lanes: []types.GitLane{types.GitLaneSSH},
		}), true},
		{"#380: explicit ssh lane on an org-scoped row is refused", ok(types.GitProvider{
			ID: "ado", Kind: types.GitProviderAzureDevOps, BaseURLs: []string{"https://dev.azure.com/acme"},
			Lanes: []types.GitLane{types.GitLaneSSH},
		}), true},
		{"#380: empty lanes on an org-scoped row is admitted (host-level SSH warns, not refuses)",
			ok(types.GitProvider{
				ID: "ado", Kind: types.GitProviderAzureDevOps, BaseURLs: []string{"https://dev.azure.com/acme"},
			}), false},
		{"pat needs no host", ok(types.GitProvider{
			ID: "ghes", Kind: types.GitProviderGitHub, BaseURLs: []string{"https://git.corp.example/acme"},
			Lanes: []types.GitLane{types.GitLanePAT},
		}), false},
		{"an unknown lane", ok(types.GitProvider{
			ID: "gh", Kind: types.GitProviderGitHub, BaseURLs: []string{"https://github.com/acme"},
			Lanes: []types.GitLane{"oauth"},
		}), true},

		{"storage ceilings", &types.WorkspaceProviders{Storage: &types.StorageProviders{
			Ephemeral: &types.EphemeralProvider{DefaultDiskMiB: 4096, MaxDiskMiB: 20480},
			UserDrive: &types.UserDriveProvider{MaxSizeMiB: 51200},
		}}, false},
		{"default above max is refused", &types.WorkspaceProviders{Storage: &types.StorageProviders{
			Ephemeral: &types.EphemeralProvider{DefaultDiskMiB: 40960, MaxDiskMiB: 20480},
		}}, true},
		{"a default above an UNSET max is fine", &types.WorkspaceProviders{Storage: &types.StorageProviders{
			Ephemeral: &types.EphemeralProvider{DefaultDiskMiB: 40960},
		}}, false},
		{"a negative ephemeral size is refused", &types.WorkspaceProviders{Storage: &types.StorageProviders{
			Ephemeral: &types.EphemeralProvider{MaxDiskMiB: -1},
		}}, true},
		{"a negative drive ceiling is refused", &types.WorkspaceProviders{Storage: &types.StorageProviders{
			UserDrive: &types.UserDriveProvider{MaxSizeMiB: -1},
		}}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := validateWorkspaceProviders(normalizeWorkspaceProviders(tc.block))
			if (err != nil) != tc.wantErr {
				t.Fatalf("validateWorkspaceProviders() error = %v, wantErr = %v", err, tc.wantErr)
			}
		})
	}
}

// TestValidateWorkspaceProvidersRefusalStrings pins that the refusals go out
// through the DRAFT constants rather than hand-typed prose, so the owner's canon
// sitting swaps one file and no test.
func TestValidateWorkspaceProvidersRefusalStrings(t *testing.T) {
	err := validateWorkspaceProviders(&types.WorkspaceProviders{Storage: &types.StorageProviders{
		Ephemeral: &types.EphemeralProvider{DefaultDiskMiB: 2, MaxDiskMiB: 1},
	}})
	if err == nil || err.Error() != providers400DiskOrder {
		t.Errorf("ephemeral order refusal = %v, want the providers400DiskOrder constant", err)
	}
	err = validateWorkspaceProviders(&types.WorkspaceProviders{Git: []types.GitProvider{
		githubRow("gh", false, "https://github.com/acme"),
		githubRow("gh", false, "https://git.corp.example/acme"),
	}})
	if err == nil || !strings.Contains(err.Error(), "not unique") {
		t.Errorf("duplicate id refusal = %v, want the providers400DupID constant", err)
	}
}

// TestBaseURLClientMirrorParity is the SERVER half of the base-URL parity table.
// Its twin is ui/src/app/components/screens/providers/display.test.tsx's PARITY
// list — the SAME literal cases, kinds, verdicts and order, asserted there
// against baseURLError, the console's pre-attempt mirror of this gate. A rule
// ported to one side and not the other reds the other side. The mirror existed
// and silently admitted five shapes this validator 400s (V1 lens E, finding 5),
// including `dev.azure.com/org/project` — the URL an ADO user copies out of
// their browser. Keep the two lists in lockstep.
func TestBaseURLClientMirrorParity(t *testing.T) {
	for _, tc := range []struct {
		url     string
		kind    types.GitProviderKind
		refused bool
	}{
		// github.com: an optional single /<org>, never a repo path.
		{"https://github.com", types.GitProviderGitHub, false},
		{"https://github.com/acme", types.GitProviderGitHub, false},
		{"https://github.com/acme/repo", types.GitProviderGitHub, true},
		// A doubled slash is one segment (normalizeProviderBaseURL rebuilds the path).
		{"https://github.com//acme", types.GitProviderGitHub, false},
		// dev.azure.com: the org segment is REQUIRED, and it is the ONLY one.
		{"https://dev.azure.com/acme", types.GitProviderAzureDevOps, false},
		{"https://dev.azure.com", types.GitProviderAzureDevOps, true},
		{"https://dev.azure.com/acme/proj", types.GitProviderAzureDevOps, true},
		// kind x host: the two well-known hosts belong to exactly one kind each.
		{"https://github.com/acme", types.GitProviderAzureDevOps, true},
		{"https://dev.azure.com/acme", types.GitProviderGitHub, true},
		{"https://acme.visualstudio.com", types.GitProviderGitHub, true},
		{"https://acme.visualstudio.com", types.GitProviderAzureDevOps, false},
		// Any OTHER host is accepted for either kind, with an OPTIONAL path (Q12).
		{"https://git.corp.example", types.GitProviderGitHub, false},
		{"https://git.corp.example/acme/team", types.GitProviderGitHub, false},
		{"https://git.corp.example/a/b/c", types.GitProviderGitHub, true},
		{"https://tfs.corp.example/acme", types.GitProviderAzureDevOps, false},
		// Shape: https only, no userinfo, no port, no query or fragment, a dotted
		// host, and no percent-escape in the path (it hides a second segment).
		{"http://github.com/acme", types.GitProviderGitHub, true},
		{"https://user:pw@github.com/acme", types.GitProviderGitHub, true},
		{"https://github.com:8443/acme", types.GitProviderGitHub, true},
		{"https://github.com/acme?x=1", types.GitProviderGitHub, true},
		{"https://github.com/acme#frag", types.GitProviderGitHub, true},
		{"https://localhost/acme", types.GitProviderGitHub, true},
		{"https://github.com/acme%2Fevil", types.GitProviderGitHub, true},
		{"https://github.com/%60id%60", types.GitProviderGitHub, true},
		{"not a url", types.GitProviderGitHub, true},
	} {
		block := &types.WorkspaceProviders{Git: []types.GitProvider{
			{ID: "row", Kind: tc.kind, BaseURLs: []string{tc.url}},
		}}
		err := validateWorkspaceProviders(normalizeWorkspaceProviders(block))
		if (err != nil) != tc.refused {
			t.Errorf("validate(%q, %s) = %v, want refused=%v", tc.url, string(tc.kind), err, tc.refused)
		}
	}
}

// TestNormalizeWorkspaceProviders pins the two write-time canonicalizations —
// {} becomes an ABSENT key (or GET /site-config renders an empty object forever)
// and a base URL is stored in one canonical form.
func TestNormalizeWorkspaceProviders(t *testing.T) {
	if got := normalizeWorkspaceProviders(&types.WorkspaceProviders{}); got != nil {
		t.Errorf("an empty block normalized to %+v, want nil", got)
	}
	empty := &types.WorkspaceProviders{Storage: &types.StorageProviders{}}
	if got := normalizeWorkspaceProviders(empty); got != nil {
		t.Errorf("a block whose storage halves are both nil normalized to %+v, want nil", got)
	}
	for _, tc := range []struct{ name, in, want string }{
		{"host and trailing slash", "HTTPS://GitHub.com/Acme/", "https://github.com/acme"},
		{"a doubled slash collapses, so stored == canonical",
			"https://github.com//acme", "https://github.com/acme"},
		{"trailing slashes only", "https://github.com///", "https://github.com"},
		{"github.com folds its path — GitHub treats /Acme and /acme as one org",
			"https://github.com/Acme/Team", "https://github.com/acme/team"},
		{"Azure DevOps project paths are case-SENSITIVE and are left alone",
			"https://dev.azure.com/Acme", "https://dev.azure.com/Acme"},
		{"a self-hosted host is left alone too",
			"https://git.corp.example/Acme", "https://git.corp.example/Acme"},
		{"a percent-escape is NOT decoded — validation refuses it instead",
			"https://github.com/%60id%60", "https://github.com/%60id%60"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			block := normalizeWorkspaceProviders(&types.WorkspaceProviders{
				Git: []types.GitProvider{githubRow("gh", false, tc.in)},
			})
			if block == nil || block.Git[0].BaseURLs[0] != tc.want {
				t.Errorf("normalize(%q) = %q, want %q", tc.in, block.Git[0].BaseURLs[0], tc.want)
			}
		})
	}
}

// TestProviderBaseURLRefusalSentences is nit (a): a percent-escaped slash used
// to trip the kind x host table first and be refused as "github.com is not a
// github host", which sends an admin to look at the wrong field entirely. Every
// malformed ADDRESS must refuse with the address sentence.
func TestProviderBaseURLRefusalSentences(t *testing.T) {
	for _, raw := range []string{
		"https://github.com/acme%2Fevil", "https://github.com/%60id%60",
		"http://github.com/acme", "https://github.com:8443/acme",
	} {
		err := validateWorkspaceProviders(normalizeWorkspaceProviders(&types.WorkspaceProviders{
			Git: []types.GitProvider{githubRow("gh", false, raw)},
		}))
		want := fmt.Sprintf(providers400BaseURL, 0)
		if err == nil || err.Error() != want {
			t.Errorf("validate(%q) = %v, want the base-URL sentence %q", raw, err, want)
		}
	}
}

// TestProviderForMatchRule is the match rule and the host-claim precedence,
// case by case. Every input is a DERIVED clone URL (repoCloneURL's output), which
// is what the eight admission sites pass.
func TestProviderForMatchRule(t *testing.T) {
	for _, tc := range []struct {
		name     string
		sc       types.SiteConfig
		cloneURL string
		want     bool
		wantRow  string
	}{
		{"no rows admits anything", providersConfig(nil), "https://gitlab.com/x/y.git", true, ""},
		{"an exact org path", providersConfig([]types.GitProvider{githubRow("gh", false, "https://github.com/acme")}),
			"https://github.com/acme/repo.git", true, "gh"},
		{"the base path itself", providersConfig([]types.GitProvider{githubRow("gh", false, "https://github.com/acme")}),
			"https://github.com/acme", true, "gh"},
		{"a sibling prefix is NOT inside the base path",
			providersConfig([]types.GitProvider{githubRow("gh", false, "https://github.com/acme")}),
			"https://github.com/acme-evil/repo.git", false, "gh"},
		{"a bare host base URL admits every path",
			providersConfig([]types.GitProvider{githubRow("gh", false, "https://github.com")}),
			"https://github.com/anyone/repo.git", true, "gh"},
		{"the host is folded case-insensitively",
			providersConfig([]types.GitProvider{githubRow("gh", false, "https://github.com/acme")}),
			"https://GitHub.COM/acme/repo.git", true, "gh"},
		{"github.com folds the path case, so a repo under /Acme is admitted by /acme",
			providersConfig([]types.GitProvider{githubRow("gh", false, "https://github.com/acme")}),
			"https://github.com/Acme/repo.git", true, "gh"},
		{"...and the other direction, from a base URL written /Acme",
			providersConfig([]types.GitProvider{{ID: "gh", Kind: types.GitProviderGitHub,
				BaseURLs: []string{"https://github.com/Acme"}}}),
			"https://github.com/acme/repo.git", true, "gh"},
		{"Azure DevOps does NOT fold: a different-cased project is refused",
			providersConfig([]types.GitProvider{{ID: "ado", Kind: types.GitProviderAzureDevOps,
				BaseURLs: []string{"https://dev.azure.com/Acme"}}}),
			"https://dev.azure.com/acme/_git/repo", false, "ado"},
		{"a self-hosted host does not fold either",
			providersConfig([]types.GitProvider{{ID: "ghes", Kind: types.GitProviderGitHub,
				BaseURLs: []string{"https://git.corp.example/Acme"}}}),
			"https://git.corp.example/acme/repo.git", false, "ghes"},
		{"an http clone URL never matches an https base URL",
			providersConfig([]types.GitProvider{githubRow("gh", false, "https://github.com/acme")}),
			"http://github.com/acme/repo.git", false, "gh"},
		{"an unparseable clone URL is refused",
			providersConfig([]types.GitProvider{githubRow("gh", false, "https://github.com/acme")}), "", false, ""},

		// THE TRAVERSAL ROWS (V1 lens A). Every one of these was ADMITTED by the
		// /acme row before the guard: the server compared the DECODED, unsquashed
		// path against the base URL while the sandbox's git squashed the dot
		// segments and sent %2F raw, so the proxy minted the acme PAT for
		// evil/repo. There is no prefix match that survives two readers, so the
		// SHAPE is refused — at parseCloneTarget, which every door resolves
		// through. wantRow "" because nothing gets far enough to claim the host.
		{"a parent dot segment is refused, not squashed into the base path",
			providersConfig([]types.GitProvider{githubRow("gh", false, "https://github.com/acme")}),
			"https://github.com/acme/../evil/repo.git", false, ""},
		{"a mixed . / .. path is refused",
			providersConfig([]types.GitProvider{githubRow("gh", false, "https://github.com/acme")}),
			"https://github.com/acme/./x/../../evil/repo.git", false, ""},
		{"a percent-encoded separator is refused (git sends %2F raw)",
			providersConfig([]types.GitProvider{githubRow("gh", false, "https://github.com/acme")}),
			"https://github.com/acme%2Fevil/repo.git", false, ""},
		{"an empty segment is refused",
			providersConfig([]types.GitProvider{githubRow("gh", false, "https://github.com/acme")}),
			"https://github.com/acme//../evil/repo.git", false, ""},
		{"a backslash in the path is refused",
			providersConfig([]types.GitProvider{githubRow("gh", false, "https://github.com/acme")}),
			`https://github.com/acme\..\evil/repo.git`, false, ""},
		{"the scp form gets the SAME rule, host-level admission notwithstanding",
			providersConfig([]types.GitProvider{githubRow("gh", false, "https://github.com/acme")}),
			"git@github.com:acme/../evil/repo.git", false, ""},
		{"an ssh:// URL gets the same rule",
			providersConfig([]types.GitProvider{githubRow("gh", false, "https://github.com/acme")}),
			"ssh://git@github.com/acme/%2e%2e/evil/repo.git", false, ""},

		{"scp form matches host-level",
			providersConfig([]types.GitProvider{githubRow("gh", false, "https://github.com/acme")}),
			"git@github.com:acme/repo.git", true, "gh"},
		{"scp form is host-level ONLY, so another org still matches",
			providersConfig([]types.GitProvider{githubRow("gh", false, "https://github.com/acme")}),
			"git@github.com:other/repo.git", true, "gh"},
		{"the ssh.<host> alias matches",
			providersConfig([]types.GitProvider{adoRow("ado", false, "https://dev.azure.com/acme")}),
			"git@ssh.dev.azure.com:v3/acme/proj/repo", true, "ado"},
		{"an ssh:// URL matches",
			providersConfig([]types.GitProvider{githubRow("gh", false, "https://github.com/acme")}),
			"ssh://git@github.com/acme/repo.git", true, "gh"},

		{"a disabled row REFUSES its host, never falls through",
			providersConfig([]types.GitProvider{githubRow("gh", true, "https://github.com/acme")},
				"github.com"),
			"https://github.com/acme/repo.git", false, "gh"},
		{"an enabled row claims its host KIND-WIDE, so another org is refused",
			providersConfig([]types.GitProvider{githubRow("gh", false, "https://github.com/acme")},
				"github.com"),
			"https://github.com/other/repo.git", false, "gh"},
		{"an ADO row claims dev.azure.com kind-wide even when its base URL is visualstudio.com",
			providersConfig([]types.GitProvider{adoRow("ado", false, "https://acme.visualstudio.com")},
				"dev.azure.com"),
			"https://dev.azure.com/acme/_git/repo", false, "ado"},
		{"a GHES row claims only its own host",
			providersConfig([]types.GitProvider{githubRow("ghes", false, "https://git.corp.example/acme")}),
			"https://git.corp.example/other/repo.git", false, "ghes"},

		{"an UNCLAIMED legacy host is still admitted",
			providersConfig([]types.GitProvider{githubRow("gh", false, "https://github.com/acme")},
				"gitlab.corp.example"),
			"https://gitlab.corp.example/team/repo.git", true, ""},
		{"a host on no list at all is refused",
			providersConfig([]types.GitProvider{githubRow("gh", false, "https://github.com/acme")}),
			"https://bitbucket.org/team/repo.git", false, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			row, ok := providerFor(tc.sc, tc.cloneURL)
			if ok != tc.want {
				t.Fatalf("providerFor(%q) admitted = %v, want %v", tc.cloneURL, ok, tc.want)
			}
			if row.ID != tc.wantRow {
				t.Errorf("providerFor(%q) deciding row = %q, want %q", tc.cloneURL, row.ID, tc.wantRow)
			}
		})
	}
}

// TestAdmitRepoURLReportsWhyItAdmitted pins the two admitted-WITHOUT-a-row
// cases apart: legacy open mode says nothing, an unclaimed legacy host earns the
// ADMIT.LEGACY_HOST warning. A predicate that could not tell them apart would
// warn on every run of every unconfigured install.
func TestAdmitRepoURLReportsWhyItAdmitted(t *testing.T) {
	open := admitRepoURL(providersConfig(nil), "https://github.com/acme/repo.git")
	if !open.Admitted || !open.Unconfigured || open.LegacyHost {
		t.Errorf("legacy open mode = %+v, want admitted+unconfigured with no warning", open)
	}
	legacy := admitRepoURL(
		providersConfig([]types.GitProvider{githubRow("gh", false, "https://github.com/acme")}, "gitlab.corp.example"),
		"https://gitlab.corp.example/team/repo.git")
	if !legacy.Admitted || !legacy.LegacyHost || legacy.Unconfigured {
		t.Errorf("unclaimed legacy host = %+v, want admitted with the legacy-host warning", legacy)
	}
}

// TestProvidersConfiguredIsTheUpgradePin is THE upgrade pin: with no provider
// rows, admission answers exactly what it answered before this feature existed —
// yes, to everything, including the shapes a configured install refuses.
func TestProvidersConfiguredIsTheUpgradePin(t *testing.T) {
	for _, sc := range []types.SiteConfig{
		{},
		{ScmHosts: []string{"github.example.com"}},
		// A storage-only block is still "no git providers": the storage halves
		// are a different question and must not switch admission on.
		{WorkspaceProviders: &types.WorkspaceProviders{Storage: &types.StorageProviders{
			Ephemeral: &types.EphemeralProvider{MaxDiskMiB: 1024},
		}}},
	} {
		if providersConfigured(sc) {
			t.Fatalf("providersConfigured(%+v) = true, want false", sc)
		}
		for _, url := range []string{
			"https://github.com/anyone/repo.git", "https://dev.azure.com/anyone/_git/repo",
			"https://gitlab.com/anyone/repo.git", "git@github.com:anyone/repo.git",
			"https://bitbucket.org/anyone/repo.git",
		} {
			if _, ok := providerFor(sc, url); !ok {
				t.Errorf("providerFor(%q) refused on an install with no provider rows — the upgrade is not "+
					"byte-identical", url)
			}
		}
	}
}

// TestEffectiveScmHosts pins the read-side union the three consumers swapped to.
func TestEffectiveScmHosts(t *testing.T) {
	for _, tc := range []struct {
		name string
		sc   types.SiteConfig
		want []string
	}{
		{"no providers passes the legacy list through",
			providersConfig(nil, "github.example.com", "dev.azure.com"),
			[]string{"github.example.com", "dev.azure.com"}},
		{"an enabled row contributes its hosts",
			providersConfig([]types.GitProvider{githubRow("gh", false, "https://github.com/acme", "https://git.corp.example/acme")}),
			[]string{"github.com", "git.corp.example"}},
		{"a DISABLED row subtracts its host from the legacy list",
			providersConfig([]types.GitProvider{githubRow("gh", true, "https://github.com/acme")}, "github.com"),
			nil},
		{"a kind-wide claim subtracts dev.azure.com for a visualstudio.com row",
			providersConfig([]types.GitProvider{adoRow("ado", false, "https://acme.visualstudio.com")}, "dev.azure.com"),
			[]string{"acme.visualstudio.com"}},
		{"an unclaimed legacy host survives beside a provider host",
			providersConfig([]types.GitProvider{githubRow("gh", false, "https://github.com/acme")}, "gitlab.corp.example"),
			[]string{"gitlab.corp.example", "github.com"}},
		{"a host named twice appears once",
			providersConfig([]types.GitProvider{githubRow("ghes", false, "https://git.corp.example/a", "https://git.corp.example/b")}),
			[]string{"git.corp.example"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := effectiveScmHosts(tc.sc)
			if strings.Join(got, ",") != strings.Join(tc.want, ",") {
				t.Errorf("effectiveScmHosts() = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestWorkspaceProvidersGet pins the read: a never-configured install gets the
// zero-value document with 200 and an ETag it can send back.
func TestWorkspaceProvidersGet(t *testing.T) {
	fake := &fakeProvidersStore{fakeSiteConfigStore: &fakeSiteConfigStore{}}
	srv, _ := newProvidersHarness(t, fake)
	w := do(t, srv, http.MethodGet, "/api/v1/workspace-providers", adminToken, "")
	if w.Code != http.StatusOK {
		t.Fatalf("GET = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	if got := strings.TrimSpace(w.Body.String()); got != "{}" {
		t.Errorf("unconfigured GET body = %s, want {}", got)
	}
	if w.Header().Get("ETag") == "" {
		t.Error("GET carries no ETag — the console's If-Match round trip has nothing to send")
	}
}

// TestWorkspaceProvidersGetProjectsGitPatBrokerEnabled is #381's wire contract:
// a configured install's GET states the real WARDYN_GIT_PAT_BROKER switch (on
// by default) as git_pat_broker_enabled, true or false, so the console can
// label the PAT lane correctly — and a PUT can never smuggle a stale value
// back into storage.
func TestWorkspaceProvidersGetProjectsGitPatBrokerEnabled(t *testing.T) {
	row := githubRow("gh", false, "https://github.com/acme")
	for _, tc := range []struct {
		name                string
		disableGitPATBroker bool
		want                *bool
	}{
		{"broker on (0.7.10 default)", false, boolPtr(true)},
		{"broker off (operator escape hatch)", true, boolPtr(false)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			fake := &fakeProvidersStore{fakeSiteConfigStore: &fakeSiteConfigStore{
				cfg: types.SiteConfig{WorkspaceProviders: &types.WorkspaceProviders{Git: []types.GitProvider{row}}},
			}}
			cfg := baseTestConfig(h, fake)
			cfg.DisableGitPATBroker = tc.disableGitPATBroker
			srv := New(cfg)
			w := do(t, srv, http.MethodGet, "/api/v1/workspace-providers", adminToken, "")
			if w.Code != http.StatusOK {
				t.Fatalf("GET = %d, want 200; body=%s", w.Code, w.Body.String())
			}
			var got types.WorkspaceProviders
			if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
				t.Fatalf("decode: %v (body=%s)", err, w.Body.String())
			}
			if got.GitPatBrokerEnabled == nil || *got.GitPatBrokerEnabled != *tc.want {
				t.Fatalf("git_pat_broker_enabled = %v, want %v", got.GitPatBrokerEnabled, *tc.want)
			}
		})
	}
}

// TestWorkspaceProvidersPutClearsGitPatBrokerEnabled: a client that echoes the
// GET response's git_pat_broker_enabled field back on a PUT (the ordinary
// GET-modify-PUT pattern this screen already uses) must never persist it —
// GitPatBrokerEnabled is server-projected, not stored config.
func TestWorkspaceProvidersPutClearsGitPatBrokerEnabled(t *testing.T) {
	fake := &fakeProvidersStore{fakeSiteConfigStore: &fakeSiteConfigStore{}}
	srv, _ := newProvidersHarness(t, fake)
	w := do(t, srv, http.MethodPut, "/api/v1/workspace-providers", adminToken,
		`{"git":[{"id":"gh","kind":"github","base_urls":["https://github.com/acme"]}],"git_pat_broker_enabled":true}`)
	if w.Code != http.StatusOK {
		t.Fatalf("PUT = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	if fake.putSeen == nil || fake.putSeen.WorkspaceProviders == nil {
		t.Fatal("the provider block was not written")
	}
	if fake.putSeen.WorkspaceProviders.GitPatBrokerEnabled != nil {
		t.Fatalf("stored WorkspaceProviders.GitPatBrokerEnabled = %v, want nil (never stored)",
			*fake.putSeen.WorkspaceProviders.GitPatBrokerEnabled)
	}
}

// TestWorkspaceProvidersPut is the write: it persists, it audits, it hands back
// the new ETag, and {} clears.
func TestWorkspaceProvidersPut(t *testing.T) {
	fake := &fakeProvidersStore{fakeSiteConfigStore: &fakeSiteConfigStore{
		cfg: types.SiteConfig{ScmHosts: []string{"github.example.com"}},
	}}
	srv, audit := newProvidersHarness(t, fake)

	body := `{"git":[{"id":"gh","kind":"github","base_urls":["https://GitHub.com/acme/"],"lanes":["app","pat"]}],` +
		`"storage":{"ephemeral":{"default_disk_mib":4096,"max_disk_mib":20480}}}`
	w := do(t, srv, http.MethodPut, "/api/v1/workspace-providers", adminToken, body)
	if w.Code != http.StatusOK {
		t.Fatalf("PUT = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	if fake.putSeen == nil || fake.putSeen.WorkspaceProviders == nil {
		t.Fatal("the provider block was not written")
	}
	if got := fake.putSeen.WorkspaceProviders.Git[0].BaseURLs[0]; got != "https://github.com/acme" {
		t.Errorf("stored base URL = %q, want it normalized", got)
	}
	// The legacy list is untouched: scm_hosts is never written by this feature.
	if len(fake.putSeen.ScmHosts) != 1 || fake.putSeen.ScmHosts[0] != "github.example.com" {
		t.Errorf("scm_hosts = %v, want the legacy list untouched", fake.putSeen.ScmHosts)
	}
	if fake.putSeen.EffectiveScmHosts != nil {
		t.Errorf("effective_scm_hosts = %v was persisted; it is projected on read only",
			fake.putSeen.EffectiveScmHosts)
	}
	if w.Header().Get("ETag") == "" {
		t.Error("PUT echoes no ETag")
	}
	var resp workspaceProvidersPutResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.SourcesNoLongerAdmitted != 0 {
		t.Errorf("sources_no_longer_admitted = %d, want 0 with nothing onboarded", resp.SourcesNoLongerAdmitted)
	}
	if !strings.Contains(w.Body.String(), `"sources_no_longer_admitted":0`) {
		t.Error("sources_no_longer_admitted is absent from the body — 0 is the reassurance an admin narrowing " +
			"a base URL is looking for, so it must not be omitempty")
	}

	var writes []types.AuditEvent
	for _, ev := range audit.events {
		if ev.Action == "workspace_provider.write" {
			writes = append(writes, ev)
		}
	}
	if len(writes) != 1 {
		t.Fatalf("want exactly 1 workspace_provider.write event, got %d: %+v", len(writes), audit.events)
	}
	var datum map[string]any
	if err := json.Unmarshal(writes[0].Data, &datum); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"git_count", "kinds", "base_urls", "lanes", "disabled", "sources_no_longer_admitted"} {
		if _, ok := datum[key]; !ok {
			t.Errorf("the audit datum has no %q field: %v", key, datum)
		}
	}
	if writes[0].Target != "workspace_providers" {
		t.Errorf("audit target = %q, want workspace_providers", writes[0].Target)
	}

	// {} is the clear form, and it must leave an ABSENT key rather than an empty
	// object rendered on every later GET.
	w = do(t, srv, http.MethodPut, "/api/v1/workspace-providers", adminToken, `{}`)
	if w.Code != http.StatusOK {
		t.Fatalf("clearing PUT = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	if fake.putSeen.WorkspaceProviders != nil {
		t.Errorf("an empty PUT stored %+v, want nil", fake.putSeen.WorkspaceProviders)
	}
	scBody := do(t, srv, http.MethodGet, "/api/v1/site-config", adminToken, "").Body.String()
	if strings.Contains(scBody, "workspace_providers") {
		t.Errorf("GET /site-config still renders a workspace_providers key after a clear: %s", scBody)
	}
}

// TestWorkspaceProvidersPutRefusals pins the two refusal doors: a malformed
// block never reaches the store, and a stale If-Match is a 412 carrying the
// DRAFT canon sentence rather than a silent overwrite.
func TestWorkspaceProvidersPutRefusals(t *testing.T) {
	fake := &fakeProvidersStore{fakeSiteConfigStore: &fakeSiteConfigStore{}}
	srv, audit := newProvidersHarness(t, fake)

	w := do(t, srv, http.MethodPut, "/api/v1/workspace-providers", adminToken,
		`{"git":[{"id":"gh","kind":"github","base_urls":["http://github.com/acme"]}]}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("a plain-http base URL = %d, want 400; body=%s", w.Code, w.Body.String())
	}
	if fake.putSeen != nil {
		t.Error("a refused write reached the store")
	}
	if len(audit.events) != 0 {
		t.Errorf("a refused write audited %d events", len(audit.events))
	}
	if w = do(t, srv, http.MethodPut, "/api/v1/workspace-providers", adminToken, `{"bogus":1}`); w.Code != http.StatusBadRequest {
		t.Errorf("an unknown field = %d, want 400", w.Code)
	}

	w = doWithHeaders(t, srv, http.MethodPut, "/api/v1/workspace-providers", adminToken,
		`{"git":[{"id":"gh","kind":"github","base_urls":["https://github.com/acme"]}]}`,
		map[string]string{"If-Match": `"deadbeef"`})
	if w.Code != http.StatusPreconditionFailed {
		t.Fatalf("a stale If-Match = %d, want 412; body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), providers412Stale) {
		t.Errorf("412 body = %s, want the providers412Stale constant", w.Body.String())
	}

	// And the ETag the GET handed out DOES satisfy the write.
	etag := do(t, srv, http.MethodGet, "/api/v1/workspace-providers", adminToken, "").Header().Get("ETag")
	w = doWithHeaders(t, srv, http.MethodPut, "/api/v1/workspace-providers", adminToken,
		`{"git":[{"id":"gh","kind":"github","base_urls":["https://github.com/acme"]}]}`,
		map[string]string{"If-Match": etag})
	if w.Code != http.StatusOK {
		t.Fatalf("the GET's own ETag = %d, want 200; body=%s", w.Code, w.Body.String())
	}
}

// TestWorkspaceProvidersPutCountsSourcesItRefuses pins "narrowing is never
// silent": the count covers BOTH onboarded workspace repo sources and the source
// library, and a local_dir source is never counted.
func TestWorkspaceProvidersPutCountsSourcesItRefuses(t *testing.T) {
	fake := &fakeProvidersStore{
		fakeSiteConfigStore: &fakeSiteConfigStore{},
		workspaces: []types.Workspace{{
			ID: uuid.New(), Sources: []types.WorkspaceSource{
				{Type: types.WorkspaceSourceTypeRepo, Source: "https://github.com/acme/keep.git"},
				{Type: types.WorkspaceSourceTypeRepo, Source: "https://github.com/other/cut.git"},
				{Type: types.WorkspaceSourceTypeLocalDir, Path: "/srv/work"},
			},
		}},
		sources: []types.Source{
			{ID: uuid.New(), Kind: types.SourceRepo, Locator: "https://gitlab.com/team/cut.git"},
			{ID: uuid.New(), Kind: types.SourceLocalDir, Locator: "/srv/other"},
			// The SAME repository the workspace above already carries, sitting in
			// the library too — the ordinary state after an onboard. It is ONE
			// repo the admin is about to cut off, and counting it twice
			// overstated the blast radius of their own narrowing (V1 lens A).
			{ID: uuid.New(), Kind: types.SourceRepo, Locator: "https://github.com/other/cut.git"},
		},
	}
	srv, _ := newProvidersHarness(t, fake)
	w := do(t, srv, http.MethodPut, "/api/v1/workspace-providers", adminToken,
		`{"git":[{"id":"gh","kind":"github","base_urls":["https://github.com/acme"]}]}`)
	if w.Code != http.StatusOK {
		t.Fatalf("PUT = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var resp workspaceProvidersPutResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.SourcesNoLongerAdmitted != 2 {
		t.Errorf("sources_no_longer_admitted = %d, want 2 (the other-org repo — counted ONCE though it is "+
			"both attached and in the library — and the gitlab library source; the two local_dir rows are "+
			"not repos)", resp.SourcesNoLongerAdmitted)
	}
}

// TestWorkspaceProvidersPutFailsRatherThanMiscount is the honesty half of the
// count: a list error makes the write FAIL, because a comforting "0 affected" is
// the one answer this surface must never give.
func TestWorkspaceProvidersPutFailsRatherThanMiscount(t *testing.T) {
	fake := &fakeProvidersStore{
		fakeSiteConfigStore: &fakeSiteConfigStore{},
		listErr:             context.DeadlineExceeded,
	}
	srv, _ := newProvidersHarness(t, fake)
	w := do(t, srv, http.MethodPut, "/api/v1/workspace-providers", adminToken,
		`{"git":[{"id":"gh","kind":"github","base_urls":["https://github.com/acme"]}]}`)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("PUT with an unreadable workspace list = %d, want 500; body=%s", w.Code, w.Body.String())
	}
	if fake.putSeen != nil {
		t.Error("the block was stored anyway — the admin would have been told nothing was affected")
	}
}

// TestWorkspaceProvidersRMWIsSerialized is SEAM-1 for the new door: this handler
// and PUT /site-config read-modify-write the SAME singleton, so an unguarded RMW
// here silently erases whichever landed first. Both writers run concurrently and
// the losing field must still be there at the end.
func TestWorkspaceProvidersRMWIsSerialized(t *testing.T) {
	fake := &fakeProvidersStore{fakeSiteConfigStore: &fakeSiteConfigStore{}}
	srv, _ := newProvidersHarness(t, fake)

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		do(t, srv, http.MethodPut, "/api/v1/workspace-providers", adminToken,
			`{"git":[{"id":"gh","kind":"github","base_urls":["https://github.com/acme"]}]}`)
	}()
	go func() {
		defer wg.Done()
		do(t, srv, http.MethodPut, "/api/v1/site-config", adminToken,
			`{"scm_hosts":["github.example.com"]}`)
	}()
	wg.Wait()

	final, err := fake.GetSiteConfig(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	// Whichever landed second carried the other's value forward: providers
	// because /site-config's body did not MENTION the key (the carry-forward),
	// scm_hosts because the providers handler rewrites only its own field.
	if final.WorkspaceProviders == nil || len(final.WorkspaceProviders.Git) != 1 {
		t.Errorf("provider block = %+v after two concurrent writers, want it preserved", final.WorkspaceProviders)
	}
	if len(final.ScmHosts) != 1 {
		t.Errorf("scm_hosts = %v after two concurrent writers, want it preserved", final.ScmHosts)
	}
}

// TestSiteConfigDoorWritesProviders pins the OTHER door — the one MDM uses. A
// present key is validated and stored; an ABSENT key carries the stored block
// forward (or every boot of an MDM-managed laptop would delete the org's provider
// policy); an explicit {} clears it.
func TestSiteConfigDoorWritesProviders(t *testing.T) {
	fake := &fakeProvidersStore{fakeSiteConfigStore: &fakeSiteConfigStore{}}
	srv, _ := newProvidersHarness(t, fake)

	w := do(t, srv, http.MethodPut, "/api/v1/site-config", adminToken,
		`{"workspace_providers":{"git":[{"id":"gh","kind":"github","base_urls":["https://github.com/acme"]}]}}`)
	if w.Code != http.StatusOK {
		t.Fatalf("PUT = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	if fake.putSeen.WorkspaceProviders == nil {
		t.Fatal("the provider block was not stored through PUT /site-config")
	}

	// A 0.7.1-shaped body does not mention the key.
	w = do(t, srv, http.MethodPut, "/api/v1/site-config", adminToken, `{"scm_hosts":["github.example.com"]}`)
	if w.Code != http.StatusOK {
		t.Fatalf("PUT = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	if fake.putSeen.WorkspaceProviders == nil {
		t.Error("an older client's silence deleted the provider policy — the MDM file re-applied on every " +
			"boot would wipe it")
	}

	w = do(t, srv, http.MethodPut, "/api/v1/site-config", adminToken, `{"workspace_providers":{}}`)
	if w.Code != http.StatusOK {
		t.Fatalf("PUT = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	if fake.putSeen.WorkspaceProviders != nil {
		t.Errorf("{} did not clear the block: %+v", fake.putSeen.WorkspaceProviders)
	}

	// And the same validator guards this door.
	w = do(t, srv, http.MethodPut, "/api/v1/site-config", adminToken,
		`{"workspace_providers":{"git":[{"id":"gh","kind":"github","base_urls":["http://github.com/acme"]}]}}`)
	if w.Code != http.StatusBadRequest {
		t.Errorf("a malformed block through PUT /site-config = %d, want 400; body=%s", w.Code, w.Body.String())
	}
}

// TestSiteConfigDoorReportsWhatItNarrows is fix 1: the CLI/MDM door is where a
// narrowing actually lands unwatched (a laptop re-applies its site-config file
// on every boot), so it owes the same count the providers page gets — in the
// body AND in the audit row. Absent entirely when the body named no block, so a
// 0.7.1-shaped apply is unchanged.
func TestSiteConfigDoorReportsWhatItNarrows(t *testing.T) {
	fake := &fakeProvidersStore{
		fakeSiteConfigStore: &fakeSiteConfigStore{},
		workspaces: []types.Workspace{{
			ID: uuid.New(), Sources: []types.WorkspaceSource{
				{Type: types.WorkspaceSourceTypeRepo, Source: "https://github.com/acme/keep.git"},
				{Type: types.WorkspaceSourceTypeRepo, Source: "https://github.com/other/cut.git"},
			},
		}},
	}
	srv, audit := newProvidersHarness(t, fake)

	w := do(t, srv, http.MethodPut, "/api/v1/site-config", adminToken,
		`{"workspace_providers":{"git":[{"id":"gh","kind":"github","base_urls":["https://github.com/acme"]}]}}`)
	if w.Code != http.StatusOK {
		t.Fatalf("PUT = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var resp siteConfigPutResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.SourcesNoLongerAdmitted == nil || *resp.SourcesNoLongerAdmitted != 1 {
		t.Errorf("sources_no_longer_admitted = %v, want 1 (the other-org workspace source)",
			resp.SourcesNoLongerAdmitted)
	}
	var datum map[string]any
	for _, ev := range audit.events {
		if ev.Action == "site_config.write" {
			if err := json.Unmarshal(ev.Data, &datum); err != nil {
				t.Fatal(err)
			}
		}
	}
	if got := datum["sources_no_longer_admitted"]; got != float64(1) {
		t.Errorf("audit sources_no_longer_admitted = %v, want 1", got)
	}

	// A body that names no provider block reports nothing: there is no narrowing
	// to speak of, and a 0 would read as one that was measured.
	w = do(t, srv, http.MethodPut, "/api/v1/site-config", adminToken, `{"scm_hosts":["github.example.com"]}`)
	if w.Code != http.StatusOK {
		t.Fatalf("PUT = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), "sources_no_longer_admitted") {
		t.Errorf("a body naming no provider block still reported a count: %s", w.Body.String())
	}
}

// TestSiteConfigDoorFailsRatherThanMiscount is the same honesty rule as the
// providers door: an unreadable source list must never come back as "0 affected".
func TestSiteConfigDoorFailsRatherThanMiscount(t *testing.T) {
	fake := &fakeProvidersStore{fakeSiteConfigStore: &fakeSiteConfigStore{}, listErr: context.DeadlineExceeded}
	srv, _ := newProvidersHarness(t, fake)
	w := do(t, srv, http.MethodPut, "/api/v1/site-config", adminToken,
		`{"workspace_providers":{"git":[{"id":"gh","kind":"github","base_urls":["https://github.com/acme"]}]}}`)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("PUT with an unreadable workspace list = %d, want 500; body=%s", w.Code, w.Body.String())
	}
	if fake.putSeen != nil {
		t.Error("the block was stored anyway — the admin would have been told nothing was affected")
	}
}

// TestSiteConfigWriteAuditNamesProviders pins the two datum fields that make an
// MDM-applied narrowing visible at all — this door's audit row is all an incident
// review has for a provider block written by the CLI.
func TestSiteConfigWriteAuditNamesProviders(t *testing.T) {
	fake := &fakeProvidersStore{fakeSiteConfigStore: &fakeSiteConfigStore{}}
	srv, audit := newProvidersHarness(t, fake)
	w := do(t, srv, http.MethodPut, "/api/v1/site-config", adminToken,
		`{"workspace_providers":{"git":[`+
			`{"id":"gh","kind":"github","base_urls":["https://github.com/acme"]},`+
			`{"id":"ghes","kind":"github","disabled":true,"base_urls":["https://git.corp.example/acme"]}],`+
			`"storage":{"user_drive":{"max_size_mib":10240}}}}`)
	if w.Code != http.StatusOK {
		t.Fatalf("PUT = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var datum map[string]any
	for _, ev := range audit.events {
		if ev.Action == "site_config.write" {
			if err := json.Unmarshal(ev.Data, &datum); err != nil {
				t.Fatal(err)
			}
		}
	}
	if datum == nil {
		t.Fatalf("no site_config.write event: %+v", audit.events)
	}
	if got := datum["git_providers"]; got != float64(1) {
		t.Errorf("git_providers = %v, want 1 (ENABLED rows only — the disabled row narrows nothing)", got)
	}
	if got := datum["storage_configured"]; got != true {
		t.Errorf("storage_configured = %v, want true", got)
	}
}

// TestSiteConfigGetProjectsEffectiveScmHosts pins the read-only projection and
// the one thing it must not do: change the ETag a later If-Match is compared
// against.
func TestSiteConfigGetProjectsEffectiveScmHosts(t *testing.T) {
	fake := &fakeProvidersStore{fakeSiteConfigStore: &fakeSiteConfigStore{cfg: types.SiteConfig{
		ScmHosts: []string{"github.com"},
		WorkspaceProviders: &types.WorkspaceProviders{Git: []types.GitProvider{
			githubRow("gh", true, "https://github.com/acme"),
			githubRow("ghes", false, "https://git.corp.example/acme"),
		}},
	}}}
	srv, _ := newProvidersHarness(t, fake)
	w := do(t, srv, http.MethodGet, "/api/v1/site-config", adminToken, "")
	if w.Code != http.StatusOK {
		t.Fatalf("GET = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var got types.SiteConfig
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	// github.com is claimed by the DISABLED row, so it is gone; the enabled GHES
	// row's host is in.
	if strings.Join(got.EffectiveScmHosts, ",") != "git.corp.example" {
		t.Errorf("effective_scm_hosts = %v, want [git.corp.example]", got.EffectiveScmHosts)
	}
	// The ETag hashes the STORED document: a projection-inclusive hash would be
	// one no If-Match could satisfy.
	etag := w.Header().Get("ETag")
	w = doWithHeaders(t, srv, http.MethodPut, "/api/v1/site-config", adminToken, `{}`,
		map[string]string{"If-Match": etag})
	if w.Code != http.StatusOK {
		t.Fatalf("the GET's own ETag did not satisfy the PUT (= %d) — the projection leaked into the hash; "+
			"body=%s", w.Code, w.Body.String())
	}
	if fake.putSeen.EffectiveScmHosts != nil {
		t.Errorf("effective_scm_hosts = %v was persisted; it is server-owned and read-only",
			fake.putSeen.EffectiveScmHosts)
	}
}

// TestSiteConfigGetIsByteIdenticalWithoutProviders is the upgrade pin on the
// WIRE: a 0.7.1-shaped document must not gain a workspace_providers key, which
// is the whole reason the SiteConfig field is a pointer.
func TestSiteConfigGetIsByteIdenticalWithoutProviders(t *testing.T) {
	fake := &fakeProvidersStore{fakeSiteConfigStore: &fakeSiteConfigStore{cfg: types.SiteConfig{
		UpstreamProxyURL: "http://proxy.corp.example:3128",
	}}}
	srv, _ := newProvidersHarness(t, fake)
	body := strings.TrimSpace(do(t, srv, http.MethodGet, "/api/v1/site-config", adminToken, "").Body.String())
	if body != `{"upstream_proxy_url":"http://proxy.corp.example:3128"}` {
		t.Errorf("GET body = %s, want the 0.7.1 shape byte-for-byte (no workspace_providers, no "+
			"effective_scm_hosts) — a value struct's omitempty is a no-op, which is why the field is a pointer",
			body)
	}
}
