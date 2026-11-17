// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"errors"
	"fmt"
	"net/http"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/store"
)

// TestRecordLaunchAuditDetailNamesAFaultsStepNeverItsText: a run.record.start failure row keeps the sentence of a refusal
// the route answers in its own words, and for every other failure names only the step, never the error, which can
// name a database.
func TestRecordLaunchAuditDetailNamesAFaultsStepNeverItsText(t *testing.T) {
	db := errors.New("dial tcp 10.0.0.5:5432: connect: connection refused")
	for name, c := range map[string]struct {
		err  error
		want string
	}{
		"a store fault":                {fmt.Errorf("create run: %w", db), recordAuditLaunchFault},
		"an unreadable sign-in state":  {fmt.Errorf("%w: read site config: %w", errGitCredentialUnreadable, db), recordAuditLaunchFault},
		"an unreadable provider block": {fmt.Errorf("%w: %w", errModelProvidersUnreadable, db), recordAuditLaunchFault},
		"a provider credential read":   {&modelProviderReadError{err: db}, recordAuditLaunchFault},
		"the deployment run cap":       {fmt.Errorf("create run: %w", store.ErrRunCapReached), recordAuditLaunchFault},
		"a workspace list read":        {&sourceRefusal{status: http.StatusInternalServerError, reason: reasonWorkspaceSourcesListUnavailable, err: errors.New("the workspace list could not be read")}, "workspace: the workspace list could not be read"},
		"a refused mount":              {&sourceRefusal{status: http.StatusUnprocessableEntity, reason: "r", err: errors.New("the folder is outside the roots")}, "workspace: the folder is outside the roots"},
		"an agent not offered":         {fmt.Errorf("%w: codex is not offered here", errAgentNotEnabled), "agent not enabled: codex is not offered here"},
		"a governance limit":           {fmt.Errorf("%w: at most 2 runs", errRecordCeilingLimit), "governance profile limit: at most 2 runs"},
		"a stored source target":       {fmt.Errorf("%w: bad target", errWorkspaceSourceTarget), "invalid workspace source target: bad target"},
		"a repository not admitted":    {fmt.Errorf("%w: not on an enabled provider", errRepoNotAdmitted), "repo not admitted: not on an enabled provider"},
		"a credential in a repo URL":   {errRepoURLUserinfo, repoURLUserinfo400},
		"a stale groups snapshot":      {errGroupsSnapshotStale, groupsSnapshotStaleMsg},
		"a code host sign-in missing":  {&gitCredentialRefusalError{Sentence: "connect Azure DevOps first"}, "connect Azure DevOps first"},
		"a provider refused":           {&modelProviderRefusal{choice: runProviderChoice{refusal: "no provider is granted"}}, "model provider refused: no provider is granted"},
	} {
		if got := recordLaunchAuditDetail(c.err); got != c.want {
			t.Errorf("%s: detail = %q, want %q", name, got, c.want)
		}
	}
}
