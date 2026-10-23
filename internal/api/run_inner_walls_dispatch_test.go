// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"bytes"
	"context"
	"os"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/agentpolicy"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// codexSandboxDispatch dispatches a Codex run whose walls froze walls at class.
func codexSandboxDispatch(t *testing.T, fr *fakeRunner, walls *types.HarnessInnerWalls, class types.ConfinementClass) (runner.SandboxSpec, *dispatchTestStore) {
	t.Helper()
	srv, st, _, run := dispatchTeardownFixture(t, fr, types.RunPending)
	run.Agent, run.InnerWalls, run.ConfinementClass = codexHarness, walls, class
	srv.dispatchRun(context.Background(), run, ceilingForDispatch(governanceCeiling{}, adoEntraUngraded(), bedrockCredUngraded()), dispatchParams{
		RunToken: "run-token", Image: "wardyn/codex-cli:latest",
	})
	return fr.lastSpec, st
}

// TestCodexSandboxDispatchDeliversTheRequirements: a Codex run whose walls froze a sandbox mode gets
// that mode's requirements, byte for byte the exercised golden, at the path Codex reads, and the
// mode agent-run launches codex exec in. Walls with no sandbox mode bring neither.
func TestCodexSandboxDispatchDeliversTheRequirements(t *testing.T) {
	all := []types.ConfinementClass{types.CC1, types.CC2, types.CC3}
	for _, mode := range []string{types.InnerSandboxReadOnly, types.InnerSandboxWorkspaceWrite} {
		golden, err := os.ReadFile("../agentpolicy/testdata/codex-requirements-" + mode + ".toml")
		if err != nil {
			t.Fatal(err)
		}
		spec, st := codexSandboxDispatch(t, &fakeRunner{capsClasses: all}, innerWalls(mode, types.InnerApprovalsNone), types.CC3)
		if st.state == types.RunFailed {
			t.Fatalf("%s: run failed: %s", mode, st.failureHint)
		}
		if len(spec.ManagedFiles) != 1 || spec.ManagedFiles[0].Path != agentpolicy.CodexRequirementsPath || !bytes.Equal(spec.ManagedFiles[0].Content, golden) {
			t.Errorf("%s: managed files %+v, want the one requirements golden %q", mode, spec.ManagedFiles, golden)
		}
		if err := runner.ValidateManagedFiles(spec.ManagedFiles); err != nil {
			t.Errorf("%s: ValidateManagedFiles: %v", mode, err)
		}
		if got := spec.Env["WARDYN_HARNESS_SANDBOX"]; got != mode {
			t.Errorf("%s: WARDYN_HARNESS_SANDBOX = %q", mode, got)
		}
	}
	for _, walls := range []*types.HarnessInnerWalls{nil, innerWalls(types.InnerSandboxOff, types.InnerApprovalsNone), innerWalls("", types.InnerApprovalsHarness)} {
		spec, _ := codexSandboxDispatch(t, &fakeRunner{capsClasses: all}, walls, types.CC3)
		if _, set := spec.Env["WARDYN_HARNESS_SANDBOX"]; set || len(spec.ManagedFiles) != 0 {
			t.Errorf("walls %+v: env set %v, files %+v, want neither", walls, set, spec.ManagedFiles)
		}
	}
}

// TestCodexSandboxDispatchRefusesARunnerThatCannotBindIt: where the requirements cannot be placed so
// the agent cannot rewrite them (no managed files, or krun, whose guest may run as root), the run is
// not launched: the sandbox it shows would be a wall nothing binds.
func TestCodexSandboxDispatchRefusesARunnerThatCannotBindIt(t *testing.T) {
	all := []types.ConfinementClass{types.CC1, types.CC2, types.CC3}
	for name, fr := range map[string]*fakeRunner{
		"no managed files": {capsClasses: all, noManagedFiles: true},
		"krun":             {capsClasses: all, capsResolved: map[types.ConfinementClass]string{types.CC3: "oci/krun"}},
	} {
		_, st := codexSandboxDispatch(t, fr, innerWalls(types.InnerSandboxReadOnly, types.InnerApprovalsNone), types.CC3)
		if fr.createCalls != 0 || st.state != types.RunFailed || !strings.Contains(st.failureHint, "binds Codex to its own sandbox") {
			t.Errorf("%s: CreateSandbox calls %d, state %s, hint %q, want a refused run", name, fr.createCalls, st.state, st.failureHint)
		}
	}
}
