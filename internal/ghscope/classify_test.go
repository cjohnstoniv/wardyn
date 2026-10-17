// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package ghscope

import (
	"net/http"
	"strings"
	"testing"
)

// rest is a request to the REST API, written "METHOD /path".
func rest(line string) Request {
	method, path, _ := strings.Cut(line, " ")
	return Request{Method: method, Host: APIHost, Path: path}
}

// in is a path under the repository every repo case uses.
const in = "/repos/acme/app"

type restCase struct {
	line string
	want Capability
}

// classifyAll runs one table of REST lines that must classify, and holds
// every verdict to wantRepo and wantOwner.
func classifyAll(t *testing.T, cases []restCase, wantRepo, wantOwner string) {
	t.Helper()
	for _, tc := range cases {
		t.Run(tc.line, func(t *testing.T) {
			got, err := Classify(rest(tc.line))
			if err != nil {
				t.Fatalf("Classify() refused with %v, want %q", err, tc.want)
			}
			if got.Capability != tc.want || got.Repo != wantRepo || got.Owner != wantOwner {
				t.Fatalf("Classify() = %+v, want {%s %s %s}", got, tc.want, wantRepo, wantOwner)
			}
		})
	}
}

// The repository routes, by the capability each needs. Every verdict, the
// refusals included, names the repository, so the caller's pin applies
// whatever the request classifies as.
func TestClassifyRepositoryRoutes(t *testing.T) {
	classifyAll(t, []restCase{
		{"GET " + in, CapMetadata},
		{"HEAD " + in, CapMetadata},
		{"GET " + in + "/languages", CapMetadata},
		{"GET " + in + "/license", CapMetadata},
		{"GET " + in + "/topics", CapMetadata},
		{"GET " + in + "/tags", CapMetadata},
		{"GET " + in + "/contributors", CapMetadata},
		{"GET " + in + "/collaborators", CapMetadata},
		{"GET " + in + "/collaborators/bob/permission", CapMetadata},
		{"GET " + in + "/rulesets/5", CapMetadata},
		{"GET " + in + "/rules/branches/main", CapMetadata},

		{"GET " + in + "/branches", CapCodeRead},
		{"GET " + in + "/branches/main", CapCodeRead},
		{"GET " + in + "/commits", CapCodeRead},
		{"GET " + in + "/commits/abc123", CapCodeRead},
		{"GET " + in + "/commits/abc123/pulls", CapCodeRead},
		{"GET " + in + "/commits/heads/main/status", CapCodeRead},
		{"GET " + in + "/compare/main...feature", CapCodeRead},
		{"GET " + in + "/contents", CapCodeRead},
		{"GET " + in + "/contents/README.md", CapCodeRead},
		{"GET " + in + "/contents/.github/workflows/ci.yml", CapCodeRead},
		{"GET " + in + "/contents/dir%20one/50%25off.md", CapCodeRead},
		{"GET " + in + "/readme", CapCodeRead},
		{"GET " + in + "/git/trees/abc123", CapCodeRead},
		{"GET " + in + "/git/blobs/abc123", CapCodeRead},
		{"GET " + in + "/git/ref/heads/main", CapCodeRead},
		{"GET " + in + "/git/refs/heads/main", CapCodeRead},
		{"GET " + in + "/git/matching-refs/heads/feat", CapCodeRead},
		{"GET " + in + "/releases", CapCodeRead},
		{"GET " + in + "/releases/latest", CapCodeRead},
		{"GET " + in + "/releases/assets/5", CapCodeRead},
		{"GET " + in + "/tarball/main", CapCodeRead},
		{"GET " + in + "/zipball/main", CapCodeRead},
		{"GET " + in + "/activity", CapCodeRead},
		{"GET " + in + "/pulls", CapCodeRead},
		{"GET " + in + "/pulls/5", CapCodeRead},
		{"GET " + in + "/pulls/5/files", CapCodeRead},
		{"GET " + in + "/pulls/5/commits", CapCodeRead},
		{"GET " + in + "/pulls/5/merge", CapCodeRead},
		{"GET " + in + "/pulls/5/reviews/9/comments", CapCodeRead},
		{"GET " + in + "/pulls/comments", CapCodeRead},
		{"GET " + in + "/pulls/comments/7", CapCodeRead},

		{"POST " + in + "/pulls", CapPR},
		{"PATCH " + in + "/pulls/5", CapPR},
		{"POST " + in + "/pulls/5/reviews", CapPR},
		{"PUT " + in + "/pulls/5/reviews/9/dismissals", CapPR},
		{"POST " + in + "/pulls/5/comments", CapPR},
		{"POST " + in + "/pulls/5/comments/3/replies", CapPR},
		{"POST " + in + "/pulls/5/requested_reviewers", CapPR},
		{"DELETE " + in + "/pulls/5/requested_reviewers", CapPR},
		{"PATCH " + in + "/pulls/comments/7", CapPR},
		{"DELETE " + in + "/pulls/comments/7", CapPR},
		{"POST " + in + "/pulls/comments/7/reactions", CapPR},

		{"GET " + in + "/issues", CapIssuesRead},
		{"GET " + in + "/issues/5", CapIssuesRead},
		{"GET " + in + "/issues/5/comments", CapIssuesRead},
		{"GET " + in + "/issues/5/timeline", CapIssuesRead},
		{"GET " + in + "/issues/comments/9", CapIssuesRead},
		{"GET " + in + "/labels/bug", CapIssuesRead},
		{"GET " + in + "/milestones/1/labels", CapIssuesRead},
		{"GET " + in + "/assignees/bob", CapIssuesRead},
		{"POST " + in + "/issues", CapIssuesWrite},
		{"PATCH " + in + "/issues/5", CapIssuesWrite},
		{"POST " + in + "/issues/5/comments", CapIssuesWrite},
		{"PATCH " + in + "/issues/comments/9", CapIssuesWrite},
		{"DELETE " + in + "/issues/comments/9", CapIssuesWrite},
		{"PUT " + in + "/issues/5/lock", CapIssuesWrite},
		{"DELETE " + in + "/issues/5/labels/bug", CapIssuesWrite},
		{"POST " + in + "/labels", CapIssuesWrite},
		{"POST " + in + "/milestones", CapIssuesWrite},

		{"GET " + in + "/actions/runs", CapActionsRead},
		{"GET " + in + "/actions/runs/5/jobs", CapActionsRead},
		{"GET " + in + "/actions/runs/5/logs", CapActionsRead},
		{"GET " + in + "/actions/jobs/7/logs", CapActionsRead},
		{"GET " + in + "/actions/workflows/ci.yml/runs", CapActionsRead},
		{"GET " + in + "/actions/artifacts/3/zip", CapActionsRead},
		{"GET " + in + "/actions/caches", CapActionsRead},
		{"GET " + in + "/actions/cache/usage", CapActionsRead},
		{"POST " + in + "/actions/workflows/ci.yml/dispatches", CapActionsExecute},
		{"POST " + in + "/actions/runs/5/rerun", CapActionsExecute},
		{"POST " + in + "/actions/runs/5/rerun-failed-jobs", CapActionsExecute},
		{"POST " + in + "/actions/runs/5/cancel", CapActionsExecute},
		{"POST " + in + "/actions/runs/5/force-cancel", CapActionsExecute},
		{"POST " + in + "/actions/jobs/7/rerun", CapActionsExecute},
		{"PUT " + in + "/actions/permissions", CapRepoAdmin},
		{"PUT " + in + "/actions/permissions/access", CapRepoAdmin},
		{"PUT " + in + "/actions/workflows/ci.yml/enable", CapActionsAdmin},
		{"PUT " + in + "/actions/workflows/ci.yml/disable", CapActionsAdmin},
		{"POST " + in + "/actions/runs/5/approve", CapActionsAdmin},
		{"DELETE " + in + "/actions/runs/5", CapActionsAdmin},
		{"DELETE " + in + "/actions/runs/5/logs", CapActionsAdmin},
		{"DELETE " + in + "/actions/artifacts/3", CapActionsAdmin},
		{"DELETE " + in + "/actions/caches", CapActionsAdmin},
		{"DELETE " + in + "/actions/caches/9", CapActionsAdmin},

		{"PATCH " + in, CapRepoAdmin},
		{"DELETE " + in, CapRepoAdmin},
		{"PUT " + in + "/collaborators/bob", CapRepoAdmin},
		{"DELETE " + in + "/collaborators/bob", CapRepoAdmin},
		{"PUT " + in + "/topics", CapRepoAdmin},
		{"POST " + in + "/rulesets", CapRepoAdmin},
		{"DELETE " + in + "/rulesets/5", CapRepoAdmin},
		{"GET " + in + "/branches/main/protection", CapRepoAdmin},
		{"PUT " + in + "/branches/main/protection", CapRepoAdmin},
		{"DELETE " + in + "/branches/main/protection/enforce_admins", CapRepoAdmin},
		{"GET " + in + "/branches/wardyn/run/x/protection", CapRepoAdmin},
		{"PUT " + in + "/branches/feat/x/protection/required_status_checks", CapRepoAdmin},
		{"GET " + in + "/branches/feat/x", CapCodeRead},
	}, "acme/app", "acme")
}

// What no run is granted under a repository: the refusal table, the ref
// moves, and every area nobody added.
func TestClassifyRepositoryRefusals(t *testing.T) {
	classifyAll(t, []restCase{
		{"GET " + in + "/hooks", CapDeniedHooks},
		{"POST " + in + "/hooks", CapDeniedHooks},
		{"GET " + in + "/hooks/1/deliveries", CapDeniedHooks},
		{"GET " + in + "/keys", CapDeniedTokens},
		{"POST " + in + "/keys", CapDeniedTokens},
		{"GET " + in + "/actions/runners", CapDeniedTokens},
		{"POST " + in + "/actions/runners/registration-token", CapDeniedTokens},
		{"GET " + in + "/actions/secrets", CapDeniedSecrets},
		{"GET " + in + "/actions/secrets/public-key", CapDeniedSecrets},
		{"PUT " + in + "/actions/secrets/TOKEN", CapDeniedSecrets},
		{"GET " + in + "/actions/organization-secrets", CapDeniedSecrets},
		{"PUT " + in + "/dependabot/secrets/TOKEN", CapDeniedSecrets},
		{"GET " + in + "/codespaces/secrets", CapDeniedSecrets},
		{"PUT " + in + "/environments/prod/secrets/TOKEN", CapDeniedSecrets},

		{"POST " + in, CapUnclassifiedWrite},
		{"PUT " + in, CapUnclassifiedWrite},
		{"POST " + in + "/forks", CapUnclassifiedWrite},
		{"POST " + in + "/dispatches", CapUnclassifiedWrite},
		{"POST " + in + "/transfer", CapUnclassifiedWrite},
		{"POST " + in + "/pages", CapUnclassifiedWrite},
		{"POST " + in + "/deployments", CapUnclassifiedWrite},
		{"POST " + in + "/statuses/abc123", CapUnclassifiedWrite},
		{"POST " + in + "/check-runs", CapUnclassifiedWrite},
		{"PUT " + in + "/environments/prod", CapUnclassifiedWrite},
		{"POST " + in + "/codespaces", CapUnclassifiedWrite},
		{"POST " + in + "/autolinks", CapUnclassifiedWrite},
		{"PUT " + in + "/vulnerability-alerts", CapUnclassifiedWrite},
		{"POST " + in + "/generate", CapUnclassifiedWrite},
		{"POST " + in + "/commits/abc123/comments", CapUnclassifiedWrite},
		{"POST " + in + "/branches/main", CapUnclassifiedWrite},
		{"DELETE " + in + "/tags/protection/1", CapUnclassifiedWrite},
		{"DELETE " + in + "/pulls", CapUnclassifiedWrite},
		{"DELETE " + in + "/pulls/5", CapUnclassifiedWrite},
		{"POST " + in + "/pulls/5", CapUnclassifiedWrite},
		{"POST " + in + "/pulls/5/codespaces", CapUnclassifiedWrite},
		{"POST " + in + "/pulls/comments", CapUnclassifiedWrite},
		{"PUT " + in + "/pulls/latest/merge", CapUnclassifiedWrite},
		{"POST " + in + "/actions/runs", CapUnclassifiedWrite},
		{"POST " + in + "/actions/runs/5/pending_deployments", CapUnclassifiedWrite},
		{"POST " + in + "/actions/runs/5/deployment_protection_rule", CapUnclassifiedWrite},
		{"POST " + in + "/actions/workflows/ci.yml/enable", CapUnclassifiedWrite},
		{"DELETE " + in + "/actions/workflows/ci.yml", CapUnclassifiedWrite},
		{"PUT " + in + "/actions/oidc/customization/sub", CapUnclassifiedWrite},
		{"POST " + in + "/actions/variables", CapUnclassifiedWrite},

		{"GET " + in + "/forks", CapUnclassifiedRead},
		{"GET " + in + "/pages", CapUnclassifiedRead},
		{"GET " + in + "/deployments", CapUnclassifiedRead},
		{"GET " + in + "/check-runs/5", CapUnclassifiedRead},
		{"GET " + in + "/environments", CapUnclassifiedRead},
		{"GET " + in + "/code-scanning/alerts", CapUnclassifiedRead},
		{"GET " + in + "/secret-scanning/alerts", CapUnclassifiedRead},
		{"GET " + in + "/dependabot/alerts", CapUnclassifiedRead},
		{"GET " + in + "/traffic/clones", CapUnclassifiedRead},
		{"GET " + in + "/invitations", CapUnclassifiedRead},
		{"GET " + in + "/codespaces", CapUnclassifiedRead},
		{"GET " + in + "/stargazers", CapUnclassifiedRead},
		{"GET " + in + "/events", CapUnclassifiedRead},
		{"GET " + in + "/teams", CapUnclassifiedRead},
		{"GET " + in + "/installation", CapUnclassifiedRead},
		{"GET " + in + "/merges", CapUnclassifiedRead},
		{"GET " + in + "/pulls/5/codespaces", CapUnclassifiedRead},
		{"GET " + in + "/pulls/latest", CapUnclassifiedRead},
		{"GET " + in + "/actions", CapUnclassifiedRead},
		{"GET " + in + "/actions/variables", CapUnclassifiedRead},
		{"GET " + in + "/actions/permissions", CapUnclassifiedRead},
	}, "acme/app", "acme")
}

// A1: every REST write to the repository's git data is its own class, so the
// gate refuses it by class. Endpoints from GitHub's REST reference: Git
// database (refs, blobs, trees, commits, tags), Repository contents, Commits
// (merging a branch), Branches (rename; sync a fork), Pull requests (merge,
// update branch), Releases and release assets, Source imports, and Code
// scanning autofix commits. Reads of the same routes keep code_read.
func TestClassifyRESTContentWrites(t *testing.T) {
	var cases []restCase
	for _, line := range []string{
		"POST /git/refs", "PATCH /git/refs/heads/main", "PATCH /git/refs/heads/wardyn/run/x",
		"DELETE /git/refs/heads/feature", "DELETE /git/refs/tags/v1", "PUT /git/refs/heads/main",
		"POST /git/blobs", "POST /git/trees", "POST /git/commits", "POST /git/tags",
		"PATCH /git/commits/abc123", "POST /git/blobs/abc123", "DELETE /git/tags/v1",
		"PUT /contents/README.md", "DELETE /contents/a/b.txt", "PUT /contents/.github/workflows/ci.yml",
		"POST /contents/x", "PATCH /contents",
		"POST /merges", "POST /merge-upstream",
		"POST /branches/main/rename", "POST /branches/wardyn/run/x/rename",
		"POST /branches/wardyn/run1/protection/rename", "POST /branches/feat/protection/x/rename",
		"PUT /pulls/5/merge", "PUT /pulls/5/merge-async", "PUT /pulls/5/update-branch",
		"POST /pulls/5/merge",
		"POST /releases", "PATCH /releases/5", "DELETE /releases/5",
		"PATCH /releases/assets/9", "DELETE /releases/assets/9", "POST /releases/generate-notes",
		"PUT /import", "PATCH /import", "DELETE /import", "PATCH /import/authors/3", "PATCH /import/lfs",
		"POST /code-scanning/alerts/3/autofix/commits",
	} {
		method, path, _ := strings.Cut(line, " ")
		cases = append(cases, restCase{method + " " + in + path, CapRepoContentWriteREST})
	}
	classifyAll(t, cases, "acme/app", "acme")

	every := GrantableCapabilities()
	for _, tc := range cases {
		v, _ := Classify(rest(tc.line))
		if Permits(every, []string{"acme/app"}, v) || !v.Capability.Denied() {
			t.Errorf("%s: a run holding every capability may make it", tc.line)
		}
	}
}

// Organisation routes name their organisation, so a caller can hold them to
// the owners of the run's repositories.
func TestClassifyOrganizationRoutes(t *testing.T) {
	const org = "/orgs/acme"
	classifyAll(t, []restCase{
		{"GET " + org, CapOrgRead},
		{"GET " + org + "/members", CapOrgRead},
		{"GET " + org + "/members/bob", CapOrgRead},
		{"GET " + org + "/public_members", CapOrgRead},
		{"GET " + org + "/outside_collaborators", CapOrgRead},
		{"GET " + org + "/teams", CapOrgRead},
		{"GET " + org + "/teams/eng/members", CapOrgRead},
		{"GET " + org + "/memberships/bob", CapOrgRead},
		{"GET " + org + "/invitations", CapOrgRead},
		{"GET " + org + "/repos", CapOrgRead},

		{"PATCH " + org, CapOrgAdmin},
		{"DELETE " + org + "/members/bob", CapOrgAdmin},
		{"PUT " + org + "/memberships/bob", CapOrgAdmin},
		{"POST " + org + "/teams", CapOrgAdmin},
		{"PUT " + org + "/teams/eng/repos/acme/app", CapOrgAdmin},
		{"POST " + org + "/invitations", CapOrgAdmin},
		{"PUT " + org + "/outside_collaborators/bob", CapOrgAdmin},

		{"POST " + org + "/repos", CapRepoAdmin},

		{"GET " + org + "/packages", CapPackagesRead},
		{"GET " + org + "/packages/npm/left-pad", CapPackagesRead},
		{"GET " + org + "/packages/npm/left-pad/versions/3", CapPackagesRead},
		{"GET " + org + "/docker/conflicts", CapUnclassifiedRead},
		{"DELETE " + org + "/packages/npm/left-pad", CapPackagesWrite},
		{"DELETE " + org + "/packages/npm/left-pad/versions/3", CapPackagesWrite},
		{"POST " + org + "/packages/npm/left-pad/restore", CapPackagesWrite},
		{"POST " + org + "/packages/npm/left-pad/versions/3/restore", CapPackagesWrite},
		{"POST " + org + "/packages/npm/left-pad", CapUnclassifiedWrite},
		{"PUT " + org + "/packages/npm/left-pad/versions/3", CapUnclassifiedWrite},

		{"GET " + org + "/hooks", CapDeniedHooks},
		{"POST " + org + "/hooks", CapDeniedHooks},
		{"GET " + org + "/actions/secrets", CapDeniedSecrets},
		{"PUT " + org + "/dependabot/secrets/TOKEN", CapDeniedSecrets},
		{"GET " + org + "/private-registries", CapDeniedSecrets},
		{"GET " + org + "/actions/runners", CapDeniedTokens},
		{"POST " + org + "/actions/runners/registration-token", CapDeniedTokens},
		{"GET " + org + "/personal-access-tokens", CapDeniedTokens},
		{"POST " + org + "/personal-access-token-requests", CapDeniedTokens},
		{"GET " + org + "/installations", CapDeniedTokens},
		{"GET " + org + "/credential-authorizations", CapDeniedTokens},

		{"DELETE " + org, CapUnclassifiedWrite},
		{"DELETE " + org + "/repos", CapUnclassifiedWrite},
		{"GET " + org + "/repos/app", CapUnclassifiedRead},
		{"GET " + org + "/audit-log", CapUnclassifiedRead},
		{"GET " + org + "/actions/variables", CapUnclassifiedRead},
		{"GET " + org + "/copilot/billing", CapUnclassifiedRead},
	}, "", "acme")
}

// Everything that names neither a repository nor an organisation: the
// person's own account, search, GraphQL, the token doors, and the top-level
// areas nobody added. None of it is a plain read.
func TestClassifyAccountAndTopLevelRoutes(t *testing.T) {
	classifyAll(t, []restCase{
		{"GET /", CapMetadata},
		{"GET /rate_limit", CapMetadata},
		{"HEAD /rate_limit", CapMetadata},
		{"GET /meta", CapMetadata},
		{"GET /zen", CapMetadata},

		{"GET /user", CapIdentity},
		{"HEAD /user", CapIdentity},
		{"GET /user/orgs", CapDeniedAccount},
		{"GET /user/teams", CapDeniedAccount},
		{"GET /user/memberships/orgs", CapDeniedAccount},
		{"GET /user/memberships/orgs/acme", CapDeniedAccount},

		{"PATCH /user", CapDeniedAccount},
		{"GET /user/repos", CapDeniedAccount},
		{"POST /user/repos", CapDeniedAccount},
		{"GET /user/keys", CapDeniedAccount},
		{"POST /user/keys", CapDeniedAccount},
		{"POST /user/gpg_keys", CapDeniedAccount},
		{"GET /user/emails", CapDeniedAccount},
		{"GET /user/installations", CapDeniedAccount},
		{"GET /user/installations/1/repositories", CapDeniedAccount},
		{"GET /user/codespaces", CapDeniedAccount},
		{"GET /user/packages", CapDeniedAccount},
		{"GET /user/issues", CapDeniedAccount},
		{"GET /user/orgs/acme", CapDeniedAccount},
		{"PATCH /user/memberships/orgs/acme", CapDeniedAccount},

		{"POST /graphql", CapDeniedGraphQL},
		{"GET /graphql", CapDeniedGraphQL},
		{"POST /graphql/anything", CapDeniedGraphQL},
		{"GET /search/code", CapDeniedSearch},
		{"GET /search/repositories", CapDeniedSearch},
		{"GET /search/issues", CapDeniedSearch},
		{"GET /search/labels", CapDeniedSearch},
		{"GET /search", CapDeniedSearch},

		{"GET /app", CapDeniedTokens},
		{"POST /app/installations/1/access_tokens", CapDeniedTokens},
		{"GET /installation/repositories", CapDeniedTokens},
		{"DELETE /installation/token", CapDeniedTokens},
		{"POST /applications/Iv1.abc/token", CapDeniedTokens},
		{"GET /authorizations", CapDeniedTokens},
		{"POST /app-manifests/abc/conversions", CapDeniedTokens},
		{"POST /credentials/revoke", CapDeniedTokens},

		{"POST /", CapUnclassifiedWrite},
		{"POST /markdown", CapUnclassifiedWrite},
		{"DELETE /repositories/123", CapDeniedAccount},
		{"PUT /notifications", CapDeniedAccount},
		{"POST /gists", CapDeniedAccount},
		{"POST /rate_limit", CapUnclassifiedWrite},
		{"GET /repos", CapUnclassifiedRead},
		{"GET /repos/acme", CapUnclassifiedRead},
		{"GET /orgs", CapUnclassifiedRead},
		{"GET /repositories", CapDeniedAccount},
		{"GET /repositories/123", CapDeniedAccount},
		{"GET /repositories/123/pulls", CapDeniedAccount},
		{"GET /organizations/5/members", CapUnclassifiedRead},
		{"GET /users/octocat/repos", CapUnclassifiedRead},
		{"GET /teams/5/members", CapUnclassifiedRead},
		{"GET /gists", CapDeniedAccount},
		{"GET /issues", CapUnclassifiedRead},
		{"GET /notifications", CapDeniedAccount},
		{"GET /emojis", CapUnclassifiedRead},
		{"GET /enterprises/acme/audit-log", CapUnclassifiedRead},
		{"GET /rate_limit/extra", CapUnclassifiedRead},
		{"GET /acme/app.git/info/refs", CapUnclassifiedRead},
		{"POST /acme/app.git/git-receive-pack", CapUnclassifiedWrite},
	}, "", "")

	classifyAll(t, []restCase{
		{"GET /users/octo/packages", CapPackagesRead},
		{"GET /users/octo/packages/container/img/versions", CapPackagesRead},
		{"DELETE /users/octo/packages/container/img", CapPackagesWrite},
		{"POST /users/octo/packages/container/img/versions/4/restore", CapPackagesWrite},
		{"PATCH /users/octo/packages/container/img", CapUnclassifiedWrite},
	}, "", "octo")
}

// One request, many spellings: each classifies exactly as its plain spelling
// does, and names the same repository.
func TestClassifySpellingsOfOneRoute(t *testing.T) {
	for _, tc := range []struct {
		line, repo string
		want       Capability
	}{
		{"GET /repos/acme/app/", "acme/app", CapMetadata},
		{"GET /repos/acme/app/pulls/", "acme/app", CapCodeRead},
		{"GET /REPOS/Acme/App/Pulls", "acme/app", CapCodeRead},
		{"get /repos/acme/app/pulls", "acme/app", CapCodeRead},
		{"GET /repos/ACME/APP/HOOKS", "acme/app", CapDeniedHooks},
		{"GET /repos/acme/app/%68ooks", "acme/app", CapDeniedHooks},
		{"GET /repos/acme/app/%70ulls", "acme/app", CapCodeRead},
		{"POST /repos/acme/app/Git/Refs", "acme/app", CapRepoContentWriteREST},
		{"PUT /repos/acme/app/%63ontents/x", "acme/app", CapRepoContentWriteREST},
		{"PUT /repos/acme/app.git/pulls/5/Merge/", "acme/app", CapRepoContentWriteREST},
		{"GET /repos/acme/app.git", "acme/app", CapMetadata},
		{"GET /repos/acme/app.GIT/pulls", "acme/app", CapCodeRead},
		{"DELETE /repos/acme/app.git", "acme/app", CapRepoAdmin},
		{"GET /repos/acme/.github", "acme/.github", CapMetadata},
		{"GET /repos/octo_corp/My.Repo-1", "octo_corp/my.repo-1", CapMetadata},
		{"POST /GraphQL", "", CapDeniedGraphQL},
		{"GET /Search/code", "", CapDeniedSearch},
		{"GET /USER/keys", "", CapDeniedAccount},
		{"GET /User/", "", CapIdentity},
		{"POST /graphql/", "", CapDeniedGraphQL},
	} {
		t.Run(tc.line, func(t *testing.T) {
			got, err := Classify(rest(tc.line))
			if err != nil || got.Capability != tc.want || got.Repo != tc.repo {
				t.Fatalf("Classify() = %+v, %v, want %s on %q", got, err, tc.want, tc.repo)
			}
		})
	}
	for _, host := range []string{"API.GitHub.com", "api.github.com."} {
		if got, err := Classify(Request{Method: "GET", Host: host, Path: in}); err != nil || got.Capability != CapMetadata {
			t.Errorf("host %q = %+v, %v, want it read as %s", host, got, err, APIHost)
		}
	}
}

// A method override, were GitHub to honour one, turns a POST into another
// write on the same path. Classify refuses the header outright; beside that,
// no path here classifies to two different grantable capabilities across its
// write methods, so an override the classifier never saw could not raise what
// a request was permitted as.
func TestNoPathHasTwoGrantableWrites(t *testing.T) {
	for _, path := range []string{
		in, in + "/pulls", in + "/pulls/5", in + "/pulls/5/merge", in + "/pulls/5/reviews", in + "/pulls/comments/7",
		in + "/issues", in + "/issues/5", in + "/issues/5/comments", in + "/labels/bug", in + "/milestones/1",
		in + "/collaborators/bob", in + "/rulesets/5", in + "/topics", in + "/branches/main/protection",
		in + "/actions/runs/5", in + "/actions/runs/5/rerun", in + "/actions/runs/5/approve", in + "/actions/runs/5/logs",
		in + "/actions/workflows/ci.yml/dispatches", in + "/actions/workflows/ci.yml/enable", in + "/actions/caches",
		in + "/actions/artifacts/3", in + "/actions/jobs/7/rerun", in + "/actions/permissions",
		in + "/contents/a", in + "/git/refs", in + "/git/refs/heads/x", in + "/releases/5",
		"/orgs/acme", "/orgs/acme/repos", "/orgs/acme/members/bob", "/orgs/acme/teams/eng",
		"/orgs/acme/packages/npm/x", "/orgs/acme/packages/npm/x/restore", "/users/octo/packages/npm/x",
		"/user", "/graphql",
	} {
		seen := map[Capability]string{}
		for _, m := range []string{http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete} {
			v, err := Classify(Request{Method: m, Host: APIHost, Path: path})
			if err == nil && v.Capability.Grantable() {
				seen[v.Capability] = m
			}
		}
		if len(seen) > 1 {
			t.Errorf("%s: its write methods classify to %v", path, seen)
		}
	}
}
