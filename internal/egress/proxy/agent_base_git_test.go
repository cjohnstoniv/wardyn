// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/adoscope"
)

// TestAgentBaseStub_ExecLaneGitGoesThroughTheBroker: agent-base's own
// agent-run (the stub, which runs exec-mode tasks — the CI/BYOA lane) must
// rewrite git onto the proxy's /wardyn/git/ broker before the task runs. On a
// per-person Azure DevOps lane the intercepted connection refuses git, so a
// stub that skipped the rewrite left every exec-mode `git` on agent-base
// refused. Drives the REAL stub, sourcing the REAL agent-run-lib.sh, with
// exactly the env dispatch writes, against the real proxy harness.
func TestAgentBaseStub_ExecLaneGitGoesThroughTheBroker(t *testing.T) {
	h := newADOGitHarness(t, adoscope.CapRead)
	_, self, _, _ := runtime.Caller(0)
	common := filepath.Join(filepath.Dir(self), "..", "..", "..", "deploy", "images", "common")
	stub, err := os.ReadFile(filepath.Join(common, "agent-run-stub"))
	if err != nil {
		t.Fatal(err)
	}
	// The stub sources the lib from its in-image path; point it at the tree's.
	const inImage = "source /usr/local/bin/agent-run-lib.sh"
	if !strings.Contains(string(stub), inImage) {
		t.Fatalf("agent-run-stub no longer sources %q; update this test", inImage)
	}
	patched := filepath.Join(t.TempDir(), "agent-run")
	if err := os.WriteFile(patched, []byte(strings.Replace(string(stub), inImage,
		"source "+filepath.Join(common, "agent-run-lib.sh"), 1)), 0o755); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command("bash", patched, "git ls-remote https://dev.azure.com/acme/proj/_git/app")
	cmd.Dir = h.work
	cmd.Env = append(h.env(), "WARDYN_TASK_MODE=exec", "WARDYN_PROXY_URL="+h.proxy+"/",
		"WARDYN_GIT_PAT_BROKER_HOSTS="+adoGitBrokerHosts, "HOME="+t.TempDir())
	out, err := cmd.CombinedOutput()
	if strings.Contains(string(out), h.bearer) {
		t.Fatalf("exec output carries the bearer:\n%s", out)
	}
	if err != nil || !strings.Contains(string(out), "refs/heads/main") {
		t.Fatalf("agent-base exec-mode git ls-remote: %v\n%s", err, out)
	}
	if n := len(h.fake.Requests()); n == 0 {
		t.Fatal("git reached nothing: the ls-remote never went through the broker")
	}
}
