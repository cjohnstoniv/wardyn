// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build live

package testlive

import (
	"cmp"
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/adoscope"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/pkg/client"
)

// TestLiveADO (LL2): the member, who has signed in to Wardyn and to Azure
// DevOps once, launches an exec run on an Azure DevOps repository. The run's
// command does a REST read and `git ls-remote`, both on the member's own
// captured credential; the harness holds no Azure DevOps token of its own.
//
// It runs twice: on the configured project and repository, and on a project
// and repository whose names carry a space and punctuation (#485) — named by
// WARDYN_LIVE_ADO_SPACED_PROJECT/_REPO, defaulting to the fixture names the
// tenant must hold (docs/LIVE-TESTS.md, LL2). The spaced run is launched with
// the URL typed as a person types it, spaces and all, so the run door's
// canonicalisation is on the path too.
func TestLiveADO(t *testing.T) {
	Require(t, EnvADO, EnvBaseURL, EnvIdentities, EnvADOOrg, EnvADOProject, EnvADORepo)
	ids, err := LoadIdentities()
	if err != nil {
		Fatalf(t, "%v", err)
	}
	member := ids["member"]
	if member.APIToken == "" {
		// Missing once WARDYN_LIVE_ADO=1 has been opted into is a broken
		// fixture, not an absence of intent (#463): Fatalf, not Skipf.
		Fatalf(t, "live: the identities file has no member api_token. Sign in to the console once as the member, "+
			"connect Azure DevOps when asked, create an API token under Settings, and add it as member.api_token (docs/LIVE-TESTS.md, LL2)")
	}
	org := os.Getenv(EnvADOOrg)
	for name, pr := range map[string][2]string{
		"configured": {os.Getenv(EnvADOProject), os.Getenv(EnvADORepo)},
		"spaced":     {cmp.Or(os.Getenv(EnvADOSpacedProject), "Payments Platform"), cmp.Or(os.Getenv(EnvADOSpacedRepo), "Card Auth (v2).Service")},
	} {
		t.Run(name, func(t *testing.T) { liveADORun(t, member.APIToken, org, pr[0], pr[1]) })
	}
}

// liveADORun launches one LL2 run on org/project/repo and waits for it.
func liveADORun(t *testing.T, apiToken, org, project, repo string) {
	projectURL := "https://dev.azure.com/" + org + "/" + adoscope.EscapeName(project)
	repoURL := projectURL + "/_git/" + adoscope.EscapeName(repo)
	task := fmt.Sprintf(`set -eu
code=$(curl -sS -o /dev/null -w '%%{http_code}' %s)
echo "rest=$code"
test "$code" = 200
git ls-remote %s HEAD >/dev/null
echo ls-remote=ok`, shellQuote(projectURL+"/_apis/git/repositories?api-version=7.1"), shellQuote(repoURL))

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	c := client.New(os.Getenv(EnvBaseURL), apiToken)
	created, err := c.CreateRun(ctx, client.CreateRunRequest{
		Agent: "claude-code", Repo: "https://dev.azure.com/" + org + "/" + project + "/_git/" + repo,
		Task: task, TaskMode: "exec", Title: "live-local LL2",
	})
	if err != nil {
		Fatalf(t, "create run: %v", err)
	}
	for _, w := range created.Warnings {
		Logf(t, "warning: %s", w)
	}
	run := created.AgentRun
	if run.Repo != repoURL {
		Fatalf(t, "run %s stored repo %q, want the canonical %q", run.ID, run.Repo, repoURL)
	}
	for !run.State.IsTerminal() {
		select {
		case <-ctx.Done():
			Fatalf(t, "run %s still %s after 15 minutes", run.ID, run.State)
		case <-time.After(5 * time.Second):
		}
		if run, err = c.GetRun(ctx, run.ID); err != nil {
			Fatalf(t, "get run: %v", err)
		}
	}
	if run.State != types.RunCompleted {
		Fatalf(t, "run %s ended %s: %s. If the hint names a missing Azure DevOps sign-in, sign in to Azure DevOps "+
			"once as the member in the console and re-run; if the repository is not found, create it (docs/LIVE-TESTS.md, LL2)",
			run.ID, run.State, run.FailureHint)
	}
	Logf(t, "run %s COMPLETED: REST read 200 and git ls-remote ok", run.ID)
}

// shellQuote single-quotes s for sh: an Azure DevOps repository name may hold
// an apostrophe.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
