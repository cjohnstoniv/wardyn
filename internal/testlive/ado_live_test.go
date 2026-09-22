// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build live

package testlive

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/pkg/client"
)

// TestLiveADO (LL2): the member, who has signed in to Wardyn and to Azure
// DevOps once, launches an exec run on an Azure DevOps repository. The run's
// command does a REST read and `git ls-remote`, both on the member's own
// captured credential; the harness holds no Azure DevOps token of its own.
func TestLiveADO(t *testing.T) {
	Require(t, EnvADO, EnvBaseURL, EnvIdentities, EnvADOOrg, EnvADOProject, EnvADORepo)
	ids, err := LoadIdentities()
	if err != nil {
		Fatalf(t, "%v", err)
	}
	member := ids["member"]
	if member.APIToken == "" {
		Skipf(t, "live: the identities file has no member api_token. Sign in to the console once as the member, "+
			"connect Azure DevOps when asked, create an API token under Settings, and add it as member.api_token (docs/LIVE-TESTS.md, LL2)")
	}
	org, project, repo := os.Getenv(EnvADOOrg), os.Getenv(EnvADOProject), os.Getenv(EnvADORepo)
	repoURL := fmt.Sprintf("https://dev.azure.com/%s/%s/_git/%s", org, project, repo)
	task := fmt.Sprintf(`set -eu
code=$(curl -sS -o /dev/null -w '%%{http_code}' 'https://dev.azure.com/%s/%s/_apis/git/repositories?api-version=7.1')
echo "rest=$code"
test "$code" = 200
git ls-remote '%s' HEAD >/dev/null
echo ls-remote=ok`, org, project, repoURL)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	c := client.New(os.Getenv(EnvBaseURL), member.APIToken)
	created, err := c.CreateRun(ctx, client.CreateRunRequest{
		Agent: "claude-code", Repo: repoURL, Task: task, TaskMode: "exec", Title: "live-local LL2",
	})
	if err != nil {
		Fatalf(t, "create run: %v", err)
	}
	for _, w := range created.Warnings {
		Logf(t, "warning: %s", w)
	}
	run := created.AgentRun
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
			"once as the member in the console and re-run (docs/LIVE-TESTS.md, LL2)", run.ID, run.State, run.FailureHint)
	}
	Logf(t, "run %s COMPLETED: REST read 200 and git ls-remote ok", run.ID)
}
