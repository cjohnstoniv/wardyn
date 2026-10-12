// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/secretmask"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// twoHarnessSets is a run with two included harnesses, so each gets a credential.
var twoHarnessSets = []types.HarnessToolRules{
	{Harness: "claude-code", Rules: []types.ToolRule{{Tool: "Read", Effect: types.ToolAllow}}},
	{Harness: "codex-cli", Rules: []types.ToolRule{{Tool: "Read", Effect: types.ToolHold}}},
}

// toolgateCredentialOf is the started claude-code harness's credential as dispatch
// delivered it: one agent-owned 0400 file under its reserved name, and nowhere in
// the environment, secret or not.
func toolgateCredentialOf(t *testing.T, spec runner.SandboxSpec) string {
	t.Helper()
	want := runner.ComponentSecretDir + "/" + toolgateCredentialFilePrefix + "claude-code"
	cred := ""
	for _, f := range spec.ManagedFiles {
		if f.Path == want {
			if !f.AgentOwned || f.Mode != runner.ComponentSecretFileMode || cred != "" {
				t.Fatalf("credential file %+v: want one agent-owned file at mode %04o", f, runner.ComponentSecretFileMode)
			}
			cred = string(f.Content)
		}
	}
	if cred == "" {
		t.Fatalf("no credential file %s on the spec: %+v", want, spec.ManagedFiles)
	}
	for k, v := range spec.Env {
		if v == cred || k == "WARDYN_TOOLGATE_CREDENTIAL" {
			t.Errorf("Env[%s] carries the credential", k)
		}
	}
	for k, v := range spec.SecretEnv {
		if v == cred || k == "WARDYN_TOOLGATE_CREDENTIAL" {
			t.Errorf("SecretEnv[%s] carries the credential", k)
		}
	}
	return cred
}

func dispatchTwoHarnessRun(t *testing.T, rn *fakeRunner, p dispatchParams) (*Server, *recRecorder, types.AgentRun) {
	t.Helper()
	srv, _, audit, run := dispatchTeardownFixture(t, rn, types.RunPending)
	run.Task, run.CreatedBy = "", fileOwner
	_, sec, _ := fileSecretServer(t)
	srv.cfg.Secrets, srv.cfg.MaskRegistry = sec, secretmask.NewRegistry()
	p.RunToken, p.Image, p.HarnessToolRules = "run-token", "img", twoHarnessSets
	srv.dispatchRun(context.Background(), run, ceilingForDispatch(governanceCeiling{}, adoEntraUngraded(), bedrockCredUngraded()), p)
	if rn.createCalls != 1 {
		t.Fatalf("the sandbox was not created; events: %s", auditDump(audit.events, run.ID))
	}
	return srv, audit, run
}

// A member's file_secret grant naming a reserved credential file is skipped with
// its reason before any read, and the file on the spec is the minted credential.
func TestToolgateCredential_ReservedFileSecretNameSkipped(t *testing.T) {
	rn := &fakeRunner{}
	grant := fileSecretGrant(toolgateCredentialFilePrefix+"claude-code", "deploy-token", true)
	_, audit, run := dispatchTwoHarnessRun(t, rn, dispatchParams{Policy: types.RunPolicySpec{EligibleGrants: []types.GrantSpec{grant}}})
	if cred := toolgateCredentialOf(t, rn.lastSpec); cred == fileOwnValue {
		t.Error("the member's secret was delivered as the credential")
	}
	rows := fileResolveRows(t, audit.events, run.ID)
	if len(rows) != 1 || rows[0]["outcome"] != "failure" || rows[0]["reason"] != "names a file reserved for the run's tool-approval credential" {
		t.Errorf("resolve rows = %v, want the reserved-name skip", rows)
	}
	noEventCarries(t, audit.events, fileOwnValue)
}

// Where no file can be delivered the run still launches and the started harness
// holds no credential, in any form; the proxy still has every digest.
func TestToolgateCredential_NoFileNoEnvFallback(t *testing.T) {
	for name, c := range map[string]struct {
		rn *fakeRunner
		p  dispatchParams
	}{
		"a runner that delivers no file": {&fakeRunner{noManagedFiles: true}, dispatchParams{}},
		"a shared drive":                 {&fakeRunner{}, dispatchParams{Drive: dispatchDrive(false)}},
	} {
		t.Run(name, func(t *testing.T) {
			dispatchTwoHarnessRun(t, c.rn, c.p)
			spec := c.rn.lastSpec
			for _, f := range spec.ManagedFiles {
				if f.AgentOwned {
					t.Errorf("a credential file was delivered: %s", f.Path)
				}
			}
			if _, ok := spec.SecretEnv["WARDYN_TOOLGATE_CREDENTIAL"]; ok {
				t.Error("the credential fell back to env")
			}
			if len(spec.ProxyConfig.HarnessCredentials) != 2 {
				t.Errorf("digests = %+v", spec.ProxyConfig.HarnessCredentials)
			}
		})
	}
}
