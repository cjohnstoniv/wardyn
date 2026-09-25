// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build live

package testlive

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/adoscope"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/pkg/client"
)

// TestLiveADOBounded (LL2b): a member whose run holds SOME Azure DevOps access
// but not all of it. The provider row starts runs at `read`, the ceiling
// reaches `code_write` but not `repo_admin`, and the policy's first-use mode
// is deny_with_review. The run's command:
//
//  1. reads (REST 200 and a clone) on the dispatch grant alone;
//  2. pushes a scratch branch, which is refused and raises a code_write
//     request, then retries while the harness approves it "for this run";
//  3. deletes the branch again on the approved capability;
//  4. asks to create a repository — repo_admin, above the ceiling — and must
//     get 403 without any request being raised.
//
// The branch is inside the run's own namespace, refs/heads/wardyn/<run-id>/:
// the git broker counts every other ref as protected, so a push there asks for
// policy_bypass, not code_write (adoRunRefProtected, internal/egress/proxy).
//
// It writes to the repository (one scratch branch, deleted at step 3), so it
// has its own gate, WARDYN_LIVE_ADO_WRITE, instead of LL2's.
func TestLiveADOBounded(t *testing.T) {
	Require(t, EnvADOWrite, EnvBaseURL, EnvIdentities, EnvADOOrg, EnvADOProject, EnvADORepo)
	ids, err := LoadIdentities()
	if err != nil {
		Fatalf(t, "%v", err)
	}
	member := ids["member"]
	if member.APIToken == "" {
		// Missing once WARDYN_LIVE_ADO_WRITE=1 has been opted into is a broken
		// fixture, not an absence of intent (#463): Fatalf, not Skipf.
		Fatalf(t, "live: the identities file has no member api_token. Sign in to the console once as the member, "+
			"connect Azure DevOps when asked, create an API token under Settings, and add it as member.api_token (docs/LIVE-TESTS.md, LL2)")
	}
	org, project, repo := os.Getenv(EnvADOOrg), os.Getenv(EnvADOProject), os.Getenv(EnvADORepo)
	projectURL := "https://dev.azure.com/" + org + "/" + adoscope.EscapeName(project)
	repoURL := projectURL + "/_git/" + adoscope.EscapeName(repo)

	// Each failed step exits with its own code, named in the failure below.
	task := fmt.Sprintf(`set -u
export GIT_TERMINAL_PROMPT=0
code=$(curl -sS -o /dev/null -w '%%{http_code}' %[1]s)
echo "rest-read=$code"; test "$code" = 200 || exit 11
git clone -q %[2]s /tmp/ll2b || exit 12
cd /tmp/ll2b
git -c user.name=wardyn-live -c user.email=wardyn-live@invalid commit -q --allow-empty -m "wardyn live LL2b" || exit 13
ref="refs/heads/wardyn/${WARDYN_RUN_ID}/ll2b"
if git push -q origin "HEAD:$ref"; then echo push-before-approval=ALLOWED; exit 14; fi
echo push-before-approval=refused
i=0
until git push -q origin "HEAD:$ref"; do
  i=$((i+1)); test $i -ge 60 && exit 15
  sleep 5
done
echo push-after-approval=ok
git push -q origin ":$ref" || exit 16
echo branch-deleted=ok
code=$(curl -sS -o /dev/null -w '%%{http_code}' -X POST -H 'Content-Type: application/json' \
  -d '{"name":"wardyn-live-must-not-exist"}' %[3]s)
echo "above-ceiling=$code"; test "$code" = 403 || exit 17
`, shellQuote(projectURL+"/_apis/git/repositories?api-version=7.1"), shellQuote(repoURL),
		shellQuote(projectURL+"/_apis/git/repositories?api-version=7.1"))

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()
	c := client.New(os.Getenv(EnvBaseURL), member.APIToken)
	created, err := c.CreateRun(ctx, client.CreateRunRequest{
		Agent: "claude-code", Repo: repoURL, Task: task, TaskMode: "exec", Title: "live-local LL2b",
	})
	if err != nil {
		Fatalf(t, "create run: %v", err)
	}
	for _, w := range created.Warnings {
		Logf(t, "warning: %s", w)
	}
	run := created.AgentRun

	// Approve the code_write request "for this run" once it appears. Any other
	// capability being raised is a failure: read is dispatched, and repo_admin
	// is above the ceiling, so it must be refused without a request.
	approved := false
	for !run.State.IsTerminal() {
		select {
		case <-ctx.Done():
			Fatalf(t, "run %s still %s after 20 minutes (code_write approved: %v)", run.ID, run.State, approved)
		case <-time.After(5 * time.Second):
		}
		aps, err := c.ListApprovals(ctx, types.ApprovalPending, run.ID)
		if err != nil {
			Fatalf(t, "list approvals: %v", err)
		}
		for _, ap := range aps {
			var scope struct {
				Capability string `json:"capability"`
			}
			if ap.Kind != types.ApprovalToolCall || json.Unmarshal(ap.RequestedScope, &scope) != nil || scope.Capability == "" {
				continue
			}
			if scope.Capability != string(adoscope.CapCodeWrite) {
				Fatalf(t, "run %s raised a request for %q; only code_write should ever be asked for", run.ID, scope.Capability)
			}
			if _, err := c.Approve(ctx, ap.ID, "live-local LL2b", client.DecisionOpts{Scope: types.ScopeRun}); err != nil {
				Fatalf(t, "approve %s: %v", ap.ID, err)
			}
			approved = true
			Logf(t, "approved code_write for run %s (approval %s)", run.ID, ap.ID)
		}
		if run, err = c.GetRun(ctx, run.ID); err != nil {
			Fatalf(t, "get run: %v", err)
		}
	}
	if run.State != types.RunCompleted {
		Fatalf(t, "run %s ended %s: %s. Exit codes: 11 REST read, 12 clone, 13 commit, 14 push went through WITHOUT "+
			"approval (the row's default profile holds code_write, or the lane is not enforcing), 15 push still refused "+
			"after approval (or never approvable: the member has not consented to vso.code_write, so sign in to Azure "+
			"DevOps again from Settings), 16 branch delete, 17 repository create not refused (the ceiling holds repo_admin). "+
			"Branch refs/heads/wardyn/%s/ll2b may be left behind on exit 16 (docs/LIVE-TESTS.md, LL2b)", run.ID, run.State, run.FailureHint, run.ID)
	}
	if !approved {
		Fatalf(t, "run %s completed but no code_write request was ever raised", run.ID)
	}
	Logf(t, "run %s COMPLETED: read on dispatch, push held until approved, above-ceiling refused", run.ID)
}
