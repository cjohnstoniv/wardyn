// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build k8s

package conformance_test

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/runner/k8s"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/test/conformance"
)

// TestConformanceK8sKata starts a Linux VM sandbox as a pod under a real Kata RuntimeClass and
// proves the two things a fake clientset cannot: the guest runs a kernel of its own, and from inside
// it the apiserver is unreachable while the run's proxy is reachable (the coded probe above). It
// needs a cluster whose nodes run Kata (WARDYN_TEST_K8S_KATA_RUNTIMECLASS names the class) and
// /dev/kvm on this host; hosted conformance-k8s installs no Kata, so it skips there.
func TestConformanceK8sKata(t *testing.T) {
	rc := os.Getenv("WARDYN_TEST_K8S_KATA_RUNTIMECLASS")
	if os.Getenv("WARDYN_TEST_K8S") != "1" || rc == "" {
		t.Skip("WARDYN_TEST_K8S=1 and WARDYN_TEST_K8S_KATA_RUNTIMECLASS not both set; skipping the Kata VM case")
	}
	if _, err := os.Stat("/dev/kvm"); err != nil {
		t.Skipf("no /dev/kvm on this host (%v); Kata needs KVM", err)
	}
	hostKernel, err := os.ReadFile("/proc/sys/kernel/osrelease")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
	defer cancel()
	sub, err := k8s.New(k8s.Config{
		Namespace:           os.Getenv("WARDYN_K8S_NAMESPACE"),
		ProxyImage:          os.Getenv("WARDYN_PROXY_IMAGE"),
		ConfinementRuntimes: map[types.ConfinementClass]string{types.CC3: rc},
		// Record stays off: a Linux VM sandbox is not recorded in this release.
	})
	if err != nil {
		t.Fatalf("k8s.New: %v", err)
	}
	if _, err := sub.EnableLinuxVM(ctx); err != nil {
		t.Fatalf("EnableLinuxVM: %v", err)
	}
	sb, err := sub.CreateSandbox(ctx, runner.SandboxSpec{
		RunID:       uuid.New(),
		Image:       os.Getenv("WARDYN_TEST_K8S_AGENT_IMAGE"),
		MachineKind: types.MachineLinuxVM,
		Labels:      map[string]string{"wardyn.conformance": "true"},
		ProxyConfig: conformance.LoadableProxyConfig(),
	})
	if err != nil {
		t.Fatalf("CreateSandbox: %v", err)
	}
	defer func() { _ = sub.StopSandbox(context.Background(), sb.Ref) }()
	if sb.EnforcedClass != types.CC3 || !sb.Machine.Complete() {
		t.Fatalf("sandbox = %+v, want CC3 with a complete machine", sb)
	}
	run := func(script string) int {
		t.Helper()
		if _, err := sub.Exec(ctx, sb.Ref, []string{"sh", "-c", script}); err != nil {
			t.Fatalf("Exec: %v", err)
		}
		code, err := sub.Wait(ctx, sb.Ref)
		if err != nil {
			t.Fatalf("Wait: %v", err)
		}
		return code
	}
	if code := run(`[ "$(cat /proc/sys/kernel/osrelease)" != "` + strings.TrimSpace(string(hostKernel)) + `" ]`); code != 0 {
		t.Fatalf("the guest runs the host's kernel %s (exit %d): the pod is not a VM", hostKernel, code)
	}
	if code := run(apiServerProbeScript); code != 0 {
		t.Fatalf("apiserver probe from the VM exited %d (93 = breach; 90-92 = could not run)", code)
	}
}
