// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// lockdownPolicy stands in for the rendered policy cfg.BrowserPolicy carries when the lockdown is on.
var lockdownPolicy = []byte(`{"DeveloperToolsAvailability":2,"ExtensionInstallBlocklist":["*"]}`)

const lockdownRefusal = "this deployment locks down the sandbox browser"

// browserLockdownDispatch dispatches a run with cfg.BrowserPolicy set to policy (nil: off), edited by
// edit before dispatch.
func browserLockdownDispatch(t *testing.T, fr *fakeRunner, policy []byte, edit func(*types.AgentRun)) (runner.SandboxSpec, *dispatchTestStore) {
	t.Helper()
	srv, st, _, run := dispatchTeardownFixture(t, fr, types.RunPending)
	srv.cfg.BrowserPolicy = policy
	if edit != nil {
		edit(&run)
	}
	srv.dispatchRun(context.Background(), run, ceilingForDispatch(governanceCeiling{}, adoEntraUngraded(), bedrockCredUngraded()), dispatchParams{
		RunToken: "run-token", Image: "wardyn/claude-code:latest",
	})
	return fr.lastSpec, st
}

// chromiumPolicyFiles are the spec's managed files at runner.ChromiumPolicyFile.
func chromiumPolicyFiles(spec runner.SandboxSpec) []runner.ManagedFile {
	var out []runner.ManagedFile
	for _, f := range spec.ManagedFiles {
		if f.Path == runner.ChromiumPolicyFile {
			out = append(out, f)
		}
	}
	return out
}

// TestBrowserLockdown_OffDeliversNothing: with the lockdown off the run is dispatched as before, with
// no policy file and no refusal.
func TestBrowserLockdown_OffDeliversNothing(t *testing.T) {
	fr := &fakeRunner{noManagedFiles: true}
	spec, st := browserLockdownDispatch(t, fr, nil, nil)
	if st.state == types.RunFailed && strings.Contains(st.failureHint, lockdownRefusal) {
		t.Fatalf("off refused the run: %s", st.failureHint)
	}
	if fr.createCalls == 0 {
		t.Fatalf("off: CreateSandbox was never called (state %s, hint %q)", st.state, st.failureHint)
	}
	if got := chromiumPolicyFiles(spec); len(got) != 0 {
		t.Fatalf("off delivered %+v", got)
	}
}

// TestBrowserLockdown_OnDeliversTheRootOwnedFile: with the lockdown on, the dispatched spec carries
// cfg.BrowserPolicy at runner.ChromiumPolicyFile, root-owned, and the delivery accepts it.
func TestBrowserLockdown_OnDeliversTheRootOwnedFile(t *testing.T) {
	spec, st := browserLockdownDispatch(t, &fakeRunner{}, lockdownPolicy, nil)
	if st.state == types.RunFailed {
		t.Fatalf("run failed: %s", st.failureHint)
	}
	got := chromiumPolicyFiles(spec)
	if len(got) != 1 || got[0].AgentOwned || !bytes.Equal(got[0].Content, lockdownPolicy) {
		t.Fatalf("policy files %+v, want one root-owned file holding cfg.BrowserPolicy", got)
	}
	if err := runner.ValidateManagedFiles(spec.ManagedFiles); err != nil {
		t.Fatalf("ValidateManagedFiles: %v", err)
	}
}

// browserLockdownRefused asserts the run was refused at dispatch with the lockdown's hint, and want.
func browserLockdownRefused(t *testing.T, name string, fr *fakeRunner, st *dispatchTestStore, want string) {
	t.Helper()
	if fr.createCalls != 0 || st.state != types.RunFailed || !strings.Contains(st.failureHint, lockdownRefusal) || !strings.Contains(st.failureHint, want) {
		t.Errorf("%s: CreateSandbox calls %d, state %s, hint %q; want a refused run whose hint names the lockdown and %q", name, fr.createCalls, st.state, st.failureHint, want)
	}
}

// TestBrowserLockdown_RefusesARunnerThatCannotDeliver: a runner that does not deliver managed files
// cannot carry the policy, so the run never starts unlocked.
func TestBrowserLockdown_RefusesARunnerThatCannotDeliver(t *testing.T) {
	fr := &fakeRunner{noManagedFiles: true}
	_, st := browserLockdownDispatch(t, fr, lockdownPolicy, nil)
	browserLockdownRefused(t, "no managed files", fr, st, "does not deliver managed files")
}

// TestBrowserLockdown_RefusesExecLessUnverified: on an exec-less runtime (krun), whose guest may run
// the agent as root, the runner cannot vouch for the policy.
func TestBrowserLockdown_RefusesExecLessUnverified(t *testing.T) {
	fr := &fakeRunner{capsResolved: map[types.ConfinementClass]string{types.CC1: "oci/krun"}}
	_, st := browserLockdownDispatch(t, fr, lockdownPolicy, nil)
	browserLockdownRefused(t, "krun", fr, st, krunManagedFilesUnverified)
}

// TestBrowserLockdown_RefusesWhenCapabilitiesUnreadable: unknown capabilities refuse the run; the
// hint is the refusal sentence, never the driver's own error text.
func TestBrowserLockdown_RefusesWhenCapabilitiesUnreadable(t *testing.T) {
	fr := &fakeRunner{capsErr: errors.New("docker info: connection refused")}
	_, st := browserLockdownDispatch(t, fr, lockdownPolicy, nil)
	browserLockdownRefused(t, "capabilities unreadable", fr, st, "capabilities could not be confirmed")
	if strings.Contains(st.failureHint, "connection refused") {
		t.Errorf("failure hint = %q, must not carry the driver's own error text", st.failureHint)
	}
}

// TestBrowserLockdown_RefusesMacOSVM: a macOS VM guest has no place for the policy, so a macOS VM run
// is refused at dispatch while the lockdown is on.
func TestBrowserLockdown_RefusesMacOSVM(t *testing.T) {
	fr := &fakeRunner{}
	_, st := browserLockdownDispatch(t, fr, lockdownPolicy, func(r *types.AgentRun) { r.MachineKind = types.MachineMacOSVM })
	browserLockdownRefused(t, "macOS VM", fr, st, "a macOS virtual machine cannot take the browser policy yet")
}
