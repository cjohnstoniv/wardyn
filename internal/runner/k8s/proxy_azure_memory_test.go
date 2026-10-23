// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build k8s

package k8s

import (
	"context"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/cjohnstoniv/wardyn/internal/egress/proxy"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// A run carrying an Azure route gate gets the larger proxy envelope on its proxy container; a run
// without one keeps the deployment's.
func TestCreateSandbox_ProxyPodMemoryFollowsTheAzureGate(t *testing.T) {
	t.Cleanup(func() { runner.SetProxyLimits(0, 0) })
	runner.SetProxyLimits(0, 0)
	for _, tc := range []struct {
		name  string
		gates []proxy.AzureGateConfig
		want  int64
	}{
		{"no gate", nil, runner.DefaultProxyMemoryMiB},
		{"gate", []proxy.AzureGateConfig{{Host: "h.test", Route: types.AzureRouteAnthropic, Models: []string{"m"}}}, runner.AzureProxyMemoryMiB},
	} {
		d, cs := newTestDriver(t, Config{})
		installProxyIPReactor(t, cs, "10.244.0.7")
		installAgentRunningReactor(t, cs)
		spec := testSandboxSpec()
		spec.ProxyConfig.AzureGates = tc.gates
		if _, err := d.CreateSandbox(context.Background(), spec); err != nil {
			t.Fatalf("%s: CreateSandbox: %v", tc.name, err)
		}
		pod, err := cs.CoreV1().Pods(testNamespace).Get(context.Background(), proxyPodName(spec.RunID), metav1.GetOptions{})
		if err != nil {
			t.Fatalf("%s: get proxy pod: %v", tc.name, err)
		}
		if got := pod.Spec.Containers[0].Resources.Limits.Memory().Value(); got != tc.want*1024*1024 {
			t.Errorf("%s: proxy container memory limit = %d, want %d MiB", tc.name, got, tc.want)
		}
	}
}

// The proxy container is Ready only once it accepts a connection, which it does only after its
// startup resolve succeeded; without the probe waitPodIP returns the moment the container runs.
func TestCreateSandbox_ProxyContainerHasAReadinessProbeOnItsListener(t *testing.T) {
	d, cs := newTestDriver(t, Config{})
	installProxyIPReactor(t, cs, "10.244.0.7")
	installAgentRunningReactor(t, cs)
	spec := testSandboxSpec()
	if _, err := d.CreateSandbox(context.Background(), spec); err != nil {
		t.Fatalf("CreateSandbox: %v", err)
	}
	pod, err := cs.CoreV1().Pods(testNamespace).Get(context.Background(), proxyPodName(spec.RunID), metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get proxy pod: %v", err)
	}
	probe := pod.Spec.Containers[0].ReadinessProbe
	if pod.Spec.Containers[0].Name != proxyContainerName || probe == nil || probe.TCPSocket == nil ||
		probe.TCPSocket.Port.IntValue() != runner.ProxyListenPort {
		t.Fatalf("proxy container readiness probe = %+v, want a TCP probe on %d", probe, runner.ProxyListenPort)
	}
}
