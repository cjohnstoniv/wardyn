// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build k8s

package conformance_test

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	corev1 "k8s.io/api/core/v1"
	nodev1 "k8s.io/api/node/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/runner/k8s"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/test/conformance"
)

// TestConformanceK8sIsolatedPool proves the route a RuntimeClass's scheduling gives: a CC3 run's agent pod
// lands on the isolated node, and neither its proxy pod nor a CC1 run's agent pod does. It asserts
// scheduling only: a kind node has no Kata, so the agent container never starts. It needs a cluster with
// one node labelled pool=isolated and tainted isolated=true:NoSchedule (WARDYN_TEST_K8S_ISOLATED_NODE names
// it); scripts/test-kind-kata-place.sh builds that cluster and runs this case.
func TestConformanceK8sIsolatedPool(t *testing.T) {
	isolated := os.Getenv("WARDYN_TEST_K8S_ISOLATED_NODE")
	if os.Getenv("WARDYN_TEST_K8S") != "1" || isolated == "" {
		t.Skip("WARDYN_TEST_K8S=1 and WARDYN_TEST_K8S_ISOLATED_NODE not both set; scripts/test-kind-kata-place.sh runs this case on a kind cluster with an isolated worker")
	}
	cs, ns := conformanceClientset(t)
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()
	const class = "kata-sim"
	rc := &nodev1.RuntimeClass{ObjectMeta: metav1.ObjectMeta{Name: class}, Handler: "kata", Scheduling: &nodev1.Scheduling{
		NodeSelector: map[string]string{"pool": "isolated"},
		Tolerations:  []corev1.Toleration{{Key: "isolated", Operator: corev1.TolerationOpEqual, Value: "true", Effect: corev1.TaintEffectNoSchedule}},
	}}
	if _, err := cs.NodeV1().RuntimeClasses().Create(ctx, rc, metav1.CreateOptions{}); err != nil {
		t.Fatalf("create RuntimeClass: %v", err)
	}
	t.Cleanup(func() { _ = cs.NodeV1().RuntimeClasses().Delete(context.Background(), class, metav1.DeleteOptions{}) })
	sub, err := k8s.New(k8sSuiteConfig(k8s.Config{
		Namespaces:          []string{ns},
		ProxyImage:          os.Getenv("WARDYN_PROXY_IMAGE"),
		ConfinementRuntimes: map[types.ConfinementClass]string{types.CC3: class},
	}))
	if err != nil {
		t.Fatalf("k8s.New: %v", err)
	}
	for _, tc := range []struct {
		name         string
		class        types.ConfinementClass
		agentIsolate bool
	}{{"CC3 lands on the isolated node", types.CC3, true}, {"CC1 stays off it", types.CC1, false}} {
		t.Run(tc.name, func(t *testing.T) {
			id := uuid.New()
			cctx, stop := context.WithTimeout(ctx, 3*time.Minute)
			defer stop()
			done := make(chan struct{})
			go func() {
				defer close(done)
				// The agent container cannot start without Kata, so a CC3 create does not return before stop.
				_, _ = sub.CreateSandbox(cctx, runner.SandboxSpec{RunID: id, Image: os.Getenv("WARDYN_TEST_K8S_AGENT_IMAGE"),
					ConfinementClass: tc.class, Labels: map[string]string{"wardyn.conformance": "true"}, ProxyConfig: conformance.LoadableProxyConfig()})
			}()
			agent, proxy := scheduledNodes(cctx, t, cs, ns, id)
			stop()
			<-done
			if (agent == isolated) != tc.agentIsolate {
				t.Errorf("agent pod on node %q, want it on the isolated node %q: %v", agent, isolated, tc.agentIsolate)
			}
			if proxy == isolated {
				t.Errorf("proxy pod on the isolated node %q", proxy)
			}
			t.Logf("%s: agent pod on %s, proxy pod on %s", tc.class, agent, proxy)
			pods, _ := cs.CoreV1().Pods(ns).List(context.Background(), metav1.ListOptions{LabelSelector: "wardyn.run-id=" + id.String()})
			for _, p := range pods.Items {
				if p.Labels["wardyn.component"] == "agent" {
					_ = sub.StopSandbox(context.Background(), p.Name)
				}
			}
		})
	}
}

// scheduledNodes polls the run's pods until both the agent and the proxy have a node, and returns them.
func scheduledNodes(ctx context.Context, t *testing.T, cs kubernetes.Interface, ns string, id uuid.UUID) (agent, proxy string) {
	t.Helper()
	for {
		pods, err := cs.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{LabelSelector: "wardyn.run-id=" + id.String()})
		if err == nil {
			for _, p := range pods.Items {
				switch p.Labels["wardyn.component"] {
				case "agent":
					agent = p.Spec.NodeName
				case "proxy":
					proxy = p.Spec.NodeName
				}
			}
			if agent != "" && proxy != "" {
				return agent, proxy
			}
		}
		select {
		case <-ctx.Done():
			t.Fatalf("pods of run %s not both scheduled (agent %q, proxy %q): %v", id, agent, proxy, ctx.Err())
		case <-time.After(time.Second):
		}
	}
}
