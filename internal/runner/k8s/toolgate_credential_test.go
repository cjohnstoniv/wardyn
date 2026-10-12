// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build k8s

package k8s

import (
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/cjohnstoniv/wardyn/internal/runner"
)

// The tool-approval credential, in the shape dispatch hands it over (one
// agent-owned file under its reserved name, no env), reaches the agent pod as
// that file only: no container's env names it or references the run Secret key
// that holds it, and the run Secret holds it under no env key.
func TestToolgateCredential_NotInContainerEnv(t *testing.T) {
	const cred = "toolgate-credential-plaintext-0001"
	d, cs := newTestDriver(t, Config{})
	installProxyIPReactor(t, cs, "10.244.0.12")
	installAgentRunningReactor(t, cs)
	spec := testSandboxSpec()
	spec.SecretEnv = map[string]string{"OTHER_SECRET": "other-secret-value"}
	spec.ManagedFiles = []runner.ManagedFile{{
		Path: runner.ComponentSecretDir + "/wardyn-toolgate-claude-code", Mode: runner.ComponentSecretFileMode, AgentOwned: true, Content: []byte(cred),
	}}
	ctx := context.Background()
	sb, err := d.CreateSandbox(ctx, spec)
	if err != nil {
		t.Fatalf("CreateSandbox: %v", err)
	}
	if _, err := d.Exec(ctx, sb.Ref, []string{"agent-run"}); err != nil {
		t.Fatalf("Exec: %v", err)
	}
	sec, err := cs.CoreV1().Secrets(testNamespace).Get(ctx, secretName(spec.RunID), metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get run secret: %v", err)
	}
	credKeys := map[string]bool{}
	for k, v := range sec.Data {
		if string(v) == cred {
			credKeys[k] = true
			if strings.HasPrefix(k, secretEnvDataKey("")) {
				t.Errorf("the run Secret holds the credential under env key %q", k)
			}
		}
	}
	if len(credKeys) != 1 {
		t.Fatalf("the run Secret holds the credential under %v, want one file key", credKeys)
	}
	pod, err := cs.CoreV1().Pods(testNamespace).Get(ctx, sb.Ref, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get agent pod: %v", err)
	}
	if len(pod.Spec.EphemeralContainers) == 0 {
		t.Fatal("Exec added no ephemeral container")
	}
	envs := map[string][]corev1.EnvVar{}
	for _, c := range append(pod.Spec.InitContainers, pod.Spec.Containers...) {
		envs[c.Name] = c.Env
		if len(c.EnvFrom) != 0 {
			t.Errorf("container %s takes env from a whole source: %+v", c.Name, c.EnvFrom)
		}
	}
	for _, c := range pod.Spec.EphemeralContainers {
		envs[c.Name] = c.Env
		if len(c.EnvFrom) != 0 {
			t.Errorf("container %s takes env from a whole source: %+v", c.Name, c.EnvFrom)
		}
	}
	for name, env := range envs {
		for _, e := range env {
			if e.Name == "WARDYN_TOOLGATE_CREDENTIAL" || e.Value == cred ||
				(e.ValueFrom != nil && e.ValueFrom.SecretKeyRef != nil && credKeys[e.ValueFrom.SecretKeyRef.Key]) {
				t.Errorf("container %s env %s carries the credential", name, e.Name)
			}
		}
	}
	projected := false
	for _, v := range pod.Spec.Volumes {
		if v.Secret == nil {
			continue
		}
		for _, it := range v.Secret.Items {
			projected = projected || (credKeys[it.Key] && it.Path == "wardyn-toolgate-claude-code")
		}
	}
	if !projected {
		t.Error("the credential is not projected as its reserved file")
	}
}
