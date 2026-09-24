// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package apie2e

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/pkg/client"
)

// TestADONames_ImportAndLaunch is #485 over the real HTTP API and a real
// database: an Azure DevOps repository whose project and repository names
// carry spaces and parentheses is onboarded as a person types it, stored in
// one canonical spelling (on the workspace AND in the source library, which
// names it after the repository), launched from that workspace, and reaches
// the sandbox's WARDYN_REPOS with a clone directory named after it. The run
// door canonicalises a directly-named repo the same way.
func TestADONames_ImportAndLaunch(t *testing.T) {
	const (
		typed     = "https://dev.azure.com/contoso/Payments Platform/_git/Card Auth (v2).Service"
		canonical = "https://dev.azure.com/contoso/Payments%20Platform/_git/Card%20Auth%20(v2).Service"
	)
	fr := newFakeRunner()
	h := newHarness(t, harnessOpts{withRunner: fr})
	ctx := context.Background()

	ws, err := h.sdk.CreateWorkspace(ctx, client.WorkspaceRequest{
		Name:    "payments-" + time.Now().Format("150405.000000"),
		Sources: []types.WorkspaceSource{{Type: types.WorkspaceSourceTypeRepo, Source: typed}},
	})
	if err != nil {
		t.Fatalf("CreateWorkspace(%q): %v", typed, err)
	}
	if got := ws.Sources[0].Source; got != canonical {
		t.Fatalf("workspace stored %q, want %q", got, canonical)
	}
	sources, err := h.sdk.ListSources(ctx)
	if err != nil {
		t.Fatalf("ListSources: %v", err)
	}
	if !slices.ContainsFunc(sources, func(s types.Source) bool {
		return s.Locator == canonical && s.Name == "Card Auth (v2).Service"
	}) {
		t.Errorf("no library source %q named %q in %+v", canonical, "Card Auth (v2).Service", sources)
	}

	run, err := h.sdk.CreateRun(ctx, client.CreateRunRequest{Agent: "claude-code", WorkspaceID: &ws.ID, Task: "spaced names"})
	if err != nil {
		t.Fatalf("CreateRun(workspace %s): %v", ws.ID, err)
	}
	if run.Repo != canonical || slices.ContainsFunc(run.Warnings, func(w string) bool { return strings.Contains(w, "Card") }) {
		t.Errorf("run repo %q warnings %v, want %q and no warning about it", run.Repo, run.Warnings, canonical)
	}
	var repos string
	if !waitFor(t, 5*time.Second, func() bool {
		s, ok := fr.specFor(run.ID)
		repos = s.Env["WARDYN_REPOS"]
		return ok
	}) {
		t.Fatal("the run was never dispatched")
	}
	if want := canonical + "\t/home/agent/work/Card-Auth-(v2).Service\t"; !strings.HasPrefix(repos, want) {
		t.Errorf("WARDYN_REPOS = %q, want a record starting %q", repos, want)
	}

	direct, err := h.sdk.CreateRun(ctx, client.CreateRunRequest{Agent: "claude-code", Repo: typed, Task: "spaced names, direct"})
	if err != nil {
		t.Fatalf("CreateRun(repo %q): %v", typed, err)
	}
	if direct.Repo != canonical {
		t.Errorf("a run naming %q stored %q, want %q", typed, direct.Repo, canonical)
	}
}
