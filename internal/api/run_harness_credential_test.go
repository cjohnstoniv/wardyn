// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/google/uuid"

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
				t.Fatalf("credential file %s (agent-owned %v, mode %04o): want one agent-owned file at mode %04o", f.Path, f.AgentOwned, f.Mode, runner.ComponentSecretFileMode)
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
	// The hold lane: the only one a credential is minted for.
	p.RunToken, p.Image, p.HarnessToolRules, p.ToolApprovals = "run-token", "img", twoHarnessSets, "hold"
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

// toolgateCredentialRows are run's run.toolgate_credential.write rows' data.
func toolgateCredentialRows(t *testing.T, events []types.AuditEvent, run uuid.UUID) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, ev := range events {
		if ev.Action != "run.toolgate_credential.write" || ev.RunID == nil || *ev.RunID != run {
			continue
		}
		var d map[string]any
		if err := json.Unmarshal(ev.Data, &d); err != nil {
			t.Fatal(err)
		}
		out = append(out, d)
	}
	return out
}

// A Docker host-path drive is a share, but the Docker driver delivers a secret
// file beside it, so the started harness keeps its own credential.
func TestToolgateCredential_DockerShareDriveDelivered(t *testing.T) {
	rn := &fakeRunner{}
	_, audit, run := dispatchTwoHarnessRun(t, rn, dispatchParams{Drives: []types.DriveMount{*dispatchDrive(false)}})
	toolgateCredentialOf(t, rn.lastSpec)
	if rows := toolgateCredentialRows(t, audit.events, run.ID); len(rows) != 0 {
		t.Errorf("withheld rows = %v, want none", rows)
	}
}

// Where no file can be delivered the run still launches and the started harness
// holds no credential, in any form; the proxy still has every digest, and the
// withheld credential is recorded with its reason.
func TestToolgateCredential_WithheldIsAudited(t *testing.T) {
	k8sShare := dispatchDrive(false)
	k8sShare.Backend = types.DriveBackendK8sPVCStatic
	for name, c := range map[string]struct {
		rn     *fakeRunner
		p      dispatchParams
		reason string
	}{
		"a runner that delivers no file": {&fakeRunner{noManagedFiles: true}, dispatchParams{}, "does not deliver managed files"},
		"a shared Kubernetes drive":      {&fakeRunner{}, dispatchParams{Drives: []types.DriveMount{*k8sShare}}, "the run mounts a shared Kubernetes drive"},
	} {
		t.Run(name, func(t *testing.T) {
			_, audit, run := dispatchTwoHarnessRun(t, c.rn, c.p)
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
				t.Errorf("digests = %d, want 2", len(spec.ProxyConfig.HarnessCredentials))
			}
			rows := toolgateCredentialRows(t, audit.events, run.ID)
			if len(rows) != 1 || rows[0]["delivered"] != false || rows[0]["agent"] != "claude-code" ||
				rows[0]["path"] != runner.ComponentSecretDir+"/"+toolgateCredentialFilePrefix+"claude-code" ||
				!strings.Contains(fmt.Sprint(rows[0]["reason"]), c.reason) {
				t.Errorf("withheld rows = %v, want one delivered:false row whose reason names %q", rows, c.reason)
			}
		})
	}
}
