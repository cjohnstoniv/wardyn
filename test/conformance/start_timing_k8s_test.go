// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build k8s

package conformance_test

import (
	"cmp"
	"context"
	"os"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/google/uuid"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/runner/k8s"
	"github.com/cjohnstoniv/wardyn/internal/runner/orchestrator"
)

// TestK8sStartTiming measures sandbox starts on a live cluster: for each start, CreateSandbox's wall time, when the
// proxy pod read Ready and when the agent's main container started (both from the proxy pod's creation), and whether
// the first exec reaches the proxy. Opt-in (WARDYN_TEST_K8S=1 WARDYN_TEST_K8S_START_TIMING=1); the number of starts is
// WARDYN_TEST_K8S_START_TIMING_N, default 20. It uses only what an older checkout of this package already has, so the
// same file measures either.
func TestK8sStartTiming(t *testing.T) {
	if os.Getenv("WARDYN_TEST_K8S") != "1" || os.Getenv("WARDYN_TEST_K8S_START_TIMING") != "1" {
		t.Skip("WARDYN_TEST_K8S=1 and WARDYN_TEST_K8S_START_TIMING=1 are not both set; skipping the start timing")
	}
	proxyImage, agentImage := os.Getenv("WARDYN_PROXY_IMAGE"), os.Getenv("WARDYN_TEST_K8S_AGENT_IMAGE")
	if proxyImage == "" || agentImage == "" {
		t.Fatal("WARDYN_PROXY_IMAGE and WARDYN_TEST_K8S_AGENT_IMAGE must both be set")
	}
	n := 20
	if v := os.Getenv("WARDYN_TEST_K8S_START_TIMING_N"); v != "" {
		var err error
		if n, err = strconv.Atoi(v); err != nil || n < 1 {
			t.Fatalf("WARDYN_TEST_K8S_START_TIMING_N=%q is not a positive number", v)
		}
	}
	sub, err := k8s.New(k8sSuiteConfig(k8s.Config{
		Namespaces: []string{os.Getenv("WARDYN_K8S_NAMESPACE")},
		ProxyImage: proxyImage,
		Record:     true,
		ControlPlanePeer: &k8s.ControlPlanePeer{
			Namespace: cmp.Or(os.Getenv("WARDYN_K8S_NAMESPACE"), "default"),
			PodLabels: map[string]string{standInLabelKey: standInLabelValue},
		},
	}))
	if err != nil {
		t.Fatalf("k8s.New: %v", err)
	}
	r := orchestrator.New(sub)
	cs, ns := conformanceClientset(t)
	var creates []int64
	for i := 1; i <= n; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
		caps, err := r.Capabilities(ctx)
		if err != nil || len(caps.ConfinementClasses) == 0 {
			cancel()
			t.Fatalf("Capabilities: %v (%d classes)", err, len(caps.ConfinementClasses))
		}
		spec := runner.SandboxSpec{
			RunID:            uuid.New(),
			Image:            agentImage,
			ConfinementClass: caps.ConfinementClasses[len(caps.ConfinementClasses)-1],
			Labels:           map[string]string{"wardyn.conformance": "true"},
			ProxyConfig:      runner.ProxyConfig{ControlPlaneURL: "http://127.0.0.1:9", RunToken: "conformance"},
			Resources:        runner.Resources{CPUMillis: 250, MemoryMiB: 128},
		}
		start := time.Now()
		sb, err := r.CreateSandbox(ctx, spec)
		createMS := time.Since(start).Milliseconds()
		if err != nil {
			cancel()
			t.Fatalf("start=%d CreateSandbox: %v", i, err)
		}
		execStart := time.Now()
		code, _ := keepExec(ctx, t, r, sb.Ref, "nc -z -w2 wardyn-proxy 3128")
		execMS := time.Since(execStart).Milliseconds()
		proxyReady, agentRunning := startTimes(ctx, t, cs, ns, spec.RunID, sb.Ref)
		t.Logf("start=%d create_ms=%d proxy_ready=%.3f agent_running=%.3f first_exec_ok=%t first_exec_ms=%d",
			i, createMS, proxyReady, agentRunning, code == 0, execMS)
		creates = append(creates, createMS)
		stopCtx, stop := context.WithTimeout(context.Background(), time.Minute)
		if err := r.StopSandbox(stopCtx, sb.Ref); err != nil {
			t.Errorf("start=%d StopSandbox: %v", i, err)
		}
		stop()
		cancel()
	}
	slices.Sort(creates)
	t.Logf("create_ms median=%d p90=%d over %d starts", creates[len(creates)/2], creates[(len(creates)*9+9)/10-1], len(creates))
}

// startTimes reads the proxy pod's Ready transition and the agent's main container start, each in seconds after the
// proxy pod's creation; -1 for one that cannot be read.
func startTimes(ctx context.Context, t *testing.T, cs kubernetes.Interface, ns string, runID uuid.UUID, agentRef string) (float64, float64) {
	t.Helper()
	proxy, err := cs.CoreV1().Pods(ns).Get(ctx, "wardyn-proxy-"+runID.String(), metav1.GetOptions{})
	if err != nil {
		t.Errorf("read the proxy pod: %v", err)
		return -1, -1
	}
	born := proxy.CreationTimestamp.Time
	ready, running := -1.0, -1.0
	for _, c := range proxy.Status.Conditions {
		if c.Type == corev1.PodReady && c.Status == corev1.ConditionTrue {
			ready = c.LastTransitionTime.Sub(born).Seconds()
		}
	}
	agent, err := cs.CoreV1().Pods(ns).Get(ctx, agentRef, metav1.GetOptions{})
	if err != nil {
		t.Errorf("read the agent pod: %v", err)
		return ready, -1
	}
	for _, c := range agent.Status.ContainerStatuses {
		if c.Name == agent.Spec.Containers[0].Name && c.State.Running != nil {
			running = c.State.Running.StartedAt.Sub(born).Seconds()
		}
	}
	return ready, running
}
