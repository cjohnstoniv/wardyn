// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build docker && unix

package docker

import (
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/runner"
)

func keptSpec(mode string) runner.SandboxSpec {
	s := macSpec()
	s.KeptMachine = &runner.KeptMachineSpec{ID: uuid.New(), Mode: mode, OwnerHash: "owner-hash", IdleDays: 30}
	return s
}

// A kept machine's session refuses, before anything exists, every secret env but the MCP routes and every
// file delivered in the component secret directory, naming each by key or path and never by value; the
// same spec without a kept machine is not refused for them.
func TestKeptSpecRefusesSecretEnvAndSecretFiles(t *testing.T) {
	r := newMacRig(t)
	for name, tc := range map[string]struct {
		mut  func(*runner.SandboxSpec)
		want string
	}{
		"secret env": {func(s *runner.SandboxSpec) { s.SecretEnv = map[string]string{"GH_TOKEN": "value-1"} }, "secret env GH_TOKEN"},
		"secret file": {func(s *runner.SandboxSpec) {
			s.ManagedFiles = []runner.ManagedFile{{Path: runner.ComponentSecretDir + "/db", Mode: runner.ComponentSecretFileMode, AgentOwned: true, Content: []byte("value-2")}}
		}, "secret file " + runner.ComponentSecretDir + "/db"},
		"both, with the routes": {func(s *runner.SandboxSpec) {
			s.SecretEnv = map[string]string{keptMCPSecretEnv: "value-3", "B_KEY": "value-4", "A_KEY": "value-5"}
		}, "secret env A_KEY, secret env B_KEY"},
	} {
		spec := keptSpec("resume")
		tc.mut(&spec)
		_, err := r.d.refuseMacVMSpec(spec)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: refusal %v, want it to name %q", name, err, tc.want)
			continue
		}
		if strings.Contains(err.Error(), "value-") || strings.Contains(err.Error(), keptMCPSecretEnv) {
			t.Errorf("%s: refusal %q names a value or the MCP routes", name, err)
		}
		spec.KeptMachine = nil
		if err := refuseKeptSecrets(spec); err != nil {
			t.Errorf("%s: a run with no kept machine was refused: %v", name, err)
		}
	}
	spec := keptSpec("new")
	spec.SecretEnv = map[string]string{keptMCPSecretEnv: "routes"}
	spec.ManagedFiles = []runner.ManagedFile{{Path: "/etc/wardyn/managed-settings.json", Content: []byte("{}")}}
	if err := refuseKeptSecrets(spec); err != nil {
		t.Fatalf("the MCP routes and a managed setting were refused: %v", err)
	}
}
